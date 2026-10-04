# refinementNotes — AgentsProxy

> **Purpose.** The critical review and the work queue. What is wrong, what to build next,
> and what is available today that this project is not yet using.
>
> **This is a working document.** It supersedes `PRD.md`, `STANDARDS.md` and `DONE.md`,
> which were folded into the three project docs on 2026-10-03.
>
> - Doc 1 — `README.md` → *what this project is*
> - Doc 2 — `ARCHITECTURE.md` → *how it is built, and why*
> - Doc 3 — `docs/refinementNotes.md` → *this file: what to fix and what to add*

**Audit baseline (2026-10-03):** Go 1.27.0 · one Go module · 173 `.go` files · ~28,000 LOC
(17,000 prod / 11,000 test) · 61 test files · 40 SQL migrations · 13 verticals · 53 commits,
single author · 3 GitHub workflows.

> **Note (2026-10-03, docs-truth pass A1 — history below preserved):**
> Telegram is deleted (`internal/telegram/` gone, `042_drop_telegram.sql` drops
> `telegram_bots` / `agent_reminders.bot_id`) and `internal/mcp` is deleted
> (no `/v1/mcp` route). Rows below that describe Telegram/MCP as current
> (e.g. §1 Messaging, C1, R2–R3, §5.6) are the pre-removal audit record.

**Legend** — Severity: `HIGH` (correctness, money, or security) · `MED` (maintainability,
operability) · `LOW` (hygiene). Effort: `S` ≤ ½ day · `M` 1–3 days · `L` > 1 week.

---

## 1. Stack snapshot

Condensed so this file is readable on its own. Full rationale lives in `ARCHITECTURE.md` §1.

| Layer | Choice | Version |
|---|---|---|
| Language | Go | 1.27.0 (`go.mod:3`) |
| Router | `go-chi/chi/v5` over `net/http` | v5.3.2 |
| DB driver | `jackc/pgx/v5` + `pgxpool` | v5.10.0 |
| DB (prod) | Supabase Postgres, one shared project | 17 |
| DB (local/CI) | Laragon / `postgres:16-alpine` | 16 |
| Cache | `redis/go-redis/v9` | v9.22.0 |
| Cache server | Redis (local/CI `redis:8-alpine`) | 8.x |
| Multi-tenancy | Shared schema + `tenant_id` + Postgres RLS | — |
| LLM client | `openai/openai-go/v3` (DeepSeek-compatible) | v3.64.0 |
| LLM (experimental) | `google.golang.org/genai`, `adk/v2` spike | v1.71.0 / v2.5.0 |
| Auth | bcrypt staff sessions, `X-Session-Token` + `X-Client-Token`, `ADMIN_API_KEY` | — |
| Secrets at rest | AES-256-GCM token vault (`internal/tgvault`), versioned keyring | — |
| Messaging | Telegram multi-bot (webhooks prod / polling dev) | — |
| Reports | `xuri/excelize/v2` | v2.11.0 |
| Observability | Sentry (`getsentry/sentry-go`); OTel only transitively via ADK | v0.49.0 |
| Async | `notify_jobs` + `agent_reminders`, atomic SQL claim; Go tickers **or** pg_cron + Deno Edge Functions | — |
| Frontend | **not in this repo** — Next.js apps live elsewhere and call this API | — |
| CI/CD | GitHub Actions: `ci.yml`, `security.yml`, `release.yml` (GHCR multi-arch) | — |
| Deploy | Vercel Go runtime (`maxDuration: 300`, `sin1`) **or** Docker/distroless → Railway/VPS | — |

---

## 2. Criticism

Ordered by severity. Every item cites the file that proves it.

### C1 — `HIGH` — The highest-privilege code in the repo has no tests

`internal/mcp/admin.go` is **252 LOC** of cross-tenant `admin_*` MCP tools, reachable with
`Authorization: Bearer $ADMIN_API_KEY` — the same key that creates clients and sets quotas.
`internal/mcp/server.go` has 4 tests; `admin.go` has **none**.

`internal/features/clients/sports/telegram/bots.go` is **334 LOC** that create, encrypt
(AES-GCM), and register Telegram bot tokens, validate them against `getMe`, and call
`setWebhook`. **Zero tests.** A regression here leaks or bricks tenant bot credentials.

