// DONE: PRD 6.1, 6.2 (usage row + quota cache incr) / STANDARDS A
package usage

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/davinakmalyasha/agents-proxy/internal/quota"
	"github.com/davinakmalyasha/agents-proxy/internal/redisx"
)

// Per-million-token prices (USD), set from config at boot (main.go).
// The estimate reported to every tenant; a provider swap changes the
// price, and that must not require a recompile to keep billing honest.
var (
	CostInPerMtok  = 0.14
	CostOutPerMtok = 0.28
)

// Record inserts a usage row (synchronous, source of truth) and
// best-effort increments the monthly quota cache in Redis.
//
// The insert is one definer RPC (migration 089): the old BEGIN/set_config/
// INSERT/COMMIT sequence was four round trips for one row. The definer is
// owned by the role that may write usage and granted EXECUTE only to
// app_runtime; the FK and CHECK constraints still validate every value.
//
// The counter write is one atomic Lua call (INCRBY + creator-set
// EXPIREAT): the old read-the-result-then-EXPIRE split misdetected
// creation whenever the pre-existing value happened to equal the
// delta, leaking immortal keys — and the 30s-vs-31d dual TTL meant a
// repopulated key silently became a 30-second key, forcing a full
// month SUM on the next request.
func Record(ctx context.Context, db *pgxpool.Pool, rdb *redis.Client, clientID, model, kind string, tokensIn, tokensOut int) error {
	costUSD := EstimateCost(tokensIn, tokensOut)
	if _, err := db.Exec(ctx,
		`SELECT private.record_usage($1::uuid, $2, $3, $4, $5, $6)`,
		clientID, tokensIn, tokensOut, costUSD, model, kind,
	); err != nil {
		return fmt.Errorf("insert usage: %w", err)
	}
	if rdb != nil {
		if _, err := redisx.IncrByExpireAtRun(ctx, rdb,
			quota.MonthKey(clientID), int64(tokensIn+tokensOut), quota.MonthExpiry()); err != nil {
			slog.Warn("quota cache incr failed", "err", err, "client_id", clientID)
		}
	}
	return nil
}

// EstimateCost applies the configured per-million-token price.
// Documented as an estimate, not the provider bill. Shared with the
// /chat door, which reports the same figure back to the caller: one
// formula, one place to update when pricing changes.
func EstimateCost(in, out int) float64 {
	return float64(in)/1e6*CostInPerMtok + float64(out)/1e6*CostOutPerMtok
}
