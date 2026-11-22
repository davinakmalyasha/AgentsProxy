package dbtest

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
)

func mustDSN() string {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		return ""
	}
	admin := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if admin == "" {
		return dsn
	}
	return admin
}

func freshPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping clone test")
	}
	adminDSN := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if adminDSN == "" {
		adminDSN = dsn
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	pool, err := db.NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.MigrateAll(ctx, pool); err != nil {
		t.Fatalf("MigrateAll: %v", err)
	}
	return pool
}

func TestCloneSchemaMatchesSource(t *testing.T) {
	pool := freshPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	dest := CloneSchema(t, ctx, pool, "project_centrum", "clone-proof")

	type snapshot struct {
		tables   []string
		fks      []string
		triggers []string
		policies []string
		rls      []string
	}
	load := func(schema string) snapshot {
		t.Helper()
		var s snapshot
		rows, err := pool.Query(ctx, `
			SELECT c.relname || ':' || c.relrowsecurity || ':' || c.relforcerowsecurity
			  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1 AND c.relkind = 'r'
			 ORDER BY c.relname`, schema)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			parts := strings.SplitN(v, ":", 2)
			s.tables = append(s.tables, parts[0])
			s.rls = append(s.rls, v)
		}
		rows.Close()

		rows, err = pool.Query(ctx, `
			SELECT tbl.relname || '|' || con.conname || '|' || pg_get_constraintdef(con.oid)
			  FROM pg_constraint con
			  JOIN pg_class tbl ON tbl.oid = con.conrelid
			  JOIN pg_namespace n ON n.oid = tbl.relnamespace
			 WHERE n.nspname = $1 AND con.contype = 'f'
			 ORDER BY 1`, schema)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			s.fks = append(s.fks, strings.ReplaceAll(v, schema+".", "SCHEMA."))
		}
		rows.Close()

		rows, err = pool.Query(ctx, `
			SELECT tbl.relname || '|' || pg_get_triggerdef(tg.oid)
			  FROM pg_trigger tg
			  JOIN pg_class tbl ON tbl.oid = tg.tgrelid
			  JOIN pg_namespace n ON n.oid = tbl.relnamespace
			 WHERE n.nspname = $1 AND NOT tg.tgisinternal
			 ORDER BY 1`, schema)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			s.triggers = append(s.triggers, strings.ReplaceAll(v, schema+".", "SCHEMA."))
		}
		rows.Close()

		rows, err = pool.Query(ctx, `
			SELECT tablename || '|' || policyname || '|' || permissive || '|' ||
			       cmd || '|' || array_to_string(roles, ',') || '|' ||
			       COALESCE(qual, '') || '|' || COALESCE(with_check, '')
			  FROM pg_policies WHERE schemaname = $1
			 ORDER BY 1`, schema)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			s.policies = append(s.policies, strings.ReplaceAll(v, schema+".", "SCHEMA."))
		}
		rows.Close()
		return s
	}

	src := load("project_centrum")
	clone := load(dest)
	if fmt.Sprint(src.tables) != fmt.Sprint(clone.tables) {
		t.Fatalf("tables differ\nsource: %v\nclone:  %v", src.tables, clone.tables)
	}
	if fmt.Sprint(src.rls) != fmt.Sprint(clone.rls) {
		t.Fatalf("rls flags differ\nsource: %v\nclone:  %v", src.rls, clone.rls)
	}
	if fmt.Sprint(src.fks) != fmt.Sprint(clone.fks) {
		t.Fatalf("foreign keys differ\nsource: %v\nclone:  %v", src.fks, clone.fks)
	}
	if fmt.Sprint(src.triggers) != fmt.Sprint(clone.triggers) {
		t.Fatalf("triggers differ\nsource: %v\nclone:  %v", src.triggers, clone.triggers)
	}
	if fmt.Sprint(src.policies) != fmt.Sprint(clone.policies) {
		t.Fatalf("policies differ\nsource: %v\nclone:  %v", src.policies, clone.policies)
	}
}

