// Package auth implements human authentication: cookie login/logout/me,
// password change, email OTP second step, and Google sign-in.
package auth

import (
	"strings"

	"golang.org/x/crypto/bcrypt"

	platformauth "github.com/davinakmalyasha/agents-proxy/internal/agency/auth"
)

// NormalizeEmail lowercases and trims for case-insensitive uniqueness.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// dummyHash burns comparable time when the user does not exist, preventing
// user-enumeration via timing.
const dummyHash = "$2a$12$C6UzMDM.H6dfI/f/IKcEeO7lT2h3qTZ0h6BQ2X9QrHtVJ2l8r0v0u"

// SecureCompare checks the password, or burns time and fails when hash is "".
func SecureCompare(password, hash string) bool {
	target := hash
	if target == "" {
		target = dummyHash
	}
	ok := bcrypt.CompareHashAndPassword([]byte(target), []byte(password)) == nil
	return ok && hash != ""
}

// ValidPassword delegates to the shared policy (agency/auth).
func ValidPassword(password string) bool { return platformauth.ValidPassword(password) }
