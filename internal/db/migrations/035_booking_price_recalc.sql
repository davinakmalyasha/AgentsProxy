-- 035_booking_price_recalc.sql — fix the price-change trigger on
-- sports_bookings. Idempotent.
--
-- BUG: migration 014 attached ONE function, sports_recalc_payment_status,
-- to two different tables:
--
--   sports_payments   AFTER INSERT OR UPDATE OR DELETE   -- has booking_id
--   sports_bookings   AFTER UPDATE OF override_price, price
--
-- The function body is written for a payment row:
--
--   v_booking_id uuid := COALESCE(NEW.booking_id, OLD.booking_id);
--
-- sports_bookings has no booking_id column — its key is `id`. So the
-- second trigger raised, on every fire:
--
--   ERROR: record "new" has no field "booking_id" (SQLSTATE 42703)
--
-- which the handler surfaced as a 500. The practical effect: an owner
-- could never change a booking's price or override_price. Every attempt
-- failed after the row was already written inside the transaction, so the
-- whole "adjust the price" path was non-functional, and
-- payment_status never recalculated when the price moved. The failure
-- only appeared now because this is the first change that updates price
-- on a path exercised outside the tests (the integration tests skip on a
-- managed pooler, and no unit test can reach SQL).
--
-- FIX: give sports_bookings its own function. There is nothing to
-- coalesce here — the row being updated IS the booking — so the key is
-- simply NEW.id. The payments trigger keeps the original function
-- untouched, since that one is correct for its own table.

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
  -- Only a change in the amount owed can change how much is still due.
  -- Without this, every unrelated UPDATE (status, end_time, notes) would
  -- rewrite payment_status — and, worse, a status update that fires
  -- inside another trigger's cascade could mask its own change.
  IF TG_OP = 'UPDATE'
     AND NEW.override_price IS NOT DISTINCT FROM OLD.override_price
     AND NEW.price               IS NOT DISTINCT FROM OLD.price THEN
    RETURN NULL;
  END IF;

  SELECT COALESCE(override_price, price) INTO v_price
    FROM sports_bookings WHERE id = v_booking_id;
  SELECT COALESCE(SUM(amount), 0) INTO v_paid
    FROM sports_payments WHERE booking_id = v_booking_id;

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

DROP TRIGGER IF EXISTS sports_bookings_recalc_on_price ON sports_bookings;
CREATE TRIGGER sports_bookings_recalc_on_price
  AFTER UPDATE OF override_price, price ON sports_bookings
  FOR EACH ROW EXECUTE FUNCTION sports_recalc_booking_payment_status();
