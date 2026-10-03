// DONE: PRD 5 (quota:{id}:{month} cached, SUM fallback) / STANDARDS A, B
package quota

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	cacheTTL  = 30 * time.Second
	keyPrefix = "quota:"
)

type QuotaExceededError struct {
	ResetAt time.Time
	Used    int
	Limit   int
}

func (e *QuotaExceededError) Error() string {
	return fmt.Sprintf("quota exceeded: used=%d limit=%d reset_at=%s", e.Used, e.Limit, e.ResetAt.Format(time.RFC3339))
}

// Check returns (remaining, resetAt, error). When exceeded, the
// returned error is *QuotaExceededError so handlers can surface the
// resetAt to clients.
//
// Fail-closed on the money side: a corrupt cache value falls back to the
// DB SUM (never treated as zero usage), and a DB failure denies. A
// non-positive limit denies outright — quota math must never grant.
func Check(ctx context.Context, db *pgxpool.Pool, rdb *redis.Client, clientID string, limit int) (int, time.Time, error) {
	now := time.Now().UTC()
	monthKey := now.Format("200601")
	rkey := keyPrefix + clientID + ":" + monthKey
	resetAt := nextMonthStart(now)

	if limit <= 0 {
		return 0, resetAt, &QuotaExceededError{ResetAt: resetAt, Used: 0, Limit: limit}
	}

	var used int
	fromCache := false
	v, gerr := rdb.Get(ctx, rkey).Result()
	switch {
	case gerr == nil:
		n, convErr := strconv.Atoi(strings.TrimSpace(v))
		if convErr != nil || n < 0 {
			// Corrupt cache value: never treat as zero usage. Fall
			// through to the DB SUM, same as a miss.
			slog.Warn("quota cache corrupt, refetching from DB", "client_id", clientID, "value", v, "err", convErr)
		} else {
			used, fromCache = n, true
		}
	case errors.Is(gerr, redis.Nil):
		// Normal miss: fall through to the DB SUM below.
		slog.Debug("quota cache miss", "client_id", clientID)
	default:
		slog.Warn("quota cache unavailable, using DB", "client_id", clientID, "err", gerr)
	}
	if !fromCache {
		var err error
		used, err = sumMonthUsage(ctx, db, clientID)
		if err != nil {
			slog.Error("quota sum failed, denying", "client_id", clientID, "err", err)
			return 0, time.Time{}, fmt.Errorf("quota sum: %w", err)
		}
		if used < 0 {
			used = 0
		}
		if err := rdb.Set(ctx, rkey, used, cacheTTL).Err(); err != nil {
			slog.Warn("quota cache repopulate", "client_id", clientID, "err", err)
		}
	}

	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	if used >= limit {
		return 0, resetAt, &QuotaExceededError{ResetAt: resetAt, Used: used, Limit: limit}
	}
	return remaining, resetAt, nil
}

func sumMonthUsage(ctx context.Context, db *pgxpool.Pool, clientID string) (int, error) {
	var used int
	err := db.QueryRow(ctx,
		`SELECT COALESCE(SUM(tokens_in + tokens_out), 0)::int FROM usage WHERE client_id = $1 AND created_at >= date_trunc('month', now())`,
		clientID,
	).Scan(&used)
	if err != nil {
		return 0, err
	}
	return used, nil
}

func nextMonthStart(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}

// IsExceeded reports whether err is a QuotaExceededError.
func IsExceeded(err error) bool {
	var qe *QuotaExceededError
	return errors.As(err, &qe)
}
