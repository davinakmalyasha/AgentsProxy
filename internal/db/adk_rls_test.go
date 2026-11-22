package db

import (
	"context"
	"testing"
	"time"
)

// TestADKSessionsTenantRLS pins migration 085: app_runtime sees only the
// app_name named by app.tenant_id, and an unset GUC sees nothing. app_name is
// the tenant slug, so the policy does not need a duplicate tenant_id column.
func TestADKSessionsTenantRLS(t *testing.T) {
	dsn := migrateDSN()
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping ADK RLS test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := MigrateAll(ctx, admin); err != nil {
		t.Fatal(err)
	}

	const (
		appA = "adk-rls-a"
		appB = "adk-rls-b"
	)
	cleanup := func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM sessions WHERE app_name IN ($1,$2)`, appA, appB)
		_, _ = admin.Exec(context.Background(), `DELETE FROM app_states WHERE app_name IN ($1,$2)`, appA, appB)
		_, _ = admin.Exec(context.Background(), `DELETE FROM user_states WHERE app_name IN ($1,$2)`, appA, appB)
	}
	cleanup()
	t.Cleanup(cleanup)

	for _, app := range []string{appA, appB} {
		if _, err := admin.Exec(ctx,
			`INSERT INTO sessions (app_name, user_id, id, state, create_time, update_time)
			 VALUES ($1, 'u1', $2, '{}'::jsonb, now(), now())`,
			app, app+"-s1"); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx,
			`INSERT INTO events (id, app_name, user_id, session_id)
			 VALUES ($1, $2, 'u1', $3)`,
			app+"-e1", app, app+"-s1"); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx,
			`INSERT INTO app_states (app_name, state, update_time)
			 VALUES ($1, '{}'::jsonb, now())`, app); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx,
			`INSERT INTO user_states (app_name, user_id, state, update_time)
			 VALUES ($1, 'u1', '{}'::jsonb, now())`, app); err != nil {
			t.Fatal(err)
		}
	}

	runtime, err := NewPool(ctx, toRuntimeDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	// An unset GUC fails closed: zero rows from every table.
	for _, table := range []string{"sessions", "events", "app_states", "user_states"} {
		var n int
		if err := runtime.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("unset GUC count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("unset GUC sees %d rows in %s, want 0", n, table)
		}
	}

	tx, err := runtime.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, appA); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	for _, table := range []string{"sessions", "events", "app_states", "user_states"} {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("tenant GUC count %s: %v", table, err)
		}
		if n != 1 {
			_ = tx.Rollback(ctx)
			t.Errorf("tenant GUC sees %d rows in %s, want 1", n, table)
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// A write for another app under this GUC must be rejected.
	tx, err = runtime.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, appA); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO sessions (app_name, user_id, id, state, create_time, update_time)
		 VALUES ($1, 'u1', 'cross-tenant', '{}'::jsonb, now(), now())`, appB)
	_ = tx.Rollback(ctx)
	if err == nil {
		t.Error("app_runtime inserted an ADK session for another app_name; RLS did not bind app.tenant_id")
	}
}
