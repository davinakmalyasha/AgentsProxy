// Package _template is the starter for a per-tenant custom
// feature. Copy this folder to tenants/<slug>/backend/<name>/
// and edit the three places marked TODO.
//
// DONE: PRD 6.5 (custom feature template) / STANDARDS A, F, H
package _template

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/davinakmalyasha/agents-proxy/internal/features"
)

const (
	// TODO: the feature's stable name. Lowercase, short, unique
	// across the registry. Same name appears in the client.features
	// array and in the URL: /v1/{vertical}/{name}/...
	Name = "my_custom"

	// TODO: which vertical this feature is allowed for. Empty
	// slice = all 12 verticals.
	VerticalSlugs = []string{} // e.g. {"fnb"}

	// TODO: the tenant(s) this feature is built for. Used only
	// in the README; no code references this.
	OwnerTenant = "TODO-tenant-slug"
)

func init() {
	features.Register(features.Feature{
		Name:          Name,
		Kind:          "TODO_kind_value", // e.g. "loyalty_point"
		DefaultOn:     false,             // opt-in only
		VerticalSlugs: VerticalSlugs,
		Routes:        nil, // defaults to records-based adapter routes
		ValidatePayload: func(raw json.RawMessage) error {
			// TODO: enforce the contract. Examples:
			//   - "name" must be non-empty
			//   - "points" must be a non-negative integer
			//   - "expires_at" must be a future ISO8601 date
			var p struct {
				TODOField string `json:"todo_field"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return fmt.Errorf("invalid %s payload: %w", Name, err)
			}
			if strings.TrimSpace(p.TODOField) == "" {
				return fmt.Errorf("todo_field required")
			}
			return nil
		},
	})
}
