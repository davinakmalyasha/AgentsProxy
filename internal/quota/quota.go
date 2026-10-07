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
	keyPrefix = "quota:"
)

// MonthKey is the single source for the monthly quota counter key.
// usage.Record writes what Check reads: a rename here can never split
// them silently again.
func MonthKey(clientID string) string {
	return keyPrefix + clientID + ":" + time.Now().UTC().Format("200601")
}

// MonthExpiry is the absolute expiry for a monthly counter: next month
// start + 1 day grace, so a key always dies shortly after its month.
func MonthExpiry() int64 {
	now := time.Now().UTC()
	return nextMonthStart(now).Add(24 * time.Hour).Unix()
}

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
	rkey := MonthKey(clientID)
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
		// Repopulate with the SAME month-anchored expiry the counter
		// writes use. The old 30s TTL here meant a repopulated key
		// silently became a 30-second key and the next request re-ran
		// the full month SUM.
		ttl := time.Until(nextMonthStart(now).Add(24 * time.Hour))
		if ttl < time.Minute {
			ttl = time.Minute
		}
		if err := rdb.Set(ctx, rkey, used, ttl).Err(); err != nil {
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
	// Definer path (migration 053): usage is unreadable to app_runtime
	// by policy, so the month total comes from a function that can only
	// ever see this one client's rows.
	var used int
	err := db.QueryRow(ctx, `SELECT private.month_tokens($1::uuid)`, clientID).Scan(&used)
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
