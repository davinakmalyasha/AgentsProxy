package adkrunner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
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
	"github.com/davinakmalyasha/agents-proxy/internal/llm"
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
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
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
	return &Runner{cfg: cfg, model: m, newToolSet: agentstools.NewToolSetFor}, nil
}

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
	var project *agentconfig.Project
	if r.cfg.Projects != nil {
		project = r.cfg.Projects[tenantID]
	}
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

	ts := r.newToolSet(r.cfg.Pool, tenantID, toolsRole)

	adapted := make([]tool.Tool, 0, len(ts.Names()))
	for _, name := range ts.Names() {
		t, ok := ts.Get(name)
		if !ok {
			continue
		}
		at, err := adaptTool(t, tenantID)
		if err != nil {
			// Proven at boot for every registered tool; reaching here
			// means the set changed under us, which must be loud.
			return "", err
		}
		adapted = append(adapted, at)
	}

	instruction := Assemble(Layers{
		Project:  project,
		Role:     role,
		Display:  display,
		ToolDefs: ts.Definitions(),
		Vertical: vertical,
		Now:      time.Now(),
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
		Tools: adapted,
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
