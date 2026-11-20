# Centrum migrations

Centrum now owns its business tables in `project_centrum`. The historical
platform chain created them in `public`; `0001_move_to_project_schema.sql`
is the one-time move that relocates them and rewrites the definer functions
that referenced `public.sports_*`.

Centrum still serves its legacy `/v1/sports/*` API until the frontend cutover.
The project folder and schema are already the real boundary.

New migrations for Centrum go here. Because `project.json` sets
`"auto_grant": false`, a new table must grant `app_runtime` explicitly:
Centrum's original grants are deliberate, especially `sports_sessions`,
which stays SELECT-only for the runtime role.
