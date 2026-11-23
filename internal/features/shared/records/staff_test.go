package records

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/davinakmalyasha/agents-proxy/internal/authapi"
	"github.com/davinakmalyasha/agents-proxy/internal/cache"
)

// staffProvider is the smallest auth provider that can answer a session
// profile. The records surface must accept a staff session without
// fabricating a client model; it needs only the tenant, the live role, and
// the tenant's clients.id for usage attribution.
type staffProvider struct {
	user authapi.User
}

func (p staffProvider) Authenticate(context.Context, string, string) (authapi.Credentials, error) {
	return authapi.Credentials{}, authapi.ErrInvalidCredentials
}

func (p staffProvider) Profile(context.Context, string, string) (authapi.User, error) {
	return p.user, nil
}

func (p staffProvider) Register(context.Context, string, authapi.RegisterInput) (authapi.User, error) {
	return authapi.User{}, authapi.ErrInvalidCredentials
}

// TestRecords_StaffSession proves the unified identity path reaches the same
// CRUD engine as a client token. Before this, records required a client model
// and a staff session got a 401; the fix carries clients.id on the identity
// instead of pretending the staff member is a client token.
func TestRecords_StaffSession(t *testing.T) {
	h := setupRecords(t)
	ctx := context.Background()

	var clientID string
	if err := h.admin.QueryRow(ctx,
		`SELECT id::text FROM clients WHERE tenant_id = 'tenant-a'`).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	userID := uuid.NewString()
	raw, hash, err := authapi.GenerateSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.Exec(ctx,
		`INSERT INTO public.staff_sessions (tenant_id, user_id, token_hash, expires_at)
		 VALUES ('tenant-a', $1, $2, now() + interval '1 hour')`,
		userID, hash); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = h.admin.Exec(context.Background(), `DELETE FROM staff_sessions WHERE token_hash = $1`, hash)
	})

	auth := authapi.New(h.pool, h.rdb)
	auth.RegisterProvider("tenant-a", staffProvider{user: authapi.User{
		ID: userID, TenantID: "tenant-a", Email: "staff@test.dev", Role: "owner",
		ClientID: clientID, Vertical: "fnb", Features: []string{"records"},
		DBSchema: "public",
	}})
	cli := &cache.Client{DB: h.pool, RDB: h.rdb}
	rec := New(Deps{Pool: h.pool, Cache: cli})
	router := WithIdentity(IdentityDeps{
		Pool: h.pool, Cache: cli, Auth: auth,
	})(rec.Routes("fnb"))

	srv := httptest.NewServer(router)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/",
		bytes.NewReader([]byte(`{"kind":"item","title":"Staff item","payload":{}}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Token", raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("staff create status = %d, want 201", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("X-Session-Token", raw)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("staff list status = %d, want 200", resp.StatusCode)
	}
}
