# Centrum migrations

Centrum now owns its business tables in `project_centrum`. The historical
platform chain created them in `public`; `0001_move_to_project_schema.sql`
is the one-time move that relocates them and rewrites the definer functions
that referenced `public.sports_*`.

Centrum's frontend now uses `/v1/p/centrum/*`; `/v1/sports/*` remains a
one-release rollback alias. The project folder and schema are the real
boundary.

Project migrations:
- `0001_move_to_project_schema.sql` — move tables out of `public`, rewrite
  definer functions.
- `0002_tenant_records.sql` — create the always-on `tenant_records` table
  that the project router mounts.
- `0003_force_rls_kantin.sql` — add the missing `FORCE ROW LEVEL SECURITY`
  to the three kantin tables.

New migrations for Centrum go here. Because `project.json` sets
`"auto_grant": false`, a new table must grant `app_runtime` explicitly:
Centrum's original grants are deliberate, especially `sports_sessions`,
which stays SELECT-only for the runtime role.
