package tgvault

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestRoundtrip(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	blob, err := Encrypt(key, []byte("123456:AAH-secret"))
	if err != nil {
		t.Fatal(err)
	}
	// Nonces must differ per encryption.
	blob2, _ := Encrypt(key, []byte("123456:AAH-secret"))
	if bytes.Equal(blob, blob2) {
		t.Fatal("deterministic nonce reuse")
	}
	got, err := Decrypt(key, blob)
	if err != nil || string(got) != "123456:AAH-secret" {
		t.Fatalf("roundtrip: %q %v", got, err)
	}
}

func TestFailClosed(t *testing.T) {
	key := make([]byte, 32)
	other := make([]byte, 32)
	other[0] = 1
	blob, _ := Encrypt(key, []byte("x"))
	if _, err := Decrypt(other, blob); err == nil {
		t.Fatal("wrong key must fail")
	}
	tampered := append([]byte{}, blob...)
	tampered[len(tampered)-1] ^= 1
	if _, err := Decrypt(key, tampered); err == nil {
		t.Fatal("tampered blob must fail")
	}
	if _, err := Decrypt(key, []byte{0xFF}); err == nil {
		t.Fatal("bad version must fail")
	}
}

func TestMaskToken(t *testing.T) {
	if MaskToken("123:ABCDEF") != "123:***" {
		t.Fatal("mask failed")
	}
	if MaskToken("") != "" {
		t.Fatal("empty failed")
	}
}
