package agentstools

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// MoneyCap is the per-tenant daily LLM-call cap (Centrum's guard).
const MoneyCap = 150

// CapRDB is the Redis client used by the cost cap. Set once in
// main.go wiring; tests leave it nil (no cap).
var CapRDB *redis.Client

// CapKey returns the Redis key for a tenant's daily call count.
func CapKey(tenantID string) string {
	return "agcap:" + tenantID + ":" + time.Now().Format("2006-01-02")
}

// CostCapAllowed checks (and increments) the per-tenant daily cap.
// Returns false when the tenant has hit the cap. Legacy global check.
func CostCapAllowed(ctx context.Context, tenantID string) bool {
	return CostCapAllowedForRole(ctx, tenantID, "owner")
}

// CostCapAllowedForRole checks per-tenant+role daily cap (20/80/150). Single LLM_API_KEY shared.
func CostCapAllowedForRole(ctx context.Context, tenantID, role string) bool {
	if CapRDB == nil {
		return true
	}
	cap := MoneyCap
	switch role {
	case "client":
		cap = 20
	case "pegawai":
		cap = 80
	case "owner":
		cap = 150
	}
	key := CapKeyForRole(tenantID, role)
	n, err := CapRDB.Incr(ctx, key).Result()
	if err != nil {
		return true
	}
	if n == 1 {
		CapRDB.Expire(ctx, key, 24*time.Hour)
	}
	// Also increment legacy global key for backward compat / total view
	CapRDB.Incr(ctx, CapKey(tenantID))
	return n <= int64(cap)
}

// CapKeyForRole is the per-role key: agcap:{tenant}:{role}:{date}
func CapKeyForRole(tenantID, role string) string {
	return "agcap:" + tenantID + ":" + role + ":" + time.Now().Format("2006-01-02")
}

// Per-person RPD: agcap:{tenant}:{role}:{user}:{date}
func RPDKeyForUser(tenantID, role, userID string) string {
	if userID == "" {
		userID = "anon"
	}
	return "rpd:" + tenantID + ":" + role + ":" + userID + ":" + time.Now().Format("2006-01-02")
}

// RPDCapAllowedForUser checks per-person daily cap (client 30, pegawai 80, owner 150)
// Returns false when the person has hit the cap.
func RPDCapAllowedForUser(ctx context.Context, tenantID, role, userID string) bool {
	if CapRDB == nil {
		return true
	}
	cap := 150
	switch role {
	case "client":
		cap = 30
	case "pegawai":
		cap = 80
	case "owner":
		cap = 150
	}
	key := RPDKeyForUser(tenantID, role, userID)
	n, err := CapRDB.Incr(ctx, key).Result()
	if err != nil {
		return true
	}
	if n == 1 {
		CapRDB.Expire(ctx, key, 24*time.Hour)
	}
	return n <= int64(cap)
}

// Hardcoded owner contact for abuse fallback
const OwnerContact = "0815-9408-136 (Owner)"
const HardcodedAbuseReply = "Maaf, batas tanya harian habis. Silakan hubungi nomor berikut untuk info lebih lanjut: 0815-9408-136 (Owner)."
const HardcodedScopeReply = "Maaf, saya hanya bisa bantu seputar Centrum Badminton Arena (jadwal, harga, booking, fasilitas). Silakan hubungi nomor berikut untuk info lebih lanjut: 0815-9408-136 (Owner)."
