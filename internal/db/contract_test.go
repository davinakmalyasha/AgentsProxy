package db

import (
	"context"
	"testing"
	"time"
)

// TestContractLegacyObjectsAreGone pins the end state of the contract
// release. The historical migrations still create the expand-side objects so
// an old binary can roll back; the final Centrum migrations remove them. A
// fresh database must therefore finish every migration with none of the
// compatibility layer left.
func TestContractLegacyObjectsAreGone(t *testing.T) {
	dsn := migrateDSN()
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping contract test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	pool, err := NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := MigrateAll(ctx, pool); err != nil {
		t.Fatalf("MigrateAll: %v", err)
	}

	var noLogin, noCreate, noSession, noPayments, noMoney, noRole, projectMoney bool
	if err := pool.QueryRow(ctx, `
		SELECT to_regprocedure('private.login_user(text,text)') IS NULL,
		       to_regprocedure('private.create_session(uuid,text,timestamp with time zone)') IS NULL,
		       to_regprocedure('private."session_user"(text)') IS NULL,
		       to_regclass('public.agent_staff_payments') IS NULL,
		       to_regprocedure('private.range_summary(text,date,date)') IS NULL,
		       NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'money_reader'),
		       to_regprocedure('project_centrum.money_range_summary(text,date,date)') IS NOT NULL
	`).Scan(&noLogin, &noCreate, &noSession, &noPayments, &noMoney, &noRole, &projectMoney); err != nil {
		t.Fatal(err)
	}
	if !noLogin || !noCreate || !noSession || !noPayments || !noMoney || !noRole || !projectMoney {
		t.Fatalf("contract state login=%v create=%v session=%v payments=%v money=%v role=%v projectMoney=%v",
			noLogin, noCreate, noSession, noPayments, noMoney, noRole, projectMoney)
	}

	// The retired scanner needed project-table and clients.db_schema access.
	// The new session function needs only tenant_id and status.
	var projectSelect, dbSchema, tenantSelect, statusSelect bool
	if err := pool.QueryRow(ctx, `
		SELECT has_table_privilege('auth_definer','project_centrum.sports_users','SELECT'),
		       has_column_privilege('auth_definer','public.clients','db_schema','SELECT'),
		       has_column_privilege('auth_definer','public.clients','tenant_id','SELECT'),
		       has_column_privilege('auth_definer','public.clients','status','SELECT')
	`).Scan(&projectSelect, &dbSchema, &tenantSelect, &statusSelect); err != nil {
		t.Fatal(err)
	}
	if projectSelect || dbSchema || !tenantSelect || !statusSelect {
		t.Fatalf("auth_definer grants project=%v db_schema=%v tenant=%v status=%v",
			projectSelect, dbSchema, tenantSelect, statusSelect)
	}

	var runtimeCreate, runtimeResolve bool
	if err := pool.QueryRow(ctx, `
		SELECT has_function_privilege('app_runtime',
		         'private.staff_session_create(text,uuid,text,timestamp with time zone)','EXECUTE'),
		       has_function_privilege('app_runtime',
		         'private.staff_session_resolve(text)','EXECUTE')
	`).Scan(&runtimeCreate, &runtimeResolve); err != nil {
		t.Fatal(err)
	}
	if !runtimeCreate || !runtimeResolve {
		t.Fatalf("app_runtime session privileges create=%v resolve=%v", runtimeCreate, runtimeResolve)
	}
}
