# Custom Feature Template

> **Status:** _template_ — copy this folder to start a new per-tenant feature.

## Naming

- Folder: `internal/features/clients/<tenant-slug>/`
- The folder name is a tenant slug, **not** a feature name. Two tenants
  may use the same feature (e.g. "loyalty") but each gets its own folder
  if their contracts differ.

## Required files

1. **`feature.go`** — the registration, payload contract, and optional routes.
2. **`feature_test.go`** — the **mandatory** isolation test (tenant A
   cannot see tenant B's records). A placeholder lives here; expand it
   for your feature. **A custom feature without a passing isolation
   test does not ship.**
3. **`README.md`** — this file, edited per feature.

## Mandatory checklist (custom feature)

- [ ] Migration with its own table (or shared `tenant_records` with a
      unique `kind` value) + RLS policy if it adds a table.
- [ ] Vertical allow-list set in `VerticalSlugs`.
- [ ] `Name` lowercase, short, no collisions with shared features.
- [ ] `Kind` distinct from any other feature's kind.
- [ ] `ValidatePayload` enforces every required field.
- [ ] Isolation test green.
- [ ] README filled out below.

## Status

- **live** — running in production for the tenant.
- **promoted** — used by ≥3 tenants for ≥2 months; considered for
  promotion to `internal/features/shared/`. Promotion is a code move +
  docs update; no schema changes if the kind stays the same.
- **archived** — feature is disabled for all clients. Keep the code
  around in case a tenant wants it back; do not delete migrations.

## Owner

- Tenant: <slug>
- Person: <who maintains it>
- Created: <date>
- Last touched: <date>

## Why

One paragraph on the use case. If this is a custom table (not a
`tenant_records` kind), describe the table shape, the access pattern,
and why the generic records engine wasn't enough.

## Tenants using it

- tenant-slug-1 (live since ...)
- tenant-slug-2 (live since ...)

## Schema

```sql
-- Paste the migration here, or the kind value if you use tenant_records.
```

## Endpoints

```
POST   /v1/{vertical}/{name}
GET    /v1/{vertical}/{name}
GET    /v1/{vertical}/{name}/{id}
PATCH  /v1/{vertical}/{name}/{id}
DELETE /v1/{vertical}/{name}/{id}
```

## Promotion rules

When this feature has been live for 2+ months for 3+ tenants, open a PR
to move it to `internal/features/shared/{name}/`. Steps:

1. Move the folder, preserving `feature.go` and `feature_test.go`.
2. Update `feature.go` to remove the tenant-specific name in the README.
3. Run the full test suite; cover the vertical-defaults map in
   `internal/features/defaults.go` if the feature should be on by default.
4. Update `DONE.md` with the new shared feature.
