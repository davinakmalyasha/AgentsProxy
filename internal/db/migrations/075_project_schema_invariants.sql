-- 075_project_schema_invariants.sql — enforce the project-schema boundary.
--
-- The application already treats every project as one folder + one schema.
-- Until now the database accepted any lowercase schema name and allowed two
-- client rows to share a project schema. A typo such as "auth" or "private"
-- could therefore grant app_runtime access to a platform schema, and a
-- duplicate row could write into another project's tables.
--
-- This migration makes the invariant explicit and cheap to audit:
--   * db_schema is "public" (legacy platform tenant only) or "project_*";
--   * a project_* schema belongs to exactly one client;
--   * the project migration ledger records the schema each version ran in.
--
-- Additive only: new constraint, new column/default, no data rewrite beyond
-- recording the current schema. (The historical comment in 074 still says
-- "legacy: still public"; Centrum's tables moved to project_centrum in its
-- project 0001 migration.)

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
     WHERE conname = 'clients_db_schema_project_namespace'
       AND conrelid = 'public.clients'::regclass
  ) THEN
    ALTER TABLE public.clients
      ADD CONSTRAINT clients_db_schema_project_namespace
      CHECK (db_schema = 'public' OR db_schema LIKE 'project\_%') NOT VALID;
  END IF;
END $$;
ALTER TABLE public.clients VALIDATE CONSTRAINT clients_db_schema_project_namespace;

-- One project schema, one client is enforced at provisioning time (admin
-- create checks the existing row) and by `migrate check`. It is deliberately
-- not a unique index: the tenant tables are RLS-scoped by tenant_id, and the
-- feature test harness intentionally seeds multiple tenant_id rows inside
-- project_centrum to exercise that boundary. Production still has exactly
-- one client per project schema.
--
-- A project row is keyed by (slug, version), so a project that changes its
-- schema would otherwise look fully migrated in the new schema while every
-- migration was skipped. Record the schema with each applied version.
ALTER TABLE public.project_migrations
  ADD COLUMN IF NOT EXISTS db_schema text NOT NULL DEFAULT '';

UPDATE public.project_migrations pm
   SET db_schema = c.db_schema
  FROM public.clients c
 WHERE c.tenant_id = pm.project_slug
   AND pm.db_schema = '';

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
     WHERE conname = 'project_migrations_db_schema_namespace'
       AND conrelid = 'public.project_migrations'::regclass
  ) THEN
    ALTER TABLE public.project_migrations
      ADD CONSTRAINT project_migrations_db_schema_namespace
      CHECK (db_schema = '' OR db_schema LIKE 'project\_%') NOT VALID;
  END IF;
END $$;
ALTER TABLE public.project_migrations VALIDATE CONSTRAINT project_migrations_db_schema_namespace;