`cmd/server/main.go` (**322 LOC**) — the entire dependency graph, the `app_runtime` vs admin
pool split, project validation, all five background goroutines, graceful shutdown. **Zero tests.**

> Why this is the top item: RLS is the tenant boundary and it *is* tested. These three files
> sit *above* RLS. A bug in `admin.go` is not contained by the database.

### C2 — `HIGH` — The documented quality gate does not exist

`STANDARDS.md` §B claimed `test -cover ≥70%` as a pre-commit requirement and §C claimed it
was "enforced". `ci.yml` enforces **no coverage threshold at all** — it only writes the
number to `$GITHUB_STEP_SUMMARY`. The real numbers (`DONE.md` §Coverage):

| Package | Coverage | Package | Coverage |
|---|---|---|---|
| `internal/db` | 26.6% | `internal/features/shared/records` | 44.8% |
| `internal/sentry` | 27.5% | `internal/features/.../inventory` | 44.6% |
| `internal/telegram` | 32.9% | `internal/features/.../members` | 44.5% |
| `internal/agent` | 37.0% | `internal/features/.../bookings` | 67.6% |

Two of the three lowest-covered packages are the **agent tool loop** and the **Telegram
transport** — the two subsystems that make this product agentic rather than CRUD.

### C3 — `HIGH` — No static analysis beyond `go vet`

There is **no `.golangci.yml`, no `.editorconfig`, no pre-commit hook, no `staticcheck`**, and
no `lint` target in the `Makefile`. Lint = `gofmt -l` + `go vet`. That leaves unchecked:
unhandled errors (`errcheck`), ineffectual assignments, unused code, leaked HTTP response
bodies, weak crypto/rand use (`gosec`).

`STANDARDS.md` §I also mandated a `README.md` per feature; 5 exist, 10 do not. The docs and
the practice had already diverged before this audit.

### C4 — `HIGH` — No API specification; the contract has already drifted

The contract lives only in prose, in three places that disagreed with each other and with the
code. A machine-readable contract would have caught every one of these:

| Drift | Documented | Actual |
|---|---|---|
| Rate limit default | `.env.example` `RATE_LIMIT_PER_MINUTE=60` | `internal/config/config.go:71` → `180` |
| DB pool sizing | documented only in `ops/RUNBOOK-vercel.md` | `config.go` reads `DB_POOL_MIN`/`DB_POOL_MAX`; **`.env.example` omits both** |
| Supavisor requirement | `ops/RUNBOOK-vercel.md` only | `DB_USE_SIMPLE_PROTOCOL=true` is mandatory for port 6543 |
| `PROFILES_SAMPLE_RATE` | listed in `.env.example` | **never read** by `config.go` |
| Redis / Postgres | docs say 8.10.1 / PG17 | compose + CI pin `redis:8-alpine` / `postgres:16-alpine` |
| Vercel viability | `PRD.md:6` "**NOT** Vercel, duration limits kill long LLM calls" | `README.md` calls this obsolete; `vercel.json` sets `maxDuration: 300` |
| CI Redis | `PRD.md:48` "`miniredis` in CI" | `ci.yml` runs a real `redis:8-alpine` service |

Consequence: **every consumer repo hand-rolls its own client** against a prose table, with
nothing to catch a breaking change.

### C5 — `MED` — Vercel forces the delivery logic to exist twice

`BACKGROUND_TICKERS=false` (Vercel) means `notify`/`reminder` drain from **Deno Edge
Functions** in `supabase/functions/`. `BACKGROUND_TICKERS=true` (Railway/VPS/Laragon) means
they drain from **Go goroutines**. Two implementations of the same claim protocol, in two
languages, sharing one invariant (`ARCHITECTURE.md` §13.4).

`ARCHITECTURE.md` §13.3 is admirably careful about *correctness* across drivers — but the
correctness argument is spread across 130 lines of prose in a second language's implementation.

Worse, the free tier **pauses idle Supabase projects after 7 days**, and a paused project
stops `pg_cron` — so reminders silently stop. That is documented in
`ops/RUNBOOK-delivery.md` rather than engineered around.

### C6 — `MED` — WhatsApp delivery is a stub in every environment

