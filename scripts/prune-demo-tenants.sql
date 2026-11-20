-- prune-demo-tenants.sql — one-off production hygiene.
--
-- Keeps the live Centrum tenant and the DFD agency CORS row, and removes
-- every other clients row plus any tenant-scoped data. This also removes
-- orphan test tenants that never had a clients row (for example tool-test).
--
-- Run with the ADMIN database URL, inside a psql session:
--   psql "$DATABASE_ADMIN_URL" -f scripts/prune-demo-tenants.sql
--
-- Review the SELECTs first if you want a dry run; the DELETE block is
-- transactional and only commits when the whole script succeeds.

\set ON_ERROR_STOP on

BEGIN;

CREATE TEMP TABLE keep_tenants (tenant_id text PRIMARY KEY) ON COMMIT DROP;
INSERT INTO keep_tenants (tenant_id) VALUES
  ('centrum'),
  ('dfd-agency');

\echo 'tenants before:'
SELECT tenant_id, name, status FROM clients ORDER BY tenant_id;

-- Every table with a tenant_id gets pruned, across public (platform + legacy
-- Centrum) and every project_<slug> schema. This is what removes orphan
-- tool-test rows. Project schemas are not dropped here: removing a schema is
-- a decommission step that must happen after an export, not as part of a
-- generic hygiene sweep.
DO $$
DECLARE
  r record;
BEGIN
  FOR r IN
    SELECT table_schema, table_name
    FROM information_schema.columns
    WHERE (table_schema = 'public'
           OR table_schema LIKE 'project\_%' ESCAPE '\')
      AND column_name = 'tenant_id'
      AND table_name <> 'clients'
    ORDER BY table_schema, table_name
  LOOP
    EXECUTE format(
      'DELETE FROM %I.%I WHERE tenant_id NOT IN (SELECT tenant_id FROM keep_tenants)',
      r.table_schema, r.table_name
    );
    RAISE NOTICE 'pruned %.%', r.table_schema, r.table_name;
  END LOOP;
END $$;

DELETE FROM public.project_migrations
WHERE project_slug NOT IN (SELECT tenant_id FROM keep_tenants);

DELETE FROM clients
WHERE tenant_id NOT IN (SELECT tenant_id FROM keep_tenants);

\echo 'tenants after:'
SELECT tenant_id, name, status FROM clients ORDER BY tenant_id;

-- Should return zero rows.
SELECT t.table_name, t.tenant_id
FROM (
  SELECT 'sports_bookings' AS table_name, tenant_id FROM project_centrum.sports_bookings
  UNION ALL SELECT 'sports_users', tenant_id FROM project_centrum.sports_users
  UNION ALL SELECT 'sports_facilities', tenant_id FROM project_centrum.sports_facilities
  UNION ALL SELECT 'sports_inventory', tenant_id FROM project_centrum.sports_inventory
  UNION ALL SELECT 'sports_members', tenant_id FROM project_centrum.sports_members
  UNION ALL SELECT 'sports_kantin_sales', tenant_id FROM project_centrum.sports_kantin_sales
  UNION ALL SELECT 'agent_reminders', tenant_id FROM public.agent_reminders
  UNION ALL SELECT 'agent_staff_payments', tenant_id FROM public.agent_staff_payments
  UNION ALL SELECT 'notify_jobs', tenant_id FROM public.notify_jobs
) t
WHERE t.tenant_id NOT IN ('centrum', 'dfd-agency');

COMMIT;
