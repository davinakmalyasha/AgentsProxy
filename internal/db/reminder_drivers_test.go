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
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		adminPool.Exec(cctx, `DELETE FROM agent_reminders WHERE tenant_id IN ($1, 'drv-test-q')`, tenant)
		adminPool.Exec(cctx, `DELETE FROM tenant_quarantine WHERE tenant_id = 'drv-test-q'`)
		adminPool.Exec(cctx, `DELETE FROM clients WHERE tenant_id IN ($1, 'drv-test-q')`, tenant)
	})

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
	// claimed_at reread from the row (exact timestamptz round-trip).
	var claimedAt time.Time
	if err := adminPool.QueryRow(ctx,
		`SELECT claimed_at FROM agent_reminders WHERE text='drv due'`).Scan(&claimedAt); err != nil {
		t.Fatal(err)
	}

	// Complete: true on match, false on stale claim and bad status.
	var ok bool
	if err := callerPool.QueryRow(ctx,
		`SELECT private.complete_reminder(
		   (SELECT id FROM agent_reminders WHERE text='drv due'), $1, 'sent')`,
		claimedAt).Scan(&ok); err != nil {
		t.Fatalf("complete as delivery_caller: %v", err)
	}
	if !ok {
		t.Error("complete_reminder on live claim = false, want true")
	}
	if err := callerPool.QueryRow(ctx,
		`SELECT private.complete_reminder(
		   (SELECT id FROM agent_reminders WHERE text='drv due'), now(), 'sent')`).Scan(&ok); err != nil {
		t.Fatalf("complete stale claim: %v", err)
	}
	if ok {
		t.Error("complete_reminder on stale claim = true, want false")
	}
	if err := callerPool.QueryRow(ctx,
		`SELECT private.complete_reminder(
		   (SELECT id FROM agent_reminders WHERE text='drv maxed'),
		   (SELECT claimed_at FROM agent_reminders WHERE text='drv maxed'), 'bogus')`).Scan(&ok); err != nil {
		t.Fatalf("complete bad status: %v", err)
	}
	if ok {
		t.Error("complete_reminder with bad status = true, want false")
	}

	// Fail at attempts=4 goes straight to dead; failing a non-sending
	// row returns NULL, not an error.
	var status *string
	if err := callerPool.QueryRow(ctx,
		`SELECT private.fail_reminder(
		   (SELECT id FROM agent_reminders WHERE text='drv maxed'),
		   (SELECT claimed_at FROM agent_reminders WHERE text='drv maxed'))`).Scan(&status); err != nil {
		t.Fatalf("fail as delivery_caller: %v", err)
	}
	if status == nil || *status != "dead" {
		t.Errorf("fail at max attempts = %v, want dead", status)
	}
	if err := callerPool.QueryRow(ctx,
		`SELECT private.fail_reminder(
		   (SELECT id FROM agent_reminders WHERE text='drv future'), now())`).Scan(&status); err != nil {
		t.Fatalf("fail non-sending: %v", err)
	}
	if status != nil {
		t.Errorf("fail on non-sending row = %q, want NULL", *status)
	}

	// Least privilege: the runtime role cannot execute the drivers.
	runtimePool, err := NewPool(ctx, toRuntimeDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()
	rows2, err := runtimePool.Query(ctx, `SELECT j FROM private.claim_due_reminders(1) AS j`)
	if err == nil {
		rows2.Close()
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
