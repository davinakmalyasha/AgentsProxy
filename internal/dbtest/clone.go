// Package dbtest provides database fixtures shared by integration tests.
//
// CloneSchema is the important one: it creates a real, tenant-owned copy of
// a project schema so a test tenant no longer shares project_centrum with
// the production tenant. Tables, defaults, indexes, primary/unique/exclusion
// constraints, foreign keys, triggers, RLS flags, policies, and grants are
// all reproduced. The source schema is never modified.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var schemaRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// SchemaPool returns an admin pool whose connections run with search_path
// set to schema. Test SQL can then use unqualified table names and be sure
// it addresses the clone, never the base schema.
func SchemaPool(tb testing.TB, ctx context.Context, dsn, schema string) *pgxpool.Pool {
	tb.Helper()
	if !schemaRe.MatchString(schema) {
		tb.Fatalf("dbtest: invalid schema %q", schema)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		tb.Fatalf("dbtest: parse dsn: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO "+pgx.Identifier{schema}.Sanitize())
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		tb.Fatalf("dbtest: schema pool: %v", err)
	}
	tb.Cleanup(pool.Close)
	return pool
}

// Table returns a safely quoted schema.table reference for test SQL.
func Table(schema, table string) string {
	return pgx.Identifier{schema, table}.Sanitize()
}

// CloneSchema creates project_test_<tenant>_<random> as a structural clone
// of source and registers a cleanup that drops it. The admin pool must be
// allowed to create schemas and read the source catalogs (the test admin
// DSN is).
func CloneSchema(tb testing.TB, ctx context.Context, admin *pgxpool.Pool, source, tenant string) string {
	tb.Helper()
	if !schemaRe.MatchString(source) {
		tb.Fatalf("dbtest: invalid source schema %q", source)
	}
	dest := cloneName(tenant)
	if err := cloneSchema(ctx, admin, source, dest); err != nil {
		tb.Fatalf("dbtest: clone %s -> %s: %v", source, dest, err)
	}
	tb.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.Exec(cctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{dest}.Sanitize()+" CASCADE"); err != nil {
			tb.Logf("dbtest: drop clone %s: %v", dest, err)
		}
	})
	return dest
}

// SharedSchema returns a per-process cached clone for source/tenant. Feature
// packages use this so each test resets one schema instead of creating and
// dropping seventeen tables again. The schema lives for the test binary; the
// test database is ephemeral in CI.
func SharedSchema(tb testing.TB, ctx context.Context, admin *pgxpool.Pool, source, tenant string) string {
	tb.Helper()
	key := source + "/" + tenant
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if dest, ok := sharedSchemas[key]; ok {
		return dest
	}
	if !schemaRe.MatchString(source) {
		tb.Fatalf("dbtest: invalid source schema %q", source)
	}
	dest := cloneName(tenant)
	if err := cloneSchema(ctx, admin, source, dest); err != nil {
		tb.Fatalf("dbtest: clone %s -> %s: %v", source, dest, err)
	}
	sharedSchemas[key] = dest
	return dest
}

var (
	sharedMu      sync.Mutex
	sharedSchemas = map[string]string{}
)

// ResetSchema empties every table in a clone. It is much cheaper than
// dropping and recreating the schema between tests.
func ResetSchema(ctx context.Context, admin *pgxpool.Pool, schema string) error {
	if !schemaRe.MatchString(schema) || !strings.HasPrefix(schema, "project_test_") {
		return fmt.Errorf("dbtest: refusing to reset %q", schema)
	}
	rows, err := admin.Query(ctx, `
		SELECT c.relname
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1
		   AND c.relkind = 'r'
		   AND NOT c.relispartition
		 ORDER BY c.relname`, schema)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, Table(schema, name))
	}
	_, err = admin.Exec(ctx, "TRUNCATE TABLE "+strings.Join(parts, ", ")+" CASCADE")
	return err
}

// DropSchema removes a clone immediately. Most tests use the t.Cleanup path.
func DropSchema(ctx context.Context, admin *pgxpool.Pool, schema string) error {
	if !schemaRe.MatchString(schema) || !strings.HasPrefix(schema, "project_test_") {
		return fmt.Errorf("dbtest: refusing to drop %q", schema)
	}
	_, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	return err
}

