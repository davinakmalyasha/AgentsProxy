-- 080_drop_project_key.sql — contract step for the schema-ownership key.
--
-- 079 made clients.db_schema unique. The application stopped writing
-- project_key in the same release, so the temporary key is dead weight and
-- can be removed. This is a deliberate destructive change with the code
-- references already deleted; scripts/lint-migration.sh documents the
-- exception.
--
-- Release order: 079, deploy the binary that no longer writes project_key,
-- then apply this migration.

ALTER TABLE public.clients
  DROP CONSTRAINT IF EXISTS clients_project_key_matches_schema;
DROP INDEX IF EXISTS public.clients_project_key_key;
ALTER TABLE public.clients
  DROP COLUMN IF EXISTS project_key;
