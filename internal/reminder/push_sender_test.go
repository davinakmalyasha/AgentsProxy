package reminder

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SherClockHolmes/webpush-go"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pushHarness spins a fake push service: fixed status per hit, with
// captured Authorization headers and payloads for assertion.
type pushHarness struct {
	srv      *httptest.Server
	status   int
	gotAuth  []string
	gotEnc   []string
	gotTTL   []string
	gotBody  []json.RawMessage
	endpoint string
}

func newPushHarness(t *testing.T, status int) *pushHarness {
	t.Helper()
	h := &pushHarness{status: status}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.gotAuth = append(h.gotAuth, r.Header.Get("Authorization"))
		h.gotEnc = append(h.gotEnc, r.Header.Get("Content-Encoding"))
		h.gotTTL = append(h.gotTTL, r.Header.Get("TTL"))
		body, _ := io.ReadAll(r.Body)
		h.gotBody = append(h.gotBody, append([]byte(nil), body...))
		w.WriteHeader(h.status)
	}))
	t.Cleanup(h.srv.Close)
	h.endpoint = h.srv.URL + "/push/abc123"
	return h
}

// realSubKeys generates a valid P-256/auth pair: the sender encrypts
// (RFC8291), so fixture strings fail before any HTTP happens.
func realSubKeys(t *testing.T) (p256dh, auth string) {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := key.PublicKey().Bytes()
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(pub),
		base64.RawURLEncoding.EncodeToString(salt)
}

// seedPushClient creates tenant + one subscription pointing at h.
// Cleanup removes both (registered after testPool's pool.Close, so it
// runs first — the shared-suite rule from the drivers test).
func seedPushClient(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenant, ref, endpoint string) {
	t.Helper()
	p256dh, auth := realSubKeys(t)
	if _, err := pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status)
		 VALUES ($1, $1, 'hash-push', 1000000, 'active')
		 ON CONFLICT (tenant_id) DO NOTHING`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO push_subscriptions (tenant_id, user_ref, endpoint, p256dh, auth)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (tenant_id, endpoint) DO NOTHING`, tenant, ref, endpoint, p256dh, auth); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = pool.Exec(cctx, `DELETE FROM push_subscriptions WHERE tenant_id = $1`, tenant)
		_, _ = pool.Exec(cctx, `DELETE FROM clients WHERE tenant_id = $1`, tenant)
	})
}

func testPushSender(pool *pgxpool.Pool, client *http.Client) *PushSender {
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		panic(err)
	}
	return &PushSender{
		Pool: pool, VAPIDPublic: pub, VAPIDPrivate: priv,
		Subject: "mailto:test@example.id", HTTP: client,
	}
}

func TestPushSender_DeliversAndTouches(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const tenant = "rem-push-ok"
	h := newPushHarness(t, http.StatusCreated)
	seedPushClient(t, ctx, pool, tenant, "owner", h.endpoint)

	s := testPushSender(pool, h.srv.Client())
	// Payload is RFC8291-encrypted in transit: the service sees opaque
	// ciphertext, so assert transport properties (VAPID auth, aes128gcm
	// encoding, TTL), never plaintext.
	if err := s.Send(ctx, tenant, "owner", "court at 7"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(h.gotAuth) != 1 || !strings.HasPrefix(h.gotAuth[0], "vapid ") {
		t.Errorf("auth headers = %v, want one VAPID header", h.gotAuth)
	}
	if len(h.gotEnc) != 1 || h.gotEnc[0] != "aes128gcm" {
		t.Errorf("content-encoding = %v, want aes128gcm (encrypted)", h.gotEnc)
	}
	if len(h.gotTTL) != 1 || h.gotTTL[0] != "86400" {
		t.Errorf("ttl = %v, want 86400", h.gotTTL)
	}
	if len(h.gotBody) != 1 || len(h.gotBody[0]) == 0 {
		t.Fatalf("bodies = %d, want 1 non-empty ciphertext", len(h.gotBody))
	}
	var lastUsed *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_used_at FROM push_subscriptions WHERE tenant_id = $1`, tenant).Scan(&lastUsed); err != nil {
		t.Fatal(err)
	}
	if lastUsed == nil {
		t.Error("last_used_at not stamped on success")
	}
}

func TestPushSender_GonePrunesAndFailsTransient(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const tenant = "rem-push-gone"
	h := newPushHarness(t, http.StatusGone)
	seedPushClient(t, ctx, pool, tenant, "owner", h.endpoint)

	s := testPushSender(pool, h.srv.Client())
	if err := s.Send(ctx, tenant, "owner", "x"); err == nil {
		t.Fatal("Send to 410 endpoint succeeded, want transient error")
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM push_subscriptions WHERE tenant_id = $1`, tenant).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("gone subscription not pruned, rows = %d", n)
	}
}

// TestScheduler_PushEndToEnd proves the full loop on a real database:
// Tick claims a due row, PushSender delivers to the fake push service,
// the row lands sent and the subscription is stamped. Uses its own
// tenant and asserts only its own row (Tick claims globally).
func TestScheduler_PushEndToEnd(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const tenant = "rem-push-e2e"
	h := newPushHarness(t, http.StatusCreated)
	seedPushClient(t, ctx, pool, tenant, "owner", h.endpoint)
	if _, err := pool.Exec(ctx,
		`INSERT INTO agent_reminders (tenant_id, chat_id, text, remind_at, recipient_ref)
		 VALUES ($1, '', 'TEST-PUSH-E2E', now() - interval '1 minute', 'owner')`, tenant); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = pool.Exec(cctx, `DELETE FROM agent_reminders WHERE tenant_id = $1`, tenant)
	})

	s := &Scheduler{Pool: pool, Send: testPushSender(pool, h.srv.Client()).Send}
	s.Tick()

	var status string
	var attempts int
	if err := pool.QueryRow(ctx,
		`SELECT status, attempts FROM agent_reminders WHERE tenant_id = $1`, tenant).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "sent" || attempts != 0 {
		t.Errorf("status=%q attempts=%d, want sent/0", status, attempts)
	}
	if len(h.gotAuth) != 1 {
		t.Errorf("push hits = %d, want 1", len(h.gotAuth))
	}
}

// TestScheduler_NoSubscriptionsHoldsWithoutBurning proves pre-launch
// safety: a live sender with zero subscribed devices holds rows in
// pending WITHOUT consuming the attempts budget.
func TestScheduler_NoSubscriptionsHoldsWithoutBurning(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const tenant = "rem-push-hold"
	if _, err := pool.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status)
		 VALUES ($1, $1, 'hash-hold', 1000000, 'active')
		 ON CONFLICT (tenant_id) DO NOTHING`, tenant); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = pool.Exec(cctx, `DELETE FROM agent_reminders WHERE tenant_id = $1`, tenant)
		_, _ = pool.Exec(cctx, `DELETE FROM clients WHERE tenant_id = $1`, tenant)
	})
	if _, err := pool.Exec(ctx,
		`INSERT INTO agent_reminders (tenant_id, chat_id, text, remind_at, recipient_ref)
		 VALUES ($1, '', 'TEST-PUSH-HOLD', now() - interval '1 minute', 'owner')`, tenant); err != nil {
		t.Fatal(err)
	}
	s := &Scheduler{Pool: pool, Send: testPushSender(pool, nil).Send}
	for i := 0; i < 7; i++ {
		s.Tick()
	}
	var status string
	var attempts int
	if err := pool.QueryRow(ctx,
		`SELECT status, attempts FROM agent_reminders WHERE tenant_id = $1`, tenant).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || attempts != 0 {
		t.Errorf("status=%q attempts=%d, want pending/0 (held, budget intact)", status, attempts)
	}
}

func TestPushSender_NoSubscriptionsPreserves(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := testPushSender(pool, nil)
	if err := s.Send(ctx, "rem-push-none", "owner", "x"); !errors.Is(err, errNoSubscriptions) {
		t.Errorf("Send with no subscriptions = %v, want errNoSubscriptions", err)
	}
	if err := s.Send(ctx, "rem-push-none", "", "x"); !errors.Is(err, errNoSubscriptions) {
		t.Errorf("Send with empty ref = %v, want errNoSubscriptions", err)
	}
}
