#!/usr/bin/env bash
# Run the DB-backed suite against a fresh throwaway database.
#
# A shared database was the source of the stale project_test_* schemas that
# once pushed simple statements into 30-second timeouts. Every invocation now
# creates agents_proxy_test_<timestamp>_<random>, applies the embedded chain,
# runs the suite, and drops the database with FORCE. TEST_KEEP_DB=1 keeps it
# for post-mortem inspection and prints the DSNs.
#
# Tests read TEST_DATABASE_* and TEST_REDIS_URL only. This script refuses a
# non-local target so a stale shell export cannot point the suite at staging,
# production, or a shared dev database. The destructive test setup deletes
# clients and schemas and flushes the selected Redis database.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -f .env.test ]; then
    set -a
    # shellcheck disable=SC1091
    . ./.env.test
    set +a
fi

: "${TEST_DATABASE_URL:?set TEST_DATABASE_URL (copy .env.test.example to .env.test)}"
: "${TEST_DATABASE_ADMIN_URL:?set TEST_DATABASE_ADMIN_URL (the admin DSN for the same throwaway database)}"
: "${TEST_REDIS_URL:=redis://localhost:6379/15}"

refuse() {
    echo "refusing $1=$2: DB-backed tests must target local throwaway services" >&2
    exit 1
}

check_db() {
    local name="$1" url="$2"
    case "$url" in
        *@localhost:*|*@127.0.0.1:*|*@\[::1\]:*) ;;
        *) refuse "$name" "$url" ;;
    esac
}

check_redis() {
    local name="$1" url="$2"
    case "$url" in
        localhost:*|127.0.0.1:*|\[::1\]:*|\
        *@localhost:*|*@127.0.0.1:*|*@\[::1\]:*|\
        redis://localhost:*|redis://127.0.0.1:*|redis://\[::1\]:*|\
        redis://*@localhost:*|redis://*@127.0.0.1:*|redis://*@\[::1\]:*|\
        rediss://localhost:*|rediss://127.0.0.1:*|rediss://\[::1\]:*|\
        rediss://*@localhost:*|rediss://*@127.0.0.1:*|rediss://*@\[::1\]:*) ;;
        *) refuse "$name" "$url" ;;
    esac
}

check_db TEST_DATABASE_URL "$TEST_DATABASE_URL"
check_db TEST_DATABASE_ADMIN_URL "$TEST_DATABASE_ADMIN_URL"
check_redis TEST_REDIS_URL "$TEST_REDIS_URL"

# Serialize local runs. The migration chain creates cluster-wide roles, and
# two concurrent CREATE ROLE attempts can race even though the databases are
# distinct. The lock is a directory with a PID file, not flock, so it works in
# Git Bash on Windows.
LOCK_DIR=".tmp/test-db.lock"
mkdir -p .tmp
LOCKED=0
for _ in $(seq 1 300); do
    if mkdir "$LOCK_DIR" 2>/dev/null; then
        echo "$$" > "$LOCK_DIR/pid"
        LOCKED=1
        break
    fi
    if [ -f "$LOCK_DIR/pid" ] && ! kill -0 "$(cat "$LOCK_DIR/pid")" 2>/dev/null; then
        echo "removing stale test-db lock" >&2
        rm -rf "$LOCK_DIR"
        continue
    fi
    sleep 1
done
if [ "$LOCKED" != "1" ]; then
    echo "could not acquire $LOCK_DIR after 300s" >&2
    exit 1
fi

DB_NAME=""
cleanup() {
    local code=$?
    if [ -n "$DB_NAME" ]; then
        if [ "${TEST_KEEP_DB:-0}" = "1" ]; then
            echo "kept test database: $DB_NAME" >&2
            echo "  admin:   $TEST_DATABASE_ADMIN_URL" >&2
            echo "  runtime: $TEST_DATABASE_URL" >&2
        else
            go run ./cmd/testdb -action drop \
                -admin-url "$TEST_DATABASE_ADMIN_URL" \
                -prefix agents_proxy_test \
                -name "$DB_NAME" >/dev/null 2>&1 || \
                echo "warning: could not drop test database $DB_NAME" >&2
        fi
    fi
    rm -rf "$LOCK_DIR"
    exit "$code"
}
trap cleanup EXIT INT TERM

# cmd/testdb prints shell assignments; eval keeps the URL rewriting in Go,
# where escaping is reliable, instead of duplicating it in Bash.
CREATE_OUT=$(go run ./cmd/testdb -action create \
    -admin-url "$TEST_DATABASE_ADMIN_URL" \
    -runtime-url "$TEST_DATABASE_URL" \
    -prefix agents_proxy_test) || exit 1
eval "$CREATE_OUT"
DB_NAME="${TEST_DATABASE_NAME:?testdb did not return TEST_DATABASE_NAME}"

# Apply the full embedded chain once before any package test runs. This
# creates app_runtime and the project schemas, matching CI's fresh-database
# step and removing package-order dependence.
ENV=development \
DATABASE_URL="$TEST_DATABASE_URL" \
DATABASE_ADMIN_URL="$TEST_DATABASE_ADMIN_URL" \
LLM_API_KEY="${LLM_API_KEY:-test-key}" \
ADMIN_API_KEY="${ADMIN_API_KEY:-test-admin-key}" \
ALLOW_ORIGINS="*" \
go run ./cmd/migrate apply

set +e
go test -p 1 ./... -cover "$@"
code=$?
set -e
exit "$code"
