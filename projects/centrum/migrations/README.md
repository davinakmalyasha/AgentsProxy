# Centrum migrations

Centrum now owns its business tables in `project_centrum`. The historical
platform chain created them in `public`; `0001_move_to_project_schema.sql`
is the one-time move that relocates them and rewrites the definer functions
that referenced `public.sports_*`.

Centrum's frontend uses `/v1/p/centrum/*`. The project folder and schema are
the real boundary; there is no project-owned legacy route alias.

Project migrations:
- `0001_move_to_project_schema.sql` — move tables out of `public`, rewrite
  definer functions.
- `0002_tenant_records.sql` — create the always-on `tenant_records` table
  that the project router mounts.
- `0003_force_rls_kantin.sql` — add the missing `FORCE ROW LEVEL SECURITY`
  to the three kantin tables.
- `0004_schema_relative_auth.sql` — resolve the tenant schema from
  `clients.db_schema` in the auth, money-summary, retention, and trigger
  functions; sessions move to `public.staff_sessions`.
- `0005_drop_legacy_sessions.sql` — contract step: drop the old
  project-scoped session table after the binary uses the platform directory.
- `0006_grant_money_reader_clients.sql` — let the money definer resolve
  `clients.db_schema`.
- `0007_staff_payments_to_project.sql` — create
  `project_centrum.sports_staff_payments` and copy the public ledger.
- `0008_money_summary_project.sql` — add the invoker money aggregate for the
  agent tools.
- `0009_contract_staff_payments.sql` — copy late payroll rows, then drop the
  public ledger.
- `0010_contract_legacy_auth.sql` — drop the schema-scanning auth functions
  and remove `auth_definer`'s project-data grants.
- `0011_contract_range_summary.sql` — drop `private.range_summary` and retire
  `money_reader`.

The contract migrations live in the project chain on purpose. The runner
applies platform migrations before project migrations, and Centrum `0004` /
`0006` recreate the legacy functions and grants. Cleanup therefore has to run
after `0008`, including on a fresh database.

New migrations for Centrum go here. Because `project.json` sets
`"auto_grant": false`, a new table must grant `app_runtime` explicitly:
Centrum's original grants are deliberate. Staff sessions are platform state
in `public.staff_sessions`; project users remain in this schema.
