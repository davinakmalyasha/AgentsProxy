-- 0001_move_to_project_schema.sql — Centrum's one-time schema move.
--
-- The historical platform chain created Centrum's business tables in
-- public. This migration moves them into project_centrum, their real home
-- under the one-project/one-schema architecture, and updates the definer
-- functions that referenced them as public.*.
--
-- The table move is deliberately idempotent so it can be applied by the
-- normal project runner on a fresh database and by the approved production
-- migration step on the long-lived database.
--
-- Centrum keeps its legacy /v1/sports/* surface for now. The data boundary
-- is what changes here; the route rename is a separate frontend cutover.

-- ---------------------------------------------------------------------------
-- 1. Move the business tables.
-- ---------------------------------------------------------------------------
DO $$
DECLARE
  t text;
BEGIN
  FOREACH t IN ARRAY ARRAY[
    'sports_bookings',
    'sports_court_blocks',
    'sports_customers',
    'sports_expenses',
    'sports_facilities',
    'sports_inventory',
    'sports_kantin_products',
    'sports_kantin_sale_items',
    'sports_kantin_sales',
    'sports_kantin_stock_movements',
    'sports_member_topups',
    'sports_members',
    'sports_payments',
    'sports_pricing_rules',
    'sports_recurring_series',
    'sports_sessions',
    'sports_users'
  ] LOOP
    IF to_regclass(format('public.%I', t)) IS NOT NULL
       AND to_regclass(format('project_centrum.%I', t)) IS NULL THEN
      EXECUTE format('ALTER TABLE public.%I SET SCHEMA project_centrum', t);
    END IF;
  END LOOP;
END $$;

-- ---------------------------------------------------------------------------
-- 2. Replace the definer functions that named public.sports_* explicitly.
--    CREATE OR REPLACE keeps the function OIDs, so existing triggers and
--    grants remain attached.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION private.login_user(p_tenant text, p_email text)
 RETURNS TABLE(user_id uuid, password_hash text, role text, full_name text, phone text, is_active boolean)
 LANGUAGE sql
 STABLE SECURITY DEFINER
 SET search_path TO ''
AS $function$
  SELECT u.id, u.password_hash, u.role, u.full_name, COALESCE(u.phone, ''), u.is_active
    FROM project_centrum.sports_users u
   WHERE u.tenant_id = p_tenant AND u.email = p_email;
$function$;

CREATE OR REPLACE FUNCTION private.create_session(p_user_id uuid, p_token_hash text, p_expires_at timestamp with time zone)
 RETURNS uuid
 LANGUAGE plpgsql
 SECURITY DEFINER
 SET search_path TO ''
AS $function$
DECLARE
  v_tenant text;
  v_id     uuid;
BEGIN
  SELECT u.tenant_id INTO v_tenant
    FROM project_centrum.sports_users u
   WHERE u.id = p_user_id
     AND u.is_active;
  IF v_tenant IS NULL THEN
    RETURN NULL;
  END IF;

  INSERT INTO project_centrum.sports_sessions (user_id, token_hash, expires_at, tenant_id)
  VALUES (p_user_id, p_token_hash, p_expires_at, v_tenant)
  RETURNING id INTO v_id;
  RETURN v_id;
END;
$function$;

CREATE OR REPLACE FUNCTION private.delete_session(p_token_hash text)
 RETURNS boolean
 LANGUAGE plpgsql
 SECURITY DEFINER
 SET search_path TO ''
AS $function$
DECLARE
  v_n integer;
BEGIN
  DELETE FROM project_centrum.sports_sessions s WHERE s.token_hash = p_token_hash;
  GET DIAGNOSTICS v_n = ROW_COUNT;
  RETURN v_n > 0;
END;
$function$;

CREATE OR REPLACE FUNCTION private."session_user"(p_token_hash text)
 RETURNS TABLE(user_id uuid, tenant_id text, email text, role text, full_name text, phone text)
 LANGUAGE sql
 STABLE SECURITY DEFINER
 SET search_path TO ''
AS $function$
  SELECT u.id, u.tenant_id, u.email, u.role, u.full_name, COALESCE(u.phone, '')
    FROM project_centrum.sports_sessions s
    JOIN project_centrum.sports_users u ON u.id = s.user_id
   WHERE s.token_hash = p_token_hash
     AND s.expires_at > pg_catalog.now()
     AND u.is_active
   LIMIT 1;
$function$;

CREATE OR REPLACE FUNCTION private.touch_session(p_token_hash text, p_stale_before timestamp with time zone)
 RETURNS boolean
 LANGUAGE plpgsql
 SECURITY DEFINER
 SET search_path TO ''
AS $function$
DECLARE
  v_n integer;
BEGIN
  UPDATE project_centrum.sports_sessions s
     SET last_seen_at = pg_catalog.now()
   WHERE s.token_hash = p_token_hash
     AND s.last_seen_at < p_stale_before;
  GET DIAGNOSTICS v_n = ROW_COUNT;
  RETURN v_n > 0;
END;
$function$;

CREATE OR REPLACE FUNCTION private.range_summary(p_tenant text, p_from date, p_to date)
 RETURNS jsonb
 LANGUAGE sql
 STABLE SECURITY DEFINER
 SET search_path TO ''
AS $function$
  WITH best AS (
    SELECT ((p.paid_at AT TIME ZONE 'Asia/Jakarta')::date)::text AS day,
           SUM(p.amount)::float8 AS amt
      FROM project_centrum.sports_payments p
     WHERE p.tenant_id = p_tenant
       AND p.deleted_at IS NULL
       AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= p_from
       AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  p_to
     GROUP BY 1 ORDER BY SUM(p.amount) DESC LIMIT 1
  )
  SELECT jsonb_build_object(
    'cash',  (SELECT COALESCE(SUM(p.amount) FILTER (WHERE p.method = 'cash'), 0)::float8
                FROM project_centrum.sports_payments p
               WHERE p.tenant_id = p_tenant
                 AND p.deleted_at IS NULL
                 AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= p_from
                 AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  p_to),
    'qris',  (SELECT COALESCE(SUM(p.amount) FILTER (WHERE p.method = 'qris'), 0)::float8
                FROM project_centrum.sports_payments p
               WHERE p.tenant_id = p_tenant
                 AND p.deleted_at IS NULL
                 AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= p_from
                 AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  p_to),
    'other', (SELECT COALESCE(SUM(p.amount) FILTER (WHERE p.method NOT IN ('cash', 'qris')), 0)::float8
                FROM project_centrum.sports_payments p
               WHERE p.tenant_id = p_tenant
                 AND p.deleted_at IS NULL
                 AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= p_from
                 AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  p_to),
    'count', (SELECT COUNT(*)::int
                FROM project_centrum.sports_payments p
               WHERE p.tenant_id = p_tenant
                 AND p.deleted_at IS NULL
                 AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= p_from
                 AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  p_to),
    'booked', (SELECT COALESCE(SUM(COALESCE(b.override_price, b.price)), 0)::float8
                 FROM project_centrum.sports_bookings b
                WHERE b.tenant_id = p_tenant
                  AND b.date >= p_from AND b.date < p_to
                  AND b.status IN ('confirmed','checked_in','completed')),
    'kantin', (SELECT COALESCE(SUM(k.total), 0)::float8
                 FROM project_centrum.sports_kantin_sales k
                WHERE k.tenant_id = p_tenant
                  AND k.voided_at IS NULL
                  AND (k.created_at AT TIME ZONE 'Asia/Jakarta')::date >= p_from
                  AND (k.created_at AT TIME ZONE 'Asia/Jakarta')::date <  p_to),
    'expenses', (SELECT COALESCE(SUM(e.amount), 0)::float8
                   FROM project_centrum.sports_expenses e
                  WHERE e.tenant_id = p_tenant
                    AND e.deleted_at IS NULL
                    AND e.spent_at >= p_from AND e.spent_at < p_to),
    'bookings', (SELECT COUNT(*)::int
                   FROM project_centrum.sports_bookings b
                  WHERE b.tenant_id = p_tenant
                    AND b.date >= p_from AND b.date < p_to
                    AND b.status NOT IN ('cancelled','no_show')),
    'best_day', (SELECT day FROM best),
    'best_day_amt', COALESCE((SELECT amt FROM best), 0),
    'peak', COALESCE((SELECT jsonb_agg(jsonb_build_array(h, n) ORDER BY h)
                        FROM (SELECT EXTRACT(HOUR FROM b.start_time)::int AS h, COUNT(*)::int AS n
                                FROM project_centrum.sports_bookings b
                               WHERE b.tenant_id = p_tenant
                                 AND b.date >= p_from AND b.date < p_to
                                 AND b.status NOT IN ('cancelled','no_show')
                               GROUP BY 1 ORDER BY 2 DESC LIMIT 3) t),
                     '[]'::jsonb)
  );
$function$;

CREATE OR REPLACE FUNCTION private.retention_tick()
 RETURNS TABLE(table_name text, deleted bigint)
 LANGUAGE plpgsql
 SECURITY DEFINER
 SET search_path TO ''
AS $function$
DECLARE
  v_n bigint;
  v_batch bigint;
BEGIN
  DELETE FROM project_centrum.sports_sessions
   WHERE expires_at < pg_catalog.now() - interval '7 days';
  GET DIAGNOSTICS v_n = ROW_COUNT;
  table_name := 'sports_sessions'; deleted := v_n; RETURN NEXT;

  DELETE FROM public.agent_reminders
   WHERE status = 'sent' AND created_at < pg_catalog.now() - interval '90 days';
  GET DIAGNOSTICS v_n = ROW_COUNT;
  table_name := 'agent_reminders_sent'; deleted := v_n; RETURN NEXT;

  DELETE FROM public.agent_reminders
   WHERE status = 'dead' AND created_at < pg_catalog.now() - interval '30 days';
  GET DIAGNOSTICS v_n = ROW_COUNT;
  table_name := 'agent_reminders_dead'; deleted := v_n; RETURN NEXT;

  v_n := 0;
  LOOP
    DELETE FROM public.usage WHERE ctid IN (
      SELECT ctid FROM public.usage
       WHERE created_at < pg_catalog.now() - interval '400 days'
       LIMIT 5000);
    GET DIAGNOSTICS v_batch = ROW_COUNT;
    v_n := v_n + v_batch;
    EXIT WHEN v_batch < 5000;
  END LOOP;
  table_name := 'usage'; deleted := v_n; RETURN NEXT;

  DELETE FROM public.notify_jobs
   WHERE archived_at IS NOT NULL
     AND archived_at < pg_catalog.now() - interval '14 days';
  GET DIAGNOSTICS v_n = ROW_COUNT;
  table_name := 'notify_jobs'; deleted := v_n; RETURN NEXT;

  v_n := 0;
  LOOP
    DELETE FROM public.events WHERE ctid IN (
      SELECT ctid FROM public.events
       WHERE "timestamp" < pg_catalog.now() - interval '180 days'
       LIMIT 5000);
    GET DIAGNOSTICS v_batch = ROW_COUNT;
    v_n := v_n + v_batch;
    EXIT WHEN v_batch < 5000;
  END LOOP;
  table_name := 'events'; deleted := v_n; RETURN NEXT;

  DELETE FROM public.report_objects
   WHERE expires_at < pg_catalog.now() - interval '1 day';
  GET DIAGNOSTICS v_n = ROW_COUNT;
  table_name := 'report_objects'; deleted := v_n; RETURN NEXT;

  RETURN;
END;
$function$;

CREATE OR REPLACE FUNCTION public.sports_blocked_booking_guard()
 RETURNS trigger
 LANGUAGE plpgsql
 SET search_path TO 'project_centrum', 'public'
AS $function$
DECLARE
  v_reason text;
BEGIN
  PERFORM pg_advisory_xact_lock(
    pg_catalog.hashtextextended(
      NEW.tenant_id || ':' || NEW.facility_id::text || ':' || NEW.date::text, 0::bigint));
  SELECT b.reason INTO v_reason
    FROM sports_court_blocks b
   WHERE b.tenant_id = NEW.tenant_id
     AND b.block_date = NEW.date
     AND (b.facility_id = NEW.facility_id OR b.facility_id IS NULL)
     AND b.start_time < NEW.end_time
     AND b.end_time   > NEW.start_time
    LIMIT 1;
  IF v_reason IS NOT NULL THEN
    RAISE EXCEPTION
      'slot is blocked (%): court % on % %-%',
      v_reason, NEW.facility_id, NEW.date,
      to_char(NEW.start_time, 'HH24:MI'), to_char(NEW.end_time, 'HH24:MI')
      USING ERRCODE = 'exclusion_violation';
  END IF;
  RETURN NEW;
END;
$function$;

CREATE OR REPLACE FUNCTION public.sports_recalc_booking_payment_status()
 RETURNS trigger
 LANGUAGE plpgsql
 SET search_path TO 'project_centrum', 'public'
AS $function$
DECLARE
  v_booking_id uuid := NEW.id;
  v_price numeric;
  v_paid numeric;
  v_status text;
BEGIN
  IF TG_OP = 'UPDATE'
     AND NEW.override_price IS NOT DISTINCT FROM OLD.override_price
     AND NEW.price               IS NOT DISTINCT FROM OLD.price THEN
    RETURN NULL;
  END IF;
  SELECT COALESCE(override_price, price) INTO v_price
    FROM sports_bookings WHERE id = v_booking_id;
  SELECT COALESCE(SUM(amount), 0) INTO v_paid
    FROM sports_payments WHERE booking_id = v_booking_id AND deleted_at IS NULL;
  v_status := CASE
    WHEN v_price IS NULL OR v_price <= 0 THEN 'unpaid'
    WHEN v_paid >= v_price                THEN 'paid'
    WHEN v_paid > 0                       THEN 'partial'
    ELSE 'unpaid' END;
  UPDATE sports_bookings SET payment_status = v_status
   WHERE id = v_booking_id AND payment_status IS DISTINCT FROM v_status;
  RETURN NULL;
END;
$function$;

CREATE OR REPLACE FUNCTION public.sports_recalc_payment_status()
 RETURNS trigger
 LANGUAGE plpgsql
 SET search_path TO 'project_centrum', 'public'
AS $function$
DECLARE
  v_booking_id uuid := COALESCE(NEW.booking_id, OLD.booking_id);
  v_price numeric;
  v_paid numeric;
  v_status text;
BEGIN
  IF v_booking_id IS NULL THEN
    RETURN NULL;
  END IF;
  SELECT COALESCE(override_price, price) INTO v_price
    FROM sports_bookings WHERE id = v_booking_id;
  SELECT COALESCE(SUM(amount),0) INTO v_paid
    FROM sports_payments WHERE booking_id = v_booking_id AND deleted_at IS NULL;
  v_status := CASE
    WHEN v_price IS NULL OR v_price <= 0 THEN 'unpaid'
    WHEN v_paid >= v_price                THEN 'paid'
    WHEN v_paid > 0                       THEN 'partial'
    ELSE 'unpaid' END;
  UPDATE sports_bookings SET payment_status = v_status
   WHERE id = v_booking_id AND payment_status IS DISTINCT FROM v_status;
  RETURN NULL;
END;
$function$;

-- ---------------------------------------------------------------------------
-- 3. Grants. project.json sets auto_grant=false, so the runner does not
--    apply its blanket DML grant here. Centrum's original ACLs moved with
--    the tables; these statements make the intended boundary explicit and
--    keep sports_sessions definer-only for writes.
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA project_centrum TO app_runtime;
REVOKE INSERT, UPDATE, DELETE ON project_centrum.sports_sessions FROM app_runtime;
GRANT SELECT ON project_centrum.sports_sessions TO app_runtime;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA project_centrum TO app_runtime;

-- The definer roles already own table-level grants (moved with the tables),
-- but schema USAGE does not move with a table. Without this, login_user and
-- range_summary fail with "permission denied for schema project_centrum".
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'auth_definer') THEN
    GRANT USAGE ON SCHEMA project_centrum TO auth_definer;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'money_reader') THEN
    GRANT USAGE ON SCHEMA project_centrum TO money_reader;
  END IF;
END $$;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'anon') THEN
    REVOKE ALL ON ALL TABLES IN SCHEMA project_centrum FROM anon;
    REVOKE ALL ON ALL SEQUENCES IN SCHEMA project_centrum FROM anon;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'authenticated') THEN
    REVOKE ALL ON ALL TABLES IN SCHEMA project_centrum FROM authenticated;
    REVOKE ALL ON ALL SEQUENCES IN SCHEMA project_centrum FROM authenticated;
  END IF;
END $$;

-- ---------------------------------------------------------------------------
-- 4. Point the tenant row at its schema. The row may not exist on a fresh
--    database; seed-dev.mjs then provisions it from project.json.
-- ---------------------------------------------------------------------------
UPDATE public.clients
   SET db_schema = 'project_centrum',
       updated_at = now()
 WHERE tenant_id = 'centrum'
   AND db_schema <> 'project_centrum';