func cloneName(tenant string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(tenant) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	clean := strings.Trim(b.String(), "_")
	if clean == "" {
		clean = "tenant"
	}
	if len(clean) > 24 {
		clean = clean[:24]
	}
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("dbtest: crypto/rand: " + err.Error())
	}
	name := "project_test_" + clean + "_" + hex.EncodeToString(raw[:])
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

func cloneSchema(ctx context.Context, admin *pgxpool.Pool, source, dest string) error {
	tx, err := admin.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{dest}.Sanitize()); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}

	type table struct {
		name  string
		rls   bool
		force bool
	}
	rows, err := tx.Query(ctx, `
		SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1
		   AND c.relkind = 'r'
		   AND NOT c.relispartition
		 ORDER BY c.relname`, source)
	if err != nil {
		return fmt.Errorf("list tables: %w", err)
	}
	var tables []table
	for rows.Next() {
		var t table
		if err := rows.Scan(&t.name, &t.rls, &t.force); err != nil {
			rows.Close()
			return fmt.Errorf("scan table: %w", err)
		}
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("list tables: %w", err)
	}
	rows.Close()
	if len(tables) == 0 {
		return fmt.Errorf("source schema %q has no tables", source)
	}

	for _, t := range tables {
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			"CREATE UNLOGGED TABLE %s (LIKE %s INCLUDING ALL)",
			Table(dest, t.name), Table(source, t.name))); err != nil {
			return fmt.Errorf("clone table %s: %w", t.name, err)
		}
	}

	// Deleting a client cascades through every project table that carries
	// tenant_id -> clients. The source schema has a few tables without a
	// leading tenant index; in a database with many test clones that turns
	// one cleanup into dozens of sequential scans. Give every clone a cheap
	// leading index so the cascade is an index lookup.
	for _, t := range tables {
		var hasTenant, hasIndex bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
			         SELECT 1 FROM pg_attribute
			          WHERE attrelid = $1::regclass AND attname = 'tenant_id' AND NOT attisdropped),
			       EXISTS (
			         SELECT 1 FROM pg_index i
			           JOIN pg_attribute a
			             ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
			          WHERE i.indrelid = $1::regclass AND a.attname = 'tenant_id')`,
			Table(dest, t.name)).Scan(&hasTenant, &hasIndex); err != nil {
			return fmt.Errorf("inspect tenant index %s: %w", t.name, err)
		}
		if hasTenant && !hasIndex {
			if _, err := tx.Exec(ctx, fmt.Sprintf(
				"CREATE INDEX %s ON %s (tenant_id)",
				pgx.Identifier{t.name + "_tenant_id_test_idx"}.Sanitize(),
				Table(dest, t.name))); err != nil {
				return fmt.Errorf("tenant index %s: %w", t.name, err)
			}
		}
	}

	// Foreign keys are the one object class CREATE TABLE ... LIKE does not
	// copy. Rewrite only intra-project references; public.clients stays
	// public.
	type fk struct {
		table string
		name  string
		def   string
	}
	rows, err = tx.Query(ctx, `
		SELECT tbl.relname, con.conname, pg_get_constraintdef(con.oid)
		  FROM pg_constraint con
		  JOIN pg_class tbl ON tbl.oid = con.conrelid
		  JOIN pg_namespace n ON n.oid = tbl.relnamespace
		 WHERE n.nspname = $1
		   AND con.contype = 'f'
		 ORDER BY tbl.relname, con.conname`, source)
	if err != nil {
		return fmt.Errorf("list foreign keys: %w", err)
	}
	var fks []fk
	for rows.Next() {
		var f fk
		if err := rows.Scan(&f.table, &f.name, &f.def); err != nil {
			rows.Close()
			return fmt.Errorf("scan foreign key: %w", err)
		}
		fks = append(fks, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list foreign keys: %w", err)
	}
	for _, f := range fks {
		def := strings.ReplaceAll(f.def, "REFERENCES "+source+".", "REFERENCES "+dest+".")
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			"ALTER TABLE %s ADD CONSTRAINT %s %s",
			Table(dest, f.table), pgx.Identifier{f.name}.Sanitize(), def)); err != nil {
			return fmt.Errorf("clone foreign key %s on %s: %w", f.name, f.table, err)
		}
	}

	// Triggers keep their shared function and are re-pointed at the clone.
	rows, err = tx.Query(ctx, `
		SELECT tbl.relname, tg.tgname, pg_get_triggerdef(tg.oid)
		  FROM pg_trigger tg
		  JOIN pg_class tbl ON tbl.oid = tg.tgrelid
		  JOIN pg_namespace n ON n.oid = tbl.relnamespace
		 WHERE n.nspname = $1
		   AND NOT tg.tgisinternal
		 ORDER BY tbl.relname, tg.tgname`, source)
	if err != nil {
		return fmt.Errorf("list triggers: %w", err)
	}
	type trigger struct {
		table string
		name  string
		def   string
	}
	var triggers []trigger
	for rows.Next() {
		var tr trigger
		if err := rows.Scan(&tr.table, &tr.name, &tr.def); err != nil {
			rows.Close()
			return fmt.Errorf("scan trigger: %w", err)
		}
		triggers = append(triggers, tr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list triggers: %w", err)
	}
	for _, tr := range triggers {
		def := strings.ReplaceAll(tr.def, " ON "+source+".", " ON "+dest+".")
		def = strings.ReplaceAll(def, " ON ONLY "+source+".", " ON ONLY "+dest+".")
		if _, err := tx.Exec(ctx, def); err != nil {
			return fmt.Errorf("clone trigger %s on %s: %w", tr.name, tr.table, err)
		}
	}

	for _, t := range tables {
		if t.rls {
			if _, err := tx.Exec(ctx, "ALTER TABLE "+Table(dest, t.name)+" ENABLE ROW LEVEL SECURITY"); err != nil {
				return fmt.Errorf("enable rls %s: %w", t.name, err)
			}
		}
		if t.force {
			if _, err := tx.Exec(ctx, "ALTER TABLE "+Table(dest, t.name)+" FORCE ROW LEVEL SECURITY"); err != nil {
				return fmt.Errorf("force rls %s: %w", t.name, err)
			}
		}
	}

	// Policies: pg_policies gives the normalized expressions; rewrite any
	// schema-qualified reference and recreate them on the clone.
	rows, err = tx.Query(ctx, `
		SELECT tablename, policyname, permissive, roles, cmd,
		       COALESCE(qual, ''), COALESCE(with_check, '')
		  FROM pg_policies
		 WHERE schemaname = $1
		 ORDER BY tablename, policyname`, source)
	if err != nil {
		return fmt.Errorf("list policies: %w", err)
	}
	type policy struct {
		table      string
		name       string
		permissive string
		roles      []string
		cmd        string
		qual       string
		check      string
	}
	var policies []policy
	for rows.Next() {
		var p policy
		if err := rows.Scan(&p.table, &p.name, &p.permissive, &p.roles, &p.cmd, &p.qual, &p.check); err != nil {
			rows.Close()
			return fmt.Errorf("scan policy: %w", err)
		}
		policies = append(policies, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list policies: %w", err)
	}
	for _, p := range policies {
		stmt := "CREATE POLICY " + pgx.Identifier{p.name}.Sanitize() + " ON " + Table(dest, p.table)
		if strings.EqualFold(p.permissive, "RESTRICTIVE") {
			stmt += " AS RESTRICTIVE"
		} else {
			stmt += " AS PERMISSIVE"
		}
		stmt += " FOR " + p.cmd
		if len(p.roles) > 0 {
			roles := make([]string, 0, len(p.roles))
			for _, role := range p.roles {
				if strings.EqualFold(role, "public") {
					roles = append(roles, "PUBLIC")
				} else {
					roles = append(roles, pgx.Identifier{role}.Sanitize())
				}
			}
			stmt += " TO " + strings.Join(roles, ", ")
		}
		if p.qual != "" {
			stmt += " USING (" + strings.ReplaceAll(p.qual, source+".", dest+".") + ")"
		}
		if p.check != "" {
			stmt += " WITH CHECK (" + strings.ReplaceAll(p.check, source+".", dest+".") + ")"
		}
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("clone policy %s on %s: %w", p.name, p.table, err)
		}
	}

	// Defaults that embed the source schema (e.g. nextval) must point at the
	// clone. Project tables currently use gen_random_uuid(), but a future
	// sequence default must not keep writing the base schema.
	rows, err = tx.Query(ctx, `
		SELECT tbl.relname, a.attname, pg_get_expr(d.adbin, d.adrelid)
		  FROM pg_attrdef d
		  JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
		  JOIN pg_class tbl ON tbl.oid = d.adrelid
		  JOIN pg_namespace n ON n.oid = tbl.relnamespace
		 WHERE n.nspname = $1
		   AND pg_get_expr(d.adbin, d.adrelid) LIKE '%' || $1 || '.%'
		 ORDER BY tbl.relname, a.attname`, source)
	if err != nil {
		return fmt.Errorf("list defaults: %w", err)
	}
	type defaultExpr struct {
		table string
		col   string
		expr  string
	}
	var defaults []defaultExpr
	for rows.Next() {
		var d defaultExpr
		if err := rows.Scan(&d.table, &d.col, &d.expr); err != nil {
			rows.Close()
			return fmt.Errorf("scan default: %w", err)
		}
		defaults = append(defaults, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list defaults: %w", err)
	}
	for _, d := range defaults {
		expr := strings.ReplaceAll(d.expr, source+".", dest+".")
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			"ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s",
			Table(dest, d.table), pgx.Identifier{d.col}.Sanitize(), expr)); err != nil {
			return fmt.Errorf("clone default %s.%s: %w", d.table, d.col, err)
		}
	}

	// Grants are not copied by LIKE. Reproduce every explicit table and
	// sequence grant, including project and platform definers.
	rows, err = tx.Query(ctx, `
		SELECT c.relname, c.relkind::text, COALESCE(r.rolname, 'PUBLIC'), a.privilege_type
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  CROSS JOIN LATERAL aclexplode(c.relacl) a
		  LEFT JOIN pg_roles r ON r.oid = a.grantee
		 WHERE n.nspname = $1
		   AND c.relkind IN ('r', 'S')
		 ORDER BY c.relname, 3, 4`, source)
	if err != nil {
		return fmt.Errorf("list grants: %w", err)
	}
	type grant struct {
		object string
		kind   string
		role   string
		priv   string
	}
	var grants []grant
	for rows.Next() {
		var g grant
		if err := rows.Scan(&g.object, &g.kind, &g.role, &g.priv); err != nil {
			rows.Close()
			return fmt.Errorf("scan grant: %w", err)
		}
		grants = append(grants, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list grants: %w", err)
	}
	for _, g := range grants {
		if strings.EqualFold(g.role, "postgres") {
			continue
		}
		objectKind := "TABLE"
		if g.kind == "S" {
			objectKind = "SEQUENCE"
		}
		role := "PUBLIC"
		if !strings.EqualFold(g.role, "PUBLIC") {
			role = pgx.Identifier{g.role}.Sanitize()
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			"GRANT %s ON %s %s TO %s",
			g.priv, objectKind, Table(dest, g.object), role)); err != nil {
			return fmt.Errorf("clone grant %s on %s to %s: %w", g.priv, g.object, g.role, err)
		}
	}

	// Schema grants (USAGE/CREATE) are not table grants.
	rows, err = tx.Query(ctx, `
		SELECT COALESCE(r.rolname, 'PUBLIC'), a.privilege_type
		  FROM pg_namespace n
		  CROSS JOIN LATERAL aclexplode(n.nspacl) a
		  LEFT JOIN pg_roles r ON r.oid = a.grantee
		 WHERE n.nspname = $1
		 ORDER BY 1, 2`, source)
	if err != nil {
		return fmt.Errorf("list schema grants: %w", err)
	}
	var schemaGrants []grant
	for rows.Next() {
		var g grant
		if err := rows.Scan(&g.role, &g.priv); err != nil {
			rows.Close()
			return fmt.Errorf("scan schema grant: %w", err)
		}
		schemaGrants = append(schemaGrants, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list schema grants: %w", err)
	}
	for _, g := range schemaGrants {
		if strings.EqualFold(g.role, "postgres") {
			continue
		}
		role := "PUBLIC"
		if !strings.EqualFold(g.role, "PUBLIC") {
			role = pgx.Identifier{g.role}.Sanitize()
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			"GRANT %s ON SCHEMA %s TO %s",
			g.priv, pgx.Identifier{dest}.Sanitize(), role)); err != nil {
			return fmt.Errorf("clone schema grant %s to %s: %w", g.priv, g.role, err)
		}
	}

	return tx.Commit(ctx)
}
