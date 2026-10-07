// DONE: PRD 6.1, 6.3 (GenerateToken, HashToken, ConstantTimeCompare) / STANDARDS D
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
)

const TokenHexLen = 64

// GenerateToken returns (rawToken, sha256Hex(rawToken)).
// rawToken is shown to the admin ONCE; only the hash is stored.
func GenerateToken() (string, string, error) {
	b := make([]byte, TokenHexLen/2)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	tok := hex.EncodeToString(b)
	return tok, HashToken(tok), nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