func TestCloneSchemaTriggerStaysInClone(t *testing.T) {
	pool := freshPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	const tenant = "clone-trigger"
	if _, err := pool.Exec(ctx, `DELETE FROM public.clients WHERE tenant_id = $1`, tenant); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		_, _ = pool.Exec(cctx, `DELETE FROM public.clients WHERE tenant_id = $1`, tenant)
	})

	dest := CloneSchema(t, ctx, pool, "project_centrum", tenant)
	if _, err := pool.Exec(ctx,
		`INSERT INTO public.clients
		   (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ($1, $1, $1 || '-hash', 1000000, 'active', 'sports', ARRAY['records'], $2)`,
		tenant, dest); err != nil {
		t.Fatal(err)
	}

	var facilityID string
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		`INSERT INTO %s (tenant_id, name, type, price_per_hour)
		 VALUES ($1, 'Court', 'badminton', 50000) RETURNING id::text`,
		Table(dest, "sports_facilities")), tenant).Scan(&facilityID); err != nil {
		t.Fatal(err)
	}
	var bookingID string
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		`INSERT INTO %s (tenant_id, facility_id, date, start_time, end_time, customer_name, status, price)
		 VALUES ($1, $2::uuid, '2026-12-02', '09:00', '10:00', 'X', 'confirmed', 50000)
		 RETURNING id::text`,
		Table(dest, "sports_bookings")), tenant, facilityID).Scan(&bookingID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s (tenant_id, booking_id, amount, method)
		 VALUES ($1, $2::uuid, 50000, 'cash')`,
		Table(dest, "sports_payments")), tenant, bookingID); err != nil {
		t.Fatal(err)
	}

	var status string
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT payment_status FROM %s WHERE id = $1::uuid`,
		Table(dest, "sports_bookings")), bookingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "paid" {
		t.Fatalf("clone booking status=%q want paid", status)
	}
	var base int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM project_centrum.sports_bookings WHERE id = $1::uuid`, bookingID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base != 0 {
		t.Fatalf("trigger leaked into base schema: %d rows", base)
	}
}

func TestCloneSchemaAuthStaysInClone(t *testing.T) {
	pool := freshPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	const tenant = "clone-auth"
	if _, err := pool.Exec(ctx, `DELETE FROM public.clients WHERE tenant_id = $1`, tenant); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		_, _ = pool.Exec(cctx, `DELETE FROM public.clients WHERE tenant_id = $1`, tenant)
	})

	dest := CloneSchema(t, ctx, pool, "project_centrum", tenant)
	if _, err := pool.Exec(ctx,
		`INSERT INTO public.clients
		   (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ($1, $1, $1 || '-hash', 1000000, 'active', 'sports', ARRAY['records'], $2)`,
		tenant, dest); err != nil {
		t.Fatal(err)
	}
	var userID string
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		`INSERT INTO %s (tenant_id, email, password_hash, role, full_name)
		 VALUES ($1, 'owner@clone.test', 'x', 'owner', 'Clone Owner') RETURNING id::text`,
		Table(dest, "sports_users")), tenant).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	raw := "clone-session-token"
	if _, err := pool.Exec(ctx,
		`SELECT private.staff_session_create($1, $2::uuid, $3, now() + interval '1 hour')`,
		tenant, userID, auth.HashToken(raw)); err != nil {
		t.Fatal(err)
	}
	var gotTenant, gotUser string
	if err := pool.QueryRow(ctx,
		`SELECT tenant_id, user_id::text FROM private.staff_session_resolve($1)`,
		auth.HashToken(raw)).Scan(&gotTenant, &gotUser); err != nil {
		t.Fatal(err)
	}
	if gotTenant != tenant || gotUser != userID {
		t.Fatalf("session resolved %q/%q, want %q/%s", gotTenant, gotUser, tenant, userID)
	}
	// The platform session directory holds no project data. Resolve the live
	// profile from the cloned schema, exactly as the Centrum provider does.
	var gotEmail string
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT email FROM %s WHERE id = $1::uuid AND tenant_id = $2`,
		Table(dest, "sports_users")), userID, tenant).Scan(&gotEmail); err != nil {
		t.Fatal(err)
	}
	if gotEmail != "owner@clone.test" {
		t.Fatalf("clone profile=%q want owner@clone.test", gotEmail)
	}

	// The same token must not resolve a user from the base schema.
	var baseUser string
	_ = pool.QueryRow(ctx,
		`SELECT id::text FROM project_centrum.sports_users WHERE email = 'owner@clone.test'`).Scan(&baseUser)
	if baseUser != "" {
		t.Fatalf("test user leaked into base schema: %s", baseUser)
	}
}

func TestCloneNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		name := cloneName("facilities")
		if seen[name] {
			t.Fatalf("duplicate clone name %q", name)
		}
		seen[name] = true
		if !strings.HasPrefix(name, "project_test_facilities_") {
			t.Fatalf("unexpected clone name %q", name)
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) != 100 {
		t.Fatalf("got %d names", len(names))
	}
}

func TestSharedSchemaReset(t *testing.T) {
	pool := freshPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	s1 := SharedSchema(t, ctx, pool, "project_centrum", "shared-reset")
	if err := ResetSchema(ctx, pool, s1); err != nil {
		t.Fatal(err)
	}
	if err := ResetSchema(ctx, pool, s1); err != nil {
		t.Fatal(err)
	}
	s2 := SharedSchema(t, ctx, pool, "project_centrum", "shared-reset")
	if s1 != s2 {
		t.Fatalf("shared schema changed: %s -> %s", s1, s2)
	}
	sp := SchemaPool(t, ctx, mustDSN(), s1)
	var n int
	if err := sp.QueryRow(ctx, `SELECT count(*) FROM sports_facilities`).Scan(&n); err != nil {
		t.Fatal(err)
	}
}
