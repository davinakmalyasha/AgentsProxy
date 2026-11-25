package authapi

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// User is the public staff-user shape returned by login/me/register.
// It is platform vocabulary: a project provider maps its own user row
// into this type.
type User struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	FullName string `json:"full_name"`
	Phone    string `json:"phone,omitempty"`
	// Vertical and Features are the live tenant entitlement, resolved by
	// the project provider from public.clients. The feature gate must not
	// rely on a stale cached role/profile; a disabled feature takes effect
	// on the next request.
	Vertical string   `json:"vertical,omitempty"`
	Features []string `json:"features,omitempty"`
	// ClientID is the tenant's clients.id, used to attribute staff
	// business writes to the same client the tenant's token belongs to.
	// It is internal and not part of /auth/me.
	ClientID string `json:"-"`
	// DBSchema is the tenant's business schema, resolved by the project
	// provider. Route gates compare it with the mounted surface.
	DBSchema string `json:"db_schema,omitempty"`
}

// Credentials is what a provider returns from a login lookup. The platform
// verifies PasswordHash; it never asks the provider to compare passwords.
type Credentials struct {
	User         User
	PasswordHash string
	Active       bool
}

// RegisterInput is a already-validated registration request. The platform
// normalizes the email and hashes the password; the provider owns the row.
type RegisterInput struct {
	Email        string
	PasswordHash string
	Role         string
	FullName     string
	Phone        string
}

var (
	// ErrInvalidCredentials is returned for an unknown email, wrong password,
	// inactive user, unknown tenant, or paused tenant. Callers must not reveal
	// which one it was.
	ErrInvalidCredentials = errors.New("authapi: invalid credentials")
	// ErrEmailTaken is returned by a provider when the project's user table
	// already holds the normalized email for this tenant.
	ErrEmailTaken = errors.New("authapi: email already registered")
	// ErrInvalidRole is returned when a provider does not accept the requested
	// role. The platform also validates its own role vocabulary first.
	ErrInvalidRole = errors.New("authapi: invalid role")
)

// Provider is the project-owned half of staff auth. The platform owns token
// generation, password hashing, and the session directory; the project owns
// its user table and its profile/registration rules.
//
// Implementations must treat tenantID as the only tenant boundary. Platform
// tests use a fake provider, so a provider must not assume it is Centrum.
type Provider interface {
	// Authenticate looks up a user by tenant and normalized email. It returns
	// ErrInvalidCredentials when no row exists. An inactive row is returned
	// with Active=false so the platform still runs the password comparison and
	// answers with the same uniform error.
	Authenticate(ctx context.Context, tenantID, email string) (Credentials, error)

	// Profile loads the safe public projection for a live user. It returns
	// ErrInvalidCredentials when the user is gone or inactive.
	Profile(ctx context.Context, tenantID, userID string) (User, error)

	// Register creates a user in the project's own table. The platform has
	// already checked the caller's owner session and hashed the password.
	Register(ctx context.Context, tenantID string, in RegisterInput) (User, error)
}

// SchemaAwareProvider is an optional Provider extension. The session
// directory already resolves and validates the tenant's db_schema, so a
// provider that implements this can read its user row without a second
// clients lookup and without re-deriving the tenant. Providers that do not
// implement it keep the old Profile path unchanged.
type SchemaAwareProvider interface {
	ProfileInSchema(ctx context.Context, tenantID, userID, schema string) (User, error)
}

// ProviderFactory builds a provider bound to the shared runtime pool.
type ProviderFactory func(*pgxpool.Pool) Provider

var (
	factoriesMu sync.RWMutex
	factories   = map[string]ProviderFactory{}
)

// RegisterProvider registers a project's provider factory. It is called from
// a project package's init, which the generated project imports make part of
// every server and test binary. A duplicate tenant is a boot error: two
// projects must never answer the same login.
func RegisterProvider(tenantID string, f ProviderFactory) {
	tenantID = strings.ToLower(strings.TrimSpace(tenantID))
	if tenantID == "" {
		panic("authapi.RegisterProvider: empty tenant id")
	}
	if f == nil {
		panic("authapi.RegisterProvider: nil factory for " + tenantID)
	}
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	if _, exists := factories[tenantID]; exists {
		panic("authapi.RegisterProvider: duplicate provider for " + tenantID)
	}
	factories[tenantID] = f
}

func newProviders(pool *pgxpool.Pool) map[string]Provider {
	factoriesMu.RLock()
	defer factoriesMu.RUnlock()
	out := make(map[string]Provider, len(factories))
	for tenant, f := range factories {
		out[tenant] = f(pool)
	}
	return out
}

// ProviderTenants returns the registered provider tenant ids, sorted. It is
// used by readiness checks and tests; the login path uses the map directly.
func ProviderTenants() []string {
	factoriesMu.RLock()
	defer factoriesMu.RUnlock()
	out := make([]string, 0, len(factories))
	for tenant := range factories {
		out = append(out, tenant)
	}
	sort.Strings(out)
	return out
}
