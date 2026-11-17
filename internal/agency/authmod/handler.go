package auth

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/api/idtoken"

	platformauth "github.com/davinakmalyasha/agents-proxy/internal/agency/auth"
	ratelimit "github.com/davinakmalyasha/agents-proxy/internal/agency/ratelimit"
	"github.com/davinakmalyasha/agents-proxy/internal/agency/response"
)

// Notifier sends the superadmin OTP email.
type Notifier interface {
	SendEmail(ctx context.Context, to, subject, html string) error
	ResendConfigured() bool
}

// GoogleClaims is the verified identity from a Google ID token.
type GoogleClaims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

// GoogleVerifier validates an ID token against the configured client ID.
// A variable (not a direct call) so tests inject a stub without network.
var GoogleVerifier = func(ctx context.Context, idToken, clientID string) (*GoogleClaims, error) {
	payload, err := idtoken.Validate(ctx, idToken, clientID)
	if err != nil {
		return nil, err
	}
	sub, _ := payload.Claims["sub"].(string)
	email, _ := payload.Claims["email"].(string)
	verified, _ := payload.Claims["email_verified"].(bool)
	name, _ := payload.Claims["name"].(string)
	if sub == "" || email == "" {
		return nil, fmt.Errorf("google: id token missing sub/email")
	}
	return &GoogleClaims{Subject: sub, Email: email, EmailVerified: verified, Name: name}, nil
}

// Handler serves human auth routes.
type Handler struct {
	store          *Store
	pool           *pgxpool.Pool
	logger         *slog.Logger
	jwtSecret      string
	jwtTTL         time.Duration
	secure         bool
	env            string
	googleClientID string
	notifier       Notifier
}

// NewHandler builds an auth handler.
func NewHandler(store *Store, pool *pgxpool.Pool, logger *slog.Logger, jwtSecret string, jwtTTL time.Duration, secure bool, env, googleClientID string, notifier Notifier) *Handler {
	return &Handler{store: store, pool: pool, logger: logger, jwtSecret: jwtSecret, jwtTTL: jwtTTL, secure: secure, env: env, googleClientID: googleClientID, notifier: notifier}
}

