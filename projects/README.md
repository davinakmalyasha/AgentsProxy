# Projects

One folder per client project. A project and a tenant are the same identity:
`projects/<slug>/`'s directory name and `project.json`'s `slug` are both
`clients.tenant_id`; the loader rejects a mismatch.

```text
projects/<slug>/
  project.json          identity, schema, feature/agent settings
  migrations/           project-owned SQL, applied in the project schema
  backend/              project HTTP features (Go)
    <feature>/
  agent/
    agent.md            persona + house rules
    skills/*.md         procedures, injected whole
    mcp.json            external MCP servers
    tools/*.go          project-specific tools only
```

Rules:

- **No shared business code.** A project owns its handlers, tables, skills,
  and tools. Platform packages under `internal/` are primitives (auth, DB,
  RLS, router, agent loop, MCP client), not client business logic.
- **New projects get their own schema.** Set `"schema": "project_<slug>"` (replace `-` with `_`) in
  `project.json` and put tables in `migrations/`. The schema must match
  `^project_[a-z0-9_]{1,55}$`; `public` and platform schemas such as `auth`
  or `private` are rejected by config, the admin API, and the database.
  Admin provisioning and `migrate check` enforce one client per project
  schema; `clients.project_key` is the partial unique owner key, so concurrent
  admin calls cannot claim the same schema. The runtime sets `search_path`
  from `clients.db_schema`.
- **Centrum is the route-transition exception.** Its business tables live in
  `project_centrum` (moved by its `0001` project migration). Its frontend now
  uses `/v1/p/centrum/...`; both registries stay populated so
  `/v1/sports/...` remains a rollback surface for one release. Remove the
  legacy registration after the stability window. Every project, including
  Centrum, uses its own schema; `public` is platform-only.
- **`auto_grant` defaults to true.** New projects get the blanket
  `app_runtime` DML grant after migrations. Centrum sets it false because its
  grants are deliberate: `sports_sessions` stays SELECT-only for the runtime
  role, so the definer session functions remain the only write path.
- **New project APIs are mounted at `/v1/p/<slug>/...`.** Two projects may
  both own a feature named `bookings` without colliding.
- **Add a project folder, then run `go generate ./...`.** The generated
  `cmd/server/projects_gen.go` carries the blank imports; do not edit it by
  hand.
- **Agent work is configuration first.** A new project normally adds
  `agent.md`, skills, `mcp.json`, and a tool allow-list. Write a Go tool only
  when the project needs an action no platform tool or MCP server provides.
- **`agent.model` is optional.** It overrides the platform model for this
  project's ADK agent only. Leave it unset to share the default provider
  client. The raw `/v1/chat` endpoint keeps its own allowlist.
- **MCP servers are opt-in and staff-only by default.** `agent/mcp.json`
  lists external HTTP endpoints. `tools` is a remote allow-list (empty means
  all). `roles` defaults to `owner` + `pegawai`; adding `client` or `anon` is
  a deliberate decision to expose external actions to that tier. Header
  values may use `${ENV_VAR}` and are resolved at connection time, so tokens
  do not belong in `project.json`.
- **Every project table carries `tenant_id` and `ENABLE+FORCE RLS`.** The
  schema is the physical boundary; RLS is the second lock. `migrate check`
  fails if any table in a project schema is missing either flag.
- **Records is the core feature.** The project router mounts `/records`
  unconditionally, so every project schema must have `tenant_records` with
  the same tenant policy (see Centrum's `0002_tenant_records.sql`). If a
  project truly should not expose records, change the router contract first;
  don't leave the route pointing at a missing table.

Example `agent/mcp.json`:

```json
{
  "servers": [
    {
      "name": "weather",
      "url": "https://mcp.example.com/mcp",
      "tools": ["forecast"],
      "roles": ["owner", "pegawai"],
      "headers": { "Authorization": "Bearer ${WEATHER_MCP_TOKEN}" }
    }
  ]
}
```

The agent sees remote tools as `mcp_<server>_<tool>` (for example
`mcp_weather_forecast`), so they cannot collide with native tool names. A
server that is down is logged and skipped; native tools keep working, and the
connection is retried after 30 seconds. Results are capped at 16 KB and calls
time out at 15 seconds.

## Removing or renaming a project

Decommission is deliberate and never part of `migrate apply`:

1. Export the project schema (`pg_dump --schema=project_<slug>`) and keep it
   with the client's records.
2. Pause the client (`PATCH /v1/admin/clients/{id} {"status":"paused"}`) and
   remove its frontend origins; stop the frontend.
3. Delete the `clients` row. Its `tenant_id` rows cascade in platform and
   project tables.
4. Drop the project schema manually (`DROP SCHEMA project_<slug> CASCADE`)
   only after the export is verified.
5. Remove `projects/<slug>/`, run `go generate ./...`, and delete the
   frontend/DNS entries.
6. Run `go run ./cmd/migrate check`; it fails if a client or ledger row still
   points at a schema with no project folder.

Renaming a project is a migration, not a folder rename: the ledger records
the schema each version ran in, and `migrate apply` refuses a schema change
without a deliberate export/move procedure.

## Adding a project

1. Create `projects/<slug>/project.json` with a unique slug and
   `"schema": "project_<slug>"` (replace `-` with `_`).
2. Add `migrations/0001_init.sql`. Create the tables this project needs,
   including `tenant_records` for the always-on records surface. Enable RLS,
   add `FORCE ROW LEVEL SECURITY`, and add a `tenant_id` policy on every
   table. The migration ledger records the schema, so changing
   `project.json` after the first apply is a deliberate migration procedure,
   not a config edit.
3. Add `backend/<feature>/` packages. Register them with
   `features.RegisterProject("<slug>", "<vertical>", ...)`. Mark
   `PublicRead: true` only on GET routes that are safe for the landing page
   (facilities, pricing, availability).
4. Add `agent/agent.md`, `agent/skills/*.md`, `agent/mcp.json`, and optional
   `agent/tools/*.go`.
5. Run `go generate ./...` and commit the generated imports.
6. Provision the client row with
   `go run ./cmd/tenant create --slug <slug> --name "<name>" --vertical <vertical>`.
   The CLI refuses to create a project tenant unless the folder exists;
   `--allow-missing-project` exists only for a legacy/platform row. The admin
   endpoint creates the schema and returns the client token once.
7. Run `go run ./cmd/migrate` to apply platform and project migrations, then
   `go run ./cmd/migrate check`.
8. Deploy the frontend with its own `NEXT_PUBLIC_TENANT_ID` and the new
   `/v1/p/<slug>/...` base.
