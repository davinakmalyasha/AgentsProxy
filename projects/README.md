# Projects

One folder per client project. A project and a tenant are the same identity:
`project.json`'s `slug` is `clients.tenant_id`.

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
  `project.json` and put tables in `migrations/`. The runtime sets
  `search_path` from `clients.db_schema`.
- **Centrum is the legacy route exception.** Its business tables live in
  `project_centrum` (moved by its `0001` project migration), but its frontend
  still calls `/v1/sports/...` until the route cutover. Every project,
  including Centrum, uses its own schema; `public` is platform-only.
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
- **Every project table carries `tenant_id` and RLS.** The schema is the
  physical boundary; RLS is the second lock.

## Adding a project

1. Create `projects/<slug>/project.json` with a unique slug and
   `"schema": "project_<slug>"` (replace `-` with `_`).
2. Add `migrations/0001_init.sql`. Create the tables this project needs.
   Enable RLS and add a `tenant_id` policy on every table.
3. Add `backend/<feature>/` packages. Register them with
   `features.RegisterProject("<slug>", "<vertical>", ...)`. Mark
   `PublicRead: true` only on GET routes that are safe for the landing page
   (facilities, pricing, availability).
4. Add `agent/agent.md`, `agent/skills/*.md`, `agent/mcp.json`, and optional
   `agent/tools/*.go`.
5. Run `go generate ./...` and commit the generated imports.
6. Provision the client row with `POST /v1/admin/clients` (or the tenant CLI
   once it lands). The admin endpoint creates the schema and returns the
   client token once.
7. Run `go run ./cmd/migrate` to apply platform and project migrations.
8. Deploy the frontend with its own `NEXT_PUBLIC_TENANT_ID` and the new
   `/v1/p/<slug>/...` base.
