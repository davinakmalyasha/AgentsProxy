// DONE: P4 (admin UUID 400s) / STANDARDS D
package records

import (
	"fmt"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// TenantSlugRe bounds a tenant id before it reaches a query, and keeps it
// to the lowercase-slug shape tenants actually use. Single source: identity
// (query param) and admin (provisioning) validate against this.
var TenantSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidateTenantSlug rejects empty, over-long, or non-slug tenant ids.
func ValidateTenantSlug(t string) error {
	if !TenantSlugRe.MatchString(t) {
		return fmt.Errorf("invalid tenant_id")
	}
	return nil
}

// maxQuotaTokens caps monthly_quota_tokens: above this the SUM never trips,
// which is unlimited billing by typo.
const maxQuotaTokens = 100_000_000

// ValidateQuota rejects negative quotas and absurd ones. Zero means
// "unset" at creation (default applies); on PATCH zero is rejected by the
// caller passing requireNonZero.
func ValidateQuota(q int) error {
	if q < 0 || q > maxQuotaTokens {
		return fmt.Errorf("invalid monthly_quota_tokens")
	}
	return nil
}

// WithValidID decorates an endpoint whose route contains {id}: a
// non-UUID id is rejected with 400 before the handler runs (otherwise
// it reaches a $1::uuid cast and comes back as 500 "internal").
//
// NOTE: this must wrap the ENDPOINT (runs post-match, URLParams
// populated). A top-level middleware can NOT do this — chi matches
// nested routes after outer middlewares run, so chi.URLParam is empty
// there (verified by test, don't "simplify" this back).
func WithValidID(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id := chi.URLParam(r, "id"); id != "" {
			if _, err := uuid.Parse(id); err != nil {
				WriteJSONError(w, http.StatusBadRequest, "invalid id")
				return
			}
		}
		next(w, r)
	}
}
