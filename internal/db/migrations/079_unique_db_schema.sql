-- 079_unique_db_schema.sql — one project schema, one client.
--
-- 075 enforced the project_* namespace. 076 added project_key as a
-- race-safe owner key because the RLS feature tests seeded several
-- tenant_ids inside project_centrum; a plain unique index would have
-- broken that test model.
--
-- The tests now give every tenant its own cloned schema, so the database
-- can enforce the invariant directly. project_key is no longer written by
-- the application; it is removed in the contract migration (080).
--
-- Additive: one partial unique index. The preflight reports duplicates with
-- their owners instead of failing on a raw index error.

DO $$
DECLARE
  dup text;
BEGIN
  SELECT string_agg(db_schema || ' <- ' || tenants, '; ')
    INTO dup
    FROM (
      SELECT db_schema, string_agg(tenant_id, ', ' ORDER BY tenant_id) AS tenants
        FROM public.clients
       WHERE db_schema LIKE 'project\_%'
       GROUP BY db_schema
      HAVING count(*) > 1
    ) d;
  IF dup IS NOT NULL THEN
    RAISE EXCEPTION
      'cannot make clients.db_schema unique; duplicate project schemas: %', dup;
  END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS clients_project_schema_key
  ON public.clients (db_schema)
  WHERE db_schema LIKE 'project\_%';