`notify.Sender` is an interface; `WaMeSender` produces a `wa.me` deep link with **zero
credentials**. `supabase/functions/deliver-notification/index.ts` `sendOne()` is the same
stub. So a booking confirmation never actually reaches a customer anywhere. The interface
boundary is the right design; the implementation is a placeholder that reads as done.

### C7 — `MED` — God files

| File | Size |
|---|---|
| `internal/features/clients/sports/bookings/feature.go` | 39 KB |
| `internal/features/clients/sports/kantin/feature.go` | 32 KB |
| `internal/features/clients/sports/pricing/feature.go` | 17 KB |
| `internal/agent/agent.go` | 17 KB |
| `AgentsTools/reports.go` | 14 KB |
| `internal/features/shared/records/handlers.go` | 12 KB |

Handlers + SQL + validation + state machines + business rules in one file per feature.
`bookings` holds the lifecycle state machine (`pending → confirmed → checked_in →
completed / cancelled / no_show`), pricing resolution, payment-status recalculation, and
availability math together.

### C8 — `MED` — The whole test suite is serialized and needs live infrastructure

`Makefile` `test` target: `go test -p 1 ./... -cover`. `-p 1` is required because integration
tests share one Postgres and race on DELETE/INSERT (`tuple concurrently updated`, XX000).
Every test also needs a real Postgres + Redis. There are no build tags separating unit from
integration, no fixtures, no testcontainers. Every contributor must have Laragon running, and
CI wall-clock scales linearly with package count.

### C9 — `MED` — No migration tooling; `backup.sh` is not scheduled

40 hand-numbered `.sql` files, one of which (`027`) is a **retired no-op stub** left in place,
plus `013b` to break the sort order. No checksums, no `down` path, no drift detection. Migrations
run in a single transaction behind `pg_advisory_xact_lock(734561)` — correct, but it also
means **one bad migration aborts the entire chain including the ones after it**, and on
Vercel that check re-runs on every cold start.

`ops/backup.sh` exists, is well written, and is referenced by **no workflow, no cron, no CI job**.

### C10 — `MED` — No audit log

There is a `usage` ledger (tokens + cost) and soft-delete tombstones, but **no record of who
changed what**. RLS prevents cross-tenant *reads*; it does nothing about an owner deleting
payments or a `pegawai` editing expenses. For a system that handles money and multiplies
across thousands of tenants, an append-only audit trail is a baseline expectation, not a
nice-to-have.

### C11 — `LOW` — Repo and workspace hygiene

- `.gitignore` covers the generated artifacts, but the working tree still carries `dump.rdb`,
  `proxy.log` (768 KB), `data/reports/`, `appendonlydir/`, `shots/`, `.playwright-mcp/`.
- `public/icons/` holds three PWA PNGs that **nothing serves** — no `manifest.json`, no
  static handler, no reference in any `.go` file.
- `.dockerignore` excludes `*.md`, so the images are unaffected; but `.dockerignore` also
  excludes `.env.*`, which would exclude `.env.example` (harmless).
- `scripts/add-done-blocks.ps1` references `internal/api/business.go`, which no longer exists.
- 15 `TODO`/`FIXME`/`HACK` markers across the Go tree, unreviewed.

### C12 — `LOW` — Freshness debt in the doc set

Before this consolidation, six root documents (1,096 lines) described the same system and
contradicted each other on Vercel viability, Redis/PG versions, CI Redis, coverage policy, and
feature-README policy. `RUNBOOK.md` had **two sections numbered `1.5`**, the second appearing
after `§4`. That is what motivated collapsing to three documents.

---

## 3. Current-state baseline

Absorbed from `DONE.md` so the criticism above stays measurable.

### 3.1 Shipped and working

- **Tenancy core** — `clients` (hashed tokens, quota, status), `usage`, `tenant_records`, RLS,
  `app_runtime` non-superuser role split, `adminPool` for DDL.
- **Feature platform** — registry + gate + per-vertical builder; 13 verticals; shared features
  (`records`, `menu`, `booking`, `review`, `article`, `promo`) plus 15 custom `sports` features.
