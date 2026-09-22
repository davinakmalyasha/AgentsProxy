package auth

import (
	"strings"
	"testing"
)

func TestGenerateToken_Shape(t *testing.T) {
	tok, hash, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != TokenHexLen {
		t.Errorf("token len = %d want %d", len(tok), TokenHexLen)
	}
	if len(hash) != 64 {
		t.Errorf("hash len = %d want 64", len(hash))
	}
	if strings.ToLower(tok) != tok {
		t.Errorf("token not lowercase hex: %s", tok)
	}
}

func TestGenerateToken_Unique(t *testing.T) {
	a, _, _ := GenerateToken()
	b, _, _ := GenerateToken()
	if a == b {
		t.Error("two tokens collided")
	}
}

func TestHashToken_Deterministic(t *testing.T) {
	if HashToken("abc") != HashToken("abc") {
		t.Error("hash not deterministic")
	}
	if HashToken("abc") == HashToken("abd") {
		t.Error("hash collision on different input")
	}
}

func TestEqual(t *testing.T) {
	if !Equal("hello", "hello") {
		t.Error("Equal same")
	}
	if Equal("hello", "world") {
		t.Error("Equal different")
	}
}
