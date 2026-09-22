// Package agent is the conversational tool loop for the agentic
// layer. It turns a natural-language message into tool calls against
// the SAME business logic the REST API uses (via WithTenantTx + RLS),
// then returns a natural-language reply.
//
// Tool-calling convention: the LLM returns a JSON object:
//
//	{"tool": "book_slot", "args": {...}}   → execute, feed result back
//	{"answer": "Done — Court 3 booked ..."} → final reply
//
// The loop runs at most maxRounds; if the LLM keeps calling tools
// past that, we stop and return the last partial reply.
//
// DONE: Phase 5 (telegram agentic) / STANDARDS A, D, F
package agent

import (
	"context"
	"encoding/json"
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
// but filtered ToolSet per role — the isolation you asked for client vs pegawai vs owner.
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
	// function of (pool, role) and pool is fixed for the process —
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
	for {
		rounds++
		if rounds > maxRounds {
			break
		}
		reply, _, _, err := a.LLM.Chat(ctx, hist, "")
		if err != nil {
			return "", err
		}
		// Parse the LLM's reply: tool call or final answer.
		call, answer, ok := parseReply(reply)
		if !ok {
			// Not JSON — treat as a final answer (defensive).
			if a.History != nil {
				a.History.AppendForRole(tenantID, role, userName, message, reply)
			}
			return strings.TrimSpace(reply), nil
		}
		if answer != "" {
			if a.History != nil {
				a.History.AppendForRole(tenantID, role, userName, message, answer)
			}
			return answer, nil
		}
		// Execute the tool call — isolated per role (same LLM_API_KEY, different allowlist).
		if role != "owner" {
			if !allowedToolsFor(role)[call.Tool] {
				hist = append(hist, llm.Message{Role: "assistant", Content: reply})
				hist = append(hist, llm.Message{Role: "user", Content: "Error: you don't have access to tool " + call.Tool + ". This is a client FAQ bot — try availability or booking help."})
				continue
			}
		}
		tool, ok := a.Tools.Get(call.Tool)
		if !ok {
			hist = append(hist, llm.Message{Role: "assistant", Content: reply})
			hist = append(hist, llm.Message{Role: "user", Content: "Error: unknown tool " + call.Tool + ". Pick a valid tool or answer directly."})
			continue
		}
		result := tool.Run(ctx, tenantID, call.Args)
		slog.Info("agent tool", "tenant", tenantID, "tool", call.Tool, "result", truncate(result, 200))
		// Feed the result back to the LLM.
		hist = append(hist, llm.Message{Role: "assistant", Content: reply})
		hist = append(hist, llm.Message{Role: "user", Content: "Tool " + call.Tool + " returned:\n" + result + "\nContinue: either answer the user or call another tool."})
	}
	// Ran out of rounds.
	msg := "I hit my processing limit — please rephrase or ask in smaller steps."
	if a.History != nil {
		a.History.AppendForRole(tenantID, role, userName, message, msg)
	}
	return msg, nil
}

// reply is the parsed LLM reply.
type reply struct {
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args"`
	Answer string          `json:"answer"`
}

// XML-ish wrappers some models emit around tool calls/answers. Compiled
// once — parseReply runs on every agent turn, so per-call MustCompile
// would be pure garbage under load.
var (
	reToolCall = regexp.MustCompile(`(?is)<tool_call\b[^>]*>\s*(.*?)\s*</tool_call>`)
	reAnswer   = regexp.MustCompile(`(?is)<answer\b[^>]*>\s*(.*?)\s*</answer>`)
	reFuncCall = regexp.MustCompile(`(?is)<function_call\b[^>]*>\s*(.*?)\s*</function_call>`)
)

// parseReply tries to parse the LLM's reply as a tool call or answer.
// Tolerates markdown fences and XML-ish wrappers some models emit
// (e.g. <tool_call>…</tool_call>, <answer>…</answer>, ```json … ```).
func parseReply(s string) (call reply, answer string, ok bool) {
	s = strings.TrimSpace(s)
	// Strip <tool_call> / <answer> style XML wrappers (case-insensitive, multiline)
	stripTag := func(re *regexp.Regexp) string {
		if m := re.FindStringSubmatch(s); m != nil {
			return strings.TrimSpace(m[1])
		}
		return s
	}
	s = stripTag(reToolCall)
	s = stripTag(reAnswer)
	s = stripTag(reFuncCall)
	// Strip code fences if the model wrapped the JSON.
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		// Try to salvage an object embedded after a ":" or newline (e.g. `tool: {"tool":...}`)
		if i := strings.Index(s, `{"tool"`); i >= 0 {
			s = s[i:]
		} else if i := strings.Index(s, `{"answer"`); i >= 0 {
			s = s[i:]
		} else {
			return reply{}, "", false
		}
	}
	// Trim trailing garbage after the closing brace of the first object.
	if i := strings.Index(s[1:], "\n{"); i >= 0 {
		// keep only first object if there's a second — unlikely; leave as-is
	}
	var r reply
	dec := json.NewDecoder(strings.NewReader(s))
	if err := dec.Decode(&r); err != nil {
		return reply{}, "", false
	}
	if r.Answer != "" {
		return r, r.Answer, true
	}
	if r.Tool != "" {
		return r, "", true
	}
	// Some models nest: {"tool_call": {"name": ..., "arguments": {...}}}
	if r.Tool == "" {
		var v map[string]json.RawMessage
		if err := json.Unmarshal([]byte(s), &v); err == nil {
			if tc, ok := v["tool_call"]; ok {
				var inner struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				}
				if err := json.Unmarshal(tc, &inner); err == nil && inner.Name != "" {
					return reply{Tool: inner.Name, Args: inner.Arguments}, "", true
				}
			}
		}
	}
	return reply{}, "", false
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
