package adkrunner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openai/openai-go/v3/option"
	"golang.org/x/sync/singleflight"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
	"github.com/davinakmalyasha/agents-proxy/internal/httpx"
	"github.com/davinakmalyasha/agents-proxy/internal/llm"
	"github.com/davinakmalyasha/agents-proxy/internal/mcpclient"
	"github.com/davinakmalyasha/agents-proxy/internal/tenanthealth"
)

// Config wires one Runner: the pool tools run against, the tenant projects,
// the gateway coordinates, and the shared session store.
type Config struct {
	Pool      *pgxpool.Pool
	Projects  map[string]*agentconfig.Project
	BaseURL   string
	APIKey    string
	Model     string
	MaxTokens int
	Sessions  session.Service
}

// Runner is the ADK-backed replacement for agent.Agent's loop. It exposes
// the same RunWithRole shape so call sites migrate one at a time; the model
// is shared (openai-go clients are safe for concurrent use) while the agent
// and ADK runner are built per call, so the instruction always carries
// today's date and a deploy picks up new tools without a restart.
type Runner struct {
	cfg   Config
	model model.LLM
	// newModel builds one provider client. A field so tests can prove the
	// project-to-model mapping without calling a provider.
	newModel func(name string) (model.LLM, error)
	// models caches per-project provider clients. The default model is also
	// present under cfg.Model.
	modelMu sync.Mutex
	models  map[string]model.LLM
	// mcp owns external MCP sessions. Nil in hand-built test runners.
	mcp *mcpclient.Manager
	// newToolSet builds the role-filtered set. A field (not a direct
	// NewToolSetFor call) so tests can substitute stub tools without
	// touching the global registry.
	newToolSet func(pool *pgxpool.Pool, tenant, role string) *agentstools.ToolSet
	// mu guards projects: the admin reload path swaps one tenant's
	// config while requests read. cfg.Projects is the boot snapshot and
	// must not be read directly after New.
	mu       sync.RWMutex
	projects map[string]*agentconfig.Project
	// toolSets caches the adapted ADK tool set per (tenant, toolsRole).
	// The registry is init()-static, so entries never go stale; the
	// cache is per-Runner (not global) so tests with stub tool sets
	// cannot collide with each other. Saves 18 constructors + 18
	// schema parses + 2 sorts per request.
	// prompts caches the assembled system prompt per
	// (tenant, role, WIB date). The date rides the key because the
	// prompt carries today's date; UpdateProject evicts the tenant.
	cacheMu  sync.RWMutex
	cacheGen uint64
	toolSets map[string]*cachedTools
	prompts  map[string]string
	// setFlight collapses concurrent cold builds for one
	// (tenant, toolsRole): without it N simultaneous cold agent turns
	// each run MCP discovery. Only the built value is shared; refresh
	// and generation checks still run per caller.
	setFlight singleflight.Group
}

// cachedTools is one role-filtered, ADK-adapted tool set.
type cachedTools struct {
	names   []string
	adapted []tool.Tool
	defs    []string
}

// cacheKey namespaces the tool/prompt caches. \x00 cannot appear in a
// slug, role, or date, so plain concatenation is collision-free.
func cacheKey(parts ...string) string {
	return strings.Join(parts, "\x00")
}

