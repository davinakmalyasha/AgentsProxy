// Package tgvault encrypts per-bot Telegram tokens at rest.
//
// Scale answer: thousands of bot tokens live in Postgres (AES-256-GCM),
// exactly ONE master key lives in env (TELEGRAM_VAULT_KEY, 64 hex chars).
// Rotation is supported via key versions (see KeyRing).
//
// DONE: T2 (vault)
package tgvault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// Version 1 wire format: 0x01 || nonce(12) || ciphertext.
const Version byte = 1

// MasterKeyFromEnv parses TELEGRAM_VAULT_KEY (64 hex chars = 32 bytes).
// Documented in .env.example; the operator generates it once.
func MasterKeyFromEnv() ([]byte, error) {
	s := strings.TrimSpace(os.Getenv("TELEGRAM_VAULT_KEY"))
	if s == "" {
		return nil, fmt.Errorf("TELEGRAM_VAULT_KEY not set")
	}
	key, err := hex.DecodeString(s)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("TELEGRAM_VAULT_KEY must be 64 hex chars (32 bytes)")
	}
	return key, nil
}

// Encrypt seals plaintext with key (AES-256-GCM, random nonce).
func Encrypt(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+gcm.Overhead())
	out = append(out, Version)
	out = append(out, nonce...)
	out = gcm.Seal(out, nonce, plaintext, nil)
	return out, nil
}

// Decrypt opens a blob produced by Encrypt. Wrong key, tampered bytes,
// or unknown version all fail closed.
func Decrypt(key, blob []byte) ([]byte, error) {
	if len(blob) < 1 || blob[0] != Version {
		return nil, fmt.Errorf("unknown vault version")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	n := gcm.NonceSize()
	if len(blob) < 1+n {
		return nil, fmt.Errorf("vault blob too short")
	}
	return gcm.Open(nil, blob[1:1+n], blob[1+n:], nil)
}

// MaskToken renders "123456:ABC..." as "123456:***" for logs/API output.
// Raw tokens must never leave the decrypt-send path.
func MaskToken(tok string) string {
	tok = strings.TrimSpace(tok)
	if i := strings.Index(tok, ":"); i > 0 {
		return tok[:i] + ":***"
	}
	if tok == "" {
		return ""
	}
	return "***"
}
