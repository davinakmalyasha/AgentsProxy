-- 083_backup_reader_reconcile.sql — a dump role that cannot bypass RLS
-- silently produces an empty tenant backup.
--
-- 062 only created backup_reader when absent. A pre-existing role with
-- NOLOGIN or without BYPASSRLS was left broken, and the restore smoke used to
-- pass because it only checked the migration count. Reconcile the attributes
-- and make the smoke assert real rows as app_runtime.

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'backup_reader') THEN
    ALTER ROLE backup_reader LOGIN BYPASSRLS NOSUPERUSER NOCREATEDB NOCREATEROLE;
    GRANT pg_read_all_data TO backup_reader;
  END IF;
END $$;
