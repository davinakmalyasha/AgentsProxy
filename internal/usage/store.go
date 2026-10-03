// DONE: PRD 6.1, 6.2 (usage row + quota cache incr) / STANDARDS A
package usage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// Record inserts a usage row (synchronous, source of truth) and
// best-effort increments the monthly quota cache in Redis.
func Record(ctx context.Context, db *pgxpool.Pool, rdb *redis.Client, clientID, model, kind string, tokensIn, tokensOut int) error {
	costUSD := EstimateCost(tokensIn, tokensOut)
	if _, err := db.Exec(ctx,
		`INSERT INTO usage (client_id, tokens_in, tokens_out, cost_usd, model, kind) VALUES ($1, $2, $3, $4, $5, $6)`,
		clientID, tokensIn, tokensOut, costUSD, model, kind,
	); err != nil {
		return fmt.Errorf("insert usage: %w", err)
	}
	if rdb != nil {
		monthKey := time.Now().UTC().Format("200601")
		key := "quota:" + clientID + ":" + monthKey
		n, err := rdb.IncrBy(ctx, key, int64(tokensIn+tokensOut)).Result()
		if err != nil {
			slog.Warn("quota cache incr failed", "err", err, "client_id", clientID)
		} else if n == int64(tokensIn+tokensOut) {
			// First increment created the key — bound its life to the
			// month window so abandoned clients can't leak keys forever.
			if err := rdb.Expire(ctx, key, 31*24*time.Hour).Err(); err != nil {
				slog.Warn("quota cache expire failed", "err", err, "client_id", clientID)
			}
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
