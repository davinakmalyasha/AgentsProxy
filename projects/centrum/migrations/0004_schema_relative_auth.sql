-- 0004_schema_relative_auth.sql — Centrum auth and helpers follow the schema.
--
-- 0001 moved Centrum's tables to project_centrum and rewrote the definer
-- functions with that literal schema. That is correct for the production
-- tenant but wrong for a cloned test schema (and for any future project that
-- reuses staff auth).
--
-- This migration makes every project-owned lookup resolve the schema from
-- clients.db_schema:
--
--   * login_user / create_session / session_user / delete_session /
--     touch_session use public.staff_sessions as the platform directory and
--     read sports_users from the tenant's schema;
--   * range_summary reads the tenant's payments/bookings/kantin/expenses;
--   * retention_tick cleans the platform session directory;
--   * the three sports triggers resolve TG_TABLE_SCHEMA.
--
-- Signatures are unchanged so the previous binary stays compatible during
-- the deploy window.

-- ---------------------------------------------------------------------------
-- Staff auth.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION private.login_user(p_tenant text, p_email text)
RETURNS TABLE(
  user_id       uuid,
  password_hash text,
  role          text,
  full_name     text,
  phone         text,
  is_active     boolean
)
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = ''
AS $function$
DECLARE
  v_schema text;