- **Sports vertical** — facilities, bookings (DB-enforced no-overlap), inventory, members,
  payments, pricing, expenses, customers, court blocks, recurring series, kantin (products,
  atomic sales, stock movements), availability.
- **Agent layer** — conversational tool loop over the same RLS path; per-tenant tool registry;
  `AgentsTools/CentrumBadminton` with 19 tools; xlsx reports.
- **Telegram** — multi-bot registry (AES-GCM vault), webhooks in prod / polling in dev, role
  ceilings (`owner` / `pegawai` / open FAQ), per-chat namespaced history, reminders.
- **Durable delivery** — `notify_jobs` + `agent_reminders` with atomic claim; Go ticker driver
  *and* pg_cron + Edge Function driver; `delivery_worker` / `delivery_caller` least-privilege
  roles; Telegram 429 `retry_after` handling; DLQ-style `dead` marking.
- **Staff auth** — bcrypt login, DB sessions, `owner`/`pegawai` role split per request.
- **Ops** — Sentry with PII scrubbing, healthz with DB+Redis probes, graceful shutdown,
  retention janitor, report sweeper, 3 CI workflows, 2 runbooks, `backup.sh`.

### 3.2 Known limitations carried forward

- ⏳ **Streaming SSE** for `/v1/chat` — unimplemented. The largest UX gap.
- 🟡 **Cost accounting is an estimate.** `usage.cost_usd` uses fixed DeepSeek-flash rates.
- 🟡 **No prompt-cache accounting** — see `A1` in §5, this is bigger than it looks.
- 🟡 **RLS policies are PERMISSIVE, not RESTRICTIVE** on `tenant_records`. Correct as
  documented (`ARCHITECTURE.md` §7 rule 4), but it is a deliberate choice a reviewer must
  re-verify on every policy edit.
- 🟡 **Local dev requires a non-superuser `app` role**, bootstrapped idempotently by the test
  harness.
- 🟡 **`notify` delivery has no real transport** (C6).

### 3.3 Migration inventory (40 files)

`001`–`003` tenancy · `004`–`006` features + RBAC · `007`–`010`, `018`, `019` sports venue ·
`014`–`017`, `020` sports money · `021`–`022` sports identity · `013b`, `023`, `028`, `034`,
`035`, `037`–`039` gap-filling (lifecycle, WIB day indexes, venue hours, price recalc, block
guards, kantin voids) · `011`–`013`, `024`–`026` agent + Telegram · `029`–`031`, `036` delivery
· `027` retired stub · `032`, `033`, `040` sessions, RLS, phone.

---

## 4. Recommendations

### Tier 1 — do first (highest value per hour)

| # | Item | Effort | Why |
|---|---|---|---|
| **R1** | Add `.golangci.yml` (`errcheck`, `staticcheck`, `ineffassign`, `unused`, `bodyclose`, `gosec`, `unparam`) + `make lint` + wire into `ci.yml` | S | Fixes C3. Highest-leverage single file in the repo. |
| **R2** | Enforce a real coverage gate in CI: per-package floors, hard floor on `internal/agent`, `internal/mcp`, `internal/telegram` | S | Makes C2 stop being a documentation lie. |
| **R3** | Test `internal/mcp/admin.go`, `sports/telegram/bots.go`, `cmd/server/main.go` wiring | M | Fixes C1 — the actual blast-radius risk. |
| **R4** | Write `openapi.yaml` (3.1) + `oapi-codegen` for request/response types | L | Fixes C4 permanently; also replaces ~15 hand-rolled validators. |
| **R5** | Reconcile `.env.example` with `config.go` (add `DB_POOL_MIN`/`DB_POOL_MAX`/`DB_USE_SIMPLE_PROTOCOL`, drop `PROFILES_SAMPLE_RATE` or read it, fix the rate-limit default) | S | Fixes C4 at the cheapest point. Ships in the same PR as R1. |

### Tier 2 — real capability

