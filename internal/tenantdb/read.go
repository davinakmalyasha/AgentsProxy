package tenantdb

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ReadTenant runs one read statement inside the tenant's RLS context.
//
// The normal WithTx path spends four network round trips on one SELECT:
// BEGIN, set_config, SELECT, COMMIT. Production uses Supavisor
// transaction mode, which requires the simple query protocol; in that mode
// pgx.Batch joins the context statement and the read into one simple Query
// message. PostgreSQL executes a multi-statement simple Query as a single
// implicit transaction, so SET LOCAL applies to the read and is rolled back
// with it. One message, one round trip, no leaked session state.
//
// On an extended-protocol pool (local dev/tests) that guarantee does not
// hold — each statement would be its own implicit transaction — so this
// falls back to the existing WithTx path. Callers get identical semantics
// either way, and production is the only place the fast path runs.
//
// schema may be empty; it is then resolved through the same cached clients
// lookup WithTx uses. Callers that already hold an authenticated schema
// should pass it.
func ReadTenant(ctx context.Context, pool *pgxpool.Pool, tenantID, schema, sql string, args []any, fn func(pgx.Rows) error) error {
	if pool == nil {
		return fmt.Errorf("tenantdb: nil pool")
	}
	if fn == nil {
		return fmt.Errorf("tenantdb: nil read callback")
	}
	if schema == "" {
		s, err := lookupSchemaCached(ctx, pool, tenantID)
		if err != nil {
			return err
		}
		schema = s
	}
	if !schemaRe.MatchString(schema) {
		return fmt.Errorf("tenantdb: invalid schema %q for tenant %q", schema, tenantID)
	}
	if cached, ok := cachedSchema(tenantID); !ok || cached != schema {
		storeSchema(tenantID, schema)
	}

	if pool.Config().ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol {
		// Extended protocol: keep the proven transaction path. Do not send
		// this as a pipeline — set_config would not apply to the next
		// statement outside a transaction.
		return WithTxSchema(ctx, pool, tenantID, schema, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			if err := fn(rows); err != nil {
				return err
			}
			return rows.Err()
		})
	}

	b := &pgx.Batch{}
	// set_config(..., true) is transaction-local. Both statements are one
	// implicit transaction because they share one simple Query message.
	b.Queue(`SELECT set_config('app.tenant_id', $1, true), set_config('search_path', $2, true)`,
		tenantID, schema)
	b.Queue(sql, args...)

	br := pool.SendBatch(ctx, b)
	defer func() { _ = br.Close() }()

	if _, err := br.Exec(); err != nil {
		return fmt.Errorf("tenantdb: set tenant context: %w", err)
	}
	rows, err := br.Query()
	if err != nil {
		return err
	}
	if err := fn(rows); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	return br.Close()
}
