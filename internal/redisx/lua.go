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
const incrExpireLua = `
local n = redis.call('INCR', KEYS[1])
if n == 1 then
  redis.call('EXPIRE', KEYS[1], tonumber(ARGV[1]))
end
return n
`

var IncrExpire = redis.NewScript(incrExpireLua)

// IncrExpire runs IncrExpire with a seconds TTL.
func IncrExpireRun(ctx context.Context, rdb *redis.Client, key string, ttl time.Duration) (int64, error) {
	secs := int64(ttl / time.Second)
	if secs < 1 {
		secs = 1
	}
	return IncrExpire.Run(ctx, rdb, []string{key}, secs).Int64()
}

// IncrExpireTwo pipelines two IncrExpire scripts in one round trip.
// Upstash bills per command, so this does not cut commands — it cuts
// round trips (2 RTT -> 1) for paired counters like the agent RPD + cost
// caps, which are always checked together. Failures are per-key; the
// caller decides fail-open vs fail-closed. Eval (not EvalSha) is used
// so a cold script cache auto-loads instead of NOSCRIPT-failing.
func IncrExpireTwo(ctx context.Context, rdb *redis.Client, key1 string, ttl1 time.Duration, key2 string, ttl2 time.Duration) (int64, int64, error) {
	secs1 := int64(ttl1 / time.Second)
	if secs1 < 1 {
		secs1 = 1
	}
	secs2 := int64(ttl2 / time.Second)
	if secs2 < 1 {
		secs2 = 1
	}
	var c1, c2 *redis.Cmd
	_, err := rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		c1 = p.Eval(ctx, incrExpireLua, []string{key1}, secs1)
		c2 = p.Eval(ctx, incrExpireLua, []string{key2}, secs2)
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	n1, err := c1.Int64()
	if err != nil {
		return 0, 0, err
	}
	n2, err := c2.Int64()
	if err != nil {
		return 0, 0, err
	}
	return n1, n2, nil
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
