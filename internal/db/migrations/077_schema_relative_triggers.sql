-- 077_schema_relative_triggers.sql — make the sports triggers portable.
--
-- The original trigger functions used a fixed search_path (public, or
-- project_centrum + public after Centrum's move). A cloned project schema
-- would therefore fire the trigger against the base schema: a payment in
-- a test clone would update project_centrum.sports_bookings instead of the
-- clone's own row.
--
-- Resolve the table from TG_TABLE_SCHEMA instead. The trigger is attached
-- to a table, so the schema that fired it is the schema that owns the row;
-- no caller input and no ambient search_path is involved.
--
-- Additive: CREATE OR REPLACE only, same names/signatures/returns.

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

COMMENT ON FUNCTION public.sports_blocked_booking_guard() IS
  'Rejects bookings that overlap a court block. Resolves the project schema from TG_TABLE_SCHEMA so cloned/test schemas are honoured.';
