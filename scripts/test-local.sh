#!/usr/bin/env bash
# Run the DB-backed suite against a local throwaway database.
#
# Tests read TEST_DATABASE_* and TEST_REDIS_URL only. This script refuses a
# non-local database so a stale shell export cannot point the suite at a
# staging project.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -f .env.test ]; then
    set -a
    # shellcheck disable=SC1091
    . ./.env.test
    set +a
fi

: "${TEST_DATABASE_URL:?set TEST_DATABASE_URL (copy .env.test.example to .env.test)}"

case "$TEST_DATABASE_URL" in
    *@localhost:*|*@127.0.0.1:*|*@\[::1\]:*) ;;
    *)
        echo "refusing remote TEST_DATABASE_URL; DB tests must use a local throwaway database" >&2
        exit 1
        ;;
esac

exec go test -p 1 ./... -cover "$@"
