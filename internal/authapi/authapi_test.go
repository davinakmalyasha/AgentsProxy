package authapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/db"
)

// fakeProvider proves the platform auth shell is project-agnostic. It owns an
// in-memory user map; the real Centrum provider selects from sports_users.
type fakeProvider struct {
	mu      sync.Mutex
	users   map[string]fakeUser
	tenants map[string]bool
	seq     int
}

type fakeUser struct {
	User   User
	Hash   string
	Active bool
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{users: map[string]fakeUser{}, tenants: map[string]bool{}}
}

func (f *fakeProvider) setTenantActive(tenant string, active bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenants[tenant] = active
}

func key(tenant, email string) string { return tenant + "\x00" + email }

func (f *fakeProvider) add(tenant, email, password, role, name string, active bool) User {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.tenants[tenant] = true
	id := fmt.Sprintf("00000000-0000-0000-0000-%012d", f.seq)
	hash, _ := HashPassword(password)
	u := User{ID: id, TenantID: tenant, Email: email, Role: role, FullName: name}
	f.users[key(tenant, email)] = fakeUser{User: u, Hash: hash, Active: active}
	return u
}

func (f *fakeProvider) Authenticate(_ context.Context, tenantID, email string) (Credentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if active, known := f.tenants[tenantID]; known && !active {
		return Credentials{}, ErrInvalidCredentials
	}
	row, ok := f.users[key(tenantID, email)]
	if !ok {
		return Credentials{}, ErrInvalidCredentials
	}
	return Credentials{User: row.User, PasswordHash: row.Hash, Active: row.Active}, nil
}

func (f *fakeProvider) Profile(_ context.Context, tenantID, userID string) (User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if active, known := f.tenants[tenantID]; known && !active {
		return User{}, ErrInvalidCredentials
	}
	for _, row := range f.users {
		if row.User.TenantID == tenantID && row.User.ID == userID && row.Active {
			return row.User, nil
		}
	}
	return User{}, ErrInvalidCredentials
}

func (f *fakeProvider) Register(_ context.Context, tenantID string, in RegisterInput) (User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.Role != "owner" && in.Role != "pegawai" {
		return User{}, ErrInvalidRole
	}
	if _, exists := f.users[key(tenantID, in.Email)]; exists {
		return User{}, ErrEmailTaken
	}
	f.seq++
	id := fmt.Sprintf("00000000-0000-0000-0000-%012d", f.seq)
	u := User{ID: id, TenantID: tenantID, Email: in.Email, Role: in.Role, FullName: in.FullName, Phone: in.Phone}
	f.users[key(tenantID, in.Email)] = fakeUser{User: u, Hash: in.PasswordHash, Active: true}
	return u, nil
}

func setup(t *testing.T) (*Auth, *pgxpool.Pool, *fakeProvider, context.Context) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping auth test")
	}
	adminDSN := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if adminDSN == "" {
		adminDSN = dsn
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	pool, err := db.NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.MigrateAll(ctx, pool); err != nil {
		t.Fatal(err)
	}

	const tenant = "auth-t"
	cleanup := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_, _ = pool.Exec(cctx, `DELETE FROM public.clients WHERE tenant_id=$1`, tenant)
	}
	cleanup()
	t.Cleanup(cleanup)
	if _, err := pool.Exec(ctx,
		`INSERT INTO public.clients
		   (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ('auth test', $1, 'hash-auth-t', 1000, 'active', 'sports', ARRAY['records'], 'public')`,
		tenant); err != nil {
		t.Fatal(err)
	}

	a := New(pool, nil)
	fake := newFakeProvider()
	a.RegisterProvider(tenant, fake)
	return a, pool, fake, ctx
}

func do(t *testing.T, a *Auth, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Session-Token", token)
	}
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
}

func login(t *testing.T, a *Auth, tenant, email, password string) string {
	t.Helper()
	rec := do(t, a, http.MethodPost, "/login", "",
		`{"tenant_id":"`+tenant+`","email":"`+email+`","password":"`+password+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Token string `json:"token"`
	}
	decode(t, rec, &out)
	if out.Token == "" {
		t.Fatal("login returned no token")
	}
	return out.Token
}

func TestLoginMeLogout(t *testing.T) {
	a, _, fake, _ := setup(t)
	fake.add("auth-t", "owner@test.dev", "sekret123", "owner", "Owner", true)

	tok := login(t, a, "auth-t", "owner@test.dev", "sekret123")
	rec := do(t, a, http.MethodGet, "/me", tok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("me status=%d body=%s", rec.Code, rec.Body.String())
	}
	var me struct {
		User User `json:"user"`
	}
	decode(t, rec, &me)
	if me.User.Email != "owner@test.dev" || me.User.TenantID != "auth-t" || me.User.Role != "owner" {
		t.Fatalf("me=%+v", me.User)
	}

	if rec := do(t, a, http.MethodPost, "/logout", tok, ""); rec.Code != http.StatusOK {
		t.Fatalf("logout status=%d", rec.Code)
	}
	if rec := do(t, a, http.MethodGet, "/me", tok, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout=%d want 401", rec.Code)
	}
}

