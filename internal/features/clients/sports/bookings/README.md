# Sports — bookings feature

> **Status:** live (Phase 4 pilot vertical)

## Owner

- Tenant: sports (vertical)
- Person: platform team
- Created: 2026-09-01

## Why

Bookings are the heart of the sports vertical and the hardest
concurrency problem on the platform: two people can try to book the
same court at the same time. This is solved at the DATABASE level with
an exclusion constraint — no application code can accidentally
double-book, no matter how concurrent the requests are.

## Schema

```sql
CREATE TABLE sports_bookings (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      text NOT NULL REFERENCES clients(tenant_id) ON DELETE CASCADE,
  facility_id    uuid NOT NULL REFERENCES sports_facilities(id) ON DELETE RESTRICT,
  date           date NOT NULL,
  start_time     time NOT NULL,
  end_time       time NOT NULL,
  customer_name  text NOT NULL,
  customer_phone text NOT NULL DEFAULT '',
  status         text NOT NULL DEFAULT 'confirmed', -- confirmed|cancelled|checked_in
  price          numeric(10,2) NOT NULL DEFAULT 0,
  deposit        numeric(10,2) NOT NULL DEFAULT 0,
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT sports_bookings_no_overlap
    EXCLUDE USING gist (
      tenant_id WITH =,
      facility_id WITH =,
      tsrange(date + start_time, date + end_time) WITH &&
    ),
  CHECK (end_time > start_time)
);
-- RLS policy sports_bookings_isolation (tenant_id = app.tenant_id)
-- GRANT SELECT, INSERT, UPDATE, DELETE ON sports_bookings TO app_runtime;
```

Why `tsrange` and NOT `tstzrange`: the implicit timestamp→timestamptz
cast in `tstzrange` is timezone-dependent (not IMMUTABLE), which
Postgres forbids in index expressions. Venue slots are local-time
anyway — timezone has no business meaning here.

The range is half-open `[start, end)`: 19:00–20:00 and 20:00–21:00
back-to-back is allowed; 19:00–20:00 and 19:30–20:30 conflict.

## Endpoints

```
POST   /v1/sports/bookings                       create (409 "slot already booked" on overlap)
GET    /v1/sports/bookings?date=YYYY-MM-DD[&facility_id=&status=]  schedule view
GET    /v1/sports/bookings/{id}                  get one
PATCH  /v1/sports/bookings/{id}                  update status/price/deposit/customer
DELETE /v1/sports/bookings/{id}                  soft delete → status=cancelled
```

On a confirmed create, the handler calls `NotifyFunc` (wired to the
notify queue in main.go) to send the customer a WhatsApp message.

## Tenants using it

- sports (pilot, Phase 4)

## Mandatory checklist

- [x] Migration + own table + RLS policy + app_runtime grant
- [x] VerticalSlugs = ["sports"]
- [x] Name lowercase, no collision ("bookings")
- [x] Kind empty (custom table, not tenant_records)
- [x] Exclusion constraint prevents double-booking at the DB level
- [x] Isolation test green (TestBookings_TenantIsolation)
- [x] Double-booking concurrency test green (TestBookings_DoubleBooking)
- [x] README filled out
