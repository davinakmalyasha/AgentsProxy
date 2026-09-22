-- 014_sports_payments.sql — Phase 6: per-booking payments (cash/QRIS,
-- partial, auto-recalc payment_status). Port of CentrumBadminton's
-- payments + recalc_payment_status trigger.
-- DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F

CREATE TABLE IF NOT EXISTS sports_payments (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  booking_id uuid NOT NULL REFERENCES sports_bookings(id) ON DELETE CASCADE,
  amount     numeric(12,2) NOT NULL CHECK (amount > 0),
  method     text NOT NULL DEFAULT 'cash',       -- cash | qris
  paid_at    timestamptz NOT NULL DEFAULT now(),
  note       text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS sports_payments_booking_idx ON sports_payments(booking_id);
CREATE INDEX IF NOT EXISTS sports_payments_tenant_paid_idx ON sports_payments(tenant_id, paid_at);

ALTER TABLE sports_payments ENABLE ROW LEVEL SECURITY;
ALTER TABLE sports_payments FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sports_payments_isolation ON sports_payments;
CREATE POLICY sports_payments_isolation ON sports_payments
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON sports_payments TO app_runtime;

-- ------------------------------------------------------------
-- payment_status on sports_bookings stays in sync with payments
-- (unpaid | partial | paid), same logic as Centrum's trigger.
-- ------------------------------------------------------------
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
    FROM sports_payments WHERE booking_id = v_booking_id;
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

DROP TRIGGER IF EXISTS sports_payments_recalc ON sports_payments;
CREATE TRIGGER sports_payments_recalc
  AFTER INSERT OR UPDATE OR DELETE ON sports_payments
  FOR EACH ROW EXECUTE FUNCTION sports_recalc_payment_status();

DROP TRIGGER IF EXISTS sports_bookings_recalc_on_price ON sports_bookings;
CREATE TRIGGER sports_bookings_recalc_on_price
  AFTER UPDATE OF override_price, price ON sports_bookings
  FOR EACH ROW EXECUTE FUNCTION sports_recalc_payment_status();