func TestLoginRejectsUnknownTenantAndBadPassword(t *testing.T) {
	a, _, fake, _ := setup(t)
	fake.add("auth-t", "owner@test.dev", "sekret123", "owner", "Owner", true)

	if rec := do(t, a, http.MethodPost, "/login", "",
		`{"tenant_id":"nope","email":"owner@test.dev","password":"sekret123"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown tenant=%d want 401", rec.Code)
	}
	if rec := do(t, a, http.MethodPost, "/login", "",
		`{"tenant_id":"auth-t","email":"owner@test.dev","password":"wrong"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad password=%d want 401", rec.Code)
	}
	if rec := do(t, a, http.MethodPost, "/login", "", `{"tenant_id":"auth-t"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing fields=%d want 400", rec.Code)
	}
}

func TestRegisterRequiresOwnerAndRejectsDuplicates(t *testing.T) {
	a, _, fake, _ := setup(t)
	fake.add("auth-t", "owner@test.dev", "sekret123", "owner", "Owner", true)
	fake.add("auth-t", "staff@test.dev", "sekret123", "pegawai", "Staff", true)

	staffTok := login(t, a, "auth-t", "staff@test.dev", "sekret123")
	if rec := do(t, a, http.MethodPost, "/register", staffTok,
		`{"email":"new@test.dev","password":"sekret123","full_name":"New"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("staff register=%d want 403", rec.Code)
	}

	ownerTok := login(t, a, "auth-t", "owner@test.dev", "sekret123")
	rec := do(t, a, http.MethodPost, "/register", ownerTok,
		`{"email":"new@test.dev","password":"sekret123","full_name":"New"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("owner register=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, a, http.MethodPost, "/register", ownerTok,
		`{"email":"new@test.dev","password":"sekret123","full_name":"New"}`); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate register=%d want 409", rec.Code)
	}
	if rec := do(t, a, http.MethodPost, "/register", ownerTok,
		`{"email":"other@test.dev","password":"sekret123","role":"admin"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad role=%d want 400", rec.Code)
	}
	if rec := do(t, a, http.MethodPost, "/register", ownerTok,
		`{"email":"short@test.dev","password":"short"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("short password=%d want 400", rec.Code)
	}
}

func TestExpiredAndPausedSessionsFail(t *testing.T) {
	a, pool, fake, ctx := setup(t)
	fake.add("auth-t", "owner@test.dev", "sekret123", "owner", "Owner", true)

	tok := login(t, a, "auth-t", "owner@test.dev", "sekret123")
	if _, err := pool.Exec(ctx,
		`UPDATE public.staff_sessions SET expires_at = now() - interval '1 hour' WHERE token_hash=$1`,
		HashToken(tok)); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, a, http.MethodGet, "/me", tok, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired session=%d want 401", rec.Code)
	}

	tok = login(t, a, "auth-t", "owner@test.dev", "sekret123")
	if _, err := pool.Exec(ctx, `UPDATE public.clients SET status='paused' WHERE tenant_id='auth-t'`); err != nil {
		t.Fatal(err)
	}
	fake.setTenantActive("auth-t", false)
	if rec := do(t, a, http.MethodGet, "/me", tok, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("paused tenant session=%d want 401", rec.Code)
	}
	if rec := do(t, a, http.MethodPost, "/login", "",
		`{"tenant_id":"auth-t","email":"owner@test.dev","password":"sekret123"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("paused tenant login=%d want 401", rec.Code)
	}
}

func TestSessionCreateRejectsInactiveTenant(t *testing.T) {
	_, pool, _, ctx := setup(t)
	var id *string
	if err := pool.QueryRow(ctx,
		`SELECT private.staff_session_create('auth-t', $1::uuid, 'active-hash', now() + interval '1 hour')::text`,
		"00000000-0000-0000-0000-000000000001").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id == nil {
		t.Fatal("active tenant session was not created")
	}
	if _, err := pool.Exec(ctx, `UPDATE public.clients SET status='paused' WHERE tenant_id='auth-t'`); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT private.staff_session_create('auth-t', $1::uuid, 'inactive-hash', now() + interval '1 hour')::text`,
		"00000000-0000-0000-0000-000000000001").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id != nil {
		t.Fatalf("paused tenant session created: %v", *id)
	}
}

func TestUserByTokenReturnsNoRowsForUnknownProvider(t *testing.T) {
	_, pool, _, ctx := setup(t)
	// A session row for a tenant with no registered provider must be an
	// invalid session, not a 500. New() only knows the globally registered
	// providers (Centrum), not this test tenant.
	if _, err := pool.Exec(ctx,
		`INSERT INTO public.staff_sessions (tenant_id, user_id, token_hash, expires_at)
		 VALUES ('auth-t', $1::uuid, $2, now() + interval '1 hour')`,
		"00000000-0000-0000-0000-000000000002", HashToken("orphan-token")); err != nil {
		t.Fatal(err)
	}
	bare := New(pool, nil)
	if _, err := bare.UserByToken(ctx, "orphan-token"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unknown provider err=%v, want pgx.ErrNoRows", err)
	}
}
