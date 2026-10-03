package records

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// WithValidID must wrap the ENDPOINT: chi only populates URLParams after
// the route matches, so a top-level middleware sees an empty {id}.
// (A middleware version was tried and proven blind by this same test.)
func TestWithValidIDAtEndpoint(t *testing.T) {
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Get("/bookings/{id}", WithValidID(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	})
	bad := httptest.NewRequest("GET", "/v1/bookings/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, bad)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage id: got %d, want 400", rec.Code)
	}
	ok := httptest.NewRequest("GET", "/v1/bookings/3c5a1d53-ce9d-4139-beec-89181f328a16", nil)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, ok)
	if rec2.Code != http.StatusOK {
		t.Fatalf("valid uuid: got %d, want 200", rec2.Code)
	}
}
