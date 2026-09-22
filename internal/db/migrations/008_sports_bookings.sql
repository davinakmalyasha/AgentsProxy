-- 008_sports_bookings.sql — sports vertical: bookings with a
-- double-booking-proof exclusion constraint.
-- DONE: Phase 4 (sports vertical) / STANDARDS A, D, F
--
-- THE HARD PART. The exclusion constraint (btree_gist) makes the
-- DATABASE refuse two bookings for the same facility whose time
-- ranges overlap — application code cannot accidentally double-book,
-- no matter how concurrent the requests are.

CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE IF NOT EXISTS sports_bookings (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  facility_id   uuid NOT NULL REFERENCES sports_facilities(id) ON DELETE RESTRICT,
  date          date NOT NULL,
  start_time    time NOT NULL,
  end_time      time NOT NULL,
  customer_name text NOT NULL,
  customer_phone text NOT NULL DEFAULT '',
  status        text NOT NULL DEFAULT 'confirmed', -- confirmed | cancelled | checked_in
  price         numeric(10,2) NOT NULL DEFAULT 0,
  deposit       numeric(10,2) NOT NULL DEFAULT 0,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  -- Overlap guard: same tenant + same facility + same date + time
  -- range overlaps -> rejected by the database.
  -- Uses tsrange (timestamp without time zone) — NOT tstzrange —
  -- because the implicit timestamp→timestamptz cast in tstzrange
  -- is timezone-dependent (not IMMUTABLE), which Postgres forbids
  -- in index expressions. Venue slots are local-time; tz has no
  -- business meaning here.
  CONSTRAINT sports_bookings_no_overlap
    EXCLUDE USING gist (
      tenant_id WITH =,
      facility_id WITH =,
      tsrange(date + start_time, date + end_time) WITH &&
    ),
  CHECK (end_time > start_time)
);

CREATE INDEX IF NOT EXISTS sports_bookings_tenant_date_idx
  ON sports_bookings(tenant_id, date);
CREATE INDEX IF NOT EXISTS sports_bookings_tenant_facility_idx
  ON sports_bookings(tenant_id, facility_id);

ALTER TABLE sports_bookings ENABLE ROW LEVEL SECURITY;
ALTER TABLE sports_bookings FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sports_bookings_isolation ON sports_bookings;
CREATE POLICY sports_bookings_isolation ON sports_bookings
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON sports_bookings TO app_runtime;
