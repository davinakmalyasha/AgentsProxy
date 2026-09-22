// Package booking implements the shared "booking" feature. Writes
// rows to tenant_records with kind = "booking". Requires date and
// name in the payload.
//
// DONE: PRD 6.5 (booking feature) / STANDARDS A, D, F
package booking

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/davinakmalyasha/agents-proxy/internal/features"
)

const featureName = "booking"

func init() { Register() }

// Register registers the booking feature. Safe to call from tests
// after features.ResetForTest().
func Register() {
	features.Register(features.Feature{
		Name:      featureName,
		Kind:      "booking",
		DefaultOn: true,
		ValidatePayload: func(raw json.RawMessage) error {
			var p struct {
				Date     string `json:"date"`
				Datetime string `json:"datetime"`
				Name     string `json:"name"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return fmt.Errorf("invalid booking payload: %w", err)
			}
			if strings.TrimSpace(p.Date) == "" && strings.TrimSpace(p.Datetime) == "" {
				return fmt.Errorf("date or datetime required")
			}
			if strings.TrimSpace(p.Name) == "" {
				return fmt.Errorf("name required")
			}
			return nil
		},
	})
}
