package agentstools

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
)

// MoneyCap is the per-tenant daily LLM-call cap for the owner tier.
const MoneyCap = 150

// CapRDB is the Redis client used by the cost cap. Set once in
// main.go wiring; tests leave it nil (no cap).
var CapRDB *redis.Client

// capTTL anchors a daily counter at next WIB midnight + 1h grace.
// 24h-from-creation (the old TTL) lets a 23:59 key count yesterday's
// spend for 24 more hours; midnight-anchored is both shorter and
// semantically correct.
func capTTL() time.Duration {
	now := WibNow()
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	return midnight.Add(time.Hour).Sub(now)
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
	// Atomic INCR + creator-set EXPIRE: the old split had a crash
	// window that left immortal counters capping permanently.
	n, err := redisx.IncrExpireRun(ctx, CapRDB, key, capTTL())
	if err != nil {
		slog.Error("cost cap redis unavailable, denying", "tenant", tenantID, "role", role, "err", err)
		return false
	}
	return n <= int64(cap)
}

// CapKeyForRole is the per-role key: agcap:{tenant}:{role}:{date} (WIB day).
func CapKeyForRole(tenantID, role string) string {
	return "agcap:" + tenantID + ":" + role + ":" + WibNow().Format("2006-01-02")
}

// Per-person RPD: agcap:{tenant}:{role}:{user}:{date} (WIB day).
func RPDKeyForUser(tenantID, role, userID string) string {
	if userID == "" {
		userID = "anon"
	}
	return "rpd:" + tenantID + ":" + role + ":" + userID + ":" + WibNow().Format("2006-01-02")
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
	n, err := redisx.IncrExpireRun(ctx, CapRDB, key, capTTL())
	if err != nil {
		slog.Error("rpd cap redis unavailable, denying", "tenant", tenantID, "role", role, "err", err)
		return false
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
