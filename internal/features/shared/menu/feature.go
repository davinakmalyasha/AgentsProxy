// Package menu implements the shared "menu" feature. It writes rows
// into tenant_records with kind = "menu_item". Validation is a
// payload contract: name non-empty, price number >= 0.
//
// DONE: PRD 6.5 (menu feature) / STANDARDS A, D, F
package menu

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/davinakmalyasha/agents-proxy/internal/features"
)

const featureName = "menu"

func init() { Register() }

// Register registers the menu feature in the global registry. Safe
// to call from tests after a features.ResetForTest().
func Register() {
	features.Register(features.Feature{
		Name:      featureName,
		Kind:      "menu_item",
		DefaultOn: true,
		Routes:    nil, // adapter handles routes
		ValidatePayload: func(raw json.RawMessage) error {
			var p struct {
				Name  string  `json:"name"`
				Price float64 `json:"price"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return fmt.Errorf("invalid menu payload: %w", err)
			}
			if strings.TrimSpace(p.Name) == "" {
				return fmt.Errorf("name required")
			}
			if p.Price < 0 {
				return fmt.Errorf("price must be >= 0")
			}
			return nil
		},
	})
}
