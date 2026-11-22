# Sports — members feature

> **Status:** live (Phase 4 pilot vertical)

## Owner

- Tenant: sports (vertical)
- Person: platform team
- Created: 2026-09-01

## Why

Regular customers with a prepaid balance need a NUMERIC column with
atomic top-ups, so they live in their own table (`sports_members`)
instead of `tenant_records` JSONB.

## Schema

```sql
CREATE TABLE sports_members (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  name       text NOT NULL,
  phone      text NOT NULL DEFAULT '',
  balance    numeric(10,2) NOT NULL DEFAULT 0,
  status     text NOT NULL DEFAULT 'active', -- active|paused
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
-- RLS policy sports_members_isolation (tenant_id = app.tenant_id)
-- GRANT SELECT, INSERT, UPDATE, DELETE ON sports_members TO app_runtime;
```

## Endpoints

```
POST   /v1/p/centrum/members                create (name required, balance >= 0)
GET    /v1/p/centrum/members                list
GET    /v1/p/centrum/members/{id}           get one
PATCH  /v1/p/centrum/members/{id}           update name/phone/status
DELETE /v1/p/centrum/members/{id}           hard delete
POST   /v1/p/centrum/members/{id}/topup     {amount} — amount > 0, atomic balance += amount
```

## Tenants using it

- sports (pilot, Phase 4)

## Mandatory checklist

- [x] Migration + own table + RLS policy + app_runtime grant
- [x] VerticalSlugs = ["sports"]
- [x] Name lowercase, no collision ("members")
- [x] Kind empty (custom table, not tenant_records)
- [x] Balance guarded (topup > 0 only)
- [x] Isolation test green (TestMembers_TenantIsolation)
- [x] README filled out
