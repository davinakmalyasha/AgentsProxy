package records

import (
	"net/http"
	"strings"
	"testing"
)

// isPublicRead decides who may read a collection with no token. It was a
// substring match on the path, which made it an authorisation decision
// resting on `strings.Contains` -- the wrong tool. These pin the
// segment-boundary behaviour and, more importantly, the collection set.
func TestIsPublicReadOnlyMatchesWholeSegments(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		// The three intended collections, as the middleware actually sees
		// them: the /v1/{vertical} prefix is still attached, because the
		// middleware runs on the vertical router rather than inside it.
		{http.MethodGet, "/v1/sports/facilities/", true},
		{http.MethodGet, "/v1/sports/facilities/abc", true},
		{http.MethodGet, "/v1/sports/pricing/", true},
		{http.MethodGet, "/v1/sports/pricing/abc", true},
		{http.MethodGet, "/v1/sports/availability/", true},
		// Any vertical, since the rule is about the collection not the
		// tenant of business.
		{http.MethodGet, "/v1/other/facilities/", true},

		// The bug: a raw substring anywhere in the path used to be enough,
		// so a path merely CONTAINING one of these words leaked. Each of
		// these contains "/pricing" or "/facilities" as text.
		{http.MethodGet, "/v1/sports/x-facilities", false},
		{http.MethodGet, "/v1/sports/facilities-internal", false},
		{http.MethodGet, "/v1/sports/reports/pricing-export", false},
		{http.MethodGet, "/v1/sports/facilitiesx", false},
		{http.MethodGet, "/v1/sports/sub-facilities/abc", false},
		// Same word, different depth: a different resource that happens to
		// share a name.
		{http.MethodGet, "/v1/sports/admin/facilities", false},
		{http.MethodGet, "/v1/sports/admin/pricing", false},
		// Missing the vertical prefix entirely.
		{http.MethodGet, "/facilities", false},
		{http.MethodGet, "/pricing", false},

		// Everything else stays private.
		{http.MethodGet, "/bookings", false},
		{http.MethodGet, "/v1/sports/bookings/", false},
		{http.MethodGet, "/payments", false},
		{http.MethodGet, "/customers", false},
		{http.MethodGet, "/expenses", false},
		{http.MethodGet, "/kantin/sales", false},
		{http.MethodGet, "/users", false},
		{http.MethodGet, "/", false},

		// Writes are never public, on a public collection or otherwise.
		{http.MethodPost, "/facilities", false},
		{http.MethodPost, "/v1/sports/facilities/", false},
		{http.MethodPut, "/facilities", false},
		{http.MethodPatch, "/pricing", false},
		{http.MethodDelete, "/pricing/abc", false},
		{http.MethodPost, "/availability", false},
	}
	for _, c := range cases {
		r, err := http.NewRequest(c.method, "http://x"+c.path, nil)
		if err != nil {
			t.Fatalf("bad test request %s %s: %v", c.method, c.path, err)
		}
		if got := isPublicRead(r); got != c.want {
			t.Errorf("isPublicRead(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// The public collection set is a security boundary, so it is pinned
// exactly: adding a collection to it must be a deliberate edit here.
func TestPublicReadSetIsExactlyThreeCollections(t *testing.T) {
	want := []string{"availability", "facilities", "pricing"}
	if len(publicReadSegments) != len(want) {
		t.Fatalf("publicReadSegments has %d entries, want %d: %v",
			len(publicReadSegments), len(want), publicReadSegments)
	}
	for i, seg := range publicReadSegments {
		if len(seg) != 1 || seg[0] != want[i] {
			t.Errorf("entry %d is %v, want [%q]", i, seg, want[i])
		}
	}
}

// tiny recorder, so the tests do not need a full ResponseWriter.
type tinyRecorder struct {
	code int
	body string
}

func newRecorder() *tinyRecorder { return &tinyRecorder{code: 200} }

func (r *tinyRecorder) Header() http.Header         { return http.Header{} }
func (r *tinyRecorder) Write(b []byte) (int, error) { r.body += string(b); return len(b), nil }
func (r *tinyRecorder) WriteHeader(code int)        { r.code = code }

// A public read must name its tenant. It used to default to "centrum",
// which made the endpoint look single-tenant while ?tenant_id= above it
// accepted any registered tenant -- so omitting the parameter handed a
// caller one specific venue's courts and prices with no intent expressed.
func TestTenantFromQueryRequiresAnExplicitTenant(t *testing.T) {
	// Note: tenantFromQuery takes a *http.Request, so build one per case.
	mk := func(q string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, "http://x/facilities"+q, nil)
		if err != nil {
			t.Fatalf("bad test request %q: %v", q, err)
		}
		return r
	}

	// Missing -> refused.
	rec := newRecorder()
	if _, ok := tenantFromQuery(rec, mk("")); ok {
		t.Error("a public read with no tenant_id should be refused")
	}
	if rec.code != http.StatusBadRequest {
		t.Errorf("missing tenant_id: status %d, want 400", rec.code)
	}

	// Present and well-shaped -> accepted.
	rec = newRecorder()
	tenant, ok := tenantFromQuery(rec, mk("?tenant_id=centrum"))
	if !ok || tenant != "centrum" {
		t.Errorf("tenant_id=centrum -> (%q, %v), want (\"centrum\", true)", tenant, ok)
	}

	// Values that are not a tenant slug -> refused before any query.
	for _, bad := range []string{
		"?tenant_id=centrum%27%20OR%201=1",
		"?tenant_id=CENTRUM",
		"?tenant_id=has%20space",
		"?tenant_id=-leading-dash",
		"?tenant_id=" + strings.Repeat("a", 80),
	} {
		rec = newRecorder()
		if _, ok := tenantFromQuery(rec, mk(bad)); ok {
			t.Errorf("tenantFromQuery(%s) should be refused", bad)
		}
	}
}
