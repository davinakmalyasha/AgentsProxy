// Package article implements the shared "article" feature.
// DONE: PRD 6.5 (article feature) / STANDARDS A, D, F
package article

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/davinakmalyasha/agents-proxy/internal/features"
)

const featureName = "article"

func init() { Register() }

// Register registers the article feature. Safe to call from tests
// after features.ResetForTest().
func Register() {
	features.Register(features.Feature{
		Name:      featureName,
		Kind:      "article",
		DefaultOn: true,
		ValidatePayload: func(raw json.RawMessage) error {
			var p struct {
				Title string `json:"title"`
				Body  string `json:"body"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return fmt.Errorf("invalid article payload: %w", err)
			}
			if strings.TrimSpace(p.Title) == "" {
				return fmt.Errorf("title required")
			}
			if strings.TrimSpace(p.Body) == "" {
				return fmt.Errorf("body required")
			}
			return nil
		},
	})
}
