-- 076_project_key.sql — race-safe project schema ownership.
--
-- Admin provisioning already checks that a project schema is unused, but two
-- concurrent calls could both pass the check. A unique index on db_schema
-- cannot be used because the RLS feature tests intentionally seed multiple
-- tenant_id rows inside project_centrum.
--
-- project_key is the owner key for real provisioning: admin sets it to the
-- project schema, the database enforces one row per key, and test rows leave
-- it NULL. The check constraint keeps the owner key aligned with db_schema.
--
-- Additive only: nullable column, backfill for existing project rows, NOT
-- VALID + VALIDATE constraint, partial unique index.

ALTER TABLE public.clients
  ADD COLUMN IF NOT EXISTS project_key text;

UPDATE public.clients
   SET project_key = db_schema
 WHERE db_schema LIKE 'project\_%'
   AND project_key IS NULL;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
     WHERE conname = 'clients_project_key_matches_schema'
       AND conrelid = 'public.clients'::regclass
  ) THEN
    ALTER TABLE public.clients
      ADD CONSTRAINT clients_project_key_matches_schema
      CHECK (project_key IS NULL OR project_key = db_schema) NOT VALID;
  END IF;
END $$;
ALTER TABLE public.clients VALIDATE CONSTRAINT clients_project_key_matches_schema;

CREATE UNIQUE INDEX IF NOT EXISTS clients_project_key_key
  ON public.clients (project_key)
  WHERE project_key IS NOT NULL;
