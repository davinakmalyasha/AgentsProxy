// Package clientctx owns the per-request identity: either a client
// (via X-Client-Token, the API/agent identity) or a staff user (via
// X-Session-Token, the owner/pegawai identity), plus the resolved
// tenant and role. Split into its own package to avoid import cycles
// between internal/features and internal/features/shared/records.
//
// DONE: PRD 6.5 / Phase 7 (unified identity) / STANDARDS A, F
package clientctx

import (
	"context"

	"github.com/davinakmalyasha/agents-proxy/internal/cache"
)

// Role constants.
const (
	RoleOwner  = "owner"
	RoleStaff  = "pegawai"
	RoleClient = "client" // full access via client token (API/agent)
	RolePublic = "public" // unauthenticated landing read
)

// Identity is the resolved per-request principal.
type Identity struct {
	Client *cache.ClientModel // set when authed via X-Client-Token
	// ClientID is the tenant's clients.id, for usage attribution. It is
	// set for both client-token and staff-session identities. Client is
	// the full model only when the caller presented a client token; a
	// staff session must not be turned into one just to reach a handler.
	ClientID string
	UserID   string // set when authed via X-Session-Token
	Email    string
	Role     string // owner | pegawai | client | public
	Tenant   string
	// Vertical and Features are the tenant entitlement at identity
	// resolution. They exist on the identity so every principal (client
	// token, staff session, public landing read) passes the same feature
	// gate without a second DB lookup.
	Vertical string
	Features []string
	// DBSchema is the tenant's business schema at the time the identity was
	// resolved: "public" for a platform tenant, "project_<slug>" for a
	// project tenant. The platform and project route gates compare it with
	// the surface being mounted; an identity that does not carry a schema
	// must not pass either gate.
	DBSchema string
}

// TenantID returns the tenant for the identity.
func (id *Identity) TenantID() string {
	if id == nil {
		return ""
	}
	if id.Tenant != "" {
		return id.Tenant
	}
	if id.Client != nil {
		return id.Client.TenantID
	}
	return ""
}

// IsOwner reports whether the identity is an owner or a full-access
// client token (client tokens keep owner-level DATA access for
// API/agent integrations).
//
// This is the business-data tier. It is NOT sufficient for anything
// that changes who has access - see IsStaffOwner.
func (id *Identity) IsOwner() bool {
	if id == nil {
		return false
	}
	return id.Role == RoleOwner || id.Role == RoleClient
}

// IsStaffOwner reports whether the identity is a real owner SESSION.
//
// Narrower than IsOwner on purpose: a client API token is a single
// static secret (usually embedded in a site or integration), and it
// must not be able to escalate to persistent human-level access by
// editing staff roles. Data writes stay on
// IsOwner so the API-integration model keeps working; identity and
// access changes come here.
func (id *Identity) IsStaffOwner() bool {
	if id == nil {
		return false
	}
	return id.Role == RoleOwner
}

// IsPublic reports whether this is the unauthenticated landing identity.
// Only public GET responses are safe to mark as CDN-cacheable.
func (id *Identity) IsPublic() bool {
	if id == nil {
		return false
	}
	return id.Role == RolePublic
}

type ctxKey struct{}

// With returns a new context with the identity stashed.
func With(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// From returns the *Identity previously stored by With, or nil if the
// auth middleware has not run yet.
func From(ctx context.Context) *Identity {
	v, _ := ctx.Value(ctxKey{}).(*Identity)
	return v
}

// WithClient keeps the legacy behavior: wraps a *cache.ClientModel as
// a client-role Identity.
func WithClient(ctx context.Context, m *cache.ClientModel) context.Context {
	return With(ctx, &Identity{
		Client:   m,
		ClientID: m.ID,
		Role:     RoleClient,
		Tenant:   m.TenantID,
		Vertical: m.Vertical,
		Features: m.Features,
		DBSchema: m.DBSchema,
	})
}
