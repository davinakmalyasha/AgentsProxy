-- 047_recalc_excludes_tombstones.sql — Phase B2: the payment_status recalc
-- must ignore tombstoned payments. Tombstoning is an UPDATE, so both
-- triggers fire; without this filter a "deleted" payment would keep its
-- booking marked paid.
--
-- Additive-safe: CREATE OR REPLACE of function bodies only, no schema
-- change, no data change. Re-runnable by design.

CREATE OR REPLACE FUNCTION sports_recalc_payment_status()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = public
AS $$
DECLARE
  v_booking_id uuid := COALESCE(NEW.booking_id, OLD.booking_id);
  v_price numeric;
  v_paid numeric;
BEGIN
  IF v_booking_id IS NULL THEN
    RETURN NULL;
  END IF;
  SELECT COALESCE(override_price, price) INTO v_price
    FROM sports_bookings WHERE id = v_booking_id;
  SELECT COALESCE(SUM(amount), 0) INTO v_paid
    FROM sports_payments WHERE booking_id = v_booking_id AND deleted_at IS NULL;
  UPDATE sports_bookings
     SET payment_status = CASE
       WHEN v_price IS NULL OR v_price <= 0 THEN 'unpaid'::text
       WHEN v_paid >= v_price THEN 'paid'::text
       WHEN v_paid > 0 THEN 'partial'::text
       ELSE 'unpaid'::text
     END
   WHERE id = v_booking_id;
  RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION sports_recalc_booking_payment_status()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = public
AS $$
DECLARE
  v_booking_id uuid := NEW.id;
  v_price numeric;
  v_paid numeric;
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

  UPDATE sports_bookings
     SET payment_status = CASE
           WHEN v_price IS NULL OR v_price <= 0 THEN 'unpaid'::text
           WHEN v_paid >= v_price              THEN 'paid'::text
           WHEN v_paid > 0                     THEN 'partial'::text
           ELSE                                    'unpaid'::text
         END
   WHERE id = v_booking_id;
  RETURN NULL;
END;
$$;
