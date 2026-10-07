// Package tenanthealth is the per-tenant quarantine registry: one bad
// tenant fails closed alone instead of taking every tenant down with it.
//
// A tenant is quarantined when its configuration fails validation at boot
// (or when an operator fences it via admin). Quarantined tenants get an
// honest 503 on every surface -- agent, REST, workers -- never a degraded
// reply from a half-loaded prompt. Healthy tenants are unaffected.
//
// The registry is process-global (like the feature registry) so the agent
// loop, the REST builder, the workers, and the admin surface all read one
// source without dependency plumbing. Guarded by RWMutex for concurrent
// request paths.
package tenanthealth

import (
	"errors"
	"sync"
	"time"
)

// ErrQuarantined is returned (and mapped to 503) when a quarantined tenant
// receives traffic. Distinct error, distinct body: operators must be able
// to tell "tenant fenced" apart from "tenant lookup infra down" (which is
// also a 503 with a different message).
var ErrQuarantined = errors.New("tenant quarantined")

// ResponseBody is the 503 body for quarantined tenants. It must never
// reuse the "tenant lookup unavailable" string.
const ResponseBody = "tenant quarantined"

// Entry is one tenant's quarantine state.
type Entry struct {
	Quarantined bool
	Reason      string
	Since       time.Time
}

var (
	mu      sync.RWMutex
	entries = map[string]*Entry{}
)

// Quarantine fences a tenant with a reason. Idempotent: re-quarantining
// refreshes the reason and timestamp, never duplicates.
func Quarantine(slug, reason string) {
	mu.Lock()
	defer mu.Unlock()
	entries[slug] = &Entry{Quarantined: true, Reason: reason, Since: time.Now()}
}

// Release unfences a tenant. Returns false if it was not quarantined.
func Release(slug string) bool {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := entries[slug]; !ok {
		return false
	}
	delete(entries, slug)
	return true
}

// Check reports whether a tenant is quarantined. Unknown slugs (never
// quarantined) report false: absence of config is not quarantine, and an
// unconfigured tenant still gets the base prompt / 400s as before.
func Check(slug string) (quarantined bool, reason string) {
	mu.RLock()
	defer mu.RUnlock()
	e, ok := entries[slug]
	if !ok || !e.Quarantined {
		return false, ""
	}
	return true, e.Reason
}

// Snapshot lists every currently-quarantined tenant for health/admin.
func Snapshot() map[string]Entry {
	mu.RLock()
	defer mu.RUnlock()
	out := make(map[string]Entry, len(entries))
	for slug, e := range entries {
		out[slug] = *e
	}
	return out
}
