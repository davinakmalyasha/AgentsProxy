# Sports — facilities feature

> **Status:** live (Phase 4 pilot vertical)

## Owner

- Tenant: sports (vertical)
- Person: platform team
- Created: 2026-09-01

## Why

Bookable courts/fields/pools are configuration with real column types
(TIME, NUMERIC) referenced by bookings, so they live in their own
table (`sports_facilities`) instead of `tenant_records` JSONB.

## Schema

```sql
CREATE TABLE sports_facilities (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  name            text NOT NULL,
  type            text NOT NULL DEFAULT 'other',  -- badminton|soccer|swimming|padel|other
  capacity        int  NOT NULL DEFAULT 1,
  price_per_hour  numeric(10,2) NOT NULL DEFAULT 0,
  open_time       time NOT NULL DEFAULT '06:00',
  close_time      time NOT NULL DEFAULT '22:00',
  status          text NOT NULL DEFAULT 'active', -- active|paused
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
-- RLS policy sports_facilities_isolation (tenant_id = app.tenant_id)
-- GRANT SELECT, INSERT, UPDATE, DELETE ON sports_facilities TO app_runtime;
```

## Endpoints

```
POST   /v1/sports/facilities            create (name required, type allow-list, open<close)
GET    /v1/sports/facilities            list (ordered by name)
GET    /v1/sports/facilities/{id}       get one
PATCH  /v1/sports/facilities/{id}       update any field
DELETE /v1/sports/facilities/{id}       hard delete (fails 500 if bookings reference it)
```

## Tenants using it

- sports (pilot, Phase 4)

## Mandatory checklist

- [x] Migration + own table + RLS policy + app_runtime grant
- [x] VerticalSlugs = ["sports"]
- [x] Name lowercase, no collision ("facilities")
- [x] Kind empty (custom table, not tenant_records)
- [x] ValidatePayload-equivalent validation in handlers
- [x] Isolation test green (TestFacilities_TenantIsolation)
- [x] README filled out
