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
// RLHF'd <tool_call>{function=…} wrapper instead, the hand-rolled
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
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/adkrunner"
	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
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
	Tools   *agentstools.ToolSet
	History *History

	// Projects is the per-tenant agent configuration, keyed by tenant id.
	// nil means "no project config", which is a supported state: the agent
	// then runs on the platform base prompt alone.
	Projects map[string]*agentconfig.Project

	// roleDefs memoizes the per-role tool-definition lists used in the
	// system prompt (a pure function of Tools.Pool + role, immutable).
	roleDefs sync.Map
}

// New wires an agent. history may be nil (tests).
func New(llmCli *llm.Client, tools *agentstools.ToolSet, hist *History) *Agent {
	return &Agent{LLM: llmCli, Tools: tools, History: hist}
}

// WithProjects attaches the per-tenant agent configuration and returns the
// agent, so main can keep a single expression.
func (a *Agent) WithProjects(p map[string]*agentconfig.Project) *Agent {
	a.Projects = p
	return a
}

// definitionsForRole returns the tool definitions for a role, building
// the filtered set once per (pool, role) and caching it.
func (a *Agent) definitionsForRole(role string) []string {
	if v, ok := a.roleDefs.Load(role); ok {
		return v.([]string)
	}
	defs := a.Tools.Definitions()
	if role != "owner" {
		if filtered := agentstools.NewToolSetFor(a.Tools.Pool, role); filtered != nil && len(filtered.Definitions()) > 0 {
			defs = filtered.Definitions()
		}
	}
	actual, _ := a.roleDefs.LoadOrStore(role, defs)
	return actual.([]string)
}

// ProjectFor returns the configured project for a tenant, or nil.
// (Lived in prompt.go before the prompt moved to adkrunner; kept here
// because it reads the Agent's own map. Deleted with the old loop.)
func (a *Agent) ProjectFor(tenantID string) *agentconfig.Project {
	if a.Projects == nil {
		return nil
	}
	return a.Projects[tenantID]
}

// Run processes one user message for the given tenant and returns
// the agent's reply. It appends the exchange to history.
func (a *Agent) Run(ctx context.Context, tenantID, vertical, userName, message string) (string, error) {
	return a.RunWithRole(ctx, tenantID, vertical, userName, "owner", message)
}

