package records

import (
	"context"

	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/features/clientctx"
)

// clientFromCtx is a thin alias for ClientFromContext so handlers
// don't have to import clientctx directly. Works with both
// WithClientAuth (client identity) and WithIdentity (staff identity
// falls back to nil Client — the tenant comes from Identity).
func clientFromCtx(ctx context.Context) *cache.ClientModel {
	return ClientFromContext(ctx)
}

// ClientFromContext re-exported for downstream code that wants the
// per-request client model (nil when the identity is a staff user).
func ClientFromContext(ctx context.Context) *cache.ClientModel {
	return clientctx.ClientFromContext(ctx)
}
