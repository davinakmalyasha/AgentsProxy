// DONE: PRD 5 (quota:{id}:{month} cached, SUM fallback) / STANDARDS A, B
package quota

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
func Check(ctx context.Context, db *pgxpool.Pool, rdb *redis.Client, clientID string, limit int) (int, time.Time, error) {
	now := time.Now().UTC()
	monthKey := now.Format("200601")
	rkey := keyPrefix + clientID + ":" + monthKey

	var used int
	if v, err := rdb.Get(ctx, rkey).Result(); err == nil {
		used, _ = strconv.Atoi(v)
	} else {
		// Miss → DB SUM, then SETEX.
		used, err = sumMonthUsage(ctx, db, clientID)
		if err != nil {
			return 0, time.Time{}, fmt.Errorf("quota sum: %w", err)
		}
		_ = rdb.Set(ctx, rkey, used, cacheTTL).Err()
	}

	resetAt := nextMonthStart(now)
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
