package agentstools

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
)

// Per-role daily LLM-call caps, set from config at boot (main.go).
// They guard the single shared LLM_API_KEY: tunable per deploy
// without recompiling. Defaults match the historical switches.
var (
	CapCallsClient = 20
	CapCallsStaff  = 80
	CapCallsOwner  = 150
	RPDCallsClient = 30
	RPDCallsStaff  = 80
	RPDCallsOwner  = 150
)

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
	cap := CapCallsOwner
	switch role {
	case "client":
		cap = CapCallsClient
	case "pegawai":
		cap = CapCallsStaff
	case "owner":
		cap = CapCallsOwner
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
	cap := RPDCallsOwner
	switch role {
	case "client":
		cap = RPDCallsClient
	case "pegawai":
		cap = RPDCallsStaff
	case "owner":
		cap = RPDCallsOwner
	}
	key := RPDKeyForUser(tenantID, role, userID)
	n, err := redisx.IncrExpireRun(ctx, CapRDB, key, capTTL())
	if err != nil {
		slog.Error("rpd cap redis unavailable, denying", "tenant", tenantID, "role", role, "err", err)
		return false
	}
	return n <= int64(cap)
}

// CapsAllowed checks both the per-person RPD and the per-tenant cost cap
// in one Redis round trip (pipelined Lua). It preserves the exact
// fail-closed semantics of the two sequential checks: any Redis error
// denies, and each counter is compared against its own cap. The TTL is
// computed once so both keys share the same midnight anchor.
func CapsAllowed(ctx context.Context, tenantID, role, userID string) (rpdOK, costOK bool) {
	if CapRDB == nil {
		return true, true
	}
	rpdCap, costCap := RPDCallsOwner, CapCallsOwner
	switch role {
	case "client":
		rpdCap, costCap = RPDCallsClient, CapCallsClient
	case "pegawai":
		rpdCap, costCap = RPDCallsStaff, CapCallsStaff
	case "owner":
		rpdCap, costCap = RPDCallsOwner, CapCallsOwner
	}
	ttl := capTTL()
	day := WibNow().Format("2006-01-02")
	rpdKey := "rpd:" + tenantID + ":" + role + ":" + firstNonEmpty(userID, "anon") + ":" + day
	costKey := "agcap:" + tenantID + ":" + role + ":" + day
	nRPD, nCost, err := redisx.IncrExpireTwo(ctx, CapRDB, rpdKey, ttl, costKey, ttl)
	if err != nil {
		slog.Error("caps redis unavailable, denying", "tenant", tenantID, "role", role, "err", err)
		return false, false
	}
	return nRPD <= int64(rpdCap), nCost <= int64(costCap)
}

func firstNonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
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
