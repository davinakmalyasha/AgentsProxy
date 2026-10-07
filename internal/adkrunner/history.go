package adkrunner

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// Legacy Redis conversation history (wipe only).
//
// Before the ADK cutover, turns lived in Redis under ag:{tenant}:{role}:
// {user} with a 24h TTL. Nothing writes these keys anymore -- the runner
// stores conversations in Postgres sessions -- but keys written just
// before the cutover can still exist, so "Bersihkan riwayat" deletes both
// stores. This file shrinks to that one operation; when it is gone, so is
// the file.
//
// Quota (rpd:) keys are separate and never touched here, so clearing
// can't reset rate limits.
type History struct {
	RDB *redis.Client
}

func histKey(tenantID, role, userID string) string {
	if role == "" {
		role = "owner"
	}
	if userID == "" {
		userID = "anon"
	}
	// per-role isolation: ag:{tenant}:{role}:{userId}
	return "ag:" + tenantID + ":" + role + ":" + userID
}

// ClearForRole deletes the stored turns for tenant+role+user.
func (h *History) ClearForRole(ctx context.Context, tenantID, role, userID string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return h.RDB.Del(ctx, histKey(tenantID, role, userID)).Err()
}
