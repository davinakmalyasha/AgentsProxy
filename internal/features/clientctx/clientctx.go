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
)

// Identity is the resolved per-request principal.
type Identity struct {
	Client *cache.ClientModel // set when authed via X-Client-Token
	UserID string             // set when authed via X-Session-Token
	Email  string
	Role   string // owner | pegawai | client
	Tenant string
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
	return With(ctx, &Identity{Client: m, Role: RoleClient})
}

// ClientFromContext returns the *cache.ClientModel for legacy callers
// (records feature's clientFromCtx).
func ClientFromContext(ctx context.Context) *cache.ClientModel {
	if id := From(ctx); id != nil {
		return id.Client
	}
	return nil
}
