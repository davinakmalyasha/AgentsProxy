// Package telegram_feature registers the "telegram" feature for the
// sports vertical: the multi-bot registry API (owner-managed bots).
// The Telegram bot manager (a background consumer) serves every active
// bot row; clients without the "telegram" feature flag get no bots.
//
// DONE: Phase 5 (telegram agentic) / T5 (bot admin) / STANDARDS A, F
package telegram_feature

import (
	"github.com/go-chi/chi/v5"

	"github.com/davinakmalyasha/agents-proxy/internal/features"
)

const Name = "telegram"

func init() { Register() }

// Register registers the telegram feature. Safe to call from tests
// after features.ResetForTest().
func Register() {
	features.Register(features.Feature{
		Name:          Name,
		Kind:          "", // custom table (telegram_bots)
		DefaultOn:     false,
		VerticalSlugs: []string{"sports"},
		Routes:        routes,
	})
}

func routes(r chi.Router, deps features.BuilderDeps) {
	botRoutes(r, deps.Pool)
}
