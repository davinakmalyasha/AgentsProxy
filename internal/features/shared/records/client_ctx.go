package records

import (
	"context"

	"github.com/davinakmalyasha/agents-proxy/internal/features/clientctx"
)

// caller is the records surface's view of the resolved principal.
//
// Handlers need two things: the tenant whose rows they may touch and the
// clients.id used to attribute business-token usage. Both a client token
// and a staff session provide them. A staff session is intentionally not
// represented as a *cache.ClientModel; only the entitlement and tenant
// identity are shared, and the full client model stays exclusive to a real
// client token.
type caller struct {
	TenantID string
	ClientID string
	Public   bool
}

// callerFromCtx returns nil when no identity has been resolved. A public
// landing read is a real caller for GET/HEAD (the feature metadata decides
// whether that collection is public), but it is read-only. The identity
// middleware only creates a public identity for the landing allowlist;
// requireActor re-checks the method so a future route cannot turn a public
// read into an accidental write.
func callerFromCtx(ctx context.Context) *caller {
	id := clientctx.From(ctx)
	if id == nil {
		return nil
	}
	tenantID := id.TenantID()
	if tenantID == "" {
		return nil
	}
	clientID := id.ClientID
	if clientID == "" && id.Client != nil {
		clientID = id.Client.ID
	}
	return &caller{
		TenantID: tenantID,
		ClientID: clientID,
		Public:   id.Role == clientctx.RolePublic,
	}
}
