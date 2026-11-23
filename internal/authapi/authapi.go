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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/features/clientctx"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
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
	RDB       *redis.Client
	Providers map[string]Provider

	// LoginRatePerMinute bounds attempts per tenant/email per minute.
	// It is a per-account limit on purpose: the pre-auth IP limiter
	// cannot see the account, and on Vercel every request shares the
	// proxy's socket address unless TRUSTED_PROXY_CIDRS is configured.
	LoginRatePerMinute int
	// LoginLockoutFailures is the number of failed password checks inside
	// LoginLockout that locks the account.
	LoginLockoutFailures int
	// LoginLockout is the sliding failure window and lockout duration.
	LoginLockout time.Duration
}

const (
	defaultLoginRatePerMinute   = 10
	defaultLoginLockoutFailures = 5
	defaultLoginLockout         = 15 * time.Minute
)

// New builds the handler and one provider per registered project. A nil
// Redis client disables the login limiter (tests and local shells); the
// server always passes the real client.
func New(pool *pgxpool.Pool, rdb *redis.Client) *Auth {
	return &Auth{
		Pool:                 pool,
		RDB:                  rdb,
		Providers:            newProviders(pool),
		LoginRatePerMinute:   defaultLoginRatePerMinute,
		LoginLockoutFailures: defaultLoginLockoutFailures,
		LoginLockout:         defaultLoginLockout,
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

	// Per-account throttle and lockout before any password work. The key is
	// a hash of tenant+email, so Redis never holds a raw address.
	subject := loginSubject(req.TenantID, req.Email)
	if retry, locked := a.loginLocked(r.Context(), subject); locked {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		writeErr(w, http.StatusTooManyRequests, "too many login attempts")
		return
	}
	if retry, limited := a.loginRateLimited(r.Context(), subject); limited {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		writeErr(w, http.StatusTooManyRequests, "too many login attempts")
		return
	}

	p := a.providerFor(req.TenantID)
	if p == nil {
		// Do not reveal whether the tenant exists. Still burn a bcrypt
		// verification and record the failure so timing and lockout are
		// identical to a real account with a wrong password.
		verifyDummyPassword(req.Password)
		a.recordLoginFailure(r.Context(), subject)
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	creds, err := p.Authenticate(r.Context(), req.TenantID, req.Email)
	if errors.Is(err, ErrInvalidCredentials) {
		verifyDummyPassword(req.Password)
		a.recordLoginFailure(r.Context(), subject)
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err != nil {
		slog.Error("login lookup", "tenant", req.TenantID, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if !creds.Active || !VerifyPassword(creds.PasswordHash, req.Password) {
		a.recordLoginFailure(r.Context(), subject)
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	// Correct credentials clear the failure window before any session
	// write. A later token/DB error is not a password failure.
	a.clearLoginFailures(r.Context(), subject)

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

// dummyPasswordHash keeps unknown-tenant and unknown-email login responses
// time-comparable to a real bcrypt verification. Generated once at startup;
// never accepted by any account.
var dummyPasswordHash = func() string {
	h, err := HashPassword("login-timing-equalizer-not-a-password")
	if err != nil {
		return ""
	}
	return h
}()

func verifyDummyPassword(password string) {
	if dummyPasswordHash != "" {
		VerifyPassword(dummyPasswordHash, password)
	}
}

// loginSubject is the limiter key. Hashing keeps raw emails out of Redis
// (keys can be inspected operationally) while preserving exact scoping.
func loginSubject(tenant, email string) string {
	sum := sha256.Sum256([]byte(tenant + "\x00" + email))
	return hex.EncodeToString(sum[:])
}

func (a *Auth) loginPolicy() (rate, failures int, lockout time.Duration) {
	rate, failures, lockout = a.LoginRatePerMinute, a.LoginLockoutFailures, a.LoginLockout
	if rate <= 0 {
		rate = defaultLoginRatePerMinute
	}
	if failures <= 0 {
		failures = defaultLoginLockoutFailures
	}
	if lockout <= 0 {
		lockout = defaultLoginLockout
	}
	return rate, failures, lockout
}

func loginFailureKey(subject string) string { return "auth:login:fail:" + subject }

// loginLocked reports the remaining lockout seconds. Redis errors fail open:
// a cache outage must not lock every staff user out of the product. The
// failure is logged (without email or token) so the outage is visible.
func (a *Auth) loginLocked(ctx context.Context, subject string) (int, bool) {
	if a.RDB == nil {
		return 0, false
	}
	_, maxFailures, lockout := a.loginPolicy()
	key := loginFailureKey(subject)
	n, err := a.RDB.Get(ctx, key).Int()
	if errors.Is(err, redis.Nil) {
		return 0, false
	}
	if err != nil {
		slog.Warn("login lockout check, failing open", "err", err)
		return 0, false
	}
	if n < maxFailures {
		return 0, false
	}
	ttl, err := a.RDB.TTL(ctx, key).Result()
	if err != nil || ttl <= 0 {
		ttl = lockout
	}
	retry := int(ttl.Seconds())
	if retry < 1 {
		retry = 1
	}
	return retry, true
}

// loginRateLimited counts every attempt (success or failure) for one account
// in a fixed one-minute window. The account key is what survives IP rotation
// and NAT sharing, which the pre-auth IP limiter cannot see.
func (a *Auth) loginRateLimited(ctx context.Context, subject string) (int, bool) {
	if a.RDB == nil {
		return 0, false
	}
	rate, _, _ := a.loginPolicy()
	if rate <= 0 {
		return 0, false
	}
	now := time.Now().UTC()
	key := fmt.Sprintf("auth:login:rate:%s:%s", subject, now.Format("200601021504"))
	n, err := redisx.IncrExpireRun(ctx, a.RDB, key, 65*time.Second)
	if err != nil {
		slog.Warn("login rate check, failing open", "err", err)
		return 0, false
	}
	if int(n) > rate {
		retry := 60 - now.Second()
		if retry < 1 {
			retry = 60
		}
		return retry, true
	}
	return 0, false
}

func (a *Auth) recordLoginFailure(ctx context.Context, subject string) {
	if a.RDB == nil {
		return
	}
	_, maxFailures, lockout := a.loginPolicy()
	n, err := redisx.IncrExpireRun(ctx, a.RDB, loginFailureKey(subject), lockout)
	if err != nil {
		slog.Warn("login failure record, failing open", "err", err)
		return
	}
	if int(n) == maxFailures {
		// No email or token in the log: the subject is already hashed.
		slog.Warn("login locked after repeated failures", "failures", n, "lockout", lockout.String())
	}
}

func (a *Auth) clearLoginFailures(ctx context.Context, subject string) {
	if a.RDB == nil {
		return
	}
	if err := a.RDB.Del(ctx, loginFailureKey(subject)).Err(); err != nil {
		slog.Warn("login failure clear", "err", err)
	}
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
