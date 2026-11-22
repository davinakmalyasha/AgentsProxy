#!/usr/bin/env bash
# scripts/lint-migration.sh — additive-DDL law (GOVERNANCE §3).
# Fails on non-additive DDL: DROP COLUMN, ALTER TYPE, RENAME, DROP TABLE.
# Historical exceptions: documented removals, pre-law (042), or with all
# code references already deleted in the same change (071). 080 removes the
# temporary project_key after 079 made db_schema unique and the binary
# stopped writing it.
set -euo pipefail
cd "$(dirname "$0")/.."

fail=0
for f in internal/db/migrations/*.sql; do
  case "$f" in
    *042_drop_telegram.sql|*071_drop_invite_columns.sql|*080_drop_project_key.sql) continue ;;
  esac
  # Strip -- comments so documented words ("never DROP COLUMN") don't trip it.
  body=$(grep -v '^[[:space:]]*--' "$f" || true)
  for pat in 'DROP[[:space:]]+COLUMN' 'ALTER[[:space:]]+.*TYPE' 'RENAME[[:space:]]+(COLUMN|TO)' 'DROP[[:space:]]+TABLE'; do
    if echo "$body" | grep -Ei "$pat" >/dev/null; then
      echo "NON-ADDITIVE DDL in $f (pattern: $pat)"
      echo "  Additive only: ADD COLUMN NULL/default, CREATE INDEX (concurrently, outside boot tx),"
      echo "  new CHECK NOT VALID, new tables. Drops/renames need a phased plan + explicit exception."
      fail=1
    fi
  done
done
if [ "$fail" = 1 ]; then exit 1; fi
echo "migrations additive-clean ($(ls internal/db/migrations/*.sql | wc -l) files checked)"
