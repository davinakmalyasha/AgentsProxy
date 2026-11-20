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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openai/openai-go/v3/option"
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
	toolSets map[string]*cachedTools
	prompts  map[string]string
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
// building and caching it on first use. Maps are lazily initialised
// so test-constructed Runners (which bypass New) work too.
func (r *Runner) cachedSet(tenantID, toolsRole string) (*cachedTools, error) {
	key := cacheKey(tenantID, toolsRole)
	r.cacheMu.RLock()
	ct, ok := r.toolSets[key]
	r.cacheMu.RUnlock()
	if ok {
		return ct, nil
	}
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if r.toolSets == nil {
		r.toolSets = map[string]*cachedTools{}
	}
	if ct, ok := r.toolSets[key]; ok {
		return ct, nil
	}
	ts := r.newToolSet(r.cfg.Pool, tenantID, toolsRole)
	if p := r.projectFor(tenantID); p != nil && len(p.Agent.Tools) > 0 {
		ts = ts.Only(p.Agent.Tools)
	}
	names := ts.Names()
	adapted := make([]tool.Tool, 0, len(names))
	for _, name := range names {
		t, ok := ts.Get(name)
		if !ok {
			continue
		}
		at, err := adaptTool(t, tenantID)
		if err != nil {
			// Proven at boot for every registered tool; reaching here
			// means the set changed under us, which must be loud.
			return nil, err
		}
		adapted = append(adapted, at)
	}
	ct = &cachedTools{names: names, adapted: adapted, defs: ts.Definitions()}
	r.toolSets[key] = ct
	return ct, nil
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
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if r.prompts == nil {
		r.prompts = map[string]string{}
	}
	if p, ok := r.prompts[key]; ok {
		return p
	}
	p := build()
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
// prompts; the tool set is registry-derived and unaffected.
func (r *Runner) UpdateProject(p *agentconfig.Project) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.projects == nil {
		r.projects = map[string]*agentconfig.Project{}
	}
	r.projects[p.Slug] = p
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	prefix := p.Slug + "\x00"
	for k := range r.prompts {
		if strings.HasPrefix(k, prefix) {
			delete(r.prompts, k)
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
	opts := []option.RequestOption{
		option.WithJSONSet("reasoning_effort", "low"),
	}
	if llm.IsReasoningModel(cfg.Model) {
		opts = append(opts, option.WithJSONSet("thinking", map[string]string{"type": "disabled"}))
	}
	m, err := openaimodel.NewModel(context.Background(), cfg.Model, &openaimodel.ClientConfig{
		APIKey:     cfg.APIKey,
		BaseURL:    cfg.BaseURL,
		API:        openaimodel.APIChatCompletions,
		HTTPClient: httpx.NewClient(60 * time.Second),
		Options:    opts,
	})
	if err != nil {
		return nil, fmt.Errorf("adkrunner: model: %w", err)
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
	return &Runner{cfg: cfg, model: m, projects: cfg.Projects, newToolSet: agentstools.NewToolSetFor,
		toolSets: map[string]*cachedTools{}, prompts: map[string]string{}}, nil
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
	if !agentstools.RPDCapAllowedForUser(ctx, tenantID, role, userName) {
		return agentstools.AbuseReplyFor(contact), nil
	}
	if !agentstools.CostCapAllowedForRole(ctx, tenantID, role) {
		return "Daily AI usage limit reached. Try again tomorrow.", nil
	}

	ts, err := r.cachedSet(tenantID, toolsRole)
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
		Model:       r.model,
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