| # | Item | Effort | Why |
|---|---|---|---|
| **R6** | **Prompt/prefix cache accounting** (§5.1) | S | Largest recurring-cost lever in the repo. |
| **R7** | SSE streaming for `/v1/chat` and `/v1/agent/*` | M | Closes the longest-standing known gap. |
| **R8** | Real OpenTelemetry: manual spans around LLM, DB, Redis; export OTLP | M | You already pull OTel transitively. Currently no traces of your own. |
| **R9** | Redis token bucket in Lua for rate limiting; `HGETEX` for the client cache | M | `ARCHITECTURE.md` §3.3 already specifies it; §3.2 already specifies `HGETEX`. Code does neither. |
| **R10** | `testing/synctest` for ticker/claim/reaper loops | M | Fixes C8 for the concurrency code; instant + deterministic. |
| **R11** | Golden-file contract tests per endpoint | M | Catches payload drift without a full OpenAPI rollout. |
| **R12** | BRIN indexes + monthly range partitioning on `usage`, `notify_jobs`, `agent_reminders` | M | These are append-only and the retention janitor `DELETE`s them. |

### Tier 3 — architecture & operations

| # | Item | Effort | Why |
|---|---|---|---|
| **R13** | Decompose the god files (`bookings`, `kantin`) into `handlers/store/service/validate` | L | Fixes C7. |
| **R14** | Single delivery driver — or an explicit, tested parity contract between the Go and Deno implementations | L | Fixes C5. Lower priority if you stay on one deploy shape. |
| **R15** | Real WhatsApp/Twilio `Sender` + dead-letter view | M | Fixes C6. Bookings are the product. |
| **R16** | Atlas or Goose for migrations (checksums, `down`, drift detection) | M | Fixes C9. |
| **R17** | Append-only `audit_log` table + read endpoint | M | Fixes C10. |
| **R18** | Schedule `ops/backup.sh` from a GitHub Actions cron with a restore smoke test | S | Fixes the other half of C9 — backups that are not scheduled are not backups. |
| **R19** | Split unit vs integration tests with build tags; containerize the integration lane | L | Fixes C8. |
| **R20** | Session rotation + refresh, optional TOTP for `owner` | M | 794 LOC of bespoke auth with no rotation. |
| **R21** | Global/env feature kill switch, not only `clients.features[]` | S | Cheap insurance for a new vertical. |
| **R22** | Delete `public/icons/` or serve it with a manifest; refresh `scripts/add-done-blocks.ps1` | S | Fixes C11. |

---

## 5. Things this project probably does not know it should be using

### 5.1 Prompt/prefix caching — a ~30x input-cost discount, currently unmeasured

**This is the highest-value finding in the audit.**

DeepSeek (the configured `LLM_BASE_URL`) does **automatic prefix caching**. The API returns
`usage.prompt_cache_hit_tokens` and `usage.prompt_cache_miss_tokens`, billed at a steep
discount versus uncached input — for `deepseek-v4-pro`, roughly **$0.022/M cached vs $0.66/M
uncached off-peak (~30x)**; Flash is in the same ratio band. *Verify current rates against
the provider's pricing page before modelling this — these move, and DeepSeek also applies
peak/off-peak multipliers.*

**What the code does today:** nothing.

- `internal/api/chat.go:50-51` and `internal/llm/client.go:115-116` declare only
  `PromptTokens` and `CompletionTokens`.
- The provider's cache fields are discarded at the decode boundary.
- `usage.cost_usd` therefore cannot distinguish a 30x-cheap input from an expensive one.

**Why the prompt is already shaped correctly:** prefix caching requires a byte-identical
prefix from token 0. `internal/agentconfig` loads a per-tenant `SystemPrompt.md`
(budgeted to 8,000 runes), plus skills and tool definitions — all **static per tenant** and
all prepended. Measured provider behaviour on a stable prefix is a 98–99% hit rate; a single
varying first line drops it to **0%**.

**So the work is small and the payoff is large:**

1. Parse `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens`; assert
   `hit + miss == prompt_tokens` (the API contract) and fail loudly if not.
2. Persist both to `usage`; add a cache-hit-ratio column to `GET /v1/admin/usage`.
3. Compute `cost_usd` from a real price table (hit / miss / output rates) instead of the
   fixed estimate — closing `DONE.md`'s standing "cost is an estimate" limitation.
4. **Guard the prompt order.** Assert in a test that the first N bytes of the assembled
   request are byte-stable per tenant and that no timestamp, request ID, chat ID, or tenant
   lookup result is prepended. This is the single most common way teams silently lose the
   discount, and it fails *silently* — no error, no premium, just a discount that never arrives.
