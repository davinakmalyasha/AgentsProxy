#!/usr/bin/env bash
# ops/backup.sh — a portable pg_dump of the application schemas.
#
# The workflow in .github/workflows/backup.yml runs this every 6 hours and
# restores the artifact into a scratch postgres:17 before it is accepted.
# Nothing here is scheduled by itself.
#
# Usage:  ops/backup.sh                # uses DATABASE_URL_ADMIN from env
#         DATABASE_URL_ADMIN=... ops/backup.sh
# Restores:
#   psql "$DATABASE_ADMIN_URL" -f scripts/restore-roles.sql
#   gunzip -c backup.sql.gz | psql "$DATABASE_ADMIN_URL" -v ON_ERROR_STOP=1
#
# DONE: PRD §H (backup) / STANDARDS §H

set -euo pipefail

ADMIN_URL="${DATABASE_URL_ADMIN:-${DATABASE_ADMIN_URL:-${DATABASE_URL:-postgres://postgres:postgres@localhost:5432/agents_proxy}}}"
OUT_DIR="${BACKUP_DIR:-./backups}"
KEEP_DAYS="${BACKUP_KEEP_DAYS:-14}"

# The dump contains every tenant's data. Never leave it world-readable.
umask 077
mkdir -p "$OUT_DIR"
ts=$(date -u +%Y-%m-%dT%H-%M-%SZ)
out="$OUT_DIR/agents_proxy-$ts.sql.gz"
label="${ADMIN_URL##*@}"   # host/db, never the password

echo "[backup] dumping ${label%%\?*} -> $out"
# App schemas only: public (platform + agency), private, and every
# project_<slug> schema. Supabase-managed layers (auth/storage/vault/cron/
# extensions) are covered by the platform's own backups; including them
# breaks portable restores and drags platform PII into the artifact.
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

# A dump role without BYPASSRLS sees zero rows in every FORCE-RLS tenant
# table. Fail before writing a plausible-looking empty artifact. Supabase's
# postgres role cannot ALTER another role's BYPASSRLS attribute, so this is a
# deployment invariant checked at use, not a migration.
if [ "$(psql "$ADMIN_URL" -Atc \
  "SELECT coalesce((SELECT rolbypassrls FROM pg_roles WHERE rolname = current_user), false)" \
  | tr -d '\r')" != "t" ]; then
  echo "[backup] refusing: the backup role must be LOGIN BYPASSRLS or tenant tables dump empty" >&2
  exit 1
fi
# Ownership and ACLs are part of the restore contract: definer functions
# must stay owned by their least-privilege roles, and app_runtime must get
# the grants the migrations created. The restore pre-creates the roles.
pg_dump --clean --if-exists \
  "${schema_args[@]}" "$ADMIN_URL" | gzip -9 > "$out"
ls -lh "$out"

# Retention: prune backups older than KEEP_DAYS.
find "$OUT_DIR" -type f -name 'agents_proxy-*.sql.gz' -mtime +"$KEEP_DAYS" -delete -print

echo "[backup] done. KEEP_DAYS=$KEEP_DAYS"
