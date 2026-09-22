-- 015_sports_pricing_rules.sql — Phase 6: date-aware pricing rules.
-- Port of CentrumBadminton's pricing_rules: the rule active for a
-- booking's date determines day/evening prices and open/close hours.
-- DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F

CREATE TABLE IF NOT EXISTS sports_pricing_rules (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  valid_from    date NOT NULL,
  open_time     time NOT NULL DEFAULT '07:00',
  close_time    time NOT NULL DEFAULT '23:00',
  day_start     time NOT NULL DEFAULT '07:00',
  day_end       time NOT NULL DEFAULT '17:00',
  price_day     numeric(12,2) NOT NULL DEFAULT 0 CHECK (price_day >= 0),
  price_evening numeric(12,2) NOT NULL DEFAULT 0 CHECK (price_evening >= 0),
  is_active     boolean NOT NULL DEFAULT true,
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, valid_from)
);

CREATE INDEX IF NOT EXISTS sports_pricing_rules_tenant_idx
  ON sports_pricing_rules(tenant_id, valid_from);

ALTER TABLE sports_pricing_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE sports_pricing_rules FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sports_pricing_rules_isolation ON sports_pricing_rules;
CREATE POLICY sports_pricing_rules_isolation ON sports_pricing_rules
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON sports_pricing_rules TO app_runtime;