5. Report peak/off-peak split — DeepSeek applies time-of-day multipliers, so shifting
   non-interactive work (reminders, report generation) off-peak is free money.

> Context for why this matters more here than in a typical chat app: an agent tool loop
> re-sends the entire transcript plus tool results on **every** step. Prompt size grows with
> turn count. Prefix discipline is therefore not a micro-optimization — it is the difference
> between a flat and a superlinear per-conversation bill.

### 5.2 `testing/synctest` — deterministic tests for the concurrency code

Stable since Go 1.25; Go 1.27 adds `synctest.Sleep(d)` (= `Sleep` + `Wait`).

This repo runs **five background goroutines** per process — Telegram sync (60s), reminder
ticker, notify drain, report sweeper, retention janitor — plus atomic claim/reaper logic
that is timing-sensitive by design. Those are exactly the packages with the worst coverage
(`internal/agent` 37%, `internal/telegram` 33%, `internal/reminder` 56%).

`synctest.Test` runs code in a bubble with a fake clock that advances only when every
goroutine is durably blocked. Tests become instant and non-flaky, and **no production code
needs to change** — `time.Ticker` and `time.AfterFunc` work as written. Pair with
`httptest.NewTestServer` (new in Go 1.27) for an in-memory network.

Directly attacks C8: these tests would not need Postgres or Redis at all, so they could
parallelize while the integration lane stays `-p 1`.

### 5.3 An agent eval harness — the missing half of the agent feature

`internal/agent` is tested with a **fake** LLM, so it proves the loop dispatches tools
correctly. Nothing anywhere measures the question that matters: *does "book Court 3 tomorrow
19:00" actually book Court 3?*

The task is an agent, so single-output accuracy is the wrong instrument. Score the
trajectory:

| Signal | Question |
|---|---|
| Tool selection | Did it pick the right tool, and skip tools it did not need? |
| Argument accuracy | Were the extracted arguments right (date, court, time)? |
| Outcome (E2E) | Did the DB row exist afterwards? Gate the release on **environment state**, not on a model's opinion. |
| Step count / cost | 4 steps vs 15 is a real cost difference even when both succeed. |
| Flake rate | Run N-of-M (N=5) and report pass rate. A task that passes 3/5 is an untriaged regression. |

Method that works (and is cheap here): deterministic graders first — tool calls are
structured data, so required/forbidden tool sets, argument schemas, and terminal DB state are
all exact-match checks. Use an LLM judge only for semantic properties, give it a "return
Unknown" escape hatch, and calibrate it against human labels before trusting it.

Do **not** assert an exact reference trajectory — it is brittle and punishes valid paths.
Grade the outcome, keep step-level traces for debugging.

Then seed the eval set from production failures: every mis-booked court becomes a golden case.
That habit is the highest-ROI part of the whole harness.

### 5.4 Go 1.27 specifics this repo is not using

| Feature | Action | Note |
|---|---|---|
| **`uuid` in the standard library** | Drop `github.com/google/uuid` | v1.27 adds RFC 9562 v4 **and time-ordered v7**. `v7` is the better key for the append-only tables in §R12 — sequential insert locality. The stdlib type is `[16]byte` with a different method set, so check any code touching extra methods. |
| **`encoding/json/v1` is now backed by v2** | **Nothing** | Unmarshalling is significantly faster *already*, no code change. Only error message text may differ. Import `encoding/json/v2` explicitly if you want the stricter defaults (rejects duplicate keys, invalid UTF-8) — relevant to any client-supplied JSON you decode. |
| **`goroutineleak` profile (GA)** | Add a `/debug/pprof` route + runbook entry | Finds goroutines that can provably never unblock via GC reachability — the classic silent production failure, and this service owns 5 long-lived loops. |
| **`stdversion` vet check now runs by default** | Free | Catches stdlib symbols newer than the `go` directive. Already active; documented here so it isn't a surprise. |
| **PGO** | Build with `-pgo=auto` once a CPU profile exists | Single-digit % CPU for zero code change. |
| **`os.Root`** | Use for the xlsx report path | `GET /v1/reports/{token}` generates and serves files from disk. `os.Root` (Go 1.24+) confines access and blocks symlink escape; `ARCHITECTURE.md` §9/`STANDARDS.md` §D already mandate it but the report path is the obvious place it should apply. |
| **`go test -fuzz`** | Fuzz the date/availability logic | Commit `eae9173 fix(timezone): default "today" to WIB, not the server's clock` is exactly the class of bug a fuzzer finds for free. `availability` (212 LOC) is pure and untested — wrong answers there silently look like "no slots". |
| **Small-allocation size classes** | Nothing | Up to 30% cheaper allocations under 80 bytes, ~1% whole-program. Already in the runtime. |
| **`GOEXPERIMENT=simd`** | **Avoid** | Experimental. Do not build on it. |

