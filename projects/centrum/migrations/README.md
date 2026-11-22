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

New migrations for Centrum go here. Because `project.json` sets
`"auto_grant": false`, a new table must grant `app_runtime` explicitly:
Centrum's original grants are deliberate. Staff sessions are platform state
in `public.staff_sessions`; project users remain in this schema.
