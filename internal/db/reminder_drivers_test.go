package db

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestReminderDrivers_AsDeliveryCaller pins migration 057's least-
// privilege split: the reminder drivers execute for delivery_caller
// (the Edge Function credential) and for NOBODY else — app_runtime,
// which owns the agent request path, must get 42501.
//
// Covered, through the real functions on a migrated database:
//   - claim scoping: due rows claimed; future + quarantined-tenant rows not.
//   - complete_reminder: true on match, false on stale claim / bad status.
//   - fail_reminder: attempts bump, 'dead' at max, NULL when not sending.
//
// Seeding and cleanup run on the admin pool (delivery_caller holds no
// table grants by design).
func TestReminderDrivers_AsDeliveryCaller(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	adminDSN := os.Getenv("DATABASE_ADMIN_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	if adminDSN == "" {
		adminDSN = dsn
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adminPool, err := NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer adminPool.Close()
	if err := Migrate(ctx, adminPool); err != nil {
		t.Fatal(err)
	}

	const tenant = "drv-test"
	// Cleanup owns a SEPARATE admin pool: t.Cleanup callbacks run AFTER
	// the test function's deferred closes, so reusing adminPool here
	// would Exec on a closed pool, fail silently, and leak due rows —
	// which the reminder package's global-claim Tick would then send,
	// failing an unrelated package (seen on CI as sent=2 want 1).
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cpool, err := NewPool(cctx, adminDSN)
		if err != nil {
			return
		}
		defer cpool.Close()
		cpool.Exec(cctx, `DELETE FROM agent_reminders WHERE tenant_id IN ($1, 'drv-test-q')`, tenant)
		cpool.Exec(cctx, `DELETE FROM tenant_quarantine WHERE tenant_id = 'drv-test-q'`)
		cpool.Exec(cctx, `DELETE FROM clients WHERE tenant_id IN ($1, 'drv-test-q')`, tenant)
	})

	if _, err := adminPool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status)
		 VALUES ('drv', $1, 'drv-hash', 1000000, 'active'),
		        ('drvq', 'drv-test-q', 'drv-hash-q', 1000000, 'active')
		 ON CONFLICT (tenant_id) DO NOTHING`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx,
		`INSERT INTO tenant_quarantine (tenant_id, reason) VALUES ('drv-test-q', 'test')
		 ON CONFLICT (tenant_id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx,
		`DELETE FROM agent_reminders WHERE tenant_id IN ($1, 'drv-test-q')`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx,
		`INSERT INTO agent_reminders (tenant_id, chat_id, text, remind_at, recipient_ref) VALUES
		 ($1, '1', 'drv due', now() - interval '1 minute', 'owner'),
		 ($1, '1', 'drv future', now() + interval '1 hour', 'owner'),
		 ('drv-test-q', '1', 'drv quarantined', now() - interval '1 minute', 'owner'),
		 ($1, '1', 'drv maxed', now() - interval '1 minute', 'owner')`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx,
		`UPDATE agent_reminders SET attempts = 4 WHERE text = 'drv maxed'`); err != nil {
		t.Fatal(err)
	}
	callerPool, err := NewPool(ctx, toCallerDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer callerPool.Close()

	// Claim: exactly the two due, unquarantined rows.
	rows, err := callerPool.Query(ctx, `SELECT j FROM private.claim_due_reminders(20) AS j`)
	if err != nil {
		t.Fatalf("claim as delivery_caller: %v", err)
	}
	claimed := map[string]bool{}
	for rows.Next() {
		var raw []byte
		var payload map[string]any
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		text, _ := payload["text"].(string)
		claimed[text] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 || !claimed["drv due"] || !claimed["drv maxed"] {
		t.Errorf("claimed = %v, want exactly {drv due, drv maxed}", claimed)
	}
	// IDs + claimed_at prefetched via admin: delivery_caller holds no
	// table grants, so subselects inside its queries die with 42501.
	// The definer boundary is exactly what the least-privilege pin
	// below asserts — the test must not smuggle table reads across it.
	var dueID, maxedID, futureID string
	var claimedAt, maxedClaimedAt time.Time
	if err := adminPool.QueryRow(ctx,
		`SELECT id::text, claimed_at FROM agent_reminders WHERE text='drv due'`).Scan(&dueID, &claimedAt); err != nil {
		t.Fatal(err)
	}
	if err := adminPool.QueryRow(ctx,
		`SELECT id::text, claimed_at FROM agent_reminders WHERE text='drv maxed'`).Scan(&maxedID, &maxedClaimedAt); err != nil {
		t.Fatal(err)
	}
	if err := adminPool.QueryRow(ctx,
		`SELECT id::text FROM agent_reminders WHERE text='drv future'`).Scan(&futureID); err != nil {
		t.Fatal(err)
	}

	// Complete: true on match, false on stale claim and bad status.
	var ok bool
	if err := callerPool.QueryRow(ctx,
		`SELECT private.complete_reminder($1::uuid, $2, 'sent')`,
		dueID, claimedAt).Scan(&ok); err != nil {
		t.Fatalf("complete as delivery_caller: %v", err)
	}
	if !ok {
		t.Error("complete_reminder on live claim = false, want true")
	}
	if err := callerPool.QueryRow(ctx,
		`SELECT private.complete_reminder($1::uuid, now(), 'sent')`, dueID).Scan(&ok); err != nil {
		t.Fatalf("complete stale claim: %v", err)
	}
	if ok {
		t.Error("complete_reminder on stale claim = true, want false")
	}
	if err := callerPool.QueryRow(ctx,
		`SELECT private.complete_reminder($1::uuid, $2, 'bogus')`,
		maxedID, maxedClaimedAt).Scan(&ok); err != nil {
		t.Fatalf("complete bad status: %v", err)
	}
	if ok {
		t.Error("complete_reminder with bad status = true, want false")
	}

	// Fail at attempts=4 goes straight to dead; failing a non-sending
	// row returns NULL, not an error.
	var status *string
	if err := callerPool.QueryRow(ctx,
		`SELECT private.fail_reminder($1::uuid, $2)`,
		maxedID, maxedClaimedAt).Scan(&status); err != nil {
		t.Fatalf("fail as delivery_caller: %v", err)
	}
	if status == nil || *status != "dead" {
		t.Errorf("fail at max attempts = %v, want dead", status)
	}
	if err := callerPool.QueryRow(ctx,
		`SELECT private.fail_reminder($1::uuid, now())`, futureID).Scan(&status); err != nil {
		t.Fatalf("fail non-sending: %v", err)
	}
	if status != nil {
		t.Errorf("fail on non-sending row = %q, want NULL", *status)
	}

	// Least privilege: the runtime role cannot execute the drivers.
	// NOTE: pgx surfaces server errors lazily — Query itself returns
	// nil and the 42501 arrives at rows.Err() after iteration. Checking
	// only the Query error would false-pass this pin.
	runtimePool, err := NewPool(ctx, toRuntimeDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()
	rows2, err := runtimePool.Query(ctx, `SELECT j FROM private.claim_due_reminders(1) AS j`)
	denied := err != nil
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
		}
		if rerr := rows2.Err(); rerr != nil {
			denied = true
			err = rerr
		}
	}
	if !denied {
		t.Error("claim as app_runtime succeeded, want 42501 denial")
	} else if !strings.Contains(err.Error(), "42501") && !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("claim as app_runtime: unexpected error %v (want permission denied)", err)
	}
}

// toCallerDSN rewrites a DSN to the delivery_caller login role,
// mirroring toRuntimeDSN (rbac_test.go). The role is passwordless;
// trust auth on CI and local dev lets it in like app_runtime.
func toCallerDSN(dsn string) string {
	i := strings.Index(dsn, "://")
	if i < 0 {
		return dsn
	}
	rest := dsn[i+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return dsn
	}
	return dsn[:i+3] + "delivery_caller" + rest[at:]
}
