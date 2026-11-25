package authapi

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/davinakmalyasha/agents-proxy/internal/auth"
)

// SessionTTL is how long a login lasts before re-auth.
const SessionTTL = 30 * 24 * time.Hour

// HashPassword bcrypt-hashes a password (cost 10 default).
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

// VerifyPassword checks a password against a bcrypt hash.
func VerifyPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// GenerateSessionToken returns (raw, sha256Hex(raw)) like client tokens.
// Both token kinds use the same platform primitive; only their storage and
// lifetime differ.
func GenerateSessionToken() (string, string, error) {
	return auth.GenerateToken()
}

// HashToken sha256-hashes a session token for lookup.
func HashToken(token string) string {
	return auth.HashToken(token)
}

// providerFor returns the project provider that serves this tenant. A tenant
// without a provider cannot log in; callers turn that into invalid
// credentials rather than revealing whether the tenant exists.
func (a *Auth) providerFor(tenantID string) Provider {
	if a == nil || len(a.Providers) == 0 {
		return nil
	}
	return a.Providers[strings.ToLower(strings.TrimSpace(tenantID))]
}

// RegisterProvider overrides the provider for one tenant. Production builds
// the map from project init registrations; tests use this to bind a fixture
// provider to a cloned tenant without inventing a project folder.
func (a *Auth) RegisterProvider(tenantID string, p Provider) {
	tenantID = strings.ToLower(strings.TrimSpace(tenantID))
	if tenantID == "" || p == nil {
		return
	}
	if a.Providers == nil {
		a.Providers = map[string]Provider{}
	}
	a.Providers[tenantID] = p
}

// RegisterUser creates a staff user for a tenant through its provider. It is
// the shared path for the owner-session /register endpoint and the admin
// bootstrap endpoint; the caller is responsible for authorization.
func (a *Auth) RegisterUser(ctx context.Context, tenantID string, in RegisterInput) (User, error) {
	p := a.providerFor(tenantID)
	if p == nil {
		return User{}, ErrInvalidCredentials
	}
	u, err := p.Register(ctx, tenantID, in)
	if err != nil {
		return User{}, err
	}
	u.TenantID = tenantID
	return u, nil
}

// UserByToken resolves a session token to a user, checking expiry and asking
// the tenant's project provider for the live profile. A missing user,
// inactive user, unknown tenant, paused tenant, or missing provider all become
// pgx.ErrNoRows so the caller's existing 401/public-read fallback stays
// intact.
//
// Migration 090 resolves the session, its clients row, and the once-a-minute
// last_seen_at touch in one statement. The project provider then reads only
// its own user table through the schema that statement just validated. Role
// and is_active are still live on every request: there is deliberately no
// cached profile here.
func (a *Auth) UserByToken(ctx context.Context, tok string) (*User, error) {
	hash := HashToken(tok)

	var userID, tenantID, clientID, schema, status, vertical string
	var features []string
	err := a.Pool.QueryRow(ctx,
		`SELECT user_id::text, tenant_id, client_id::text, db_schema, status, vertical, features
		   FROM private.staff_session_resolve_v2($1)`, hash).
		Scan(&userID, &tenantID, &clientID, &schema, &status, &vertical, &features)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, pgx.ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	if status != "active" {
		return nil, pgx.ErrNoRows
	}

	p := a.providerFor(tenantID)
	if p == nil {
		return nil, pgx.ErrNoRows
	}
	var u User
	if sap, ok := p.(SchemaAwareProvider); ok && schema != "" {
		u, err = sap.ProfileInSchema(ctx, tenantID, userID, schema)
	} else {
		u, err = p.Profile(ctx, tenantID, userID)
	}
	if errors.Is(err, ErrInvalidCredentials) {
		return nil, pgx.ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	u.TenantID = tenantID
	u.ClientID = clientID
	u.DBSchema = schema
	u.Vertical = vertical
	u.Features = features
	return &u, nil
}
