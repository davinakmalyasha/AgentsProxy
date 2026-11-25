package reqmetrics

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

func TestSpanRecordsDependencies(t *testing.T) {
	ctx := With(context.Background(), New())
	tr := PGTracer{}
	qctx := tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{})
	time.Sleep(time.Millisecond)
	tr.TraceQueryEnd(qctx, nil, pgx.TraceQueryEndData{})

	h := RedisHook{}
	next := func(context.Context, redis.Cmder) error {
		time.Sleep(time.Millisecond)
		return nil
	}
	if err := h.ProcessHook(next)(ctx, redis.NewCmd(ctx, "PING")); err != nil {
		t.Fatal(err)
	}
	db, rd := From(ctx).Duration()
	if db <= 0 {
		t.Fatal("db duration not recorded")
	}
	if rd <= 0 {
		t.Fatal("redis duration not recorded")
	}
	if dbCalls, redisCalls := From(ctx).Calls(); dbCalls != 1 || redisCalls != 1 {
		t.Fatalf("calls db=%d redis=%d, want 1/1", dbCalls, redisCalls)
	}
}

func TestNoSpanIsNoop(t *testing.T) {
	tr := PGTracer{}
	qctx := tr.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{})
	tr.TraceQueryEnd(qctx, nil, pgx.TraceQueryEndData{})
	h := RedisHook{}
	if err := h.ProcessHook(func(context.Context, redis.Cmder) error { return nil })(
		context.Background(), redis.NewCmd(context.Background(), "PING")); err != nil {
		t.Fatal(err)
	}
}
