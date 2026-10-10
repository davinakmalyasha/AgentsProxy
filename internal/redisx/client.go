// DONE: PRD 3 (go-redis singleton, scheme auto-prepend for Laragon) / STANDARDS A
package redisx

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

func NewClient(ctx context.Context, rawURL string) (*redis.Client, error) {
	addr := rawURL
	if !strings.Contains(rawURL, "://") {
		addr = "redis://" + rawURL + "/0"
	}
	opts, err := redis.ParseURL(addr)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	opts.DialTimeout = 5 * time.Second
	opts.ReadTimeout = 3 * time.Second
	opts.WriteTimeout = 3 * time.Second
	// Serverless-tuned pool: the go-redis default PoolSize (10x
	// GOMAXPROCS) parks dozens of idle TLS connections per instance
	// against Upstash; 16 bounds it without starving bursts.
	// MinIdleConns keeps 2 warm so the first request after a cold
	// start does not pay a handshake. MaxRetries 1 (not the default
	// 3): one transient Upstash timeout must not become 4 sequential
	// attempts. ContextTimeoutEnabled lets request deadlines actually
	// cancel a stuck command instead of waiting out ReadTimeout.
	opts.PoolSize = 16
	opts.MinIdleConns = 2
	opts.MaxRetries = 1
	opts.ContextTimeoutEnabled = true
	c := redis.NewClient(opts)
	// A TLS blip during a cold start must not kill the instance:
	// retry the readiness probe a few times before failing boot.
	var pingErr error
	for i := 0; i < 3; i++ {
		if pingErr = c.Ping(ctx).Err(); pingErr == nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			_ = c.Close()
			return nil, fmt.Errorf("ping redis: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
	_ = c.Close()
	return nil, fmt.Errorf("ping redis: %w", pingErr)
}
