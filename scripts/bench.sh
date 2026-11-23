#!/usr/bin/env bash
# Benchmark runner for the exit-gate performance baseline.
#
# Local mode creates a fresh agents_proxy_bench_* database, applies migrations,
# seeds a realistic Centrum tenant, starts mockllm + the real server, runs the
# HTTP harness, captures pprof/DB/Redis evidence, and drops the database.
#
# Remote mode (BENCH_BASE_URL=https://...) is read-only and rate-limit safe:
# it defaults to health + public GETs unless BENCH_SCENARIOS explicitly names
# authenticated workloads and tokens are supplied.
#
# Common overrides:
#   BENCH_DURATION=20s BENCH_WARMUP=5s BENCH_CONCURRENCY=16
#   BENCH_SCENARIOS=all BENCH_KEEP_DB=1 BENCH_PROFILE=1 BENCH_COLD=1
set -euo pipefail
cd "$(dirname "$0")/.."

STAMP=$(date -u +%Y%m%dT%H%M%SZ)
OUT=".tmp/bench/$STAMP"
mkdir -p "$OUT" .tmp/bench
DURATION="${BENCH_DURATION:-20s}"
WARMUP="${BENCH_WARMUP:-5s}"
CONC="${BENCH_CONCURRENCY:-16}"
PROFILE="${BENCH_PROFILE:-1}"
COLD="${BENCH_COLD:-0}"
KEEP="${BENCH_KEEP_DB:-0}"
PORT="${BENCH_PORT:-$((19080 + $$ % 1000))}"
PPROF_PORT="${BENCH_PPROF_PORT:-$((16060 + $$ % 1000))}"
MOCK_PORT="${BENCH_MOCK_PORT:-$((18080 + $$ % 1000))}"
ADMIN_KEY="${BENCH_ADMIN_KEY:-bench-admin-key-0123456789abcdef0123456789abcdef}"
SERVER_PID=""
MOCK_PID=""
CPU_PID=""
BENCH_DB=""

