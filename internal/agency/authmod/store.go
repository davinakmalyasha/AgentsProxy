package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	platformauth "github.com/davinakmalyasha/agents-proxy/internal/agency/auth"
)

// Store owns auth reads/writes.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore builds an auth Store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func int64Ptr(n pgtype.Int8) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	v := t.String
	return &v
}

// User is the authenticated profile.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Role     string `json:"role"`
}

// LoginResult carries the user plus the session material.
type LoginResult struct {
	User         User
	TokenVersion int
}

// FindForLogin returns the user row for credential checking, or pgx.ErrNoRows.
func (s *Store) FindForLogin(ctx context.Context, email string) (User, string, int, *time.Time, error) {
	var (
		u       User
		hash    string
		version int
		deleted pgtype.Timestamptz
	)
	err := s.pool.QueryRow(ctx, `SELECT id, username, email, password_hash, role, token_version, deleted_at
		FROM users WHERE email = $1`, NormalizeEmail(email),
	).Scan(&u.ID, &u.Username, &u.Email, &hash, &u.Role, &version, &deleted)
	if err != nil {
		return User{}, "", 0, nil, err
	}
	var deletedAt *time.Time
	if deleted.Valid {
		v := deleted.Time
		deletedAt = &v
	}
	return u, hash, version, deletedAt, nil
}

// FindForLoginIdentity resolves email or username to the credential row,
// or pgx.ErrNoRows. Emails match case-insensitively, usernames exactly.
func (s *Store) FindForLoginIdentity(ctx context.Context, identity string) (User, string, int, *time.Time, error) {
	identity = strings.TrimSpace(identity)
	if strings.Contains(identity, "@") {
		return s.FindForLogin(ctx, identity)
	}
	var (
		u       User
		hash    string
		version int
		deleted pgtype.Timestamptz
	)
	err := s.pool.QueryRow(ctx, `SELECT id, username, email, password_hash, role, token_version, deleted_at
		FROM users WHERE username = $1`, identity,
	).Scan(&u.ID, &u.Username, &u.Email, &hash, &u.Role, &version, &deleted)
	if err != nil {
		return User{}, "", 0, nil, err
	}
	var deletedAt *time.Time
	if deleted.Valid {
		v := deleted.Time
		deletedAt = &v
	}
	return u, hash, version, deletedAt, nil
}

// BumpTokenVersion invalidates every session for the user.
func (s *Store) BumpTokenVersion(ctx context.Context, userID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET token_version = token_version + 1 WHERE id = $1`, userID)
	return err
}

// GetProfile returns the public profile, or pgx.ErrNoRows.
func (s *Store) GetProfile(ctx context.Context, userID int64) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `SELECT id, username, email, role FROM users
		WHERE id = $1 AND deleted_at IS NULL`, userID).Scan(&u.ID, &u.Username, &u.Email, &u.Role)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// OTP policy: 6 digits, 10-minute life, 5 attempts, single use.
const (
	OTPLength      = 6
	OTPTTL         = 10 * time.Minute
	OTPMaxAttempts = 5
)

// HashOTPCode hashes a plaintext code for storage (SHA-256 hex).
func HashOTPCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// CreateLoginOTP stores a code hash and invalidates older unused codes.
func (s *Store) CreateLoginOTP(ctx context.Context, userID int64, codeHash string, ttl time.Duration) error {
	_, err := s.pool.Exec(ctx, `UPDATE login_otps SET used_at = now()
		WHERE user_id = $1 AND used_at IS NULL`, userID)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO login_otps (user_id, code_hash, expires_at)
		VALUES ($1, $2, now() + ($3::text)::interval)`, userID, codeHash, ttl.String())
	return err
}

// VerifyLoginOTP checks the code, burning one attempt per call. Returns
// (ok, locked) where locked means attempts exhausted — the caller maps it
// to 429 without revealing whether the code was close.
func (s *Store) VerifyLoginOTP(ctx context.Context, userID int64, code string) (bool, bool, error) {
	var (
		id       int64
		hash     string
		attempts int
		expiry   time.Time
		used     pgtype.Timestamptz
	)
	err := s.pool.QueryRow(ctx, `SELECT id, code_hash, attempts, expires_at, used_at
		FROM login_otps WHERE user_id = $1 AND used_at IS NULL
		ORDER BY created_at DESC LIMIT 1`, userID,
	).Scan(&id, &hash, &attempts, &expiry, &used)
	if err != nil {
		return false, false, err
	}
	if used.Valid || time.Now().After(expiry) {
		return false, false, nil
	}
	if attempts >= OTPMaxAttempts {
		return false, true, nil
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(HashOTPCode(code))) != 1 {
		_, err = s.pool.Exec(ctx, `UPDATE login_otps SET attempts = attempts + 1 WHERE id = $1`, id)
		if err != nil {
			return false, false, err
		}
		if attempts+1 >= OTPMaxAttempts {
			return false, true, nil
		}
		return false, false, nil
	}
	_, err = s.pool.Exec(ctx, `UPDATE login_otps SET used_at = now(), attempts = attempts + 1 WHERE id = $1`, id)
	if err != nil {
		return false, false, err
	}
	return true, false, nil
}

