// Unified identity middleware: staff (X-Session-Token) OR client
// (X-Client-Token). Phase 7.
//
// DONE: Phase 7 (staff auth) / STANDARDS A, D, F
package records

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/authapi"
	"github.com/davinakmalyasha/agents-proxy/internal/cache"
	"github.com/davinakmalyasha/agents-proxy/internal/features/clientctx"
	"github.com/davinakmalyasha/agents-proxy/internal/middleware"
)

// IdentityDeps is what the unified middleware needs.
type IdentityDeps struct {
	Pool  *pgxpool.Pool
	Cache *cache.Client
	Auth  *authapi.Auth
}

// WithIdentity resolves the request principal:
//  1. X-Session-Token → staff user (owner/pegawai) via sports_sessions
//  2. fallback X-Client-Token → client (full access) via the cache
//
// Public reads (GET /availability, /facilities, /pricing) are allowed
// without a token for the landing page. The proxy defers a scoped public
// token design — for now the tenant is inferred from ?tenant_id= or
// defaults to "centrum" and the identity is role "public" (read-only).
// Writes remain 401 without a token; owner gates remain 403 for public.
//
// 401 on bad/missing, 403 on paused/blocked, 500 on internal.
func WithIdentity(deps IdentityDeps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sessTok := strings.TrimSpace(r.Header.Get("X-Session-Token"))
			if sessTok != "" {
				u, err := deps.Auth.UserByToken(r.Context(), sessTok)
				if err != nil {
					if errors.Is(err, pgx.ErrNoRows) {
						// For public landing reads, stale/invalid session on browser shouldn't block
						// availability — fallback to public identity instead of 401 (was CORS + 401 loop)
						if isPublicRead(r) {
							if id := publicIdentity(w, r, deps); id != nil {
								ctx := clientctx.With(r.Context(), id)
								next.ServeHTTP(w, r.WithContext(ctx))
							}
							return
						}
						writeJSONError(w, http.StatusUnauthorized, "invalid or expired session")
						return
					}
					slog.Error("session auth lookup", "err", err)
					writeJSONError(w, http.StatusInternalServerError, "internal")
					return
				}
				id := &clientctx.Identity{
					UserID: u.ID,
					Email:  u.Email,
					Role:   u.Role,
					Tenant: u.TenantID,
				}
				ctx := clientctx.With(r.Context(), id)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			tok := r.Header.Get("X-Client-Token")
			if tok == "" {
				if isPublicRead(r) {
					if id := publicIdentity(w, r, deps); id != nil {
						ctx := clientctx.With(r.Context(), id)
						next.ServeHTTP(w, r.WithContext(ctx))
					}
					return
				}
				writeJSONError(w, http.StatusUnauthorized, "missing credentials")
				return
			}
			cm, err := deps.Cache.GetByToken(r.Context(), tok)
			if err != nil {
				if errors.Is(err, cache.ErrInvalidToken) {
					writeJSONError(w, http.StatusUnauthorized, "invalid token")
					return
				}
				slog.Error("client auth lookup", "err", err)
				writeJSONError(w, http.StatusInternalServerError, "internal")
				return
			}
			if cm.Status != "active" {
				writeJSONError(w, http.StatusForbidden, "client "+cm.Status)
				return
			}
			ctx := clientctx.WithClient(r.Context(), cm)
			ctx = middleware.SetClientID(ctx, cm.ID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireIdentity reads the resolved identity from context. Writes
// 401 and returns nil if the auth middleware was bypassed.
func RequireIdentity(w http.ResponseWriter, r *http.Request) *clientctx.Identity {
	id := clientctx.From(r.Context())
	if id == nil {
		writeJSONError(w, http.StatusUnauthorized, "identity not resolved")
	}
	return id
}

// RequireOwner rejects staff pegawai (owner + client tokens pass).
//
// This is the BUSINESS-DATA tier: it answers "may this caller change
// tenant data?" and deliberately admits a client token, because a
// client site's backend legitimately writes through X-Client-Token and
// RLS pins the tenant. Data writes are bounded by quota and auditable.
//
// For anything that changes WHO HAS ACCESS, use RequireStaffOwner.
func RequireOwner(w http.ResponseWriter, r *http.Request) *clientctx.Identity {
	id := RequireIdentity(w, r)
	if id == nil {
		return nil
	}
	if !id.IsOwner() {
		writeJSONError(w, http.StatusForbidden, "owner only")
		return nil
	}
	return id
}

// RequireStaffOwner is the SECURITY-ADMINISTRATION tier and is
// deliberately narrower than RequireOwner: a real owner session only,
// never a client token.
//
// The split is the threat model, not a role preference. A client token
// is one static secret, typically embedded in a site or an integration.
// Business writes are quota-bounded and auditable, so letting that
// token make them is the platform's API-integration model and stays.
// But letting the same token rewrite a Telegram bot's allow_chat_ids or
// promote a staff account would turn a leaked integration secret into
// persistent human-level access, and no quota limits that.
//
// Use this on: staff identity/role changes, the bot registry, and
// anything else that alters access rather than data.
func RequireStaffOwner(w http.ResponseWriter, r *http.Request) *clientctx.Identity {
	id := RequireIdentity(w, r)
	if id == nil {
		return nil
	}
	if !id.IsStaffOwner() {
		writeJSONError(w, http.StatusForbidden, "owner session required")
		return nil
	}
	return id
}

func isPublicRead(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	p := r.URL.Path
	// Paths are seen inside the vertical router: /availability, /facilities, /pricing etc.
	return strings.Contains(p, "/availability") ||
		strings.Contains(p, "/facilities") ||
		strings.Contains(p, "/pricing")
}

func tenantFromQuery(r *http.Request) string {
	if t := strings.TrimSpace(r.URL.Query().Get("tenant_id")); t != "" {
		return t
	}
	return "centrum"
}

// publicIdentity builds the read-only landing identity, validating the
// tenant against the clients table. Unknown tenants get a 400 (and nil)
// instead of becoming RLS contexts or Redis/history key material.
//
// A lookup that fails for infrastructure reasons is a 503, not a 400:
// reporting it as a bad request hides server faults behind "unknown
// tenant" and points whoever is debugging at the caller.
func publicIdentity(w http.ResponseWriter, r *http.Request, deps IdentityDeps) *clientctx.Identity {
	tenant := tenantFromQuery(r)
	known, err := CheckTenant(r.Context(), deps.Pool, tenant)
	if err != nil {
		slog.Error("public tenant lookup failed", "tenant", tenant, "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "tenant lookup unavailable")
		return nil
	}
	if !known {
		writeJSONError(w, http.StatusBadRequest, "unknown tenant")
		return nil
	}
	return &clientctx.Identity{Role: "public", Tenant: tenant}
}