BEGIN
  SELECT c.db_schema INTO v_schema
    FROM public.clients c
   WHERE c.tenant_id = p_tenant;
  IF v_schema IS NULL OR v_schema !~ '^project_[a-z0-9_]+$' THEN
    RETURN;
  END IF;
  IF to_regclass(pg_catalog.format('%I.sports_users', v_schema)) IS NULL THEN
    RETURN;
  END IF;
  IF NOT has_table_privilege('auth_definer',
        pg_catalog.format('%I.sports_users', v_schema), 'SELECT') THEN
    RETURN;
  END IF;
  RETURN QUERY EXECUTE pg_catalog.format(
    'SELECT u.id, u.password_hash, u.role, u.full_name,
            COALESCE(u.phone, ''''), u.is_active
       FROM %I.sports_users u
      WHERE u.tenant_id = $1 AND u.email = $2',
    v_schema)
    USING p_tenant, p_email;
END;
$function$;

CREATE OR REPLACE FUNCTION private.create_session(
  p_user_id    uuid,
  p_token_hash text,
  p_expires_at timestamptz
)
RETURNS uuid
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $function$
DECLARE
  v_schema text;
  v_tenant text;
  v_id     uuid;
BEGIN
  -- A session lookup has only the user id here. Every active project is a
  -- candidate; the unique clients.db_schema means this is a small loop, and
  -- a user id belongs to exactly one project.
  FOR v_schema IN
    SELECT c.db_schema
      FROM public.clients c
     WHERE c.status = 'active'
       AND c.db_schema LIKE 'project\_%'
     ORDER BY c.tenant_id
  LOOP
    CONTINUE WHEN v_schema !~ '^project_[a-z0-9_]+$';
    CONTINUE WHEN to_regclass(pg_catalog.format('%I.sports_users', v_schema)) IS NULL;
    CONTINUE WHEN NOT has_table_privilege('auth_definer',
      pg_catalog.format('%I.sports_users', v_schema), 'SELECT');
    BEGIN
      EXECUTE pg_catalog.format(
        'SELECT u.tenant_id FROM %I.sports_users u
          WHERE u.id = $1 AND u.is_active',
        v_schema)
        INTO v_tenant USING p_user_id;
    EXCEPTION WHEN undefined_table OR insufficient_privilege THEN
      v_tenant := NULL;
    END;
    EXIT WHEN v_tenant IS NOT NULL;
  END LOOP;

  IF v_tenant IS NULL THEN
    RETURN NULL;
  END IF;

  INSERT INTO public.staff_sessions (user_id, token_hash, expires_at, tenant_id)
  VALUES (p_user_id, p_token_hash, p_expires_at, v_tenant)
  ON CONFLICT (token_hash) DO UPDATE
    SET expires_at   = EXCLUDED.expires_at,
        tenant_id    = EXCLUDED.tenant_id,
        last_seen_at = pg_catalog.now()
  RETURNING id INTO v_id;
  RETURN v_id;
END;
$function$;

CREATE OR REPLACE FUNCTION private.delete_session(p_token_hash text)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $function$
DECLARE
  v_n integer;
BEGIN
  DELETE FROM public.staff_sessions s WHERE s.token_hash = p_token_hash;
  GET DIAGNOSTICS v_n = ROW_COUNT;
  RETURN v_n > 0;
END;
$function$;

CREATE OR REPLACE FUNCTION private."session_user"(p_token_hash text)
RETURNS TABLE(
  user_id   uuid,
  tenant_id text,
  email     text,
  role      text,
  full_name text,
  phone     text
)
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = ''
AS $function$
DECLARE
  v_user    uuid;
  v_tenant  text;
  v_schema  text;
  r_user    uuid;
  r_tenant  text;
  r_email   text;
  r_role    text;
  r_full    text;
  r_phone   text;
BEGIN
  SELECT s.user_id, s.tenant_id, c.db_schema
    INTO v_user, v_tenant, v_schema
    FROM public.staff_sessions s
    JOIN public.clients c ON c.tenant_id = s.tenant_id
   WHERE s.token_hash = p_token_hash
     AND s.expires_at > pg_catalog.now()
   LIMIT 1;
  IF v_user IS NULL OR v_schema IS NULL OR v_schema !~ '^project_[a-z0-9_]+$' THEN
    RETURN;
  END IF;
  IF to_regclass(pg_catalog.format('%I.sports_users', v_schema)) IS NULL THEN
    RETURN;
  END IF;
  IF NOT has_table_privilege('auth_definer',
        pg_catalog.format('%I.sports_users', v_schema), 'SELECT') THEN
    RETURN;
  END IF;
  BEGIN
    EXECUTE pg_catalog.format(
      'SELECT u.id, u.tenant_id, u.email, u.role, u.full_name,
              COALESCE(u.phone, '''')
         FROM %I.sports_users u
        WHERE u.id = $1 AND u.tenant_id = $2 AND u.is_active',
      v_schema)
      INTO r_user, r_tenant, r_email, r_role, r_full, r_phone
      USING v_user, v_tenant;
  EXCEPTION WHEN undefined_table OR insufficient_privilege THEN
    RETURN;
  END;
  IF r_user IS NULL THEN
    RETURN;
  END IF;
  user_id := r_user;
  tenant_id := r_tenant;
  email := r_email;
  role := r_role;
  full_name := r_full;
  phone := r_phone;
  RETURN NEXT;
END;
$function$;

CREATE OR REPLACE FUNCTION private.touch_session(
  p_token_hash    text,
  p_stale_before  timestamptz
)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $function$
DECLARE
  v_n integer;
BEGIN
  UPDATE public.staff_sessions s
     SET last_seen_at = pg_catalog.now()
   WHERE s.token_hash = p_token_hash
     AND s.last_seen_at < p_stale_before;
  GET DIAGNOSTICS v_n = ROW_COUNT;
  RETURN v_n > 0;
END;
$function$;

-- ---------------------------------------------------------------------------
-- Money summary: same result shape, tenant schema resolved at call time.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION private.range_summary(
  p_tenant text,
  p_from   date,
  p_to     date
)
RETURNS jsonb
LANGUAGE plpgsql
STABLE
SECURITY DEFINER
SET search_path = ''
AS $function$
DECLARE
  v_schema text;
  v_out    jsonb;
BEGIN
  SELECT c.db_schema INTO v_schema
    FROM public.clients c
   WHERE c.tenant_id = p_tenant;
  IF v_schema IS NULL OR v_schema !~ '^project_[a-z0-9_]+$' THEN
    RETURN jsonb_build_object(
      'cash', 0, 'qris', 0, 'other', 0, 'count', 0,
      'booked', 0, 'kantin', 0, 'expenses', 0, 'bookings', 0,
      'best_day', NULL, 'best_day_amt', 0, 'peak', '[]'::jsonb);
  END IF;
  IF to_regclass(pg_catalog.format('%I.sports_payments', v_schema)) IS NULL THEN
    RETURN jsonb_build_object(
      'cash', 0, 'qris', 0, 'other', 0, 'count', 0,
      'booked', 0, 'kantin', 0, 'expenses', 0, 'bookings', 0,
      'best_day', NULL, 'best_day_amt', 0, 'peak', '[]'::jsonb);
  END IF;
  IF NOT has_table_privilege('money_reader',
        pg_catalog.format('%I.sports_payments', v_schema), 'SELECT') THEN
    RETURN jsonb_build_object(
      'cash', 0, 'qris', 0, 'other', 0, 'count', 0,
      'booked', 0, 'kantin', 0, 'expenses', 0, 'bookings', 0,
      'best_day', NULL, 'best_day_amt', 0, 'peak', '[]'::jsonb);
  END IF;

  EXECUTE pg_catalog.format($query$
    WITH best AS (
      SELECT ((p.paid_at AT TIME ZONE 'Asia/Jakarta')::date)::text AS day,
             SUM(p.amount)::float8 AS amt
        FROM %I.sports_payments p
       WHERE p.tenant_id = $1
         AND p.deleted_at IS NULL
         AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= $2
         AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  $3
       GROUP BY 1 ORDER BY SUM(p.amount) DESC LIMIT 1
    )
    SELECT jsonb_build_object(
      'cash',  (SELECT COALESCE(SUM(p.amount) FILTER (WHERE p.method = 'cash'), 0)::float8
                  FROM %I.sports_payments p
                 WHERE p.tenant_id = $1
                   AND p.deleted_at IS NULL
                   AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= $2
                   AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  $3),
      'qris',  (SELECT COALESCE(SUM(p.amount) FILTER (WHERE p.method = 'qris'), 0)::float8
                  FROM %I.sports_payments p
                 WHERE p.tenant_id = $1
                   AND p.deleted_at IS NULL
                   AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= $2
                   AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  $3),
      'other', (SELECT COALESCE(SUM(p.amount) FILTER (WHERE p.method NOT IN ('cash', 'qris')), 0)::float8
                  FROM %I.sports_payments p
                 WHERE p.tenant_id = $1
                   AND p.deleted_at IS NULL
                   AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= $2
                   AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  $3),
      'count', (SELECT COUNT(*)::int
                  FROM %I.sports_payments p
                 WHERE p.tenant_id = $1
                   AND p.deleted_at IS NULL
                   AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date >= $2
                   AND (p.paid_at AT TIME ZONE 'Asia/Jakarta')::date <  $3),
      'booked', (SELECT COALESCE(SUM(COALESCE(b.override_price, b.price)), 0)::float8
                   FROM %I.sports_bookings b
                  WHERE b.tenant_id = $1
                    AND b.date >= $2 AND b.date < $3
                    AND b.status IN ('confirmed','checked_in','completed')),
      'kantin', (SELECT COALESCE(SUM(k.total), 0)::float8
                   FROM %I.sports_kantin_sales k
                  WHERE k.tenant_id = $1
                    AND k.voided_at IS NULL
                    AND (k.created_at AT TIME ZONE 'Asia/Jakarta')::date >= $2
                    AND (k.created_at AT TIME ZONE 'Asia/Jakarta')::date <  $3),
      'expenses', (SELECT COALESCE(SUM(e.amount), 0)::float8
                     FROM %I.sports_expenses e
                    WHERE e.tenant_id = $1
                      AND e.deleted_at IS NULL
                      AND e.spent_at >= $2 AND e.spent_at < $3),
      'bookings', (SELECT COUNT(*)::int
                     FROM %I.sports_bookings b
                    WHERE b.tenant_id = $1
                      AND b.date >= $2 AND b.date < $3
                      AND b.status NOT IN ('cancelled','no_show')),
      'best_day', (SELECT day FROM best),
      'best_day_amt', COALESCE((SELECT amt FROM best), 0),
      'peak', COALESCE((SELECT jsonb_agg(jsonb_build_array(h, n) ORDER BY h)
                          FROM (SELECT EXTRACT(HOUR FROM b.start_time)::int AS h, COUNT(*)::int AS n
                                  FROM %I.sports_bookings b
                                 WHERE b.tenant_id = $1
                                   AND b.date >= $2 AND b.date < $3
                                   AND b.status NOT IN ('cancelled','no_show')
                                 GROUP BY 1 ORDER BY 2 DESC LIMIT 3) t),
                       '[]'::jsonb)
    );
  $query$,
    v_schema, v_schema, v_schema, v_schema, v_schema, v_schema,
    v_schema, v_schema, v_schema, v_schema)
    INTO v_out
    USING p_tenant, p_from, p_to;
  RETURN v_out;
END;
$function$;

-- ---------------------------------------------------------------------------
-- Retention: sessions live in the platform directory now.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION private.retention_tick()
RETURNS TABLE(table_name text, deleted bigint)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $function$
DECLARE
  v_n bigint;
  v_batch bigint;
BEGIN
  DELETE FROM public.staff_sessions
   WHERE expires_at < pg_catalog.now() - interval '7 days';
  GET DIAGNOSTICS v_n = ROW_COUNT;
  table_name := 'staff_sessions'; deleted := v_n; RETURN NEXT;

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

-- ---------------------------------------------------------------------------
-- Triggers: the same Centrum logic as 0001, resolved from TG_TABLE_SCHEMA.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION public.sports_blocked_booking_guard()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = ''
AS $function$
DECLARE
  v_reason text;
BEGIN
  PERFORM pg_catalog.pg_advisory_xact_lock(
    pg_catalog.hashtextextended(
      NEW.tenant_id || ':' || NEW.facility_id::text || ':' || NEW.date::text, 0::bigint));
  EXECUTE pg_catalog.format(
    'SELECT b.reason FROM %I.sports_court_blocks b
      WHERE b.tenant_id = $1
        AND b.block_date = $2
        AND (b.facility_id = $3 OR b.facility_id IS NULL)
        AND b.start_time < $4
        AND b.end_time   > $5
      LIMIT 1',
    TG_TABLE_SCHEMA)
    INTO v_reason
    USING NEW.tenant_id, NEW.date, NEW.facility_id, NEW.end_time, NEW.start_time;
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
SET search_path = ''
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
  EXECUTE pg_catalog.format(
    'SELECT COALESCE(override_price, price) FROM %I.sports_bookings WHERE id = $1',
    TG_TABLE_SCHEMA)
    INTO v_price USING v_booking_id;
  EXECUTE pg_catalog.format(
    'SELECT COALESCE(SUM(amount), 0) FROM %I.sports_payments
      WHERE booking_id = $1 AND deleted_at IS NULL',
    TG_TABLE_SCHEMA)
    INTO v_paid USING v_booking_id;
  v_status := CASE
    WHEN v_price IS NULL OR v_price <= 0 THEN 'unpaid'
    WHEN v_paid >= v_price                THEN 'paid'
    WHEN v_paid > 0                       THEN 'partial'
    ELSE 'unpaid' END;
  EXECUTE pg_catalog.format(
    'UPDATE %I.sports_bookings SET payment_status = $1
      WHERE id = $2 AND payment_status IS DISTINCT FROM $1',
    TG_TABLE_SCHEMA)
    USING v_status, v_booking_id;
  RETURN NULL;
END;
$function$;

CREATE OR REPLACE FUNCTION public.sports_recalc_payment_status()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = ''
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
  EXECUTE pg_catalog.format(
    'SELECT COALESCE(override_price, price) FROM %I.sports_bookings WHERE id = $1',
    TG_TABLE_SCHEMA)
    INTO v_price USING v_booking_id;
  EXECUTE pg_catalog.format(
    'SELECT COALESCE(SUM(amount),0) FROM %I.sports_payments
      WHERE booking_id = $1 AND deleted_at IS NULL',
    TG_TABLE_SCHEMA)
    INTO v_paid USING v_booking_id;
  v_status := CASE
    WHEN v_price IS NULL OR v_price <= 0 THEN 'unpaid'
    WHEN v_paid >= v_price                THEN 'paid'
    WHEN v_paid > 0                       THEN 'partial'
    ELSE 'unpaid' END;
  EXECUTE pg_catalog.format(
    'UPDATE %I.sports_bookings SET payment_status = $1
      WHERE id = $2 AND payment_status IS DISTINCT FROM $1',
    TG_TABLE_SCHEMA)
    USING v_status, v_booking_id;
  RETURN NULL;
END;
$function$;

-- ---------------------------------------------------------------------------
-- Ownership and grants. CREATE OR REPLACE preserves the existing owner, but
-- being explicit makes a fresh database and a repaired database identical.
-- ---------------------------------------------------------------------------

ALTER FUNCTION private.login_user(text, text)                         OWNER TO auth_definer;
ALTER FUNCTION private.create_session(uuid, text, timestamptz)        OWNER TO auth_definer;
ALTER FUNCTION private.delete_session(text)                           OWNER TO auth_definer;
ALTER FUNCTION private.session_user(text)                             OWNER TO auth_definer;
ALTER FUNCTION private.touch_session(text, timestamptz)               OWNER TO auth_definer;
ALTER FUNCTION private.range_summary(text, date, date)                OWNER TO money_reader;
ALTER FUNCTION private.retention_tick()                                OWNER TO postgres;
ALTER FUNCTION public.sports_blocked_booking_guard()                   OWNER TO postgres;
ALTER FUNCTION public.sports_recalc_booking_payment_status()           OWNER TO postgres;
ALTER FUNCTION public.sports_recalc_payment_status()                   OWNER TO postgres;

REVOKE ALL ON FUNCTION private.login_user(text, text)                  FROM PUBLIC;
REVOKE ALL ON FUNCTION private.create_session(uuid, text, timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION private.delete_session(text)                    FROM PUBLIC;
REVOKE ALL ON FUNCTION private.session_user(text)                      FROM PUBLIC;
REVOKE ALL ON FUNCTION private.touch_session(text, timestamptz)        FROM PUBLIC;
REVOKE ALL ON FUNCTION private.range_summary(text, date, date)         FROM PUBLIC;
REVOKE ALL ON FUNCTION private.retention_tick()                        FROM PUBLIC;

GRANT EXECUTE ON FUNCTION private.login_user(text, text)                   TO app_runtime;
GRANT EXECUTE ON FUNCTION private.create_session(uuid, text, timestamptz)  TO app_runtime;
GRANT EXECUTE ON FUNCTION private.delete_session(text)                     TO app_runtime;
GRANT EXECUTE ON FUNCTION private.session_user(text)                       TO app_runtime;
GRANT EXECUTE ON FUNCTION private.touch_session(text, timestamptz)         TO app_runtime;
GRANT EXECUTE ON FUNCTION private.range_summary(text, date, date)          TO app_runtime;

GRANT USAGE ON SCHEMA public TO auth_definer;
GRANT SELECT (tenant_id, db_schema, status) ON public.clients TO auth_definer;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.staff_sessions TO auth_definer;
GRANT USAGE ON SCHEMA project_centrum TO auth_definer;
GRANT USAGE ON SCHEMA project_centrum TO money_reader;
GRANT SELECT ON project_centrum.sports_users TO auth_definer;
GRANT SELECT ON project_centrum.sports_payments,
                project_centrum.sports_bookings,
                project_centrum.sports_kantin_sales,
                project_centrum.sports_expenses TO money_reader;
