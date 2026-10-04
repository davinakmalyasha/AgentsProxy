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
pg_dump --no-owner --no-privileges --clean --if-exists "$ADMIN_URL" | gzip -9 > "$out"
ls -lh "$out"

# Retention: prune backups older than KEEP_DAYS.
find "$OUT_DIR" -type f -name 'agents_proxy-*.sql.gz' -mtime +"$KEEP_DAYS" -delete -print

echo "[backup] done. KEEP_DAYS=$KEEP_DAYS"
