// Package redisx holds the shared Redis client constructor plus the Lua
// scripts that collapse read-then-act pairs into one round trip.
//
// WHY LUA, NOT PIPELINES: Upstash bills per command, and a pipeline
// cuts round trips but not commands. Lua cuts both — and only Lua can
// express "INCR, and if the result is 1 then EXPIRE" atomically, which
// is every interesting case here (rate buckets, caps, quota counters).
package redisx

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// IncrExpire atomically INCRs key and sets its TTL — but only when the
// INCR created the key (n == 1). The split INCR-then-conditional-EXPIRE
// it replaces has a crash window: a process dying between the two
// leaves an immortal counter that caps permanently.
var IncrExpire = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then
  redis.call('EXPIRE', KEYS[1], tonumber(ARGV[1]))
end
return n
`)

// IncrExpire runs IncrExpire with a seconds TTL.
func IncrExpireRun(ctx context.Context, rdb *redis.Client, key string, ttl time.Duration) (int64, error) {
	secs := int64(ttl / time.Second)
	if secs < 1 {
		secs = 1
	}
	return IncrExpire.Run(ctx, rdb, []string{key}, secs).Int64()
}

// IncrByExpireAt atomically adds delta and anchors expiry — but only
// when the key has no TTL yet. Creator-set TTL, enforced inside Lua.
//
// This fixes the classic create-detection bug (`n == delta` only holds
// when the pre-existing value was 0: a repopulated counter that happens
// to equal the delta silently becomes immortal).
var IncrByExpireAt = redis.NewScript(`
local n = redis.call('INCRBY', KEYS[1], tonumber(ARGV[1]))
if redis.call('TTL', KEYS[1]) < 0 then
  redis.call('EXPIREAT', KEYS[1], tonumber(ARGV[2]))
end
return n
`)

// IncrByExpireAtRun runs IncrByExpireAt with a delta and an absolute
// expiry (unix seconds, e.g. next-month-start + grace).
func IncrByExpireAtRun(ctx context.Context, rdb *redis.Client, key string, delta int64, expireAtUnix int64) (int64, error) {
	return IncrByExpireAt.Run(ctx, rdb, []string{key}, delta, expireAtUnix).Int64()
}
