-- 020_sports_kantin.sql — Phase 6: Kantin POS. Port of
-- CentrumBadminton's 0005_kantin: products (with cost + stock),
-- sales, sale items, stock movements. The atomic sale is done in Go
-- (one WithTenantTx) — server-side price computation, stock never
-- negative.
-- DONE: Phase 6 (Centrum migration) / STANDARDS A, D, F

CREATE TABLE IF NOT EXISTS sports_kantin_products (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id           text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  name                text NOT NULL,
  category            text NOT NULL DEFAULT 'lainnya',
  price               numeric(12,2) NOT NULL CHECK (price >= 0),
  cost_price          numeric(12,2) NOT NULL DEFAULT 0 CHECK (cost_price >= 0),
  track_stock         boolean NOT NULL DEFAULT false,
  stock               integer NOT NULL DEFAULT 0,
  low_stock_threshold integer NOT NULL DEFAULT 5,
  is_active           boolean NOT NULL DEFAULT true,
  created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sports_kantin_sales (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  total         numeric(12,2) NOT NULL CHECK (total >= 0),
  method        text NOT NULL DEFAULT 'cash', -- cash | qris
  customer_name text NOT NULL DEFAULT '',
  note          text NOT NULL DEFAULT '',
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sports_kantin_sale_items (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    text REFERENCES clients(tenant_id) ON DELETE CASCADE,
  sale_id      uuid NOT NULL REFERENCES sports_kantin_sales(id) ON DELETE CASCADE,
  product_id   uuid REFERENCES sports_kantin_products(id) ON DELETE SET NULL,
  product_name text NOT NULL,
  qty          integer NOT NULL CHECK (qty > 0),
  unit_price   numeric(12,2) NOT NULL,
  unit_cost    numeric(12,2) NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS sports_kantin_stock_movements (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  product_id uuid NOT NULL REFERENCES sports_kantin_products(id) ON DELETE CASCADE,
  delta      integer NOT NULL,
  reason     text NOT NULL, -- sale | restock | waste | correction | void
  note       text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS sports_kantin_sales_tenant_created_idx
  ON sports_kantin_sales(tenant_id, created_at);
CREATE INDEX IF NOT EXISTS sports_kantin_items_sale_idx
  ON sports_kantin_sale_items(sale_id);

-- Backfill path for DBs created before tenant_id was added to
-- sale_items (idempotent).
ALTER TABLE sports_kantin_sale_items
  ADD COLUMN IF NOT EXISTS tenant_id text REFERENCES clients(tenant_id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS sports_kantin_movements_product_idx
  ON sports_kantin_stock_movements(product_id, created_at);

ALTER TABLE sports_kantin_products        ENABLE ROW LEVEL SECURITY;
ALTER TABLE sports_kantin_sales           ENABLE ROW LEVEL SECURITY;
ALTER TABLE sports_kantin_sale_items      ENABLE ROW LEVEL SECURITY;
ALTER TABLE sports_kantin_stock_movements ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS sports_kantin_products_isolation ON sports_kantin_products;
CREATE POLICY sports_kantin_products_isolation ON sports_kantin_products
  FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS sports_kantin_sales_isolation ON sports_kantin_sales;
CREATE POLICY sports_kantin_sales_isolation ON sports_kantin_sales
  FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS sports_kantin_items_isolation ON sports_kantin_sale_items;
CREATE POLICY sports_kantin_items_isolation ON sports_kantin_sale_items
  FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS sports_kantin_movements_isolation ON sports_kantin_stock_movements;
CREATE POLICY sports_kantin_movements_isolation ON sports_kantin_stock_movements
  FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON sports_kantin_products TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON sports_kantin_sales TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON sports_kantin_sale_items TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON sports_kantin_stock_movements TO app_runtime;
