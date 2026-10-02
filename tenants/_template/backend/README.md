# backend/ — one Go package per app feature

A new tenant starts with an empty backend/ folder. To add a feature,
copy any package from the centrum tenant's backend/ and adapt it:

- The package self-registers via `init()` (`features.Register(...)`
  with its `VerticalSlugs`); the vertical router discovers it from the
  registry, so no route table needs editing.
- The package is compiled in through a blank import in
  cmd/server/main.go, next to the other tenants' backend packages.
- Tenant isolation is by `tenant_id` + RLS in every query, never by
  code copy: shared tables stay shared, rows stay separated.
- Feature tests live next to the code (`feature_test.go`) with the
  shared `testutil` harness pattern.
