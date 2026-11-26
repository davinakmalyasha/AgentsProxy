// Package reqmetrics is the per-request timing span shared by HTTP
// middleware, pgx, and Redis. It answers one operational question with no
// external APM on the free plan: how much of the server time is database,
// Redis, or application work?
//
// The span is created per HTTP request and attached to the request context.
// pgx and Redis look it up from the context they already receive, so call
// sites do not change. Boot/background work has no span and is a no-op.
package reqmetrics

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

type ctxKey struct{}

// Span accumulates one request's dependency time. Fields are atomics because
// agent/tool paths may issue calls from more than one goroutine.
type Span struct {
	dbNanos    atomic.Int64
	redisNanos atomic.Int64
	dbCalls    atomic.Int64
	redisCalls atomic.Int64
}

// New returns an empty span.
func New() *Span { return &Span{} }

// With attaches s to ctx.
func With(ctx context.Context, s *Span) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, s)
}

// From returns the span attached to ctx, or nil.
func From(ctx context.Context) *Span {
	s, _ := ctx.Value(ctxKey{}).(*Span)
	return s
}

// Duration returns (db, redis).
func (s *Span) Duration() (time.Duration, time.Duration) {
	if s == nil {
		return 0, 0
	}
	return time.Duration(s.dbNanos.Load()), time.Duration(s.redisNanos.Load())
}

// Calls returns (db, redis).
func (s *Span) Calls() (int64, int64) {
	if s == nil {
		return 0, 0
	}
	return s.dbCalls.Load(), s.redisCalls.Load()
}

func (s *Span) addDB(d time.Duration) {
	if s == nil || d <= 0 {
		return
	}
	s.dbNanos.Add(int64(d))
	s.dbCalls.Add(1)
}

func (s *Span) addRedis(d time.Duration) {
	if s == nil || d <= 0 {
		return
	}
	s.redisNanos.Add(int64(d))
	s.redisCalls.Add(1)
}

type dbStartKey struct{}

// PGTracer implements pgx.QueryTracer. TraceQueryStart stores the start
// instant in the returned context; pgx passes that context back to
// TraceQueryEnd. Queries without a request span pay only two time.Now calls.
type PGTracer struct{}

func (PGTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	if From(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, dbStartKey{}, time.Now())
}

func (PGTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	start, ok := ctx.Value(dbStartKey{}).(time.Time)
	if !ok {
		return
	}
	if s := From(ctx); s != nil {
		s.addDB(time.Since(start))
	}
}

// RedisHook implements redis.Hook. It measures command and pipeline time
// without changing the command or its context.
type RedisHook struct{}

func (RedisHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (RedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if From(ctx) == nil {
			return next(ctx, cmd)
		}
		start := time.Now()
		err := next(ctx, cmd)
		if s := From(ctx); s != nil {
			s.addRedis(time.Since(start))
		}
		return err
	}
}

func (RedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if From(ctx) == nil {
			return next(ctx, cmds)
		}
		start := time.Now()
		err := next(ctx, cmds)
		if s := From(ctx); s != nil {
			s.addRedis(time.Since(start))
		}
		return err
	}
}
