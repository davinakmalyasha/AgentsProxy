-- 088_project_entitlement_backfill.sql — restore Centrum's effective feature set.
--
-- Before the architecture closure, project features were mounted without
-- checking clients.features, so the live row was never populated (production
-- had '{}', staging had the generic fnb starter set). The gate now enforces
-- entitlement for every identity, including public landing reads.
--
-- This is a one-time normalization to the set the running product already
-- exposed: it must match projects/centrum/project.json. Admin PATCH remains
-- the supported way to change it afterwards; a tenant may intentionally run
-- a subset once it has a non-empty, explicit list.
--
-- Idempotent: re-running writes the same array. The row identity and schema
-- pin prevent a future unrelated tenant named centrum in public from matching.

UPDATE public.clients
   SET features = ARRAY[
         'records',
         'availability',
         'bookings',
         'courtblocks',
         'customers',
         'expenses',
         'facilities',
         'inventory',
         'kantin',
         'members',
         'payments',
         'pricing',
         'series',
         'users'
       ]::text[],
       updated_at = now()
 WHERE tenant_id = 'centrum'
   AND db_schema = 'project_centrum';
