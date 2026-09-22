-- 017_sports_customers.sql — Phase 6: customer directory with
-- no-show tracking. Port of CentrumBadminton's customers.
-- DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F

CREATE TABLE IF NOT EXISTS sports_customers (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  name          text NOT NULL,
  phone         text NOT NULL DEFAULT '',
  no_show_count integer NOT NULL DEFAULT 0,
  notes         text NOT NULL DEFAULT '',
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, phone)
);

CREATE INDEX IF NOT EXISTS sports_customers_tenant_idx
  ON sports_customers(tenant_id, name);

ALTER TABLE sports_customers ENABLE ROW LEVEL SECURITY;
ALTER TABLE sports_customers FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sports_customers_isolation ON sports_customers;
CREATE POLICY sports_customers_isolation ON sports_customers
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON sports_customers TO app_runtime;
