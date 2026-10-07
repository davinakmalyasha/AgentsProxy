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

// Record inserts a usage row (synchronous, source of truth) and
// best-effort increments the monthly quota cache in Redis.
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
		`INSERT INTO usage (client_id, tokens_in, tokens_out, cost_usd, model, kind) VALUES ($1, $2, $3, $4, $5, $6)`,
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

// EstimateCost applies a fixed per-million-token price. Documented as
// an estimate, not the provider bill. Shared with the /chat door, which
// reports the same figure back to the caller: one formula, one place to
// update when pricing changes.
func EstimateCost(in, out int) float64 {
	// DeepSeek flash-ish: 0.14 / 0.28 per 1M.
	return float64(in)/1e6*0.14 + float64(out)/1e6*0.28
}
