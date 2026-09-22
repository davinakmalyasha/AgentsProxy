-- 019_sports_recurring_series.sql — Phase 6: recurring weekly booking
-- series (community groups). Port of CentrumBadminton's recurring_series.
-- DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F

CREATE TABLE IF NOT EXISTS sports_recurring_series (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  facility_id   uuid NOT NULL REFERENCES sports_facilities(id) ON DELETE CASCADE,
  customer_name text NOT NULL,
  customer_phone text NOT NULL DEFAULT '',
  weekday       smallint NOT NULL CHECK (weekday BETWEEN 0 AND 6), -- 0=Sunday
  start_time    time NOT NULL,
  end_time      time NOT NULL CHECK (end_time > start_time),
  start_date    date NOT NULL,
  end_date      date,
  status        text NOT NULL DEFAULT 'active', -- active | paused | ended
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS sports_recurring_series_tenant_idx
  ON sports_recurring_series(tenant_id, weekday);

ALTER TABLE sports_recurring_series ENABLE ROW LEVEL SECURITY;
ALTER TABLE sports_recurring_series FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sports_recurring_series_isolation ON sports_recurring_series;
CREATE POLICY sports_recurring_series_isolation ON sports_recurring_series
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON sports_recurring_series TO app_runtime;
