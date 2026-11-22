package config

import (
	"strings"
	"testing"
)

func TestValidateServerSecretsRejectsWeakAdminKey(t *testing.T) {
	base := Config{AdminAPIKey: strings.Repeat("a", 32)}
	if err := base.ValidateServerSecrets(); err != nil {
		t.Fatalf("strong key rejected: %v", err)
	}
	weak := Config{AdminAPIKey: "short"}
	if err := weak.ValidateServerSecrets(); err == nil {
		t.Fatal("short ADMIN_API_KEY accepted")
	}
	placeholder := Config{AdminAPIKey: "REPLACE_ME_WITH_openssl_rand_hex_32"}
	if err := placeholder.ValidateServerSecrets(); err == nil {
		t.Fatal("placeholder ADMIN_API_KEY accepted")
	}
}

func TestValidateServerSecretsRequiresVercelPoolBoundary(t *testing.T) {
	t.Setenv("VERCEL_ENV", "production")
	ok := Config{
		AdminAPIKey:       strings.Repeat("a", 32),
		BackgroundTickers: false,
		DBSimpleProtocol:  true,
		DBPoolMin:         0,
		DBPoolMax:         2,
	}
	if err := ok.ValidateServerSecrets(); err != nil {
		t.Fatalf("valid Vercel config rejected: %v", err)
	}
	bad := ok
	bad.DBPoolMin = 2
	if err := bad.ValidateServerSecrets(); err == nil {
		t.Fatal("DB_POOL_MIN=2 accepted on Vercel")
	}
	bad = ok
	bad.BackgroundTickers = true
	if err := bad.ValidateServerSecrets(); err == nil {
		t.Fatal("BACKGROUND_TICKERS=true accepted on Vercel")
	}
	bad = ok
	bad.DBSimpleProtocol = false
	if err := bad.ValidateServerSecrets(); err == nil {
		t.Fatal("DB_USE_SIMPLE_PROTOCOL=false accepted on Vercel")
	}
}
