-- 009_sports_inventory.sql — sports vertical: food/stock items.
-- DONE: Phase 4 (sports vertical) / STANDARDS A, D, F
--
-- Custom table (not tenant_records) because quantity tracking + a
-- low-stock threshold need real INT columns and atomic decrements.

CREATE TABLE IF NOT EXISTS sports_inventory (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id          text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  name               text NOT NULL,
  sku                text NOT NULL DEFAULT '',
  quantity           int  NOT NULL DEFAULT 0,
  unit_price         numeric(10,2) NOT NULL DEFAULT 0,
  low_stock_threshold int NOT NULL DEFAULT 5,
  status             text NOT NULL DEFAULT 'active', -- active | paused
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS sports_inventory_tenant_idx ON sports_inventory(tenant_id);

ALTER TABLE sports_inventory ENABLE ROW LEVEL SECURITY;
ALTER TABLE sports_inventory FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sports_inventory_isolation ON sports_inventory;
CREATE POLICY sports_inventory_isolation ON sports_inventory
  FOR ALL
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON sports_inventory TO app_runtime;
