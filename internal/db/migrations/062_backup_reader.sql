-- 062_backup_reader.sql — least-privilege login role for scheduled backups.
--
-- R18: GitHub Actions runs ops/backup.sh nightly against this role's DSN
-- (repo secret BACKUP_DATABASE_URL). pg_dump needs read access to every
-- table; BYPASSRLS is REQUIRED: the tenant tables are FORCE ROW LEVEL
-- SECURITY keyed on app.tenant_id, and a NOBYPASSRLS reader would dump
-- EMPTY tables (the policy filters every row) — the one failure mode
-- where a backup quietly becomes worthless.
--
-- pg_read_all_data (PG14+) covers SELECT on all current AND future
-- tables/views/sequences plus schema USAGE, so new tables never need a
-- re-grant. The password is set out-of-band (never in a migration) and
-- lives only in the GitHub secret.
--
-- Blast radius: read-only by construction (no INSERT/UPDATE/DELETE
-- grants), but BYPASSRLS means "reads everything" — rotate the password
-- like any privileged credential.

DO $$
BEGIN
  CREATE ROLE backup_reader LOGIN BYPASSRLS;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

GRANT CONNECT ON DATABASE postgres TO backup_reader;
GRANT pg_read_all_data TO backup_reader;