// FindByGoogleSub returns the user linked to a Google subject, or
// pgx.ErrNoRows.
func (s *Store) FindByGoogleSub(ctx context.Context, sub string) (User, int, error) {
	var (
		u       User
		version int
	)
	err := s.pool.QueryRow(ctx, `SELECT id, username, email, role, token_version
		FROM users WHERE google_sub = $1 AND deleted_at IS NULL`, sub,
	).Scan(&u.ID, &u.Username, &u.Email, &u.Role, &version)
	if err != nil {
		return User{}, 0, err
	}
	return u, version, nil
}

// LinkGoogleSub binds a Google subject to an existing user.
func (s *Store) LinkGoogleSub(ctx context.Context, userID int64, sub string) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET google_sub = $1 WHERE id = $2 AND deleted_at IS NULL`, sub, userID)
	return err
}

// FindByEmail returns the login row for a normalized email, or
// pgx.ErrNoRows. Used to link Google sign-ins to existing accounts.
func (s *Store) FindByEmail(ctx context.Context, email string) (User, int, error) {
	u, hash, version, deletedAt, err := s.FindForLogin(ctx, email)
	if err != nil {
		return User{}, 0, err
	}
	_ = hash
	if deletedAt != nil {
		return User{}, 0, pgx.ErrNoRows
	}
	return u, version, nil
}

// CreateClientUser inserts a JIT-provisioned CLIENT from Google sign-in.
// The password is an unusable placeholder: Google is the credential.
func (s *Store) CreateClientUser(ctx context.Context, username, email, sub string) (User, int, error) {
	hash, err := platformauth.HashPassword("unusable-" + username)
	if err != nil {
		return User{}, 0, err
	}
	var (
		u       User
		version int
	)
	err = s.pool.QueryRow(ctx, `INSERT INTO users (username, email, password_hash, role, google_sub)
		VALUES ($1, $2, $3, 'CLIENT', $4)
		RETURNING id, username, email, role, token_version`,
		username, NormalizeEmail(email), hash, sub,
	).Scan(&u.ID, &u.Username, &u.Email, &u.Role, &version)
	if err != nil {
		return User{}, 0, err
	}
	return u, version, nil
}

// UsernameTaken reports whether a username is already claimed.
func (s *Store) UsernameTaken(ctx context.Context, username string) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE username = $1`, username).Scan(&n)
	return n > 0, err
}

// SignupRequest is a pending email-verified client signup.
type SignupRequest struct {
	ID        int64
	Username  string
	Email     string
	ExpiresAt time.Time
	Attempts  int
}

// CreateSignup stores (or refreshes) a pending signup and returns its id.
// The email must not already belong to a user. A previous pending row for
// the same email is replaced, invalidating its old code.
func (s *Store) CreateSignup(ctx context.Context, username, email, passwordHash, codeHash string, ttl time.Duration) (int64, error) {
	email = NormalizeEmail(email)
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM users WHERE email = $1 AND deleted_at IS NULL)`, email,
	).Scan(&exists); err != nil {
		return 0, err
	}
	if exists {
		return 0, errEmailTaken
	}
	// Opportunistic hygiene: dropped-off pending signups are worthless.
	_, _ = s.pool.Exec(ctx, `DELETE FROM signup_requests
		WHERE created_at < now() - interval '24 hours'`)

	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO signup_requests
		(username, email, password_hash, code_hash, expires_at)
		VALUES ($1,$2,$3,$4, now() + ($5::text)::interval)
		ON CONFLICT (email) DO UPDATE SET
			username = EXCLUDED.username,
			password_hash = EXCLUDED.password_hash,
			code_hash = EXCLUDED.code_hash,
			attempts = 0,
			expires_at = EXCLUDED.expires_at,
			consumed_at = NULL,
			created_at = now()
		RETURNING id`,
		strings.TrimSpace(username), email, passwordHash, codeHash, ttl.String(),
	).Scan(&id)
	return id, err
}

