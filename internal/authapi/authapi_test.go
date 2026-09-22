// Auth API tests: bcrypt, login, me, logout, expiry.
//
// DONE: Phase 7 (staff auth) / STANDARDS A, D, F
package authapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/db"
)

func harness(t *testing.T) (*Auth, *pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	adminDSN := os.Getenv("DATABASE_ADMIN_URL")
	if dsn == "" || adminDSN == "" {
		t.Skip("DATABASE_URL / DATABASE_ADMIN_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := db.NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	// Ensure grants for the runtime role (idempotent).
	for _, g := range []string{
		`GRANT SELECT, INSERT, UPDATE, DELETE ON sports_users TO app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON sports_sessions TO app`,
	} {
		if _, err := admin.Exec(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	// Cleanup + seed an owner user for tenant 'auth-t'.
	for _, stmt := range []string{
		`DELETE FROM sports_sessions WHERE user_id IN (SELECT id FROM sports_users WHERE tenant_id='auth-t')`,
		`DELETE FROM sports_users WHERE tenant_id='auth-t'`,
		`DELETE FROM clients WHERE tenant_id='auth-t'`,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO clients (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features)
		 VALUES ('auth-t','auth-t','hash-auth-t',1000,'active','sports',ARRAY['records'])`); err != nil {
		t.Fatal(err)
	}
	hash, _ := HashPassword("sekret123")
	if _, err := admin.Exec(ctx,
		`INSERT INTO sports_users (tenant_id, email, password_hash, role, full_name)
		 VALUES ('auth-t','owner@centrum.test',$1,'owner','Budi')`, hash); err != nil {
		t.Fatal(err)
	}

	pool, err := db.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &Auth{Pool: pool}, admin, hash
}

func post(t *testing.T, h http.Handler, path, body, token string) *http.Response {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	var req *http.Request
	if body != "" {
		req, _ = http.NewRequest("POST", srv.URL+path, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, _ = http.NewRequest("POST", srv.URL+path, nil)
	}
	if token != "" {
		req.Header.Set("X-Session-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func get(t *testing.T, h http.Handler, path, token string) *http.Response {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	if token != "" {
		req.Header.Set("X-Session-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

func TestHashPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("pw")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, "pw") {
		t.Error("correct password rejected")
	}
	if VerifyPassword(hash, "wrong") {
		t.Error("wrong password accepted")
	}
	if hash == "pw" {
		t.Error("password stored in plaintext")
	}
}

func TestLoginSuccessAndMe(t *testing.T) {
	a, _, _ := harness(t)
	mux := a.Routes()
	resp := post(t, mux, "/login", `{"tenant_id":"auth-t","email":"owner@centrum.test","password":"sekret123"}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("login status=%d", resp.StatusCode)
	}
	m := readJSON(t, resp)
	tok, _ := m["token"].(string)
	if tok == "" {
		t.Fatal("no token")
	}
	u := m["user"].(map[string]any)
	if u["role"] != "owner" {
		t.Errorf("role=%v want owner", u["role"])
	}
	// /me with the token.
	resp2 := get(t, mux, "/me", tok)
	if resp2.StatusCode != 200 {
		t.Fatalf("me status=%d", resp2.StatusCode)
	}
	m2 := readJSON(t, resp2)
	if m2["user"].(map[string]any)["email"] != "owner@centrum.test" {
		t.Errorf("me user=%v", m2["user"])
	}
	// Logout → token invalid.
	post(t, mux, "/logout", "", tok)
	resp3 := get(t, mux, "/me", tok)
	if resp3.StatusCode != 401 {
		t.Errorf("me after logout status=%d want 401", resp3.StatusCode)
	}
}

// TestRegisterOwnerSession is the regression test for the live 500:
// register INSERT ran on the runtime pool without the tenant GUC, so
// the RLS WITH CHECK rejected the row. The harness Auth uses the
// runtime-role pool (like production), so this fails without the fix.
func TestRegisterOwnerSession(t *testing.T) {
	a, _, _ := harness(t)
	mux := a.Routes()
	resp := post(t, mux, "/login", `{"tenant_id":"auth-t","email":"owner@centrum.test","password":"sekret123"}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("login status=%d", resp.StatusCode)
	}
	sess := readJSON(t, resp)["token"].(string)

	r1 := post(t, mux, "/register", `{"email":"peg@centrum.test","password":"pegawai123","full_name":"Peg"}`, sess)
	if r1.StatusCode != 201 {
		t.Fatalf("register status=%d want 201", r1.StatusCode)
	}
	r1.Body.Close()

	// Duplicate email → 409.
	r2 := post(t, mux, "/register", `{"email":"peg@centrum.test","password":"pegawai123","full_name":"Dup"}`, sess)
	if r2.StatusCode != 409 {
		t.Errorf("dup register status=%d want 409", r2.StatusCode)
	}
	r2.Body.Close()

	// No session (client API token is not a staff identity) → 401.
	r3 := post(t, mux, "/register", `{"email":"x@centrum.test","password":"pegawai123","full_name":"X"}`, "")
	if r3.StatusCode != 401 {
		t.Errorf("no-session register status=%d want 401", r3.StatusCode)
	}
	r3.Body.Close()

	// New pegawai can log in.
	r4 := post(t, mux, "/login", `{"tenant_id":"auth-t","email":"peg@centrum.test","password":"pegawai123"}`, "")
	if r4.StatusCode != 200 {
		t.Errorf("pegawai login status=%d want 200", r4.StatusCode)
	}
	r4.Body.Close()
}

func TestLoginWrongPassword(t *testing.T) {
	a, _, _ := harness(t)
	resp := post(t, a.Routes(), "/login", `{"tenant_id":"auth-t","email":"owner@centrum.test","password":"wrong"}`, "")
	if resp.StatusCode != 401 {
		t.Errorf("wrong pw status=%d want 401", resp.StatusCode)
	}
}

func TestLoginUnknownUserUniform(t *testing.T) {
	a, _, _ := harness(t)
	resp := post(t, a.Routes(), "/login", `{"tenant_id":"auth-t","email":"nobody@x.test","password":"sekret123"}`, "")
	m := readJSON(t, resp)
	if resp.StatusCode != 401 || m["error"] != "invalid credentials" {
		t.Errorf("unknown user: status=%d body=%v", resp.StatusCode, m)
	}
}

func TestSessionExpiry(t *testing.T) {
	a, admin, _ := harness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Insert an already-expired session directly.
	raw, hash, _ := GenerateSessionToken()
	if _, err := admin.Exec(ctx,
		`INSERT INTO sports_sessions (user_id, token_hash, expires_at)
		 SELECT id, $1, now() - interval '1 hour' FROM sports_users WHERE tenant_id='auth-t'`,
		hash); err != nil {
		t.Fatal(err)
	}
	resp := get(t, a.Routes(), "/me", raw)
	if resp.StatusCode != 401 {
		t.Errorf("expired session status=%d want 401", resp.StatusCode)
	}
}

// TestUserByToken_ThrottlesLastSeen: UserByToken bumps last_seen_at at
// most ~once a minute, so a busy dashboard doesn't take a write lock
// on the session row every authed request.
func TestUserByToken_ThrottlesLastSeen(t *testing.T) {
	a, admin, _ := harness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, hash, _ := GenerateSessionToken()
	if _, err := admin.Exec(ctx,
		`INSERT INTO sports_sessions (user_id, token_hash, expires_at)
		 SELECT id, $1, now() + interval '1 hour' FROM sports_users WHERE tenant_id='auth-t'`,
		hash); err != nil {
		t.Fatal(err)
	}

	// First call sets last_seen_at (row default now()).
	if _, err := a.UserByToken(ctx, raw); err != nil {
		t.Fatal(err)
	}
	var first time.Time
	if err := admin.QueryRow(ctx, `SELECT last_seen_at FROM sports_sessions WHERE token_hash=$1`, hash).Scan(&first); err != nil {
		t.Fatal(err)
	}

	// A second call within the same minute must NOT rewrite it.
	time.Sleep(50 * time.Millisecond)
	if _, err := a.UserByToken(ctx, raw); err != nil {
		t.Fatal(err)
	}
	var second time.Time
	if err := admin.QueryRow(ctx, `SELECT last_seen_at FROM sports_sessions WHERE token_hash=$1`, hash).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if !second.Equal(first) {
		t.Errorf("last_seen advanced on throttled call: %v -> %v", first, second)
	}

	// Backdate last_seen_at > 1 min → next call bumps it.
	if _, err := admin.Exec(ctx,
		`UPDATE sports_sessions SET last_seen_at = now() - interval '2 minutes' WHERE token_hash=$1`, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := a.UserByToken(ctx, raw); err != nil {
		t.Fatal(err)
	}
	var third time.Time
	if err := admin.QueryRow(ctx, `SELECT last_seen_at FROM sports_sessions WHERE token_hash=$1`, hash).Scan(&third); err != nil {
		t.Fatal(err)
	}
	if !third.After(first) {
		t.Errorf("last_seen did not bump after backdate: %v -> %v", first, third)
	}
}