// RegisterRoutes mounts auth chi routes (mounted under /api/v1/auth).
func (h *Handler) RegisterRoutes(r chi.Router, loginLimiter *ratelimit.RateLimiter) {
	cookieGuard := platformauth.CookieAuth(h.jwtSecret, h.pool, h.jwtTTL)
	r.With(loginLimiter.Limit).Post("/login", h.login)
	r.With(loginLimiter.Limit).Post("/login/verify", h.loginVerify)
	r.With(loginLimiter.Limit).Post("/google", h.google)
	r.With(cookieGuard).Post("/logout", h.logout)
	r.With(cookieGuard).Get("/me", h.me)
	r.With(cookieGuard).Post("/change-password", h.changePassword)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Identity string `json:"identity"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	identity := strings.TrimSpace(body.Identity)
	if identity == "" {
		identity = strings.TrimSpace(body.Email)
	}
	if identity == "" || body.Password == "" {
		// Same generic message as a real failure — no enumeration.
		response.Unauthorized(w, "Invalid credentials")
		return
	}
	user, hash, version, deletedAt, err := h.store.FindForLoginIdentity(r.Context(), identity)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		h.logger.ErrorContext(r.Context(), "login lookup failed", "error", err)
		response.Internal(w)
		return
	}
	active := err == nil && deletedAt == nil
	if !SecureCompare(body.Password, hash) || !active {
		response.Unauthorized(w, "Invalid credentials")
		return
	}
	if user.Role == "SUPERADMIN" {
		h.startOTP(w, r, user)
		return
	}
	token, err := platformauth.SignJWT(h.jwtSecret, h.jwtTTL, user.ID, user.Role, version)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "login sign failed", "error", err)
		response.Internal(w)
		return
	}
	h.setCookie(w, token)
	response.OK(w, map[string]any{"user": user}, "Login successful")
}

// startOTP issues the second-step email code for superadmins.
func (h *Handler) startOTP(w http.ResponseWriter, r *http.Request, user User) {
	unconfigured := h.notifier == nil || !h.notifier.ResendConfigured()
	if unconfigured && strings.EqualFold(h.env, "production") {
		// Fail closed in production: password alone must never suffice.
		if h.logger != nil {
			h.logger.WarnContext(r.Context(), "otp email unavailable", "user", user.ID)
		}
		response.Fail(w, http.StatusServiceUnavailable, "Email verification unavailable, try again later", "OTP_UNAVAILABLE")
		return
	}
	code, err := generateOTPCode()
	if err != nil {
		h.logger.ErrorContext(r.Context(), "otp generate failed", "error", err)
		response.Internal(w)
		return
	}
	if err := h.store.CreateLoginOTP(r.Context(), user.ID, HashOTPCode(code), OTPTTL); err != nil {
		h.logger.ErrorContext(r.Context(), "otp store failed", "error", err)
		response.Internal(w)
		return
	}
	subject := "Your DFD sign-in code"
	html := "<p>Your DFD verification code is:</p><h1>" + code + "</h1><p>It expires in 10 minutes. If you didn't request this, ignore this email.</p>"
	if err := h.notifier.SendEmail(r.Context(), user.Email, subject, html); err != nil {
		h.logger.ErrorContext(r.Context(), "otp send failed", "error", err)
		response.Fail(w, http.StatusBadGateway, "Could not send verification email", "OTP_SEND_FAILED")
		return
	}
	response.OK(w, map[string]any{"otpRequired": true, "userId": user.ID, "email": maskEmail(user.Email)}, "Verification code sent")
}

func (h *Handler) loginVerify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserID int64  `json:"userId"`
		Code   string `json:"code"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	code := strings.TrimSpace(body.Code)
	if body.UserID == 0 || code == "" {
		response.BadRequest(w, "userId and code are required")
		return
	}
	ok, locked, err := h.store.VerifyLoginOTP(r.Context(), body.UserID, code)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.Unauthorized(w, "Invalid or expired code")
			return
		}
		h.logger.ErrorContext(r.Context(), "otp verify failed", "error", err)
		response.Internal(w)
		return
	}
	if locked {
		response.Fail(w, http.StatusTooManyRequests, "Too many attempts, request a new code", "OTP_LOCKED")
		return
	}
	if !ok {
		response.Unauthorized(w, "Invalid or expired code")
		return
	}
	user, err := h.store.GetProfile(r.Context(), body.UserID)
	if err != nil {
		h.clearCookie(w)
		response.Unauthorized(w, "Not authenticated")
		return
	}
	if user.Role != "SUPERADMIN" {
		response.Unauthorized(w, "Invalid or expired code")
		return
	}
	version, err := h.store.TokenVersion(r.Context(), user.ID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "otp version failed", "error", err)
		response.Internal(w)
		return
	}
	token, err := platformauth.SignJWT(h.jwtSecret, h.jwtTTL, user.ID, user.Role, version)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "otp sign failed", "error", err)
		response.Internal(w)
		return
	}
	h.setCookie(w, token)
	response.OK(w, map[string]any{"user": user}, "Login successful")
}