// cachedSet returns the adapted tool set for (tenant, toolsRole),
// building and caching it on first use. The build (including external MCP
// discovery) runs without the global cache lock; only the final map write
// is locked. The generation check prevents an in-flight build from
// repopulating a key after an admin UpdateProject evicted it.
func (r *Runner) cachedSet(ctx context.Context, tenantID, toolsRole string) (*cachedTools, error) {
	key := cacheKey(tenantID, toolsRole)
	p := r.projectFor(tenantID)

	// An MCP server that failed at first use is retried after a short
	// backoff. The registry half is static, but the MCP half is not, so
	// drop the cached set when the manager says a reconnect is due.
	if r.mcp != nil && p != nil && r.mcp.RefreshDue(tenantID, toolsRole, p.Agent.MCP) {
		r.cacheMu.Lock()
		delete(r.toolSets, key)
		r.cacheMu.Unlock()
	}
	r.cacheMu.RLock()
	ct, ok := r.toolSets[key]
	gen := r.cacheGen
	r.cacheMu.RUnlock()
	if ok {
		return ct, nil
	}

	// Collapse concurrent cold builds: N simultaneous turns for a fresh
	// (tenant, role) share one MCP discovery + adaptation. Failures are
	// not cached -- the store below only runs on success.
	v, err, _ := r.setFlight.Do(key, func() (any, error) {
		ts := r.newToolSet(r.cfg.Pool, tenantID, toolsRole)
		if p != nil && len(p.Agent.Tools) > 0 {
			ts = ts.Only(p.Agent.Tools)
		}
		ts = r.withMCP(ctx, ts, p, tenantID, toolsRole)
		names := ts.Names()
		adapted := make([]tool.Tool, 0, len(names))
		for _, name := range names {
			t, ok := ts.Get(name)
			if !ok {
				continue
			}
			at, err := adaptTool(t, tenantID)
			if err != nil {
				// Native tool schemas are proven at boot. A remote MCP
				// server can publish a schema the ADK cannot parse; skip
				// that tool instead of making the whole agent unusable.
				if strings.HasPrefix(t.Name, "mcp_") {
					slog.Warn("adkrunner: skipping MCP tool with unparsable schema",
						"tenant", tenantID, "tool", t.Name, "err", err)
					continue
				}
				return nil, err
			}
			adapted = append(adapted, at)
		}
		built := &cachedTools{names: names, adapted: adapted, defs: ts.Definitions()}
		r.cacheMu.Lock()
		if r.cacheGen == gen {
			if r.toolSets == nil {
				r.toolSets = map[string]*cachedTools{}
			}
			r.toolSets[key] = built
		}
		r.cacheMu.Unlock()
		return built, nil
	})
	if err != nil {
		return nil, err
	}
	ct, _ = v.(*cachedTools)
	return ct, nil
}

// withMCP appends the project's external MCP tools to the native set. The
// native tool allow-list (agent.tools) is applied before this step, so it
// keeps meaning "native tools only"; the MCP server's own tools list is the
// remote allow-list.
func (r *Runner) withMCP(ctx context.Context, native *agentstools.ToolSet, p *agentconfig.Project, tenantID, role string) *agentstools.ToolSet {
	if r.mcp == nil || p == nil || len(p.Agent.MCP) == 0 {
		return native
	}
	tools := make([]agentstools.Tool, 0, len(native.Names()))
	for _, name := range native.Names() {
		if t, ok := native.Get(name); ok {
			tools = append(tools, t)
		}
	}
	seen := map[string]bool{}
	for _, name := range native.Names() {
		seen[name] = true
	}
	servers := make([]agentconfig.MCPServer, 0, len(p.Agent.MCP))
	for _, srv := range p.Agent.MCP {
		if mcpclient.ServerAllowsRole(srv, role) {
			servers = append(servers, srv)
		}
	}
	if len(servers) == 0 {
		return native
	}
	for _, remote := range r.mcp.Tools(ctx, tenantID, role, servers) {
		if !remote.Allowed(role) {
			continue
		}
		if seen[remote.Name] {
			slog.Warn("adkrunner: MCP tool name collides with an existing tool",
				"tenant", tenantID, "tool", remote.Name)
			continue
		}
		seen[remote.Name] = true
		tools = append(tools, remote.Tool)
	}
	return agentstools.NewToolSetFrom(native.Pool, tools...)
}

// modelFor resolves the provider client for a tenant. A project-level
// agent.model overrides the platform default; an unset or empty value keeps
// the shared client. A model that failed to build falls back to the default
// with a warning rather than taking the agent offline.
func (r *Runner) modelFor(tenantID string) model.LLM {
	name := ""
	if p := r.projectFor(tenantID); p != nil {
		name = strings.TrimSpace(p.Agent.Model)
	}
	if name == "" || name == r.cfg.Model || r.newModel == nil {
		return r.model
	}
	r.modelMu.Lock()
	defer r.modelMu.Unlock()
	if m, ok := r.models[name]; ok {
		return m
	}
	m, err := r.newModel(name)
	if err != nil {
		slog.Warn("adkrunner: project model unavailable; using default",
			"tenant", tenantID, "model", name, "err", err)
		return r.model
	}
	if r.models == nil {
		r.models = map[string]model.LLM{}
	}
	r.models[name] = m
	return m
}

// Close releases external MCP sessions. Native tools hold no resources.
func (r *Runner) Close() {
	if r != nil && r.mcp != nil {
		r.mcp.Close()
	}
}

