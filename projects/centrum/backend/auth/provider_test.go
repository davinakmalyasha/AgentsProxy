package auth_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/authapi"
	"github.com/davinakmalyasha/agents-proxy/internal/db"
	"github.com/davinakmalyasha/agents-proxy/projects/centrum/backend/auth"
	"github.com/davinakmalyasha/agents-proxy/projects/centrum/backend/testutil"
)

type fixture struct {
	ctx      context.Context
	admin    *pgxpool.Pool
	runtime  *pgxpool.Pool
	provider authapi.Provider
	schema   string
	tenant   string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping provider test")
	}
	adminDSN := os.Getenv("TEST_DATABASE_ADMIN_URL")
	if adminDSN == "" {
		adminDSN = dsn
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	admin, err := db.NewPool(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	testutil.Migrate(t, admin)

	const tenant = "auth-prov"
	cleanup := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_, _ = admin.Exec(cctx, `DELETE FROM public.clients WHERE tenant_id=$1`, tenant)
	}
	cleanup()
	t.Cleanup(cleanup)

	schema := testutil.CloneTenantSchema(t, ctx, admin, tenant)
	if _, err := admin.Exec(ctx,
		`INSERT INTO public.clients
		   (name, tenant_id, token_hash, monthly_quota_tokens, status, vertical, features, db_schema)
		 VALUES ('provider test', $1, 'hash-auth-prov', 1000, 'active', 'sports', ARRAY['records'], $2)`,
		tenant, schema); err != nil {
		t.Fatal(err)
	}
	runtimePool := testutil.RuntimePool(t, ctx, dsn)
	return &fixture{
		ctx:      ctx,
		admin:    admin,
		runtime:  runtimePool,
		provider: auth.New(runtimePool),
		schema:   schema,
		tenant:   tenant,
	}
}

func (f *fixture) addUser(t *testing.T, email, password, role, name string, active bool) string {
	t.Helper()
	hash, err := authapi.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := f.admin.QueryRow(f.ctx, fmt.Sprintf(
		`INSERT INTO %s (tenant_id, email, password_hash, role, full_name, is_active)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id::text`,
		testutil.Table(f.schema, "sports_users")),
		f.tenant, email, hash, role, name, active).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestProviderAuthenticateAndProfile(t *testing.T) {
	f := setup(t)
	id := f.addUser(t, "owner@test.dev", "sekret123", "owner", "Owner", true)

	cred, err := f.provider.Authenticate(f.ctx, f.tenant, "owner@test.dev")
	if err != nil {
		t.Fatal(err)
	}
	if cred.User.ID != id || cred.User.Role != "owner" || !cred.Active {
		t.Fatalf("credentials=%+v active=%v", cred.User, cred.Active)
	}
	if !authapi.VerifyPassword(cred.PasswordHash, "sekret123") {
		t.Fatal("returned hash does not verify")
	}

	u, err := f.provider.Profile(f.ctx, f.tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "owner@test.dev" || u.TenantID != f.tenant || u.FullName != "Owner" {
		t.Fatalf("profile=%+v", u)
	}

	if _, err := f.provider.Authenticate(f.ctx, f.tenant, "missing@test.dev"); !errors.Is(err, authapi.ErrInvalidCredentials) {
		t.Fatalf("missing user err=%v", err)
	}
	if _, err := f.provider.Authenticate(f.ctx, "other-tenant", "owner@test.dev"); !errors.Is(err, authapi.ErrInvalidCredentials) {
		t.Fatalf("wrong tenant err=%v", err)
	}
	if _, err := f.provider.Profile(f.ctx, "other-tenant", id); !errors.Is(err, authapi.ErrInvalidCredentials) {
		t.Fatalf("cross-tenant profile err=%v", err)
	}

	if _, err := f.admin.Exec(f.ctx, fmt.Sprintf(
		`UPDATE %s SET is_active=false WHERE id=$1::uuid`,
		testutil.Table(f.schema, "sports_users")), id); err != nil {
		t.Fatal(err)
	}
	inactive, err := f.provider.Authenticate(f.ctx, f.tenant, "owner@test.dev")
	if err != nil {
		t.Fatalf("inactive user lookup err=%v", err)
	}
	if inactive.Active {
		t.Fatal("inactive user returned Active=true")
	}
	if _, err := f.provider.Profile(f.ctx, f.tenant, id); !errors.Is(err, authapi.ErrInvalidCredentials) {
		t.Fatalf("inactive profile err=%v", err)
	}
}

func TestProviderRegisterAndRoleRules(t *testing.T) {
	f := setup(t)
	hash, err := authapi.HashPassword("sekret123")
	if err != nil {
		t.Fatal(err)
	}
	u, err := f.provider.Register(f.ctx, f.tenant, authapi.RegisterInput{
		Email:        "Staff@Test.Dev",
		PasswordHash: hash,
		Role:         "pegawai",
		FullName:     "Staff",
		Phone:        "0812",
	})
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "staff@test.dev" || u.Role != "pegawai" || u.TenantID != f.tenant {
		t.Fatalf("registered=%+v", u)
	}
	if _, err := f.provider.Register(f.ctx, f.tenant, authapi.RegisterInput{
		Email: "staff@test.dev", PasswordHash: hash, Role: "pegawai",
	}); !errors.Is(err, authapi.ErrEmailTaken) {
		t.Fatalf("duplicate err=%v", err)
	}
	if _, err := f.provider.Register(f.ctx, f.tenant, authapi.RegisterInput{
		Email: "admin@test.dev", PasswordHash: hash, Role: "admin",
	}); !errors.Is(err, authapi.ErrInvalidRole) {
		t.Fatalf("role err=%v", err)
	}
}

func TestProviderFollowsTenantSchemaNotBase(t *testing.T) {
	f := setup(t)
	// A row with the same tenant id in the base schema must stay invisible:
	// the provider resolves clients.db_schema, which points at the clone.
	const email = "base-only@test.dev"
	hash, _ := authapi.HashPassword("sekret123")
	var baseID string
	if err := f.admin.QueryRow(f.ctx,
		`INSERT INTO `+testutil.Table(testutil.SourceSchema, "sports_users")+`
		   (tenant_id, email, password_hash, role, full_name)
		 VALUES ($1,$2,$3,'owner','Base') RETURNING id::text`,
		f.tenant, email, hash).Scan(&baseID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.admin.Exec(context.Background(),
			`DELETE FROM `+testutil.Table(testutil.SourceSchema, "sports_users")+` WHERE id=$1::uuid`, baseID)
	})

	if _, err := f.provider.Authenticate(f.ctx, f.tenant, email); !errors.Is(err, authapi.ErrInvalidCredentials) {
		t.Fatalf("base-schema user leaked into clone lookup: %v", err)
	}
}

func TestProviderRejectsPausedTenant(t *testing.T) {
	f := setup(t)
	id := f.addUser(t, "owner@test.dev", "sekret123", "owner", "Owner", true)
	if _, err := f.admin.Exec(f.ctx,
		`UPDATE public.clients SET status='paused' WHERE tenant_id=$1`, f.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := f.provider.Authenticate(f.ctx, f.tenant, "owner@test.dev"); !errors.Is(err, authapi.ErrInvalidCredentials) {
		t.Fatalf("paused authenticate err=%v", err)
	}
	if _, err := f.provider.Profile(f.ctx, f.tenant, id); !errors.Is(err, authapi.ErrInvalidCredentials) {
		t.Fatalf("paused profile err=%v", err)
	}
}