### 5.5 Redis 8 atomics already designed but not implemented

`ARCHITECTURE.md` §3.2 documents the 2 RTT → 1 RTT table and §3.3 gives a complete Lua token
bucket. The code still does `GET` + `EXPIRE` (a race) and `INCR` + non-atomic `EXPIRE`
(permits a `limit×2` burst across a minute boundary). Both are `R9`. Use `HGETEX` for the
client cache and the Lua bucket for rate limiting.

### 5.6 MCP protocol posture — upstream went stateless

The current spec revision (`2026-07-28`) removed protocol-level sessions: no
`Mcp-Session-Id`, no `initialize` handshake. Version and capabilities now travel per request
in `_meta` (`io.modelcontextprotocol/protocolVersion`), the GET/SSE endpoint is replaced by
`subscriptions/listen`, tasks moved to an official extension, SSE resumability was removed,
and `Mcp-Method` / `Mcp-Name` headers are required on POST.

`internal/mcp` implements the older sessioned shape. That is fine today — but note the
posture:

- **Version negotiation** is the mitigation: read the version, accept both shapes, return
  `UnsupportedProtocolVersionError` for anything else. Your local Langchain bridge is the
  consumer to coordinate with.
- **Content-Length / auth per request** stays mandatory — being stateless does not exempt a
  server from auth.
- **Older clients** should keep working; the spec defines the fallback sequence.
- No migration is urgent. It becomes urgent the day a client library bumps its default
  version and silently stops negotiating.

### 5.7 `pgmq` — already evaluated and rejected, and that decision is right

`ARCHITECTURE.md` §4.4 records the rejection: migrations run in a single transaction, so a
Supabase-only extension dependency would abort the whole chain on Laragon or any VPS. Sound
reasoning, and `notify_jobs` reimplements the model (read counter + visibility timeout) in
four statements.

The nuance worth recording: pgmq ships **no DLQ, no retry limit, and no failure alerting**, so
adopting it would have meant *building the parts this repo already has* (migrations 029–031
already implement claim/complete/fail with an archive path). Keep the hand-rolled queue. Add
the one thing pgmq users have to build anyway: **the database checking on itself** — a
scheduled assertion on queue depth and oldest-message age, alerting when the drain loop dies
quietly. That is the failure mode this architecture is actually exposed to (C5, and the
Supabase idle-pause footgun).

### 5.8 Append-only table scaling

`usage`, `notify_jobs`, and `agent_reminders` grow forever and are pruned by a janitor that
`DELETE`s rows. At "thousands of tenants" they dominate table size and VACUUM cost.

- **BRIN indexes** on the timestamp column — tiny, and near-optimal for time-correlated
  appends. Far cheaper than the btree indexes these tables likely carry.
- **Monthly range partitioning** — the retention janitor becomes `DROP PARTITION` instead of
  a mass `DELETE`, which is instant and does not bloat.
- Both compose with the existing migration discipline (C9 notwithstanding).

### 5.9 Consider, not recommend

- **Vector search (`pgvector`)** for `AgentsMemory/Information/*.md`. Tempting — those docs
  are hand-maintained knowledge. But the corpus is 2 files and ~40 lines today. Revisit at
  ~50 documents per tenant. Add embeddings to the usage/cost model when you do.
- **Multi-provider LLM failover.** `/v1/chat` is DeepSeek-shaped; `genai` is imported but
  barely used. Worth it for availability, not yet worth the complexity.
- **A dedicated worker binary** for delivery, instead of in-process goroutines. Solves C5
  cleanly on Railway/VPS; irrelevant on Vercel.
