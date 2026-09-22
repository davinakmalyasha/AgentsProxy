-- 018_sports_court_blocks.sql — Phase 6: court blocks for
-- maintenance / private events (per-court or all courts).
-- Port of CentrumBadminton's court_blocks.
-- DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F

CREATE TABLE IF NOT EXISTS sports_court_blocks (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  facility_id uuid REFERENCES sports_facilities(id) ON DELETE CASCADE, -- NULL = all courts
  block_date date NOT NULL,
  start_time time NOT NULL DEFAULT '07:00',
  end_time   time NOT NULL DEFAULT '23:00',
  reason     text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS sports_court_blocks_tenant_date_idx
  ON sports_court_blocks(tenant_id, block_date);

ALTER TABLE sports_court_blocks ENABLE ROW LEVEL SECURITY;
ALTER TABLE sports_court_blocks FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sports_court_blocks_isolation ON sports_court_blocks;
CREATE POLICY sports_court_blocks_isolation ON sports_court_blocks
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON sports_court_blocks TO app_runtime;
