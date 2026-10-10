package records

import (
	"context"

	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/features/clientctx"
)

// clientFromCtx reads the per-request client model (nil when the
// identity is a staff user) without forcing handlers to import
// clientctx directly. Works with both WithClientAuth (client identity)
// and WithIdentity (staff identity falls back to nil Client).
func clientFromCtx(ctx context.Context) *cache.ClientModel {
	return clientctx.ClientFromContext(ctx)
}
