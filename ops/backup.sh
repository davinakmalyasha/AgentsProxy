#!/usr/bin/env bash
# ops/backup.sh — pg_dump of the agents_proxy database.
# NOTE: nothing schedules this yet — wire it to a cron / GitHub Actions
# schedule before calling it "nightly".
# Usage:  ops/backup.sh                # uses DATABASE_URL_ADMIN from env
#         DATABASE_URL_ADMIN=... ops/backup.sh
# Restores: psql "$DATABASE_URL_ADMIN" < backups/agents_proxy-YYYY-MM-DDTHH-MM-SS.sql.gz
#
# DONE: PRD §H (backup) / STANDARDS §H

set -euo pipefail

ADMIN_URL="${DATABASE_URL_ADMIN:-${DATABASE_ADMIN_URL:-${DATABASE_URL:-postgres://postgres:postgres@localhost:5432/agents_proxy}}}"
OUT_DIR="${BACKUP_DIR:-./backups}"
KEEP_DAYS="${BACKUP_KEEP_DAYS:-14}"

mkdir -p "$OUT_DIR"
ts=$(date -u +%Y-%m-%dT%H-%M-%SZ)
out="$OUT_DIR/agents_proxy-$ts.sql.gz"

echo "[backup] dumping $ADMIN_URL -> $out"
# App schemas only: public (platform), private, project_centrum, and every
# project_<slug> schema. The platform layers (auth/storage/vault/...) are
# Supabase-managed and covered by the platform WAL-G backups; including
# them breaks portable restores (hosted-only extensions, pg_cron config
# table data, event triggers) and drags platform PII (auth password
# hashes) into the artifact.
#
# Build the schema list dynamically: the query returns exactly the schemas
# this dump owns, so an empty project set cannot make pg_dump fail.
mapfile -t schemas < <(psql "$ADMIN_URL" -Atc \
  "SELECT nspname FROM pg_namespace
    WHERE nspname IN ('public','private') OR nspname LIKE 'project\\_%' ESCAPE '\\'
    ORDER BY nspname" | tr -d '\r')
if [ "${#schemas[@]}" -eq 0 ]; then
  echo "[backup] no app schemas found" >&2
  exit 1
fi
schema_args=()
for s in "${schemas[@]}"; do
  schema_args+=(--schema="$s")
done
pg_dump --no-owner --no-privileges --clean --if-exists \
  "${schema_args[@]}" "$ADMIN_URL" | gzip -9 > "$out"
ls -lh "$out"

# Retention: prune backups older than KEEP_DAYS.
find "$OUT_DIR" -type f -name 'agents_proxy-*.sql.gz' -mtime +"$KEEP_DAYS" -delete -print

echo "[backup] done. KEEP_DAYS=$KEEP_DAYS"