// google implements "Continue with Google": verified ID token in, session
// cookie out. Unknown verified emails JIT-provision CLIENT accounts
// ("Get started"); existing accounts link by verified email. Roles never
// come from Google.
func (h *Handler) google(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDToken string `json:"idToken"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.IDToken) == "" {
		response.BadRequest(w, "idToken is required")
		return
	}
	if h.googleClientID == "" {
		response.Fail(w, http.StatusServiceUnavailable, "Google login is not configured", "GOOGLE_DISABLED")
		return
	}
	claims, err := GoogleVerifier(r.Context(), strings.TrimSpace(body.IDToken), h.googleClientID)
	if err != nil {
		response.Unauthorized(w, "Invalid Google credential")
		return
	}
	if !claims.EmailVerified {
		response.Fail(w, http.StatusBadRequest, "Google email is not verified", "GOOGLE_UNVERIFIED")
		return
	}
	user, version, err := h.store.FindByGoogleSub(r.Context(), claims.Subject)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		h.logger.ErrorContext(r.Context(), "google lookup failed", "error", err)
		response.Internal(w)
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		user, version, err = h.linkOrProvision(r.Context(), claims)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "google provision failed", "error", err)
			response.Internal(w)
			return
		}
	}
	token, err := platformauth.SignJWT(h.jwtSecret, h.jwtTTL, user.ID, user.Role, version)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "google sign failed", "error", err)
		response.Internal(w)
		return
	}
	h.setCookie(w, token)
	response.OK(w, map[string]any{"user": user}, "Login successful")
}

// linkOrProvision binds a verified Google identity: existing email links,
// unknown email provisions a CLIENT.
func (h *Handler) linkOrProvision(ctx context.Context, claims *GoogleClaims) (User, int, error) {
	if existing, version, err := h.store.FindByEmail(ctx, claims.Email); err == nil {
		if err := h.store.LinkGoogleSub(ctx, existing.ID, claims.Subject); err != nil {
			return User{}, 0, err
		}
		return existing, version, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return User{}, 0, err
	}
	username, err := h.freeUsername(ctx, claims)
	if err != nil {
		return User{}, 0, err
	}
	return h.store.CreateClientUser(ctx, username, claims.Email, claims.Subject)
}

// freeUsername derives a unique username from the Google name or email.
func (h *Handler) freeUsername(ctx context.Context, claims *GoogleClaims) (string, error) {
	base := strings.ToLower(strings.TrimSpace(claims.Name))
	base = strings.Map(func(c rune) rune {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
			return c
		case c == ' ' || c == '.' || c == '@':
			return '_'
		default:
			return -1
		}
	}, base)
	if base == "" {
		base = strings.ToLower(strings.TrimSpace(claims.Email))
		if at := strings.Index(base, "@"); at > 0 {
			base = base[:at]
		}
	}
	if base == "" {
		base = "client"
	}
	candidate := base
	for i := 2; ; i++ {
		taken, err := h.store.UsernameTaken(ctx, candidate)
		if err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s%d", base, i)
	}
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if actor, ok := platformauth.ActorFrom(r.Context()); ok && actor.Type == "user" {
		_ = h.store.BumpTokenVersion(r.Context(), actor.UserID)
	}
	h.clearCookie(w)
	response.OK(w, nil, "Logged out and tokens invalidated")
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	actor, ok := platformauth.ActorFrom(r.Context())
	if !ok || actor.Type != "user" {
		response.Unauthorized(w, "Not authenticated")
		return
	}
	user, err := h.store.GetProfile(r.Context(), actor.UserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.clearCookie(w)
			response.Unauthorized(w, "Not authenticated")
			return
		}
		h.logger.ErrorContext(r.Context(), "me failed", "error", err)
		response.Internal(w)
		return
	}
	response.OK(w, map[string]any{"user": user}, "User profile retrieved")
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	actor, ok := platformauth.ActorFrom(r.Context())
	if !ok || actor.Type != "user" {
		response.Unauthorized(w, "Not authenticated")
		return
	}
	var body struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.CurrentPassword == "" || !ValidPassword(body.NewPassword) {
		response.ValidationError(w, "Invalid password change", map[string]string{
			"newPassword": "New password must be at least 8 characters with letters and numbers",
		})
		return
	}
	hash, err := platformauth.HashPassword(body.NewPassword)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "hash failed", "error", err)
		response.Internal(w)
		return
	}
	version, err := h.store.ChangePassword(r.Context(), actor.UserID, body.CurrentPassword, hash)
	if err != nil {
		if errors.Is(err, errWrongPassword) {
			response.Unauthorized(w, "Current password is incorrect")
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			h.clearCookie(w)
			response.Unauthorized(w, "Not authenticated")
			return
		}
		h.logger.ErrorContext(r.Context(), "change password failed", "error", err)
		response.Internal(w)
		return
	}
	// Re-issue this session; every other session is now dead (tokenVersion++).
	profile, err := h.store.GetProfile(r.Context(), actor.UserID)
	if err != nil {
		h.clearCookie(w)
		response.Unauthorized(w, "Not authenticated")
		return
	}
	token, err := platformauth.SignJWT(h.jwtSecret, h.jwtTTL, profile.ID, profile.Role, version)
	if err != nil {
		response.Internal(w)
		return
	}
	h.setCookie(w, token)
	response.OK(w, nil, "Password changed")
}

// generateOTPCode returns a zero-padded 6-digit code from crypto/rand.
func generateOTPCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// maskEmail hides most of the local part (otp step response only).
func maskEmail(email string) string {
	at := strings.Index(email, "@")
	if at <= 1 {
		return "***"
	}
	local := email[:at]
	if len(local) > 2 {
		local = local[:2] + "***"
	} else {
		local = local[:1] + "***"
	}
	return local + email[at:]
}

func (h *Handler) setCookie(w http.ResponseWriter, token string) {
	sameSite := http.SameSiteStrictMode
	if h.secure {
		// Cross-site production frontend needs None + Secure.
		sameSite = http.SameSiteNoneMode
	}
	http.SetCookie(w, &http.Cookie{
		Name:     platformauth.CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.secure,
		SameSite: sameSite,
		MaxAge:   int(h.jwtTTL.Seconds()),
	})
}

func (h *Handler) clearCookie(w http.ResponseWriter) {
	platformauth.ClearCookie(w, h.secure)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(raw) == 0 {
		response.BadRequest(w, "Request body is required")
		return false
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		response.BadRequest(w, "Invalid JSON body")
		return false
	}
	return true
}
