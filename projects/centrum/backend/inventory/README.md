# Sports — inventory feature

> **Status:** live (Phase 4 pilot vertical)

## Owner

- Tenant: sports (vertical)
- Person: platform team
- Created: 2026-09-01

## Why

Food/stock items sold at the venue counter need quantity tracking +
a low-stock threshold, which need real INT columns and atomic
increments (a concurrent sale must never drive stock negative).

## Schema

```sql
CREATE TABLE sports_inventory (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id           text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  name                text NOT NULL,
  sku                 text NOT NULL DEFAULT '',
  quantity            int  NOT NULL DEFAULT 0,
  unit_price          numeric(10,2) NOT NULL DEFAULT 0,
  low_stock_threshold int  NOT NULL DEFAULT 5,
  status              text NOT NULL DEFAULT 'active', -- active|paused
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now()
);
-- RLS policy sports_inventory_isolation (tenant_id = app.tenant_id)
-- GRANT SELECT, INSERT, UPDATE, DELETE ON sports_inventory TO app_runtime;
```

## Endpoints

```
POST   /v1/p/centrum/inventory                create (name required, quantity >= 0)
GET    /v1/p/centrum/inventory                list (with low_stock flag)
GET    /v1/p/centrum/inventory/{id}           get one
PATCH  /v1/p/centrum/inventory/{id}           update name/sku/price/threshold/status
DELETE /v1/p/centrum/inventory/{id}           hard delete
POST   /v1/p/centrum/inventory/{id}/adjust    {delta} — positive=restock, negative=sale
                                           (409 "insufficient stock" if it'd go negative)
```

## Tenants using it

- sports (pilot, Phase 4)

## Mandatory checklist

- [x] Migration + own table + RLS policy + app_runtime grant
- [x] VerticalSlugs = ["sports"]
- [x] Name lowercase, no collision ("inventory")
- [x] Kind empty (custom table, not tenant_records)
- [x] Quantity never negative (atomic UPDATE guard)
- [x] Isolation test green (TestInventory_TenantIsolation)
- [x] README filled out