cleanup() {
    code=$?
    set +e
    [ -n "$CPU_PID" ] && kill "$CPU_PID" 2>/dev/null
    [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null
    [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null
    if [ -n "$BENCH_DB" ]; then
        if [ "$KEEP" = "1" ]; then
            echo "kept benchmark database: $BENCH_DB" >&2
            echo "  admin:   $BENCH_ADMIN_URL" >&2
            echo "  runtime: $BENCH_RUNTIME_URL" >&2
        else
            go run ./cmd/testdb -action drop \
                -admin-url "$TEST_DATABASE_ADMIN_URL" \
                -prefix agents_proxy_bench -name "$BENCH_DB" >/dev/null 2>&1 || true
        fi
    fi
    exit "$code"
}
trap cleanup EXIT INT TERM

if [ -n "${BENCH_BASE_URL:-}" ]; then
    SCENARIOS="${BENCH_SCENARIOS:-health,public-facilities,public-availability,public-pricing}"
    echo "remote benchmark: $BENCH_BASE_URL scenarios=$SCENARIOS"
    set +e
    go run ./cmd/bench \
        -base-url "$BENCH_BASE_URL" \
        -tenant "${BENCH_TENANT:-centrum}" \
        -client-token "${BENCH_CLIENT_TOKEN:-}" \
        -staff-token "${BENCH_STAFF_TOKEN:-}" \
        -admin-key "${BENCH_ADMIN_KEY:-}" \
        -concurrency "$CONC" -warmup "$WARMUP" -duration "$DURATION" \
        -rps "${BENCH_RPS:-0.5}" \
        -scenarios "$SCENARIOS" \
        -json "$OUT/results.json" -markdown "$OUT/results.md"
    code=$?
    set -e
    echo "results: $OUT/results.md"
    exit "$code"
fi

if [ -f .env.test ]; then
    set -a
    # shellcheck disable=SC1091
    . ./.env.test
    set +a
fi
: "${TEST_DATABASE_ADMIN_URL:?set TEST_DATABASE_ADMIN_URL (local admin DSN)}"
: "${TEST_DATABASE_URL:?set TEST_DATABASE_URL (local runtime DSN)}"
BENCH_REDIS_URL="${BENCH_REDIS_URL:-redis://localhost:6379/14}"

CREATE_OUT=$(go run ./cmd/testdb -action create \
    -admin-url "$TEST_DATABASE_ADMIN_URL" \
    -runtime-url "$TEST_DATABASE_URL" \
    -prefix agents_proxy_bench) || exit 1
eval "$CREATE_OUT"
BENCH_DB="${TEST_DATABASE_NAME:?testdb did not return TEST_DATABASE_NAME}"
BENCH_ADMIN_URL="$TEST_DATABASE_ADMIN_URL"
BENCH_RUNTIME_URL="$TEST_DATABASE_URL"

echo "fresh benchmark database: $BENCH_DB"
ENV=development \
DATABASE_URL="$BENCH_RUNTIME_URL" \
DATABASE_ADMIN_URL="$BENCH_ADMIN_URL" \
LLM_API_KEY=bench-unused \
ADMIN_API_KEY=bench-unused \
ALLOW_ORIGINS='*' \
go run ./cmd/migrate apply >/dev/null

SEED_OUT=$(go run ./cmd/benchseed \
    -admin-url "$BENCH_ADMIN_URL" \
    -tenant centrum \
    -bookings "${BENCH_BOOKINGS:-500}" \
    -records "${BENCH_RECORDS:-500}") || exit 1
eval "$SEED_OUT"

# Redis DB 14 is reserved for benchmarks; never flush the developer DB 0.
REDIS_CLI="${REDIS_CLI:-}"
if [ -z "$REDIS_CLI" ]; then
    if command -v redis-cli >/dev/null 2>&1; then
        REDIS_CLI=redis-cli
    elif [ -x "/d/laragon/bin/redis/redis-8.10.1/redis-cli.exe" ]; then
        REDIS_CLI="/d/laragon/bin/redis/redis-8.10.1/redis-cli.exe"
    fi
fi
if [ -n "$REDIS_CLI" ]; then
    "$REDIS_CLI" -u "$BENCH_REDIS_URL" flushdb >/dev/null
    "$REDIS_CLI" -u "$BENCH_REDIS_URL" INFO stats > "$OUT/redis-before.txt"
else
    echo "redis-cli not found; benchmark DB was not flushed" >&2
fi

GO_BIN_SUFFIX=""
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) GO_BIN_SUFFIX=".exe";; esac
SERVER_BIN="$OUT/server$GO_BIN_SUFFIX"
MOCK_BIN="$OUT/mockllm$GO_BIN_SUFFIX"
go build -o "$SERVER_BIN" ./cmd/server
go build -o "$MOCK_BIN" ./cmd/mockllm

MOCKLLM_ADDR=":$MOCK_PORT" "$MOCK_BIN" > "$OUT/mockllm.log" 2>&1 &
MOCK_PID=$!
for _ in $(seq 1 60); do
    (echo > "/dev/tcp/127.0.0.1/$MOCK_PORT") 2>/dev/null && break
    sleep 0.25
done

start_server() {
    PORT="$PORT" \
    DATABASE_URL="$BENCH_RUNTIME_URL" \
    DATABASE_ADMIN_URL="$BENCH_ADMIN_URL" \
    REDIS_URL="$BENCH_REDIS_URL" \
    ENV=development \
    RUN_MIGRATIONS=false \
    BACKGROUND_TICKERS=false \
    LLM_BASE_URL="http://127.0.0.1:$MOCK_PORT" \
    LLM_API_KEY=bench-mock \
    LLM_MODEL=mock-model \
    ADMIN_API_KEY="$ADMIN_KEY" \
    RATE_LIMIT_PER_MINUTE=1000000 \
    AGENT_RATE_LIMIT_PER_MINUTE=1000000 \
    LOGIN_RATE_LIMIT_PER_MINUTE=1000000 \
    LOGIN_LOCKOUT_FAILURES=1000000 \
    ALLOW_ORIGINS='*' \
    PPROF_ADDR="127.0.0.1:$PPROF_PORT" \
    "$SERVER_BIN" >> "$OUT/server.log" 2>&1 &
    SERVER_PID=$!
}

wait_health() {
    for _ in $(seq 1 240); do
        if curl -fsS --max-time 2 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
            return 0
        fi
        sleep 0.25
    done
    echo "server did not become healthy; see $OUT/server.log" >&2
    return 1
}

if [ "$COLD" = "1" ]; then
    : > "$OUT/cold-start.txt"
    for i in $(seq 1 5); do
        start_server
        start_ns=$(date +%s%N)
        wait_health
        end_ns=$(date +%s%N)
        echo "run $i: $(( (end_ns - start_ns) / 1000000 )) ms" >> "$OUT/cold-start.txt"
        kill "$SERVER_PID" 2>/dev/null || true
        wait "$SERVER_PID" 2>/dev/null || true
        SERVER_PID=""
        sleep 1
    done
fi

start_server
wait_health

SCENARIOS="${BENCH_SCENARIOS:-all}"
echo "benchmark: scenarios=$SCENARIOS duration=$DURATION warmup=$WARMUP concurrency=$CONC"

if [ "$PROFILE" = "1" ]; then
    # CPU profile spans the measurement window plus a little slack.
    secs=$(echo "$DURATION" | sed 's/[^0-9].*$//')
    [ -z "$secs" ] && secs=20
    curl -fsS --max-time $((secs + 20)) \
        "http://127.0.0.1:$PPROF_PORT/debug/pprof/profile?seconds=$((secs + 2))" \
        > "$OUT/cpu.pprof" 2>/dev/null &
    CPU_PID=$!
fi

set +e
go run ./cmd/bench \
    -base-url "http://127.0.0.1:$PORT" \
    -tenant centrum \
    -client-token "$BENCH_CLIENT_TOKEN" \
    -staff-token "$BENCH_STAFF_TOKEN" \
    -staff-email "$BENCH_STAFF_EMAIL" \
    -staff-password "$BENCH_STAFF_PASSWORD" \
    -admin-key "$ADMIN_KEY" \
    -concurrency "$CONC" -warmup "$WARMUP" -duration "$DURATION" \
    -rps "${BENCH_RPS:-0}" \
    -scenarios "$SCENARIOS" \
    -json "$OUT/results.json" -markdown "$OUT/results.md"
BENCH_CODE=$?
set -e

if [ -n "$CPU_PID" ]; then
    wait "$CPU_PID" 2>/dev/null || true
    CPU_PID=""
fi
curl -fsS --max-time 10 "http://127.0.0.1:$PPROF_PORT/debug/pprof/heap" > "$OUT/heap.pprof" 2>/dev/null || true

if [ -n "$REDIS_CLI" ]; then
    "$REDIS_CLI" -u "$BENCH_REDIS_URL" INFO stats > "$OUT/redis-after.txt" || true
fi

# Hot-path plans. These are read-only and run after the load, so they do not
# contaminate the latency samples.
psql "$BENCH_ADMIN_URL" -v ON_ERROR_STOP=1 > "$OUT/explain.txt" <<'SQL' || true
\echo facilities list
EXPLAIN (ANALYZE, BUFFERS) SELECT id, tenant_id, name, type, capacity, price_per_hour, open_time, close_time, status
  FROM project_centrum.sports_facilities WHERE tenant_id='centrum' ORDER BY name;
\echo pricing list
EXPLAIN (ANALYZE, BUFFERS) SELECT id, tenant_id, valid_from, open_time, close_time, price_day, price_evening
  FROM project_centrum.sports_pricing_rules WHERE tenant_id='centrum' ORDER BY valid_from DESC;
\echo records list
EXPLAIN (ANALYZE, BUFFERS) SELECT id, tenant_id, kind, title, payload, status, created_at
  FROM project_centrum.tenant_records WHERE tenant_id='centrum' ORDER BY created_at DESC LIMIT 50;
\echo clients admin list
EXPLAIN (ANALYZE, BUFFERS) SELECT id, name, tenant_id, status, vertical, features FROM public.clients ORDER BY created_at DESC LIMIT 50;
SQL

echo "results: $OUT/results.md"
echo "evidence: $OUT"
exit "$BENCH_CODE"
