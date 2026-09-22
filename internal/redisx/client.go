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
	c := redis.NewClient(opts)
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return c, nil
}
