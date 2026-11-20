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
	dsn := os.Getenv("TEST_DATABASE_URL")
	adminDSN := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
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
	claimedRefs := map[string]string{}
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
		ref, _ := payload["recipient_ref"].(string)
		claimed[text] = true
		claimedRefs[text] = ref
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 || !claimed["drv due"] || !claimed["drv maxed"] {
		t.Errorf("claimed = %v, want exactly {drv due, drv maxed}", claimed)
	}
	// 059: routing rides the payload — the Edge driver must not need a
	// follow-up table read to find the recipient.
	if claimedRefs["drv due"] != "owner" || claimedRefs["drv maxed"] != "owner" {
		t.Errorf("claim payload recipient_ref = %v, want owner on both rows", claimedRefs)
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

// TestReminderPushRPCs_AsDeliveryCaller pins migration 059: the Edge
// driver's RPC-only surface. delivery_caller executes the push helpers
// and release_reminder but still holds NO table grants; app_runtime
// executes none of them.
//
// Covered, through the real functions on a migrated database:
//   - push_subscriptions_for: tenant+ref scoped, empty ref returns none.
//   - push_subscription_touch: stamps last_used_at; false when missing.
//   - push_subscription_gone: deletes the row; false when missing.
//   - release_reminder: back to pending with attempts intact; false on
//     a stale/no-longer-claimed row.
//   - least privilege: app_runtime gets 42501 on every RPC, and
//     delivery_caller gets 42501 on direct table reads.
//
// Seeding and cleanup run on the admin pool (delivery_caller holds no
// table grants by design); cleanup owns a separate pool (see the note
// in TestReminderDrivers_AsDeliveryCaller).
func TestReminderPushRPCs_AsDeliveryCaller(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	adminDSN := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
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

	const tenantA, tenantB = "push-test", "push-test-b"
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cpool, err := NewPool(cctx, adminDSN)
		if err != nil {
			return
		}
		defer cpool.Close()
		cpool.Exec(cctx, `DELETE FROM agent_reminders WHERE tenant_id IN ($1, $2)`, tenantA, tenantB)
		cpool.Exec(cctx, `DELETE FROM push_subscriptions WHERE tenant_id IN ($1, $2)`, tenantA, tenantB)
		cpool.Exec(cctx, `DELETE FROM clients WHERE tenant_id IN ($1, $2)`, tenantA, tenantB)
	})

	if _, err := adminPool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status)
		 VALUES ('pusha', $1, 'push-hash-a', 1000000, 'active'),
		        ('pushb', $2, 'push-hash-b', 1000000, 'active')
		 ON CONFLICT (tenant_id) DO NOTHING`, tenantA, tenantB); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx,
		`DELETE FROM agent_reminders WHERE tenant_id IN ($1, $2)`, tenantA, tenantB); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx,
		`DELETE FROM push_subscriptions WHERE tenant_id IN ($1, $2)`, tenantA, tenantB); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx,
		`INSERT INTO push_subscriptions (tenant_id, user_ref, endpoint, p256dh, auth) VALUES
		 ($1, 'dev', 'https://push.test/ep1', 'k1', 'a1'),
		 ($1, 'dev', 'https://push.test/ep2', 'k2', 'a2'),
		 ($1, 'other', 'https://push.test/ep3', 'k3', 'a3'),
		 ($2, 'dev', 'https://push.test/ep4', 'k4', 'a4')`, tenantA, tenantB); err != nil {
		t.Fatal(err)
	}

	callerPool, err := NewPool(ctx, toCallerDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer callerPool.Close()

	// Lookup: exactly this tenant's ref rows, endpoint-ordered; the
	// same-ref subscription of the other tenant must not leak in.
	rows, err := callerPool.Query(ctx,
		`SELECT endpoint FROM private.push_subscriptions_for($1, 'dev')`, tenantA)
	if err != nil {
		t.Fatalf("push_subscriptions_for as delivery_caller: %v", err)
	}
	var endpoints []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		endpoints = append(endpoints, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(endpoints) != 2 ||
		endpoints[0] != "https://push.test/ep1" || endpoints[1] != "https://push.test/ep2" {
		t.Errorf("push_subscriptions_for = %v, want exactly [ep1 ep2] (tenant+ref scoped)", endpoints)
	}
	var none int
	if err := callerPool.QueryRow(ctx,
		`SELECT count(*) FROM private.push_subscriptions_for($1, '')`, tenantA).Scan(&none); err != nil {
		t.Fatal(err)
	}
	if none != 0 {
		t.Errorf("push_subscriptions_for with empty ref = %d rows, want 0", none)
	}

	// Touch: boolean result, and the admin sees the effect.
	var ok bool
	if err := callerPool.QueryRow(ctx,
		`SELECT private.push_subscription_touch($1, 'https://push.test/ep1')`, tenantA).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("touch on live endpoint = false, want true")
	}
	if err := adminPool.QueryRow(ctx,
		`SELECT last_used_at IS NOT NULL FROM push_subscriptions
		  WHERE tenant_id = $1 AND endpoint = 'https://push.test/ep1'`, tenantA).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("touch did not stamp last_used_at")
	}
	if err := callerPool.QueryRow(ctx,
		`SELECT private.push_subscription_touch($1, 'https://push.test/missing')`, tenantA).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("touch on missing endpoint = true, want false")
	}

	// Gone: deletes exactly the one row, idempotent on the second call.
	if err := callerPool.QueryRow(ctx,
		`SELECT private.push_subscription_gone($1, 'https://push.test/ep2')`, tenantA).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("gone on live endpoint = false, want true")
	}
	var subCount int
	if err := adminPool.QueryRow(ctx,
		`SELECT count(*) FROM push_subscriptions
		  WHERE tenant_id = $1 AND endpoint = 'https://push.test/ep2'`, tenantA).Scan(&subCount); err != nil {
		t.Fatal(err)
	}
	if subCount != 0 {
		t.Error("gone did not delete the subscription row")
	}
	if err := callerPool.QueryRow(ctx,
		`SELECT private.push_subscription_gone($1, 'https://push.test/ep2')`, tenantA).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("gone on already-deleted endpoint = true, want false")
	}

	// Release: claim a due row, hand it back without burning attempts.
	if _, err := adminPool.Exec(ctx,
		`INSERT INTO agent_reminders (tenant_id, chat_id, text, remind_at, recipient_ref, attempts)
		 VALUES ($1, '1', 'push release me', now() - interval '1 minute', 'dev', 2)`, tenantA); err != nil {
		t.Fatal(err)
	}
	rows2, err := callerPool.Query(ctx, `SELECT j FROM private.claim_due_reminders(20) AS j`)
	if err != nil {
		t.Fatalf("claim as delivery_caller: %v", err)
	}
	var relID, relClaimedAt, relText string
	claimedCount := 0
	for rows2.Next() {
		var raw []byte
		var payload map[string]any
		if err := rows2.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		claimedCount++
		relText, _ = payload["text"].(string)
		relID, _ = payload["id"].(string)
		relClaimedAt, _ = payload["claimed_at"].(string)
	}
	rows2.Close()
	if err := rows2.Err(); err != nil {
		t.Fatal(err)
	}
	if claimedCount != 1 || relText != "push release me" || relID == "" || relClaimedAt == "" {
		t.Fatalf("claimed %d rows (text=%q id=%q), want exactly the seeded due row", claimedCount, relText, relID)
	}
	// 060: the RPC claim stamps millisecond precision because
	// postgres.js (the Edge driver's client) truncates ISO strings to
	// ms on bind — a microsecond claim could never be released,
	// completed, or failed from the Edge path.
	if ts, perr := time.Parse(time.RFC3339Nano, relClaimedAt); perr != nil {
		t.Errorf("claim claimed_at %q not RFC3339: %v", relClaimedAt, perr)
	} else if ts.Nanosecond()%1_000_000 != 0 {
		t.Errorf("claim claimed_at %q has sub-millisecond precision; postgres.js cannot round-trip it", relClaimedAt)
	}
	if err := callerPool.QueryRow(ctx,
		`SELECT private.release_reminder($1::uuid, $2::timestamptz)`, relID, relClaimedAt).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("release_reminder on live claim = false, want true")
	}
	var status string
	var attempts int
	var claimedAt *time.Time
	if err := adminPool.QueryRow(ctx,
		`SELECT status, attempts, claimed_at FROM agent_reminders WHERE id = $1::uuid`, relID).
		Scan(&status, &attempts, &claimedAt); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || attempts != 2 || claimedAt != nil {
		t.Errorf("after release: status=%q attempts=%d claimed_at=%v, want pending/2/nil", status, attempts, claimedAt)
	}
	if err := callerPool.QueryRow(ctx,
		`SELECT private.release_reminder($1::uuid, $2::timestamptz)`, relID, relClaimedAt).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("release_reminder on non-sending row = true, want false")
	}

	// Least privilege: app_runtime cannot execute any 059 RPC. pgx
	// surfaces 42501 lazily, so iterate before checking rows.Err().
	// The statements are harmless if a denial ever regresses silently:
	// they match no rows for tenant 'x'.
	runtimePool, err := NewPool(ctx, toRuntimeDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()
	for _, stmt := range []string{
		`SELECT * FROM private.push_subscriptions_for('x', 'y')`,
		`SELECT private.push_subscription_touch('x', 'y')`,
		`SELECT private.push_subscription_gone('x', 'y')`,
		`SELECT private.release_reminder('00000000-0000-0000-0000-000000000000'::uuid, now())`,
	} {
		rows3, qerr := runtimePool.Query(ctx, stmt)
		denied := qerr != nil
		if qerr == nil {
			for rows3.Next() {
			}
			if rerr := rows3.Err(); rerr != nil {
				denied = true
				qerr = rerr
			}
			rows3.Close()
		}
		if !denied {
			t.Errorf("app_runtime executed %q, want 42501", stmt)
		} else if !strings.Contains(qerr.Error(), "42501") && !strings.Contains(qerr.Error(), "permission denied") {
			t.Errorf("app_runtime on %q: unexpected error %v (want permission denied)", stmt, qerr)
		}
	}

	// The Edge credential still holds zero table grants (059 keeps every
	// row touch behind definer functions).
	for _, stmt := range []string{
		`SELECT count(*) FROM push_subscriptions`,
		`SELECT count(*) FROM agent_reminders`,
	} {
		rows4, qerr := callerPool.Query(ctx, stmt)
		denied := qerr != nil
		if qerr == nil {
			for rows4.Next() {
			}
			if rerr := rows4.Err(); rerr != nil {
				denied = true
				qerr = rerr
			}
			rows4.Close()
		}
		if !denied {
			t.Errorf("delivery_caller read %q, want 42501", stmt)
		} else if !strings.Contains(qerr.Error(), "42501") && !strings.Contains(qerr.Error(), "permission denied") {
			t.Errorf("delivery_caller on %q: unexpected error %v (want permission denied)", stmt, qerr)
		}
	}
}