// cachedPrompt returns the assembled system prompt for
// (tenant, role, date), building and caching it on first use. Stale
// dates are swept on a miss so the cache holds one day only.
func (r *Runner) cachedPrompt(tenantID, role, date string, build func() string) string {
	key := cacheKey(tenantID, role, date)
	r.cacheMu.RLock()
	if r.prompts != nil {
		if p, ok := r.prompts[key]; ok {
			r.cacheMu.RUnlock()
			return p
		}
	}
	r.cacheMu.RUnlock()
	// Build outside the write lock: assembly is pure string work and the
	// old shape blocked every tenant while one prompt built. Double-check
	// under the lock so concurrent builders store once.
	p := build()
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if r.prompts == nil {
		r.prompts = map[string]string{}
	}
	if existing, ok := r.prompts[key]; ok {
		return existing
	}
	r.prompts[key] = p
	for k := range r.prompts {
		if !strings.HasSuffix(k, "\x00"+date) {
			delete(r.prompts, k)
		}
	}
	return p
}

// projectFor returns one tenant's config under read lock. A nil project
// is not an error (base prompt serves unconfigured tenants); a
// quarantined tenant never reaches here (fail-closed above it).
func (r *Runner) projectFor(tenantID string) *agentconfig.Project {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.projects[tenantID]
}

// UpdateProject hot-swaps one tenant's config (admin reload path). Only
// called after the candidate passes LoadSingle + Validate + tool proof,
// so serving config is always known-good. Evicts the tenant's cached
// prompts and tool set; the latter now also carries MCP servers, whose
// sessions are closed and reconnected from the new config.
func (r *Runner) UpdateProject(p *agentconfig.Project) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.projects == nil {
		r.projects = map[string]*agentconfig.Project{}
	}
	r.projects[p.Slug] = p
	if r.mcp != nil {
		r.mcp.CloseProject(p.Slug)
	}
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	// Invalidate in-flight cachedSet builds: they may have started with
	// the previous project snapshot and must not repopulate the cache.
	r.cacheGen++
	prefix := p.Slug + "\x00"
	for k := range r.prompts {
		if strings.HasPrefix(k, prefix) {
			delete(r.prompts, k)
		}
	}
	for k := range r.toolSets {
		if strings.HasPrefix(k, prefix) {
			delete(r.toolSets, k)
		}
	}
}

// AgentEnabled reports whether a configured project allows the agent. A
// tenant with no project folder keeps the historical base-prompt behaviour
// (true); a project with agent.enabled=false is explicitly off.
func (r *Runner) AgentEnabled(tenantID string) bool {
	p := r.projectFor(tenantID)
	return p == nil || p.Agent.Enabled
}

// TenantSlugs lists tenants with serving configs (for admin health).
func (r *Runner) TenantSlugs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.projects))
	for slug := range r.projects {
		out = append(out, slug)
	}
	sort.Strings(out)
	return out
}

// New builds the shared model and proves every registered tool adapts.
// A tool whose schema does not parse fails the boot, not the first user
// who happens to need that tool.
func New(cfg Config) (*Runner, error) {
	if cfg.Pool == nil {
		return nil, errors.New("adkrunner: pool is required")
	}
	if cfg.Sessions == nil {
		return nil, errors.New("adkrunner: session service is required")
	}
	if cfg.Model == "" {
		return nil, errors.New("adkrunner: model is required")
	}
	newModel := func(name string) (model.LLM, error) {
		opts := []option.RequestOption{
			option.WithJSONSet("reasoning_effort", "low"),
			// OpenCode Go rejects every request without this header. A stable
			// process ID satisfies the gateway and keeps prompt-cache affinity
			// for the life of the deployment; other providers ignore it.
			option.WithHeader("x-opencode-session", uuid.NewString()),
		}
		if llm.IsReasoningModel(name) {
			opts = append(opts, option.WithJSONSet("thinking", map[string]string{"type": "disabled"}))
		}
		return openaimodel.NewModel(context.Background(), name, &openaimodel.ClientConfig{
			APIKey:     cfg.APIKey,
			BaseURL:    cfg.BaseURL,
			API:        openaimodel.APIChatCompletions,
			HTTPClient: httpx.NewClient(60 * time.Second),
			Options:    opts,
		})
	}
	m, err := newModel(cfg.Model)
	if err != nil {
		return nil, fmt.Errorf("adkrunner: model: %w", err)
	}
	models := map[string]model.LLM{cfg.Model: m}
	for slug, p := range cfg.Projects {
		name := strings.TrimSpace(p.Agent.Model)
		if name == "" || name == cfg.Model {
			continue
		}
		if _, ok := models[name]; ok {
			continue
		}
		pm, err := newModel(name)
		if err != nil {
			return nil, fmt.Errorf("adkrunner: project %q model %q: %w", slug, name, err)
		}
		models[name] = pm
		slog.Info("adkrunner: project model", "tenant", slug, "model", name)
	}
	full := agentstools.AllTools(cfg.Pool)
	for _, t := range full {
		if _, err := adaptTool(t, "boot-check"); err != nil {
			return nil, err
		}
	}
	// Log the live tool surface per tenant: names plus total schema
	// bytes. This is the one place a human can see exactly what each
	// tenant's model gets to call -- the declarations themselves only
	// exist on the wire otherwise.
	for _, tenant := range agentstools.Tenants() {
		ts := agentstools.NewToolSetFor(cfg.Pool, tenant, "owner")
		schemaBytes := 0
		for _, name := range ts.Names() {
			if t, ok := ts.Get(name); ok {
				schemaBytes += len(t.Parameters)
			}
		}
		slog.Info("adkrunner: tenant tools live",
			"tenant", tenant, "tools", len(ts.Names()), "schema_bytes", schemaBytes)
	}
	return &Runner{
		cfg:        cfg,
		model:      m,
		newModel:   newModel,
		models:     models,
		mcp:        mcpclient.NewManager(),
		projects:   cfg.Projects,
		newToolSet: agentstools.NewToolSetFor,
		toolSets:   map[string]*cachedTools{},
		prompts:    map[string]string{},
	}, nil
}