// RunWithRole is the role-aware entry. Same shared LLM key (AgentsProxy/.env LLM_API_KEY)
// but filtered agentstools.ToolSet per role Ã¢â‚¬â€ the isolation you asked for client vs pegawai vs owner.
func (a *Agent) RunWithRole(ctx context.Context, tenantID, vertical, userName, role, message string) (string, error) {
	// Scope lock for client: if message is clearly off-topic/jailbreak, hardcode fallback without LLM
	if role == "client" && agentstools.IsOffTopic(message) {
		return agentstools.HardcodedScopeReply, nil
	}
	// Per-person RPD (30/80/150) + per-tenant+role cap (20/80/150) for abuse
	if !agentstools.RPDCapAllowedForUser(ctx, tenantID, role, userName) {
		return agentstools.HardcodedAbuseReply, nil
	}
	if !agentstools.CostCapAllowedForRole(ctx, tenantID, role) {
		return "Daily AI usage limit reached. Try again tomorrow.", nil
	}
	// Build role-filtered prompt. The filtered agentstools.ToolSet is a pure
	// function of (pool, role) and pool is fixed for the process Ã¢â‚¬â€
	// cache per role instead of rebuilding ~20 closures + defs every
	// message. Definitions() output is immutable, so a shared copy is
	// safe to hand the prompt builder.
	toolDefs := a.definitionsForRole(role)

	// Project config for this tenant, if one is configured. A tenant with no
	// projects/ folder still gets a working agent on the platform base
	// alone: adding agent config must not be a precondition for the agent
	// existing.
	project := a.ProjectFor(tenantID)

	sys := adkrunner.Assemble(adkrunner.Layers{
		Project:  project,
		Role:     role,
		ToolDefs: toolDefs,
		Message:  message,
		Vertical: vertical,
	})

	// Load history per tenant+role+user (good-as-possible, central store)
	hist := []llm.Message{{Role: "system", Content: sys}}
	if a.History != nil {
		hist = append(hist, a.History.LoadForRole(tenantID, role, userName)...)
	}
	hist = append(hist, llm.Message{Role: "user", Content: userName + ": " + message})

	rounds := 0
	// Route once, in Go, over the already role-filtered agentstools.ToolSet. A agentstools.Tool
	// the role may not call is not in the set, so it cannot be selected.
	routed := a.Tools.Route(message)
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

		// askForPinnedTool gets a call for the pinned tool, retrying once
		// if the provider answers in prose instead. The gateway honours a
		// PINNED tool_choice but does not guarantee a call comes back, and
		// returning that prose used to be the end of it â€” so the model
		// answered "how much did we take" out of chat history rather than
		// the database, a number with no query behind it presented with
		// the same confidence as one that had been looked up.
		//
		// It also drops any call for a tool other than the pinned one.
		// The gateway accepts a call for whatever name the model emits,
		// even under a pinned choice â€” it was observed volunteering a
		// second tool alongside the pinned one â€” so the pin was advisory.
		// That is harmless for the read-only reporting tools and not
		// harmless for the write ones: a user asking "how much did we take
		// today" would have had the agent cancel a booking or take a
		// payment because the model felt like mentioning it.
		//
		// What the gate may refuse is a WRITE the model offered on its own
		// initiative. It may NOT refuse a read. The first version refused
		// both, and that turned a routing mistake into a total outage: the
		// router pinned book_slot for "hari ini ada pesanan?", the model
		// correctly called list_bookings, the gate deleted that call, the
		// retry repeated it, and the owner was told the system could not
		// fetch data -- with a "Coba lagi" button that re-ran the identical
		// deterministic path. Role isolation does not need this gate:
		// agent.go's execution loop resolves every call through
		// a.Tools.Get, and that set is already role-filtered.
		keepUsable := func(calls []llm.ToolCall) []llm.ToolCall {
			out := make([]llm.ToolCall, 0, 1)
			for _, c := range calls {
				name := c.Function.Name
				if name == routed.Tool {
					out = append(out, c)
					continue
				}
				tool, known := a.Tools.Get(name)
				if !known {
					// Not ours. The execution loop refuses it with "no such
					// tool" and lets the model correct itself; counting it
					// here would only suppress that retry.
					continue
				}
				if !tool.Mutating {
					out = append(out, c)
					continue
				}
				slog.Info("agent: refused an unpinned write tool",
					"tenant", tenantID, "role", role,
					"pinned", routed.Tool, "refused", name)
			}
			return out
		}
		askForPinnedTool := func() (*llm.Completion, []llm.ToolCall) {
			comp, err := a.LLM.ChatWithTools(ctx, hist, defs, choice, "")
			if err != nil {
				return nil, nil
			}
			if routed.Tool == "" {
				return comp, comp.ToolCalls
			}
			calls := keepUsable(comp.ToolCalls)
			if len(calls) > 0 {
				return comp, calls
			}
			// Nothing usable came back. Say so once, with the requirement
			// stated outright, and try again.
			hist = append(hist, llm.Message{Role: "assistant", Content: comp.Content})
			hist = append(hist, llm.Message{
				Role: "user",
				Content: fmt.Sprintf(
					"Answer this by calling the %s agentstools.Tool. Do not answer from memory or from "+
						"earlier messages in this conversation: the agentstools.Tool result is the only "+
						"source of these numbers. Call it now.",
					routed.Tool),
			})
			comp2, err := a.LLM.ChatWithTools(ctx, hist, defs, choice, "")
			if err != nil {
				return nil, nil
			}
			return comp2, keepUsable(comp2.ToolCalls)
		}

		comp, calls := askForPinnedTool()
		if comp == nil {
			return "", fmt.Errorf("agent: provider call failed")
		}
		if len(calls) == 0 {
			if routed.Tool != "" {
				// Two attempts, still nothing. Refusing is the only honest
				// outcome: the pinned tool never ran, so every figure in
				// this reply would be invented. A visible failure is
				// strictly better than a confident wrong number about
				// money.
				//
				// finish_reason is logged because "the model ignored the
				// tool" and "the model ran out of tokens" look identical
				// from here, and they have completely different fixes. That
				// distinction used to be captured on the Completion and read
				// by nobody.
				slog.Error("agent: no usable agentstools.Tool call after retry",
					"tenant", tenantID, "role", role,
					"pinned", routed.Tool, "score", routed.Score,
					"finish_reason", comp.FinishReason,
					"completion_tokens", comp.CompletionTok,
					"truncated", comp.FinishReason == "length")
				return a.finish(ctx, tenantID, role, userName, message,
					refusedNoData+" Silakan coba phrasing lain."), nil
			}
			// Nothing was routed â€” a greeting, or a question with no data
			// behind it. The model's own answer is correct here, and
			// inventing a tool call for it would be worse.
			return a.finish(ctx, tenantID, role, userName, message, agentstools.SanitiseReply(comp.Content)), nil
		}

		// Phase 2: run the tool. Tool results are recorded against the
		// provider's own call id so the next turn can be matched to it.
		for _, call := range calls {
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
			args := agentstools.NormaliseArguments(call.Function.Arguments)
			// Hold the tool to the schema it was advertised with. A tool's
			// Run unmarshals into a struct, and that silently ignores a
			// field of the wrong type â€” so bad arguments used to run the
			// tool with defaults and report success. Feeding the complaint
			// back as the tool result lets the model correct itself on the
			// next round instead.
			if verr := agentstools.ValidateArgs(tool.Parameters, args); verr != nil {
				slog.Warn("agent: rejected tool arguments",
					"tenant", tenantID, "role", role,
					"tool", call.Function.Name, "err", verr)
				hist = append(hist, llm.Message{
					Role: "assistant", Content: comp.Content, ToolCalls: comp.ToolCalls,
				})
				hist = append(hist, llm.Message{
					Role: "tool", ToolCallID: call.ID,
					Content: "Error: invalid arguments â€” " + verr.Error() + ". Call the tool again with corrected arguments.",
				})
				continue
			}
			result := tool.Run(ctx, tenantID, args)
			slog.Info("agent tool", "tenant", tenantID, "role", role,
				"tool", call.Function.Name, "result", agentstools.Truncate(result, 200))
			hist = append(hist, llm.Message{
				Role: "assistant", Content: comp.Content, ToolCalls: comp.ToolCalls,
			})
			hist = append(hist, llm.Message{Role: "agentstools.Tool", ToolCallID: call.ID, Content: result})
		}
		// The next turn narrates. The declarations are withdrawn, but the
		// model is not told that, and it has been observed writing a
		// <tool_call> wrapper by hand to chain a second call -- which the
		// output guard then had to suppress, so a user asking a perfectly
		// ordinary question got "Maaf, jawabannya tidak bisa ditampilkan"
		// for an answer that had already been computed. Saying the agentstools.Tool
		// phase is over stops that at the source.
		hist = append(hist, llm.Message{
			Role: "user",
			Content: "The data you need is in the agentstools.Tool result above. Answer the original " +
				"question in plain language now. You cannot call any more tools in this turn, " +
				"so do not attempt one and do not write agentstools.Tool-call syntax.",
		})
		narr, err := a.LLM.Complete(ctx, hist, nil, nil, "")
		if err != nil {
			return "", err
		}
		return a.finish(ctx, tenantID, role, userName, message, agentstools.SanitiseReply(narr.Content)), nil
	}
	// Ran out of rounds.
	msg := "I hit my processing limit Ã¢â‚¬â€ please rephrase or ask in smaller steps."
	if a.History != nil {
		a.History.AppendForRole(tenantID, role, userName, message, msg)
	}
	return msg, nil
}

// refusedNoData is the reply used when a data question was routed, no
// usable tool call came back, and the only honest answer is that the agent
// cannot ground itself. Named so tests assert on one string.
//
// It deliberately does NOT contain "coba lagi": the frontend's isErrorReply
// matches that phrase and renders "Gagal terhubung - periksa koneksi",
// which blames the user's network for a server-side refusal that is
// deterministic and will repeat identically if they press the button again.
const refusedNoData = "Maaf, saya belum bisa menjawab itu dengan data dari sistem."

// finish records the exchange and returns the reply.
func (a *Agent) finish(ctx context.Context, tenantID, role, userName, message, reply string) string {
	if a.History != nil {
		a.History.AppendForRole(tenantID, role, userName, message, reply)
	}
	return reply
}
