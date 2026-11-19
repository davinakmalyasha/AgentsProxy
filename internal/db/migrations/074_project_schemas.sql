-- 074_project_schemas.sql — project-owned schemas and project migrations.
--
-- The platform keeps shared identity tables in public. Each client project
-- owns its business tables in its own schema:
--
--   public            clients, sessions, usage, origins, agency tables
--   project_centrum   Centrum business tables (legacy: still public)
--   project_acme      Acme business tables
--
-- clients.db_schema is the authoritative routing value. The runtime sets
-- search_path from it inside every tenant transaction (records.WithTenantTx),
-- so a project handler can query `bookings` without knowing its schema.
--
-- project_migrations is separate from schema_migrations because project
-- versions are scoped per project: every project may have 0001_init.sql.
-- Additive only: new column, new table, no existing rows touched.
ALTER TABLE clients ADD COLUMN IF NOT EXISTS db_schema text NOT NULL DEFAULT 'public';

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'clients_db_schema_format'
  ) THEN
    ALTER TABLE clients ADD CONSTRAINT clients_db_schema_format
      CHECK (db_schema ~ '^[a-z][a-z0-9_]{0,62}$') NOT VALID;
  END IF;
END $$;
ALTER TABLE clients VALIDATE CONSTRAINT clients_db_schema_format;

CREATE TABLE IF NOT EXISTS project_migrations (
  project_slug text NOT NULL,
  version      text NOT NULL,
  checksum     text NOT NULL DEFAULT '',
  applied_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (project_slug, version)
);

ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum text NOT NULL DEFAULT '';

-- Only the migration runner (admin pool) touches these tables. No grant to
-- app_runtime: a project cannot rewrite its own migration ledger.