// TurnTimeout bounds one agent turn end to end. Worst case without it
// is maxModelCalls × the per-model-call HTTP timeout (5 × 60s = 300s),
// which exceeds both the HTTP WriteTimeout and Vercel's function cap —
// the server would kill the connection mid-write. 80s fits a full
// 5-call turn at typical provider latency with headroom, and leaves
// 10s of slack inside the 90s WriteTimeout derived from it in main.
const TurnTimeout = 80 * time.Second

// RunWithRole processes one user message and returns the reply. Same
// signature as the old loop: role selects both the persona and the tool
// set.
func (r *Runner) RunWithRole(ctx context.Context, tenantID, vertical, userName, role, message string) (string, error) {
	return r.RunWithToolsRole(ctx, tenantID, vertical, userName, role, role, message)
}

// RunWithToolsRole splits the tiers the old ClientChat paired: the public
// landing widget runs anon tools (pricing + availability only, safe against
// a caller-chosen tenant) with the client persona. Every other caller
// passes the same tier twice.
func (r *Runner) RunWithToolsRole(ctx context.Context, tenantID, vertical, userName, toolsRole, role, message string) (string, error) {
	// Fail closed: a quarantined tenant never reaches the model. Silence
	// with an honest 503 (mapped by the handler) instead of a confident
	// reply from a half-loaded prompt.
	if q, reason := tenanthealth.Check(tenantID); q {
		slog.Warn("quarantined tenant blocked from agent", "tenant", tenantID, "reason", reason)
		return "", tenanthealth.ErrQuarantined
	}
	// Bound the whole turn. Without this the worst case is maxModelCalls
	// × the per-call provider timeout, which outlives both WriteTimeout
	// and the platform cap. Handlers map DeadlineExceeded to 504.
	ctx, cancel := context.WithTimeout(ctx, TurnTimeout)
	defer cancel()
	project := r.projectFor(tenantID)
	// Tenant identity for shared text: the project's display and contact,
	// falling back to the tenant slug and the platform default. For
	// centrum these resolve to the exact strings the hardcoded replies
	// used, which the isolation tests lock in byte for byte.
	display := displayFor(project, tenantID)
	contact := ""
	effectiveVertical := vertical
	if project != nil {
		contact = project.Contact
		if project.Vertical != "" {
			effectiveVertical = project.Vertical
		}
	}
	// Pre-guards, identical to the old loop: no model call is spent on
	// abuse, and clearing history cannot reset these (quota keys are
	// separate from session state).
	if role == "client" && agentstools.IsOffTopicFor(message, effectiveVertical) {
		return agentstools.ScopeReplyFor(display, contact), nil
	}
	// One Redis round trip for both caps (pipelined Lua), same
	// fail-closed semantics as the two sequential checks.
	rpdOK, costOK := agentstools.CapsAllowed(ctx, tenantID, role, userName)
	if !rpdOK {
		return agentstools.AbuseReplyFor(contact), nil
	}
	if !costOK {
		return "Daily AI usage limit reached. Try again tomorrow.", nil
	}

	ts, err := r.cachedSet(ctx, tenantID, toolsRole)
	if err != nil {
		return "", err
	}

	now := agentstools.WibNow()
	instruction := r.cachedPrompt(tenantID, role, now.Format("2006-01-02"), func() string {
		return Assemble(Layers{
			Project:  project,
			Role:     role,
			Display:  display,
			ToolDefs: ts.defs,
			Vertical: vertical,
			Now:      now,
		})
	})

	ag, err := llmagent.New(llmagent.Config{
		Name:        fmt.Sprintf("%s-%s", tenantID, role),
		Description: fmt.Sprintf("venue assistant for tenant %s at tier %s", tenantID, role),
		Model:       r.modelFor(tenantID),
		// InstructionProvider, NOT Instruction: ADK runs {key}
		// templating over the static Instruction string, and our tenant
		// content carries {{DATE}} / {{TOOLS}} placeholders that break
		// that templating ("state key does not exist"). The provider's
		// return value is used verbatim, so the model sees byte-for-byte
		// what the old loop sent. This is the documented spelling for
		// opting out of {} handling.
		InstructionProvider: func(ctx adkagent.ReadonlyContext) (string, error) {
			return instruction, nil
		},
		Tools: ts.adapted,
		GenerateContentConfig: &genai.GenerateContentConfig{
			MaxOutputTokens: int32(r.cfg.MaxTokens),
		},
	})
	if err != nil {
		return "", fmt.Errorf("adkrunner: agent: %w", err)
	}

	gp, err := newBudgetPlugin(maxModelCalls)
	if err != nil {
		return "", fmt.Errorf("adkrunner: guard: %w", err)
	}
	adk, err := runner.New(runner.Config{
		AppName:           tenantID,
		Agent:             ag,
		SessionService:    r.cfg.Sessions,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{gp}},
		// Compaction deliberately unset: sliding-window/tail retention
		// would spend summarizer model calls and change what the model
		// sees. History growth is bounded instead by the 180-day events
		// retention (ops/retention.sql) and served by
		// events_session_time_idx (migration 053). Revisit when a
		// single session's turn count, not the table, becomes the cost
		// center — that needs per-session windowing, not a global flag.
	})
	if err != nil {
		return "", fmt.Errorf("adkrunner: runner: %w", err)
	}

	// Session key preserves the old history partition exactly:
	// ag:{tenant}:{role}:{user}. A user who talked to the old loop
	// starts fresh here (Redis vs Postgres), which the per-callsite
	// migration makes acceptable: no entry point shares keys.
	msg := &genai.Content{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromText(message)},
	}
	var last string
	for ev, err := range adk.Run(ctx, userName, role, msg, adkagent.RunConfig{}) {
		if err != nil {
			if IsMaxRounds(err) {
				return maxRoundsMessage, nil
			}
			return "", err
		}
		if ev == nil {
			continue
		}
		if ev.IsFinalResponse() {
			if text := eventText(ev); text != "" {
				last = text
			}
		}
	}
	if last == "" {
		return refusedNoData, nil
	}
	return agentstools.SanitiseReply(last), nil
}

// ClearSession deletes one conversation: the "Bersihkan riwayat" path.
// It mirrors the old History.ClearForRole key (tenant, role-as-session,
// user) so the wipe removes exactly what RunWithRole stored. Quota state
// lives in Redis under separate keys and is untouched, same as before.
func (r *Runner) ClearSession(ctx context.Context, tenantID, userID, role string) error {
	return r.cfg.Sessions.Delete(ctx, &session.DeleteRequest{
		AppName:   tenantID,
		UserID:    userID,
		SessionID: role,
	})
}

// refusedNoData is the reply when the run ends with no final text. Named
// like the old loop's constant and with the same constraint: it must NOT
// contain "coba lagi", which the frontend renders as a connectivity failure
// for what is a deterministic server-side outcome.
const refusedNoData = "Maaf, saya belum bisa menjawab itu dengan data dari sistem."

// eventText concatenates the text parts of a final event. Tool-call and
// thought parts are not prose and are skipped.
func eventText(ev *session.Event) string {
	if ev == nil || ev.Content == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range ev.Content.Parts {
		if p == nil || p.Text == "" {
			continue
		}
		b.WriteString(p.Text)
	}
	return b.String()
}
