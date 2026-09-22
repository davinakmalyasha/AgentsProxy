// DONE: P4 (admin UUID 400s) / STANDARDS D
package records

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

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
