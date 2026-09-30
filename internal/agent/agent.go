// Package agent is the conversational tool loop for the agentic
// layer. It turns a natural-language message into tool calls against
// the SAME business logic the REST API uses (via WithTenantTx + RLS),
// then returns a natural-language reply.
//
// Tool-calling is the provider's own function-calling protocol, not a
// convention invented in a prompt:
//
//  1. route(message) picks a tool in Go, from the role-filtered ToolSet
//  2. ChatWithTools sends the declarations with tool_choice PINNED to
//     that tool, which is the only form this gateway honours
//  3. the tool runs; its result goes back as a real role:"tool" message
//     carrying the provider's tool_call_id
//  4. the model narrates the result
//
// This replaced a text protocol where the model was asked to answer with
// {"tool": ..., "args": ...} in prose. The model preferred its own
// RLHF'd <tool_call>{function=â€¦} wrapper instead, the hand-rolled
// parser did not recognise it, and the raw markup was rendered to the user
// and stored in history. See router.go for why the tool is chosen here
// rather than by the model.
//
// The loop runs at most maxRounds.
//
// DONE: Phase 5 (telegram agentic) / STANDARDS A, D, F
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/davinakmalyasha/agents-proxy/internal/llm"
)

const maxRounds = 5

// CtxChatID carries the sender's chat ID (Telegram) through the agent
// loop into tools. Tools that deliver outside the chat (reminders)
// require it; web/landing callers have none.
type ctxKey string

const CtxChatID ctxKey = "chat_id"

// ChatIDFromCtx returns the sender chat ID, or "" when the caller is
// not a Telegram chat (webchat, MCP, tests).
func ChatIDFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(CtxChatID).(int64); ok && v != 0 {
		return strconv.FormatInt(v, 10)
	}
	if v, ok := ctx.Value(CtxChatID).(string); ok {
		return v
	}
	return ""
}

// WithChatID stamps a context with the sender's Telegram chat ID.
func WithChatID(ctx context.Context, chatID int64) context.Context {
	return context.WithValue(ctx, CtxChatID, chatID)
}

// CtxBotID carries the originating bot uuid so tools and scheduler send through the owning bot.
const CtxBotID ctxKey = "bot_id"

// WithBotID stamps a context with the Telegram bot uuid.
func WithBotID(ctx context.Context, botID string) context.Context {
	return context.WithValue(ctx, CtxBotID, botID)
}

// BotIDFromCtx returns the bot uuid, or "" outside Telegram delivery.
func BotIDFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(CtxBotID).(string); ok {
		return v
	}
	return ""
}

// Agent is the conversational loop for one tenant context.
type Agent struct {
	LLM     *llm.Client
	Tools   *ToolSet
	History *History

	// roleDefs memoizes the per-role tool-definition lists used in the
	// system prompt (a pure function of Tools.Pool + role, immutable).
	roleDefs sync.Map
}

// New wires an agent. history may be nil (tests).
func New(llmCli *llm.Client, tools *ToolSet, hist *History) *Agent {
	return &Agent{LLM: llmCli, Tools: tools, History: hist}
}

// definitionsForRole returns the tool definitions for a role, building
// the filtered set once per (pool, role) and caching it.
func (a *Agent) definitionsForRole(role string) []string {
	if v, ok := a.roleDefs.Load(role); ok {
		return v.([]string)
	}
	defs := a.Tools.Definitions()
	if role != "owner" {
		if filtered := NewToolSetFor(a.Tools.Pool, role); filtered != nil && len(filtered.Definitions()) > 0 {
			defs = filtered.Definitions()
		}
	}
	actual, _ := a.roleDefs.LoadOrStore(role, defs)
	return actual.([]string)
}

// Run processes one user message for the given tenant and returns
// the agent's reply. It appends the exchange to history.
func (a *Agent) Run(ctx context.Context, tenantID, vertical, userName, message string) (string, error) {
	return a.RunWithRole(ctx, tenantID, vertical, userName, "owner", message)
}

