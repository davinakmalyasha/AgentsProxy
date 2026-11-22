#!/usr/bin/env bash
# Run the DB-backed suite against a local throwaway database.
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
: "${TEST_REDIS_URL:?set TEST_REDIS_URL (a local, throwaway Redis)}"

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
    local url="$1"
    case "$url" in
        localhost:*|127.0.0.1:*|\[::1\]:*|\
        *@localhost:*|*@127.0.0.1:*|*@\[::1\]:*|\
        redis://localhost:*|redis://127.0.0.1:*|redis://\[::1\]:*|\
        redis://*@localhost:*|redis://*@127.0.0.1:*|redis://*@\[::1\]:*|\
        rediss://localhost:*|rediss://127.0.0.1:*|rediss://\[::1\]:*|\
        rediss://*@localhost:*|rediss://*@127.0.0.1:*|rediss://*@\[::1\]:*) ;;
        *) refuse "TEST_REDIS_URL" "$url" ;;
    esac
}

check_db TEST_DATABASE_URL "$TEST_DATABASE_URL"
check_db TEST_DATABASE_ADMIN_URL "$TEST_DATABASE_ADMIN_URL"
check_redis "$TEST_REDIS_URL"

# The tests delete rows and drop schemas. Require a database whose name makes
# its purpose obvious; a local dev database is still data worth protecting.
db_name="${TEST_DATABASE_URL##*/}"
db_name="${db_name%%\?*}"
case "$db_name" in
    *test*|*ci*) ;;
    *)
        if [ "${ALLOW_NON_TEST_DB:-0}" != "1" ]; then
            echo "refusing TEST_DATABASE_URL database '$db_name': name must contain 'test' or 'ci'" >&2
            echo "set ALLOW_NON_TEST_DB=1 only if this is a disposable local database" >&2
            exit 1
        fi
        ;;
esac

# The admin DSN must address the same database as the runtime DSN, otherwise
# the suite would prepare one database and test another.
admin_name="${TEST_DATABASE_ADMIN_URL##*/}"
admin_name="${admin_name%%\?*}"
if [ "$db_name" != "$admin_name" ]; then
    echo "refusing: TEST_DATABASE_URL database '$db_name' != TEST_DATABASE_ADMIN_URL database '$admin_name'" >&2
    exit 1
fi

exec go test -p 1 ./... -cover "$@"
