package features

// DONE: PRD 6.5 (default feature set) / STANDARDS F

// defaultFeatures is the feature set every new client gets unless
// the admin explicitly overrides. Same for all 12 verticals in this
// phase; the map exists so we can specialize per-vertical later
// without touching the registration call sites.
var defaultByVertical = map[string][]string{
	"fnb":      {"records", "menu", "booking", "review", "article", "promo"},
	"salon":    {"records", "booking", "review", "article", "promo"},
	"laundry":  {"records", "booking", "review", "promo"},
	"klinik":   {"records", "booking", "review", "article", "promo"},
	"gym":      {"records", "booking", "review", "article", "promo"},
	"bimbel":   {"records", "booking", "review", "article"},
	"bengkel":  {"records", "booking", "review", "promo"},
	"beauty":   {"records", "menu", "booking", "review", "article", "promo"},
	"travel":   {"records", "booking", "review", "article", "promo"},
	"property": {"records", "booking", "review", "article", "promo"},
	"katering": {"records", "menu", "booking", "review", "promo"},
	"photo":    {"records", "booking", "review", "article", "promo"},
	"sports":   {"records", "booking", "review", "promo"},
}

// DefaultSetFor returns the default feature names for the given
// vertical, falling back to the conservative "records only" set
// for unknown verticals.
func DefaultSetFor(vertical string) []string {
	if v, ok := defaultByVertical[vertical]; ok {
		out := make([]string, len(v))
		copy(out, v)
		return out
	}
	return []string{"records"}
}

// IsKnownFeature returns true when name is a registered feature.
// It also returns true for the implicit "records" feature, which is
// always available even if not registered.
func IsKnownFeature(name string) bool {
	if name == "records" {
		return true
	}
	return HasFeature(name)
}