// RunWithRole is the role-aware entry. Same shared LLM key (AgentsProxy/.env LLM_API_KEY)
// but filtered ToolSet per role â€” the isolation you asked for client vs pegawai vs owner.
func (a *Agent) RunWithRole(ctx context.Context, tenantID, vertical, userName, role, message string) (string, error) {
	// Scope lock for client: if message is clearly off-topic/jailbreak, hardcode fallback without LLM
	if role == "client" && isOffTopic(message) {
		return HardcodedScopeReply, nil
	}
	// Per-person RPD (30/80/150) + per-tenant+role cap (20/80/150) for abuse
	if !RPDCapAllowedForUser(ctx, tenantID, role, userName) {
		return HardcodedAbuseReply, nil
	}
	if !CostCapAllowedForRole(ctx, tenantID, role) {
		return "Daily AI usage limit reached. Try again tomorrow.", nil
	}
	// Build role-filtered prompt. The filtered ToolSet is a pure
	// function of (pool, role) and pool is fixed for the process â€”
	// cache per role instead of rebuilding ~20 closures + defs every
	// message. Definitions() output is immutable, so a shared copy is
	// safe to hand the prompt builder.
	toolDefs := a.definitionsForRole(role)
	sys := SystemPromptForRole(vertical, tenantID, role, toolDefs)

	// Load history per tenant+role+user (good-as-possible, central store)
	hist := []llm.Message{{Role: "system", Content: sys}}
	if a.History != nil {
		hist = append(hist, a.History.LoadForRole(tenantID, role, userName)...)
	}
	hist = append(hist, llm.Message{Role: "user", Content: userName + ": " + message})

	rounds := 0
	// Route once, in Go, over the already role-filtered ToolSet. A tool
	// the role may not call is not in the set, so it cannot be selected.
	routed := a.Tools.route(message)
	defs := a.Tools.Defs()

	for {
		rounds++
		if rounds > maxRounds {
			break
		}

		// Phase 1: get the tool call. Offer the declarations, pinning the
		// choice when routing found something. Nothing is executed on the
		// strength of a guess â€” the pinned name is the only thing that
		// reaches the provider.
		var choice *llm.ToolChoice
		if routed.Tool != "" {
			choice = llm.ForceTool(routed.Tool)
		}
		comp, err := a.LLM.ChatWithTools(ctx, hist, defs, choice, "")
		if err != nil {
			return "", err
		}
		if len(comp.ToolCalls) == 0 {
			// No call. Either the model answered directly, or it ignored a
			// pinned tool. Both are legitimate; a retry with a different
			// tool is not, because routing is deterministic and already
			// had its shot.
			if routed.Tool != "" {
				slog.Warn("agent: provider returned no tool call",
					"tenant", tenantID, "role", role,
					"pinned", routed.Tool, "score", routed.Score,
					"finish", comp.FinishReason)
			}
			return a.finish(ctx, tenantID, role, userName, message, sanitiseReply(comp.Content)), nil
		}

		// Phase 2: run the tool. Tool results are recorded against the
		// provider's own call id so the next turn can be matched to it.
		for _, call := range comp.ToolCalls {
			tool, ok := a.Tools.Get(call.Function.Name)
			if !ok {
				slog.Warn("agent: model asked for an unavailable tool",
					"tenant", tenantID, "role", role, "tool", call.Function.Name)
				hist = append(hist, llm.Message{
					Role: "assistant", Content: comp.Content, ToolCalls: comp.ToolCalls,
				})
				hist = append(hist, llm.Message{
					Role: "tool", ToolCallID: call.ID,
					Content: "Error: no such tool " + call.Function.Name,
				})
				continue
			}
			args := normaliseArguments(call.Function.Arguments)
			result := tool.Run(ctx, tenantID, args)
			slog.Info("agent tool", "tenant", tenantID, "role", role,
				"tool", call.Function.Name, "result", truncate(result, 200))
			hist = append(hist, llm.Message{
				Role: "assistant", Content: comp.Content, ToolCalls: comp.ToolCalls,
			})
			hist = append(hist, llm.Message{Role: "tool", ToolCallID: call.ID, Content: result})
		}
		// The next turn narrates: no declarations, so no further calls and
		// the loop cannot wander.
		comp, err = a.LLM.Complete(ctx, hist, nil, nil, "")
		if err != nil {
			return "", err
		}
		return a.finish(ctx, tenantID, role, userName, message, sanitiseReply(comp.Content)), nil
	}
	// Ran out of rounds.
	msg := "I hit my processing limit â€” please rephrase or ask in smaller steps."
	if a.History != nil {
		a.History.AppendForRole(tenantID, role, userName, message, msg)
	}
	return msg, nil
}

// finish records the exchange and returns the reply.
func (a *Agent) finish(ctx context.Context, tenantID, role, userName, message, reply string) string {
	if a.History != nil {
		a.History.AppendForRole(tenantID, role, userName, message, reply)
	}
	return reply
}

