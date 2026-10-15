package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"
)

// errBadSuffix rejects suffix rules that could over-match.
var errBadSuffix = errors.New("suffix must look like .example.com (leading dot, no scheme/slashes)")

// errBadOrigin rejects registry values that could over-match or
// misroute: wildcards, non-https (except loopback for dev), paths.
var errBadOrigin = errors.New("origin must be https://host[:port] (http only for localhost), no *, no path")

// ValidateOrigin normalizes and validates a registry origin for write.
// Returns the canonical form to store. Rejects: empty, anything with
// `*`, non-https schemes (http allowed only for loopback, so dev keeps
// working without weakening production), and any path/query/fragment
// (origins are scheme+host, never pages).
func ValidateOrigin(raw string) (string, error) {
	n := NormalizeOrigin(raw)
	if n == "" || strings.Contains(n, "*") {
		return "", errBadOrigin
	}
	u, err := url.Parse(n)
	if err != nil || u.Host == "" {
		return "", errBadOrigin
	}
	if u.Scheme == "https" {
		// ok
	} else if u.Scheme == "http" && isLoopbackHost(u.Hostname()) {
		// ok: local dev only
	} else {
		return "", errBadOrigin
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", errBadOrigin
	}
	return u.Scheme + "://" + u.Host, nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// Origin resolution for N frontends sharing one backend (migration 058).
//
// The ALLOW_ORIGINS env list does not scale past a handful of origins
// (one edit + redeploy per onboarding). Origins are tenant data instead:
// frontend_origins (exact) + origin_suffix_rules (dot-boundary suffix),
// loaded at boot and refreshed on an interval. Enforcement stays
// fail-closed: boot without a readable registry exits (boot already
// exits without a database), and a failed refresh keeps serving the
// last-known set rather than closing every browser flow on a DB wobble.

// OriginSet is an immutable allow snapshot: exact origins plus suffix
// rules. Swapped wholesale under the resolver lock, never mutated.
type OriginSet struct {
	exact    map[string]struct{}
	suffixes []string
}

// NormalizeOrigin canonicalizes for comparison: trim spaces, strip one
// trailing slash (browsers never send it; dashboard pastes include it),
// lowercase (scheme and host are case-insensitive).
func NormalizeOrigin(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "/")
	return strings.ToLower(s)
}

// ParseSuffix validates a suffix rule: leading dot, something real
// after it, no scheme/slashes/spaces. The leading dot IS the
// evil-subdomain defense: "https://evil-example.com" does not end
// with ".example.com", while "https://a.example.com" does.
func ParseSuffix(s string) (string, error) {
	n := NormalizeOrigin(s)
	if !strings.HasPrefix(n, ".") || len(n) < 3 || strings.ContainsAny(n, "/: ") {
		return "", errBadSuffix
	}
	return n, nil
}

// Allowed reports membership: exact hit, else any dot-boundary suffix.
func (s *OriginSet) Allowed(origin string) bool {
	if s == nil {
		return false
	}
	n := NormalizeOrigin(origin)
	if n == "" || n == "*" {
		return false
	}
	if _, ok := s.exact[n]; ok {
		return true
	}
	for _, suf := range s.suffixes {
		if strings.HasSuffix(n, suf) {
			return true
		}
	}
	return false
}

// Counts for the boot log (never secrets — origins are public URLs).
func (s *OriginSet) Counts() (exact, suffix int) {
	if s == nil {
		return 0, 0
	}
	return len(s.exact), len(s.suffixes)
}

// originsQuery reads the whole registry in one round trip.
const originsQuery = `SELECT origin, 'exact' AS kind FROM public.frontend_origins
UNION ALL
SELECT suffix, 'suffix' FROM public.origin_suffix_rules`

// LoadOrigins reads and validates the registry. Any transport error
// fails closed (boot exits); invalid ROWS are skipped with a warning
// (a bad row must not brick boot — writes are validated at the admin
// endpoint, this is the belt).
func LoadOrigins(ctx context.Context, pool *pgxpool.Pool) (*OriginSet, error) {
	rows, err := pool.Query(ctx, originsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := &OriginSet{exact: map[string]struct{}{}}
	for rows.Next() {
		var raw, kind string
		if err := rows.Scan(&raw, &kind); err != nil {
			rows.Close()
			return nil, err
		}
		if kind == "suffix" {
			suf, err := ParseSuffix(raw)
			if err != nil {
				slog.Warn("origins skipping invalid suffix row", "row", raw)
				continue
			}
			set.suffixes = append(set.suffixes, suf)
			continue
		}
		n, err := ValidateOrigin(raw)
		if err != nil {
			slog.Warn("origins skipping invalid exact row", "row", raw)
			continue
		}
		set.exact[n] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return set, nil
}

// DBResolver is the cached registry: boot-loaded, refreshed on an
// interval, stale-on-error. Zero DB hits on the request path.
type DBResolver struct {
	mu   sync.RWMutex
	set  *OriginSet
	pool *pgxpool.Pool
	sf   singleflight.Group
}

// NewDBResolver boot-loads the registry. Error = fail closed; the
// caller (main.go) exits, same as a dead database.
func NewDBResolver(ctx context.Context, pool *pgxpool.Pool) (*DBResolver, error) {
	set, err := LoadOrigins(ctx, pool)
	if err != nil {
		return nil, err
	}
	return &DBResolver{set: set, pool: pool}, nil
}

// Refresh reloads the set (singleflight: concurrent ticks and request
// paths share one query). On failure the old set keeps serving and the
// failure is logged — a DB wobble must not close every browser flow.
func (r *DBResolver) Refresh(ctx context.Context) error {
	_, err, _ := r.sf.Do("origins", func() (any, error) {
		set, err := LoadOrigins(ctx, r.pool)
		if err != nil {
			slog.Warn("origins refresh failed, keeping last-known set", "err", err)
			return nil, err
		}
		r.mu.Lock()
		r.set = set
		r.mu.Unlock()
		return nil, nil
	})
	return err
}

// Start refreshes on an interval until ctx ends. Run gated like every
// other background loop (BackgroundTickers): on serverless the process
// dies between requests, so boot-load is the refresh there.
func (r *DBResolver) Start(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = r.Refresh(ctx)
			}
		}
	}()
}

// Allowed is the request-path check: lock-guarded map/suffix lookup,
// no I/O.
func (r *DBResolver) Allowed(origin string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.set.Allowed(origin)
}

// Counts reports the live set sizes for the boot log.
func (r *DBResolver) Counts() (exact, suffix int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.set.Counts()
}