- **Redis client-side caching** (`CLIENT TRACKING`) for `ck:*`. At current volume the
  `HGETEX` change in R9 captures most of the benefit at a fraction of the complexity.

---

## 6. Sequenced roadmap

**Week 1 — make the claims true** (S-effort items, one PR each)
R1 golangci-lint · R5 `.env.example` reconciliation · R2 coverage gate · R6 prompt-cache
accounting · R18 schedule the backups.
*Rationale: four of these are under a day, and together they convert three documentation
lies (§C2, C3, C4) into enforced reality. R6 is the highest-ROI single change in the repo.*

**Month 1 — close the risk and the gap**
R3 privileged-path tests · R9 Redis atomics + Lua token bucket · R7 SSE streaming ·
R10 synctest for the delivery loops · R22 hygiene.
*Rationale: removes the real blast-radius risk (C1), finishes the design already written
(§3.2/§3.3), and closes the standing known gap.*

**Quarter 1 — structural**
R4 OpenAPI + codegen · R5→R11 golden contract tests · R8 OTel · R12 partitioning/BRIN ·
R16 migration tooling · R17 audit log · R15 real WhatsApp sender.
*Rationale: R4 is the largest single unlock (it unblocks every consumer repo); R15 is
required before bookings are actually a product; R17 is table stakes for handling money.*

**Only after the quarter-1 items:** R13 decompose god files, R14 unify delivery, R19 test
split, R20 auth hardening. All are real but none is currently load-bearing.

---

## 7. Open decisions

Recorded so they are not silently re-litigated.

| Question | Current state | Recommendation |
|---|---|---|
| Vercel **or** persistent container? | Both supported; `BACKGROUND_TICKERS` picks the delivery driver | Pick one per environment and write it down. C5 is a direct consequence of supporting both. Railway/VPS is the better default: one delivery implementation, no idle-pause footgun, no cold-start migration check. |
| `pgmq` vs hand-rolled `notify_jobs` | Rejected (extension-in-migration) | Keep hand-rolled. Add self-monitoring (§5.7). |
| Shared features vs per-tenant | Three tiers, promotion rule documented | Working as designed. No change. |
| Should `simd` / `jsonv2` strict mode be adopted? | Experimental / opt-in | No. Revisit when stable. |
| Coverage target | Documented 70%, actual 27–95% | Set a **realistic floor** (e.g. 40% global with 70% on `internal/api`, `internal/auth`, `internal/quota`) rather than a number the codebase cannot meet and therefore does not enforce. |
| Per-feature `README.md` | Required by old standards; 5 of 15 exist | Either enforce it or drop the requirement. Prefer dropping it — `feature.go`'s doc comment is closer to the code and cannot go stale silently. |

---

## 8. Superseded documents

| Removed | Folded into |
|---|---|
| `PRD.md` (349 lines) | `README.md` §1–2 (context, business reason, goals/non-goals) · `ARCHITECTURE.md` §4, §6, §11, §14 |
| `STANDARDS.md` (131 lines) | `ARCHITECTURE.md` §1, §14–§17 (stack, security, performance, API rules) · §2 C3 / §4 3.2 of this file |
| `DONE.md` (198 lines) | §3 of this file (baseline, shipped scope, known limitations, migration inventory) |
| `docs/phase3-feature-platform.md` (168 lines) | `ARCHITECTURE.md` §6 (feature platform) |

Kept unchanged and still authoritative: `RUNBOOK.md` (operational incident playbooks),
`ops/RUNBOOK-delivery.md`, `ops/RUNBOOK-vercel.md`, the per-feature
`internal/features/**/README.md` files, and the embedded runtime content in `AgentsMemory/`
and `AgentsSkills/` (which is read by `agentprojects.go`, not documentation).

**Known follow-up:** three Go comments still reference sections of the old docs
(`031_delivery_roles.sql:24` → `ARCHITECTURE.md` §7; `agentprojects.go:11` → `ARCHITECTURE.md`;
`handlers.go:72` → `RUNBOOK.md`). Section numbering in `ARCHITECTURE.md` was deliberately
preserved so §7 still resolves; the remaining two should be re-pointed in a future code
change.