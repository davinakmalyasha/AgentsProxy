-- scripts/restore-roles.sql — pre-create the cluster roles a logical dump
-- references. Run this before restoring ops/backup.sh output into a fresh
-- cluster. The dump carries ownership and grants; PostgreSQL needs the roles
-- to exist before it can apply them.
--
-- Passwords are intentionally not set here. Set them out-of-band with
-- ALTER ROLE ... PASSWORD, as documented in RUNBOOK.md.
--
-- The attributes mirror the migrations:
--   app_runtime     LOGIN  NOBYPASSRLS  tenant traffic
--   auth_definer    NOLOGIN BYPASSRLS  session directory
--   delivery_worker NOLOGIN BYPASSRLS  notification definers
--   delivery_caller LOGIN  NOBYPASSRLS  Edge Function entry point
--   report_definer  NOLOGIN BYPASSRLS  report manifest
--   quota_reader    NOLOGIN BYPASSRLS  month_tokens definer
--   backup_reader   LOGIN  BYPASSRLS   pg_dump only

\set ON_ERROR_STOP on

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_runtime') THEN
    CREATE ROLE app_runtime LOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'auth_definer') THEN
    CREATE ROLE auth_definer NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'delivery_worker') THEN
    CREATE ROLE delivery_worker NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'delivery_caller') THEN
    CREATE ROLE delivery_caller LOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'report_definer') THEN
    CREATE ROLE report_definer NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'quota_reader') THEN
    CREATE ROLE quota_reader NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'backup_reader') THEN
    CREATE ROLE backup_reader LOGIN;
  END IF;
  -- Supabase-managed roles that appear in the dump's ACLs (default
  -- privileges and the project-schema revokes). A scratch cluster needs
  -- them to apply GRANT/REVOKE statements even though the app never logs in
  -- as them.
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'anon') THEN
    CREATE ROLE anon NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'authenticated') THEN
    CREATE ROLE authenticated NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'service_role') THEN
    CREATE ROLE service_role NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'authenticator') THEN
    CREATE ROLE authenticator NOLOGIN;
  END IF;
  -- Legacy app role referenced by historical grants in the dump. It is not
  -- a login path in the current runtime, but the ACL statements need it.
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app') THEN
    CREATE ROLE app NOLOGIN;
  END IF;
  -- Default ACLs in the dump are owned by Supabase's admin role; the role is
  -- needed to apply the ALTER DEFAULT PRIVILEGES statements in a scratch
  -- cluster. It is not a superuser here and is never a login path.
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'supabase_admin') THEN
    CREATE ROLE supabase_admin NOLOGIN;
  END IF;
END $$;

ALTER ROLE app_runtime      LOGIN  NOBYPASSRLS NOSUPERUSER NOCREATEDB NOCREATEROLE;
ALTER ROLE auth_definer     NOLOGIN BYPASSRLS NOSUPERUSER NOCREATEDB NOCREATEROLE;
ALTER ROLE delivery_worker  NOLOGIN BYPASSRLS NOSUPERUSER NOCREATEDB NOCREATEROLE;
ALTER ROLE delivery_caller  LOGIN  NOBYPASSRLS NOSUPERUSER NOCREATEDB NOCREATEROLE;
ALTER ROLE report_definer   NOLOGIN BYPASSRLS NOSUPERUSER NOCREATEDB NOCREATEROLE;
ALTER ROLE quota_reader     NOLOGIN BYPASSRLS NOSUPERUSER NOCREATEDB NOCREATEROLE;
ALTER ROLE backup_reader    LOGIN  BYPASSRLS NOSUPERUSER NOCREATEDB NOCREATEROLE;
