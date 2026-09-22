// Package review implements the shared "review" feature.
// DONE: PRD 6.5 (review feature) / STANDARDS A, D, F
package review

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/davinakmalyasha/agents-proxy/internal/features"
)

const featureName = "review"

func init() { Register() }

// Register registers the review feature. Safe to call from tests
// after features.ResetForTest().
func Register() {
	features.Register(features.Feature{
		Name:      featureName,
		Kind:      "review",
		DefaultOn: true,
		ValidatePayload: func(raw json.RawMessage) error {
			var p struct {
				Rating int    `json:"rating"`
				Author string `json:"author"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return fmt.Errorf("invalid review payload: %w", err)
			}
			if p.Rating < 1 || p.Rating > 5 {
				return fmt.Errorf("rating must be between 1 and 5")
			}
			if strings.TrimSpace(p.Author) == "" {
				return fmt.Errorf("author required")
			}
			return nil
		},
	})
}
