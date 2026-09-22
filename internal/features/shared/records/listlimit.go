package records

import (
	"net/http"
	"strconv"
	"strings"
)

// ListLimitBounds is the default/max row cap for list endpoints that
// return unbounded per-tenant tables. Custom sports handlers should
// apply ParseLimit + a SQL LIMIT so one tenant's history can't be
// materialized in a single response.
const (
	DefaultListLimit = 200
	MaxListLimit     = 2000
)

// ParseLimit reads ?limit and clamps it to [1, MaxListLimit].
// Returns (limit, error) where a non-empty, non-numeric, or <1 value
// is a 400 (like the payments/kantin handlers already do).
func ParseLimit(r *http.Request) (int, error) {
	s := strings.TrimSpace(r.URL.Query().Get("limit"))
	if s == "" {
		return DefaultListLimit, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, err
	}
	if n > MaxListLimit {
		n = MaxListLimit
	}
	return n, nil
}
