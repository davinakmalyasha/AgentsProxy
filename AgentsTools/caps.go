package agentstools

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// MoneyCap is the per-tenant daily LLM-call cap for the owner tier.
const MoneyCap = 150

// CapRDB is the Redis client used by the cost cap. Set once in
// main.go wiring; tests leave it nil (no cap).
var CapRDB *redis.Client

// CapKey returns the Redis key for a tenant's daily call count.
func CapKey(tenantID string) string {
	return "agcap:" + tenantID + ":" + time.Now().Format("2006-01-02")
}

// CostCapAllowedForRole checks per-tenant+role daily cap (20/80/150). Single LLM_API_KEY shared.
//
// Fail-closed: a Redis error denies, because this guards money (the shared
// key). Unlimited spend on an outage is worse than blocked chat. The nil-RDB
// bypass is test-harness only; production always wires CapRDB in main.go.
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
		slog.Error("cost cap redis unavailable, denying", "tenant", tenantID, "role", role, "err", err)
		return false
	}
	if n == 1 {
		if err := CapRDB.Expire(ctx, key, 24*time.Hour).Err(); err != nil {
			slog.Warn("cost cap expire", "tenant", tenantID, "role", role, "err", err)
		}
	}
	// Also increment legacy global key for backward compat / total view
	if err := CapRDB.Incr(ctx, CapKey(tenantID)).Err(); err != nil {
		slog.Warn("cost cap legacy incr", "tenant", tenantID, "err", err)
	}
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
// Returns false when the person has hit the cap -- or when Redis is
// unavailable (fail-closed, same money reasoning as CostCapAllowedForRole).
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
		slog.Error("rpd cap redis unavailable, denying", "tenant", tenantID, "role", role, "err", err)
		return false
	}
	if n == 1 {
		if err := CapRDB.Expire(ctx, key, 24*time.Hour).Err(); err != nil {
			slog.Warn("rpd cap expire", "tenant", tenantID, "role", role, "err", err)
		}
	}
	return n <= int64(cap)
}

// DefaultOwnerContact is the fallback when a tenant names no contact in
// its project.json. It is deliberately NOT a real person's number: a tenant
// without its own contact must never route its visitors to another tenant.
// Every tenant sets its own contact in memory/project.json.
const DefaultOwnerContact = "kontak resmi venue"

// ScopeReplyFor is the client-tier refusal for out-of-scope messages,
// addressed to the tenant's own venue and contact. With the centrum
// values this renders byte-identical to the old hardcoded reply, which
// the isolation tests lock in.
func ScopeReplyFor(display, contact string) string {
	if display == "" {
		display = "this venue"
	}
	if contact == "" {
		contact = DefaultOwnerContact
	}
	return "Maaf, saya hanya bisa bantu seputar " + display +
		" (jadwal, harga, booking, fasilitas). Silakan hubungi nomor berikut untuk info lebih lanjut: " +
		contact + "."
}

// AbuseReplyFor is the client-tier reply when the daily question budget
// is spent, pointing at the tenant's own contact.
func AbuseReplyFor(contact string) string {
	if contact == "" {
		contact = DefaultOwnerContact
	}
	return "Maaf, batas tanya harian habis. Silakan hubungi nomor berikut untuk info lebih lanjut: " +
		contact + "."
}