// normaliseArguments guards against a provider sending empty or non-object
// argument text. A tool's json.Unmarshal into a struct is a no-op for
// anything that isn't an object, so an empty string would otherwise reach
// Run and be silently ignored.
func normaliseArguments(raw string) json.RawMessage {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return json.RawMessage(`{}`)
	}
	if !strings.HasPrefix(trimmed, "{") {
		slog.Warn("agent: tool arguments were not a JSON object", "args", truncate(trimmed, 80))
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(trimmed)
}

// eachPattern holds the shapes a model falls back on when it is not given
// a real tool interface: the Hermes/Qwen plain-text wrappers, and both
// envelopes the previous prompt-based protocol asked for
// ({"tool":...,"args":...} and {"answer":...}).
//
// They are kept as separate patterns rather than one alternation so a
// suppression can name which shape appeared — see firstMarkupHit.
//
// This is defence in depth. The primary fix is that a tool call now travels
// as a real tool_calls field, so the model has no reason to improvise. But
// the guard is what guarantees a regression cannot put protocol internals
// in front of a user or into stored history — which is the failure that
// shipped raw <tool_call>{function=…} markup into the chat and then
// persisted it, repeating on every later turn of the same conversation.
var eachPattern = []*regexp.Regexp{
	regexp.MustCompile(`(?is)<\s*/?\s*tool_call`),
	regexp.MustCompile(`(?is)<\s*/?\s*function_call`),
	regexp.MustCompile(`(?is)<\s*/?\s*parameter`),
	regexp.MustCompile(`(?is)<\s*/?\s*answer`),
	regexp.MustCompile(`(?i)"tool_call"\s*:`),
	regexp.MustCompile(`(?i)"function_call"\s*:`),
	regexp.MustCompile(`(?i)"tool"\s*:\s*"`),
	regexp.MustCompile(`(?i)"args"\s*:`),
}

// sanitiseReply is the last gate before a model turn becomes a user-visible
// answer. If the text still contains protocol markup, the caller is told so
// in plain language rather than being shown the internals.
func sanitiseReply(text string) string {
	t := strings.TrimSpace(text)
	if t == "" {
		return "Saya belum bisa menjawab itu. Coba tanya hal yang lebih spesifik tentang data usaha."
	}
	if hit := firstMarkupHit(t); hit != nil {
		slog.Warn("agent: reply contained protocol markup; suppressed",
			"pattern", hit.pattern, "at", hit.at, "text", truncate(hit.snippet, 160))
		return "Maaf, jawabannya tidak bisa ditampilkan. Coba ulangi pertanyaannya dengan kalimat lain."
	}
	return t
}

type markupHit struct {
	pattern string
	at      int
	snippet string
}

// firstMarkupHit returns the earliest match across all alternatives, so
// the reported position is the first place the text actually goes wrong
// rather than whichever pattern happened to be checked first.
func firstMarkupHit(text string) *markupHit {
	var best *markupHit
	for i, re := range eachPattern {
		loc := re.FindStringIndex(text)
		if loc == nil {
			continue
		}
		if best == nil || loc[0] < best.at {
			from := loc[0] - 40
			if from < 0 {
				from = 0
			}
			to := loc[1] + 40
			if to > len(text) {
				to = len(text)
			}
			best = &markupHit{
				pattern: fmt.Sprintf("alt%d", i),
				at:      loc[0],
				snippet: text[from:to],
			}
		}
	}
	return best
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func isOffTopic(msg string) bool {
	m := strings.ToLower(msg)
	// Very cheap heuristic for abuse/jailbreak vs badminton venue
	offMarkers := []string{
		"ignore instructions", "ignore previous", "system prompt", "jailbreak", "dan ", "do anything now",
		"write code", "python", "javascript", "translate this", "summarize this article",
		"who are you", "what is your name",
	}
	for _, k := range offMarkers {
		if strings.Contains(m, k) {
			// Allow if also contains badminton-related words, then not off-topic
			if strings.Contains(m, "badminton") || strings.Contains(m, "lapangan") || strings.Contains(m, "centrum") || strings.Contains(m, "booking") || strings.Contains(m, "jadwal") || strings.Contains(m, "harga") {
				continue
			}
			return true
		}
	}
	// If message is long and has no badminton keywords at all, treat as off-topic
	hasBadminton := strings.Contains(m, "badminton") || strings.Contains(m, "lapangan") || strings.Contains(m, "centrum") || strings.Contains(m, "booking") || strings.Contains(m, "jadwal") || strings.Contains(m, "harga") || strings.Contains(m, "fasilitas") || strings.Contains(m, "lokasi") || strings.Contains(m, "whatsapp") || strings.Contains(m, "jam")
	if !hasBadminton && len(strings.Fields(m)) > 12 {
		return true
	}
	return false
}
