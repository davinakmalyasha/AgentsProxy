package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testSet() *OriginSet {
	return &OriginSet{
		exact:    map[string]struct{}{"https://a.example.com": {}},
		suffixes: []string{".example.com", "-my-team.vercel.app"},
	}
}

func TestNormalizeOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://A.Example.com/":     "https://a.example.com",
		"  https://a.example.com  ":  "https://a.example.com",
		"https://a.example.com:8443": "https://a.example.com:8443",
		"":                           "",
	} {
		if got := NormalizeOrigin(in); got != want {
			t.Errorf("NormalizeOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOriginSet_AllowedMatrix(t *testing.T) {
	s := testSet()
	allow := []string{
		"https://a.example.com",      // exact
		"https://b.example.com",      // suffix
		"https://deep.b.example.com", // nested suffix
		"https://B.EXAMPLE.COM/",     // normalization
		// Hyphen-boundary suffix: provider-managed preview hosts.
		"https://centrum-badminton-9i427fi8w-my-team.vercel.app",
	}
	for _, o := range allow {
		if !s.Allowed(o) {
			t.Errorf("Allowed(%q) = false, want true", o)
		}
	}
	deny := []string{
		"",                             // empty
		"*",                            // wildcard never matches
		"https://evil-example.com",     // no dot boundary before example.com
		"https://example.com.evil.com", // suffix in the middle
		"https://example.com",          // apex is NOT covered by .example.com
		"https://other.org",
		"https://my-team.vercel.app",           // hyphen apex NOT covered
		"https://my-team.vercel.app.evil.com",  // suffix in the middle
		"https://x-my-team.vercel.app.evil.co", // trailing junk
	}
	for _, o := range deny {
		if s.Allowed(o) {
			t.Errorf("Allowed(%q) = true, want false", o)
		}
	}
	var nilSet *OriginSet
	if nilSet.Allowed("https://a.example.com") {
		t.Error("nil set allowed an origin, want closed")
	}
}

func TestParseSuffix(t *testing.T) {
	if got, err := ParseSuffix(".example.com"); err != nil || got != ".example.com" {
		t.Errorf("ParseSuffix valid = %q, %v", got, err)
	}
	// Hyphen boundary: provider-team label scope (needs two dots).
	if got, err := ParseSuffix("-my-team.vercel.app"); err != nil || got != "-my-team.vercel.app" {
		t.Errorf("ParseSuffix hyphen valid = %q, %v", got, err)
	}
	for _, bad := range []string{
		"", "example.com", "*", "*.example.com", "https://x.com", ".", ".a b", ".a/b",
		"-", "-vercel.app", "-.com", // hyphen rules need >= 2 dots
		"-.a.b", // boundary only
	} {
		if got, err := ParseSuffix(bad); err == nil {
			t.Errorf("ParseSuffix(%q) = %q, want error", bad, got)
		}
	}
}

func TestValidateOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://app.example.id":      "https://app.example.id",
		"https://app.example.id/":     "https://app.example.id",
		"https://APP.EXAMPLE.ID":      "https://app.example.id",
		"https://app.example.id:8443": "https://app.example.id:8443",
		"http://localhost:3000":       "http://localhost:3000",
		"http://127.0.0.1:8080":       "http://127.0.0.1:8080",
	} {
		if got, err := ValidateOrigin(in); err != nil || got != want {
			t.Errorf("ValidateOrigin(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"", "*", "https://*", "http://example.com", "http://192.168.1.5:3000",
		"https://app.example.id/some/path", "https://app.example.id/?x=1",
		"not a url", "ftp://files.example.com", "https://",
	} {
		if got, err := ValidateOrigin(bad); err == nil {
			t.Errorf("ValidateOrigin(%q) = %q, want error", bad, got)
		}
	}
}

func TestCORSResolving_NilParity(t *testing.T) {
	// Nil resolver behaves exactly like CORS: static allow, else silent.
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	for _, mw := range []func(http.Handler) http.Handler{
		CORS([]string{"https://a.test"}),
		CORSResolving([]string{"https://a.test"}, nil),
	} {
		for origin, want := range map[string]string{
			"https://a.test": "https://a.test",
			"https://b.test": "",
		} {
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("Origin", origin)
			rec := httptest.NewRecorder()
			mw(next).ServeHTTP(rec, req)
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != want {
				t.Errorf("origin %q header = %q, want %q", origin, got, want)
			}
		}
	}
}

func TestCORSResolving_DBSuperset(t *testing.T) {
	res := &DBResolver{set: testSet()}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	mw := CORSResolving([]string{"https://static.test"}, res)
	for origin, want := range map[string]string{
		"https://static.test":   "https://static.test",   // static leg
		"https://b.example.com": "https://b.example.com", // DB leg
		"https://nope.test":     "",
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		mw(next).ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != want {
			t.Errorf("origin %q header = %q, want %q", origin, got, want)
		}
	}
}

func TestDBResolver_RefreshStaleOnError(t *testing.T) {
	// Unroutable pool: every query fails. Refresh must keep the old set.
	cfg, err := pgxpool.ParseConfig("postgres://u@127.0.0.1:9/db?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	res := &DBResolver{set: testSet(), pool: pool}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := res.Refresh(ctx); err == nil {
		t.Error("Refresh against dead pool succeeded, want error")
	}
	if !res.Allowed("https://b.example.com") {
		t.Error("stale set lost after failed refresh; old origins must keep serving")
	}
	if _, err := NewDBResolver(ctx, pool); err == nil {
		t.Error("NewDBResolver against dead pool succeeded, want fail-closed error")
	}
}