// VerifySignup checks a code and atomically creates the CLIENT. It returns
// (user, tokenVersion, ok, locked). A username race is resolved by trying
// deterministic suffixes inside the same transaction.
func (s *Store) VerifySignup(ctx context.Context, signupID int64, code string) (User, int, bool, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, 0, false, false, err
	}
	defer tx.Rollback(ctx)

	var (
		username string
		email    string
		hash     string
		attempts int
		expiry   time.Time
		consumed pgtype.Timestamptz
	)
	err = tx.QueryRow(ctx, `SELECT username, email, code_hash, attempts, expires_at, consumed_at
		FROM signup_requests WHERE id = $1 FOR UPDATE`, signupID,
	).Scan(&username, &email, &hash, &attempts, &expiry, &consumed)
	if err != nil {
		return User{}, 0, false, false, err
	}
	if consumed.Valid || time.Now().After(expiry) {
		return User{}, 0, false, false, nil
	}
	if attempts >= OTPMaxAttempts {
		return User{}, 0, false, true, nil
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(HashOTPCode(code))) != 1 {
		if _, err := tx.Exec(ctx, `UPDATE signup_requests SET attempts = attempts + 1 WHERE id = $1`, signupID); err != nil {
			return User{}, 0, false, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return User{}, 0, false, false, err
		}
		if attempts+1 >= OTPMaxAttempts {
			return User{}, 0, false, true, nil
		}
		return User{}, 0, false, false, nil
	}

	// The email could have been claimed after registration (admin create,
	// another signup). Fail cleanly instead of relying on a raw 23505.
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM users WHERE email = $1 AND deleted_at IS NULL)`, email,
	).Scan(&taken); err != nil {
		return User{}, 0, false, false, err
	}
	if taken {
		return User{}, 0, false, false, errEmailTaken
	}

	var (
		u       User
		version int
	)
	base := strings.TrimSpace(username)
	if base == "" {
		base = "client"
	}
	for i := 0; i < 50; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s%d", base, i+1)
		}
		err = tx.QueryRow(ctx, `INSERT INTO users (username, email, password_hash, role)
			VALUES ($1,$2,$3,'CLIENT')
			ON CONFLICT DO NOTHING
			RETURNING id, username, email, role, token_version`,
			candidate, email, hash,
		).Scan(&u.ID, &u.Username, &u.Email, &u.Role, &version)
		if err == nil {
			break
		}
		if err != pgx.ErrNoRows {
			return User{}, 0, false, false, err
		}
		// Username collision: try the next suffix.
	}
	if err != nil {
		return User{}, 0, false, false, fmt.Errorf("signup: no free username for %q", base)
	}

	if _, err := tx.Exec(ctx, `UPDATE signup_requests SET consumed_at = now(), attempts = attempts + 1
		WHERE id = $1`, signupID); err != nil {
		return User{}, 0, false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, 0, false, false, err
	}
	return u, version, true, false, nil
}

// ResendSignup rotates the code for a pending signup and returns its email.
func (s *Store) ResendSignup(ctx context.Context, signupID int64, codeHash string, ttl time.Duration) (string, error) {
	var email string
	err := s.pool.QueryRow(ctx, `UPDATE signup_requests
		SET code_hash = $1, attempts = 0, expires_at = now() + ($3::text)::interval
		WHERE id = $2 AND consumed_at IS NULL
		RETURNING email`, codeHash, signupID, ttl.String(),
	).Scan(&email)
	return email, err
}

// ChangePassword verifies the current password, sets the new hash, bumps the
// token version so other sessions die, and returns the new version.
func (s *Store) ChangePassword(ctx context.Context, userID int64, currentPassword, newHash string) (int, error) {
	var hash string
	if err := s.pool.QueryRow(ctx, `SELECT password_hash FROM users
		WHERE id = $1 AND deleted_at IS NULL`, userID).Scan(&hash); err != nil {
		return 0, err
	}
	if !SecureCompare(currentPassword, hash) {
		return 0, errWrongPassword
	}
	var version int
	err := s.pool.QueryRow(ctx, `UPDATE users SET password_hash = $1, token_version = token_version + 1
		WHERE id = $2 RETURNING token_version`, newHash, userID).Scan(&version)
	return version, err
}

// TokenVersion returns the current session version for re-issue.
func (s *Store) TokenVersion(ctx context.Context, userID int64) (int, error) {
	var v int
	err := s.pool.QueryRow(ctx, `SELECT token_version FROM users WHERE id = $1 AND deleted_at IS NULL`, userID).Scan(&v)
	return v, err
}

// errWrongPassword distinguishes bad credentials from missing rows.
var errWrongPassword = errSentinel("wrong password")

// errEmailTaken means an email already belongs to an active user.
var errEmailTaken = errSentinel("email already registered")

// errGoogleStaff means a verified Google email matches a staff account.
var errGoogleStaff = errSentinel("google: staff account must use password")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }
