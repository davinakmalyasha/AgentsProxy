-- 044_sports_kantin_tenant_indexes.sql — fill tenant-leading index gaps on
-- the kantin tables. Every tenant-scoped query filters tenant_id first; without
-- a leading-tenant_id index these degrade to seq scan + RLS filter at scale.
-- Additive only (GOVERNANCE §3). Plain CREATE INDEX (not CONCURRENTLY):
-- the boot runner applies migrations inside one transaction, and CONCURRENTLY
-- cannot run in a transaction block. Tables are small; the lock is brief.

CREATE INDEX IF NOT EXISTS sports_kantin_products_tenant_idx
  ON sports_kantin_products(tenant_id);
CREATE INDEX IF NOT EXISTS sports_kantin_items_tenant_sale_idx
  ON sports_kantin_sale_items(tenant_id, sale_id);
CREATE INDEX IF NOT EXISTS sports_kantin_movements_tenant_product_idx
  ON sports_kantin_stock_movements(tenant_id, product_id, created_at);
