-- 023_centrum_gaps.sql — Phase 7: close the schema gaps so Centrum's
-- frontend runs unmodified. Idempotent.
-- DONE: Phase 7 (Centrum parity) / STANDARDS A, D, F

-- bookings: notes, customer_id (directory link), created_by (audit)
ALTER TABLE sports_bookings
  ADD COLUMN IF NOT EXISTS notes text NOT NULL DEFAULT '';

ALTER TABLE sports_bookings
  ADD COLUMN IF NOT EXISTS customer_id uuid REFERENCES sports_customers(id) ON DELETE SET NULL;

ALTER TABLE sports_bookings
  ADD COLUMN IF NOT EXISTS created_by uuid REFERENCES sports_users(id) ON DELETE SET NULL;

-- facilities: sort_order (Centrum orders courts by position)
ALTER TABLE sports_facilities
  ADD COLUMN IF NOT EXISTS sort_order integer NOT NULL DEFAULT 0;

-- kantin products: sort_order
ALTER TABLE sports_kantin_products
  ADD COLUMN IF NOT EXISTS sort_order integer NOT NULL DEFAULT 0;

-- kantin sales: booking_id (link to a booking), recorded_by (audit)
ALTER TABLE sports_kantin_sales
  ADD COLUMN IF NOT EXISTS booking_id uuid REFERENCES sports_bookings(id) ON DELETE SET NULL;

ALTER TABLE sports_kantin_sales
  ADD COLUMN IF NOT EXISTS recorded_by uuid REFERENCES sports_users(id) ON DELETE SET NULL;
