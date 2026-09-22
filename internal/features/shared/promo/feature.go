// Package promo implements the shared "promo" feature.
// DONE: PRD 6.5 (promo feature) / STANDARDS A, D, F
package promo

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/davinakmalyasha/agents-proxy/internal/features"
)

const featureName = "promo"

func init() { Register() }

// Register registers the promo feature. Safe to call from tests
// after features.ResetForTest().
func Register() {
	features.Register(features.Feature{
		Name:      featureName,
		Kind:      "promo",
		DefaultOn: true,
		ValidatePayload: func(raw json.RawMessage) error {
			var p struct {
				Title    string  `json:"title"`
				Discount float64 `json:"discount"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return fmt.Errorf("invalid promo payload: %w", err)
			}
			if strings.TrimSpace(p.Title) == "" {
				return fmt.Errorf("title required")
			}
			if p.Discount < 0 {
				return fmt.Errorf("discount must be >= 0")
			}
			return nil
		},
	})
}
