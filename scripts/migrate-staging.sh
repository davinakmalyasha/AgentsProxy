#!/usr/bin/env bash
# Apply the embedded migration chain to the staging database.
#
# Reads .env.staging (gitignored). That file must point at the second
# Supabase project, never production. The admin DSN is required; the
# runtime DSN is not used by cmd/migrate.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -f .env.staging ]; then
    set -a
    # shellcheck disable=SC1091
    . ./.env.staging
    set +a
fi

: "${DATABASE_ADMIN_URL:?set DATABASE_ADMIN_URL in .env.staging (staging admin 5432 DSN)}"
: "${DATABASE_URL:?set DATABASE_URL in .env.staging (staging admin 5432 DSN is fine)}"

case "$DATABASE_ADMIN_URL" in
    *supabase.co*|*pooler.supabase.com*) ;;
    *)
        echo "refusing DATABASE_ADMIN_URL: not a Supabase host" >&2
        exit 1
        ;;
esac

# Guard against the production project ref. Staging must have its own ref.
case "$DATABASE_ADMIN_URL" in
    *hwcizjzkqexviegsyaql*)
        echo "refusing: this is the production Supabase project" >&2
        exit 1
        ;;
esac

echo "[staging] applying platform + project migrations"
go run ./cmd/migrate
