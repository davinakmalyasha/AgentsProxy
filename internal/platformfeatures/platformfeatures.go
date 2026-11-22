// Package platformfeatures blank-imports every shared (cross-vertical)
// feature so a command that validates clients.features sees the same global
// registry as the server.
package platformfeatures

import (
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/article"
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/booking"
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/menu"
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/promo"
	_ "github.com/davinakmalyasha/agents-proxy/internal/features/shared/review"
)
