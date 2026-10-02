// Package authapi is the staff authentication API: email+password
// login with bcrypt, DB-backed sessions (sha256-hashed tokens),
// logout, and a /me endpoint. This is the proxy's own identity
// system — no Supabase Auth, no third party.
//
// DONE: Phase 7 (staff auth) / STANDARDS A, D, F
package authapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/davinakmalyasha/agents-proxy/internal/cache"
)

// SessionTTL is how long a login lasts before re-auth.
const SessionTTL = 30 * 24 * time.Hour

// Auth is the staff-auth handler.
type Auth struct {
	Pool  *pgxpool.Pool
	Cache *cache.Client
}

// User is the public user shape returned by login/me.
type User struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	FullName string `json:"full_name"`
	Phone    string `json:"phone,omitempty"`
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

// register creates a staff account. Owner session ONLY — client tokens
// can never mint users (a leaked API token must not become an interactive
// owner login). First owner on a fresh tenant is bootstrapped via psql
// against DATABASE_ADMIN_URL, never via API token.
func (a *Auth) register(w http.ResponseWriter, r *http.Request) {
	// Resolve the caller's tenant from the owner session.
	sess := strings.TrimSpace(r.Header.Get("X-Session-Token"))
	if sess == "" {
		writeErr(w, http.StatusUnauthorized, "owner session required")
		return
	}
	u, err := a.UserByToken(r.Context(), sess)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid or expired session")
		return
	}
	if u.Role != "owner" {
		writeErr(w, http.StatusForbidden, "owner only")
		return
	}
	tenantID := u.TenantID

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
		newRole = "pegawai"
	}
	if newRole != "owner" && newRole != "pegawai" {
		writeErr(w, http.StatusBadRequest, "role must be owner or pegawai")
		return
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// The INSERT is tenant-scoped by RLS (WITH CHECK on
	// app.tenant_id), so it must run inside a transaction with the
	// GUC set — a bare Pool.QueryRow has no GUC and the policy
	// rejects the row (500 on live runtime pool; superuser test
	// pools bypass RLS which hid this). Cannot reuse
	// records.WithTenantTx here: records imports authapi (cycle).
	var id string
	tx, err := a.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	if _, err := tx.Exec(r.Context(), "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// Phone was bound into the request struct and then dropped on the floor:
	// the INSERT below did not name a column for it, so an owner who filled in
	// a staff member's WhatsApp number got a 201 back with no phone on the
	// record and no indication that it had been ignored.
	err = tx.QueryRow(r.Context(),
		`INSERT INTO sports_users (tenant_id, email, password_hash, role, full_name, phone)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id::text`,
		tenantID, req.Email, hash, newRole, req.FullName, req.Phone).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			writeErr(w, http.StatusConflict, "email already registered")
			return
		}
		slog.Error("register insert", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"user": User{ID: id, TenantID: tenantID, Email: req.Email, Role: newRole, FullName: req.FullName, Phone: req.Phone},
	})
}

// clientByToken resolves a client token via the cache client model.
func (a *Auth) clientByToken(ctx context.Context, tok string) (*cache.ClientModel, error) {
	return a.Cache.GetByToken(ctx, tok)
}

func isUniqueViolation(err error) bool {
	if pgErr, ok := err.(interface{ SQLState() string }); ok {
		return pgErr.SQLState() == "23505"
	}
	return false
}

// HashPassword bcrypt-hashes a password (cost 10 default).
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

// VerifyPassword checks a password against a bcrypt hash.
func VerifyPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// GenerateSessionToken returns (raw, sha256Hex(raw)) like client tokens.
func GenerateSessionToken() (string, string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	raw := hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	return raw, hex.EncodeToString(sum[:]), nil
}

// HashToken sha256-hashes a session token for lookup.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
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
	req.TenantID = strings.TrimSpace(req.TenantID)
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.TenantID == "" || req.Email == "" || req.Password == "" {
		writeErr(w, http.StatusBadRequest, "tenant_id, email, password required")
		return
	}

	var (
		id       string
		hash     string
		role     string
		fullName string
		phone    string
		active   bool
	)
	// Login lookup goes through private.login_user (migration 021, extended
	// by 040), not a direct SELECT: sports_users is tenant-scoped by RLS and
	// this query runs with no app.tenant_id set, so a direct read would match
	// nothing. The function is SECURITY DEFINER and returns only the
	// columns login needs, which also keeps password_hash out of reach of any
	// other runtime query.
	err := a.Pool.QueryRow(r.Context(),
		`SELECT user_id::text, password_hash, role, full_name, phone, is_active
		   FROM private.login_user($1, $2)`,
		req.TenantID, req.Email,
	).Scan(&id, &hash, &role, &fullName, &phone, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		// Uniform error: don't reveal whether the user exists.
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err != nil {
		slog.Error("login query", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if !active || !VerifyPassword(hash, req.Password) {
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	raw, tokenHash, err := GenerateSessionToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "token gen")
		return
	}
	// Session creation goes through a SECURITY DEFINER function
	// (migration 032). The runtime role no longer holds INSERT on
	// sports_sessions, and the function derives tenant_id from
	// sports_users server-side rather than trusting the caller - which
	// is what removes the ability to mint a session for a user in an
	// arbitrary tenant.
	var sessionID *string
	if err := a.Pool.QueryRow(r.Context(),
		`SELECT private.create_session($1::uuid, $2, now() + $3::interval)::text`,
		id, tokenHash, SessionTTL.String(),
	).Scan(&sessionID); err != nil {
		slog.Error("login session insert", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if sessionID == nil {
		// create_session returns NULL when the user is gone or inactive
		// between the password check and here.
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"token": raw,
		"user":  User{ID: id, TenantID: req.TenantID, Email: req.Email, Role: role, FullName: fullName, Phone: phone},
	})
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	tok := sessionToken(r)
	if tok != "" {
		// Definer function, not a direct DELETE: the runtime role lost
		// DML on sports_sessions in migration 032.
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

// UserByToken resolves a session token to a user, checking expiry.
//
// Reads through private.session_user (migration 032). sports_sessions
// is now tenant-scoped by RLS and this lookup runs with no
// app.tenant_id set, so a direct read would match nothing. The function
// is SECURITY DEFINER and returns only these columns, which also
// keeps password_hash out of reach of a runtime query.
func (a *Auth) UserByToken(ctx context.Context, tok string) (*User, error) {
	var u User
	err := a.Pool.QueryRow(ctx,
		`SELECT user_id::text, tenant_id, email, role, full_name, phone
		   FROM private.session_user($1)`,
		HashToken(tok),
	).Scan(&u.ID, &u.TenantID, &u.Email, &u.Role, &u.FullName, &u.Phone)
	if err != nil {
		return nil, err
	}
	// Bump last_seen but at most ~once a minute per session: a busy
	// staff dashboard fires many authed requests and an unconditional
	// UPDATE here would take a write lock on the session row every
	// time. The threshold is passed into the function so the
	// conditional is evaluated the same way it used to be in SQL.
	_, _ = a.Pool.Exec(ctx,
		`SELECT private.touch_session($1, now() - interval '1 minute')`, HashToken(tok))
	return &u, nil
}

func sessionToken(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Session-Token"))
}

// writeJSON / writeErr use the same wire format as the rest of the
// API ({"error": msg} for errors). Self-contained to avoid an import
// cycle with internal/features/shared/records.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
