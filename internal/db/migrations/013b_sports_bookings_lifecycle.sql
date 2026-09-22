-- 013b_sports_bookings_lifecycle.sql — Phase 6: extend sports_bookings
-- with the full booking lifecycle + payment tracking, ported from
-- CentrumBadminton. Must run BEFORE 014_sports_payments.sql (the
-- payments trigger references override_price/payment_status).
-- Idempotent: constraints are added conditionally via DO blocks.
-- DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F

-- Status lifecycle: pending → confirmed → checked_in → completed,
-- plus cancelled / no_show.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'sports_bookings_status_check') THEN
    ALTER TABLE sports_bookings
      ADD CONSTRAINT sports_bookings_status_check
        CHECK (status IN ('pending','confirmed','checked_in','completed','cancelled','no_show'));
  END IF;
END $$;

-- Source of the booking.
ALTER TABLE sports_bookings
  ADD COLUMN IF NOT EXISTS source text NOT NULL DEFAULT 'whatsapp';

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'sports_bookings_source_check') THEN
    ALTER TABLE sports_bookings
      ADD CONSTRAINT sports_bookings_source_check
        CHECK (source IN ('whatsapp','walk_in'));
  END IF;
END $$;

-- override_price: staff can adjust a booking's price; recalc uses it.
ALTER TABLE sports_bookings
  ADD COLUMN IF NOT EXISTS override_price numeric(12,2);

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'sports_bookings_override_check') THEN
    ALTER TABLE sports_bookings
      ADD CONSTRAINT sports_bookings_override_check
        CHECK (override_price IS NULL OR override_price >= 0);
  END IF;
END $$;

-- payment_status kept in sync by the payments trigger (014).
ALTER TABLE sports_bookings
  ADD COLUMN IF NOT EXISTS payment_status text NOT NULL DEFAULT 'unpaid';

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'sports_bookings_payment_status_check') THEN
    ALTER TABLE sports_bookings
      ADD CONSTRAINT sports_bookings_payment_status_check
        CHECK (payment_status IN ('unpaid','partial','paid'));
  END IF;
END $$;

-- The exclusion constraint must NOT consider cancelled/no_show as
-- blocking (same partial-index idea as Centrum's). Rebuild it:
-- drop the old one (it blocked on every status) and recreate with a
-- WHERE clause. Postgres requires the constraint be dropped first.
ALTER TABLE sports_bookings DROP CONSTRAINT IF EXISTS sports_bookings_no_overlap;
ALTER TABLE sports_bookings
  ADD CONSTRAINT sports_bookings_no_overlap
    EXCLUDE USING gist (
      tenant_id WITH =,
      facility_id WITH =,
      tsrange(date + start_time, date + end_time) WITH &&
    )
    WHERE (status NOT IN ('cancelled','no_show'));
