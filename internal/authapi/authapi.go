// Package authapi is the platform staff-authentication shell: it owns the
// session directory, token primitives, the /v1/auth routes, and the provider
// registry. Project user tables and login rules live in project packages.
//
// This package must never name a project table. It resolves a session to
// (user_id, tenant_id), then asks the tenant's registered Provider for the
// live profile. That is what makes staff auth repeatable for the next project
// without editing platform code.
package authapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davinakmalyasha/agents-proxy/internal/features/clientctx"
)

// Auth is the platform staff-auth handler. Providers are constructed from the
// project factories registered at init time; tests may add a fixture provider
// with RegisterProvider.
//
// There is deliberately no cached User here. Role and is_active are
// authorization state: a demoted or deactivated owner must lose authority on
// the next request, not after a TTL. The session directory lookup plus one
// project profile read is the price of that guarantee.
type Auth struct {
	Pool      *pgxpool.Pool
	Providers map[string]Provider
}

// New builds the handler and one provider per registered project.
func New(pool *pgxpool.Pool) *Auth {
	return &Auth{
		Pool:      pool,
		Providers: newProviders(pool),
	}
}

// Routes returns the /v1/auth chi router (mounted under /v1/auth by
// api/router.go).
func (a *Auth) Routes() http.Handler {
	r := chi.NewRouter()
	r.Post("/login", a.login)
	r.Post("/logout", a.logout)
	r.Get("/me", a.me)
	r.Post("/register", a.register)
	return r
}

type loginReq struct {
	TenantID string `json:"tenant_id"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.TenantID = strings.ToLower(strings.TrimSpace(req.TenantID))
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.TenantID == "" || req.Email == "" || req.Password == "" {
		writeErr(w, http.StatusBadRequest, "tenant_id, email, password required")
		return
	}

	p := a.providerFor(req.TenantID)
	if p == nil {
		// Do not reveal whether the tenant exists.
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	creds, err := p.Authenticate(r.Context(), req.TenantID, req.Email)
	if errors.Is(err, ErrInvalidCredentials) {
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err != nil {
		slog.Error("login lookup", "tenant", req.TenantID, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if !creds.Active || !VerifyPassword(creds.PasswordHash, req.Password) {
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	raw, tokenHash, err := GenerateSessionToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "token gen")
		return
	}
	// The platform session directory has no project knowledge. The provider
	// has just verified the user/password pair for this tenant; the definer
	// writes the session row and validates that the tenant is active.
	var sessionID *string
	if err := a.Pool.QueryRow(r.Context(),
		`SELECT private.staff_session_create($1, $2::uuid, $3, now() + $4::interval)::text`,
		req.TenantID, creds.User.ID, tokenHash, SessionTTL.String(),
	).Scan(&sessionID); err != nil {
		slog.Error("login session insert", "tenant", req.TenantID, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if sessionID == nil {
		// The tenant paused between the password check and here.
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	u := creds.User
	u.TenantID = req.TenantID
	writeJSON(w, http.StatusOK, map[string]any{
		"token": raw,
		"user":  u,
	})
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	tok := sessionToken(r)
	if tok != "" {
		// Definer function, not a direct DELETE: the runtime role has no
		// direct DML on public.staff_sessions.
		_, _ = a.Pool.Exec(r.Context(),
			`SELECT private.delete_session($1)`, HashToken(tok))
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

// me resolves the session and returns the user.
func (a *Auth) me(w http.ResponseWriter, r *http.Request) {
	tok := sessionToken(r)
	if tok == "" {
		writeErr(w, http.StatusUnauthorized, "missing session token")
		return
	}
	u, err := a.UserByToken(r.Context(), tok)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid or expired session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

// register creates a staff account. Owner session ONLY — client tokens can
// never mint users (a leaked API token must not become an interactive owner
// login). The first owner is seeded by provisioning, never via an API token.
func (a *Auth) register(w http.ResponseWriter, r *http.Request) {
	sess := sessionToken(r)
	if sess == "" {
		writeErr(w, http.StatusUnauthorized, "owner session required")
		return
	}
	u, err := a.UserByToken(r.Context(), sess)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid or expired session")
		return
	}
	if u.Role != clientctx.RoleOwner {
		writeErr(w, http.StatusForbidden, "owner only")
		return
	}

	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		FullName string `json:"full_name"`
		Phone    string `json:"phone"`
		Role     string `json:"role"` // pegawai (default) | owner
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.FullName = strings.TrimSpace(req.FullName)
	if !strings.Contains(req.Email, "@") {
		writeErr(w, http.StatusBadRequest, "email invalid")
		return
	}
	if len(req.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	newRole := req.Role
	if newRole == "" {
		newRole = clientctx.RoleStaff
	}
	if newRole != clientctx.RoleOwner && newRole != clientctx.RoleStaff {
		writeErr(w, http.StatusBadRequest, "role must be owner or pegawai")
		return
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}

	created, err := a.RegisterUser(r.Context(), u.TenantID, RegisterInput{
		Email:        req.Email,
		PasswordHash: hash,
		Role:         newRole,
		FullName:     req.FullName,
		Phone:        req.Phone,
	})
	switch {
	case errors.Is(err, ErrEmailTaken):
		writeErr(w, http.StatusConflict, "email already registered")
		return
	case errors.Is(err, ErrInvalidRole):
		writeErr(w, http.StatusBadRequest, "role must be owner or pegawai")
		return
	case errors.Is(err, ErrInvalidCredentials):
		writeErr(w, http.StatusUnauthorized, "invalid or expired session")
		return
	case err != nil:
		slog.Error("register insert", "tenant", u.TenantID, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": created})
}

func sessionToken(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Session-Token"))
}

// writeJSON / writeErr use the same wire format as the rest of the API
// ({"error": msg} for errors). Self-contained to avoid an import cycle with
// internal/features/shared/records.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
