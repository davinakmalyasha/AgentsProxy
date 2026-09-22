-- 007_sports_facilities.sql — sports vertical: bookable facilities.
-- DONE: Phase 4 (sports vertical) / STANDARDS A, D, F
--
-- Custom-table feature (not tenant_records) because a facility is
-- configuration (name/type/hours/price) referenced by bookings, and
-- needs real column types (TIME, NUMERIC) + an FK from bookings.

CREATE TABLE IF NOT EXISTS sports_facilities (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  name            text NOT NULL,
  type            text NOT NULL DEFAULT 'other',   -- badminton | soccer | swimming | padel | other
  capacity        int  NOT NULL DEFAULT 1,
  price_per_hour  numeric(10,2) NOT NULL DEFAULT 0,
  open_time       time NOT NULL DEFAULT '06:00',
  close_time      time NOT NULL DEFAULT '22:00',
  status          text NOT NULL DEFAULT 'active',  -- active | paused
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS sports_facilities_tenant_idx ON sports_facilities(tenant_id);

ALTER TABLE sports_facilities ENABLE ROW LEVEL SECURITY;
ALTER TABLE sports_facilities FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sports_facilities_isolation ON sports_facilities;
CREATE POLICY sports_facilities_isolation ON sports_facilities
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON sports_facilities TO app_runtime;
