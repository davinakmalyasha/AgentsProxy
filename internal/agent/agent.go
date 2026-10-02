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
	"unicode"

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
		// strength of a guess — the pinned name is the only thing that
		// reaches the provider.
		var choice *llm.ToolChoice
		if routed.Tool != "" {
			choice = llm.ForceTool(routed.Tool)
		}

		// askForPinnedTool gets a call for the pinned tool, retrying once
		// if the provider answers in prose instead. The gateway honours a
		// PINNED tool_choice but does not guarantee a call comes back, and
		// returning that prose used to be the end of it — so the model
		// answered "how much did we take" out of chat history rather than
		// the database, a number with no query behind it presented with
		// the same confidence as one that had been looked up.
		//
		// It also drops any call for a tool other than the pinned one.
		// The gateway accepts a call for whatever name the model emits,
		// even under a pinned choice — it was observed volunteering a
		// second tool alongside the pinned one — so the pin was advisory.
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
					"Answer this by calling the %s tool. Do not answer from memory or from "+
						"earlier messages in this conversation: the tool result is the only "+
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
				slog.Error("agent: no usable tool call after retry",
					"tenant", tenantID, "role", role,
					"pinned", routed.Tool, "score", routed.Score,
					"finish_reason", comp.FinishReason,
					"completion_tokens", comp.CompletionTok,
					"truncated", comp.FinishReason == "length")
				return a.finish(ctx, tenantID, role, userName, message,
					refusedNoData+" Silakan coba phrasing lain."), nil
			}
			// Nothing was routed — a greeting, or a question with no data
			// behind it. The model's own answer is correct here, and
			// inventing a tool call for it would be worse.
			return a.finish(ctx, tenantID, role, userName, message, sanitiseReply(comp.Content)), nil
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
			args := normaliseArguments(call.Function.Arguments)
			// Hold the tool to the schema it was advertised with. A tool's
			// Run unmarshals into a struct, and that silently ignores a
			// field of the wrong type — so bad arguments used to run the
			// tool with defaults and report success. Feeding the complaint
			// back as the tool result lets the model correct itself on the
			// next round instead.
			if verr := validateArgs(tool.Parameters, args); verr != nil {
				slog.Warn("agent: rejected tool arguments",
					"tenant", tenantID, "role", role,
					"tool", call.Function.Name, "err", verr)
				hist = append(hist, llm.Message{
					Role: "assistant", Content: comp.Content, ToolCalls: comp.ToolCalls,
				})
				hist = append(hist, llm.Message{
					Role: "tool", ToolCallID: call.ID,
					Content: "Error: invalid arguments — " + verr.Error() + ". Call the tool again with corrected arguments.",
				})
				continue
			}
			result := tool.Run(ctx, tenantID, args)
			slog.Info("agent tool", "tenant", tenantID, "role", role,
				"tool", call.Function.Name, "result", truncate(result, 200))
			hist = append(hist, llm.Message{
				Role: "assistant", Content: comp.Content, ToolCalls: comp.ToolCalls,
			})
			hist = append(hist, llm.Message{Role: "tool", ToolCallID: call.ID, Content: result})
		}
		// The next turn narrates. The declarations are withdrawn, but the
		// model is not told that, and it has been observed writing a
		// <tool_call> wrapper by hand to chain a second call -- which the
		// output guard then had to suppress, so a user asking a perfectly
		// ordinary question got "Maaf, jawabannya tidak bisa ditampilkan"
		// for an answer that had already been computed. Saying the tool
		// phase is over stops that at the source.
		hist = append(hist, llm.Message{
			Role: "user",
			Content: "The data you need is in the tool result above. Answer the original " +
				"question in plain language now. You cannot call any more tools in this turn, " +
				"so do not attempt one and do not write tool-call syntax.",
		})
		narr, err := a.LLM.Complete(ctx, hist, nil, nil, "")
		if err != nil {
			return "", err
		}
		return a.finish(ctx, tenantID, role, userName, message, sanitiseReply(narr.Content)), nil
	}
	// Ran out of rounds.
	msg := "I hit my processing limit â€” please rephrase or ask in smaller steps."
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

// zws matches zero or more characters that can sit invisibly between "<"
// and a tag name. The trailing "*" is part of the constant on purpose:
// these are interpolated into patterns where the quantifier is easy to drop,
// and a class without "*" silently means "exactly one", which turns every
// plain "<tool_call>" into a non-match.
//
// This started as "whitespace or U+200B" and was wrong twice over: the
// captured real reply used U+00A0 (no-break space), which is neither \s nor
// U+200B, so the guard did not even detect the wrapper. Rather than keep
// adding one code point at a time, this covers the Unicode space-separator
// and format categories plus the specific invisibles seen in the wild. A
// legitimate answer never puts one of these inside a tag.
const zws = `[\s\p{Zs}\p{Cf}\x{200b}\x{feff}\x{00a0}]*`

// stripInvisible removes format and control characters, keeping newline and
// tab.
//
// This is the general answer to the guard being bypassable: rather than
// enumerate every invisible character a model might use to split a tag, the
// text is normalised first. A zero-width space, a word-joiner, a bidi mark
// or a stray control byte inside "<tool_call>" is never legitimate prose,
// and leaving one in the output would render as garbage anyway. Measured on
// a real captured reply, this is what separated a detected wrapper from an
// undetected one.
func stripInvisible(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\t' {
			b.WriteRune(r)
			continue
		}
		if unicode.Is(unicode.Cf, r) || unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
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
	regexp.MustCompile(`(?is)<` + zws + `/?` + zws + `tool_call`),
	regexp.MustCompile(`(?is)<` + zws + `/?` + zws + `function_call`),
	regexp.MustCompile(`(?is)<` + zws + `/?` + zws + `parameter`),
	regexp.MustCompile(`(?is)<` + zws + `/?` + zws + `answer`),
	regexp.MustCompile(`(?i)"tool_call"\s*:`),
	regexp.MustCompile(`(?i)"function_call"\s*:`),
	regexp.MustCompile(`(?i)"tool"\s*:\s*"`),
	regexp.MustCompile(`(?i)"args"\s*:`),
}

// markupBlock is a whole <tool_call>…</tool_call> region, which is what
// the model writes when it wants to chain a second call.
var markupBlock = regexp.MustCompile(`(?is)<` + zws + `/?` + zws + `(tool_call|function_call)` + zws + `>.*?<\s*/?\s*` + zws + `(tool_call|function_call)` + zws + `>`)

// markupTag is any remaining protocol-shaped tag, opened or closed.
var markupTag = regexp.MustCompile(`(?is)<` + zws + `/?` + zws + `(tool_call|function_call|parameter|answer)` + zws + `[^>]*>`)

// markupJSON finds a protocol envelope: {"tool_call":…}, {"tool":…,
// "args":…}, {"function_call":…}. It matches any small JSON-ish object and
// the decision to remove it is made in Go by jsonObjectIsProtocol, because
// the nesting depth varies and a single regex cannot both find the object
// and judge its keys.
var markupJSON = regexp.MustCompile(`(?s)\{(?:[^{}]|\{[^{}]*\})*\}`)

// jsonObjectIsProtocol reports whether a candidate JSON object carries one
// of the protocol keys.
func jsonObjectIsProtocol(s string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return false
	}
	for _, k := range []string{"tool_call", "function_call", "tool", "args", "answer"} {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// stripProtocol removes every protocol envelope found in s.
func stripProtocol(s string) string {
	return markupJSON.ReplaceAllStringFunc(s, func(m string) string {
		if jsonObjectIsProtocol(m) {
			return " "
		}
		return m
	})
}

// sanitiseReply is the last gate before a model turn becomes a user-visible
// answer.
//
// It strips rather than discards. The model sometimes finishes a correct
// answer and then, in the same turn, tries to chain another call by writing
// the wrapper in prose. The tool had already run and the numbers were
// already correct — throwing the whole reply away because of a trailing
// wrapper left the user with "Maaf, jawabannya tidak bisa ditampilkan" for
// a question that had been answered. So: remove the protocol, keep the
// prose. If nothing readable is left, the turn really was nothing but
// protocol, and the caller is told so.
func sanitiseReply(text string) string {
	t := strings.TrimSpace(stripInvisible(text))
	if t == "" {
		return "Saya belum bisa menjawab itu. Coba tanya hal yang lebih spesifik tentang data usaha."
	}
	hit := firstMarkupHit(t)
	if hit == nil {
		return t
	}
	cleaned := markupBlock.ReplaceAllString(t, " ")
	cleaned = stripProtocol(cleaned)
	cleaned = markupTag.ReplaceAllString(cleaned, " ")
	// A JSON envelope can survive with stray quotes or a lone brace behind,
	// and an unterminated tag can leave fragments. Nothing readable
	// survives, so the emptiness check below catches those.
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" || !hasReadableText(cleaned) {
		slog.Warn("agent: reply was entirely protocol markup; suppressed",
			"pattern", hit.pattern, "at", hit.at, "text", truncate(hit.snippet, 160))
		return "Maaf, jawabannya tidak bisa ditampilkan. Coba ulangi pertanyaannya dengan kalimat lain."
	}
	// Prose survived, so the user gets their answer and the protocol still
	// never reaches them. Not a warning: this is the model chaining a call,
	// which is noisy rather than dangerous.
	slog.Info("agent: stripped protocol markup from a reply, kept the prose",
		"pattern", hit.pattern, "at", hit.at)
	return cleaned
}

// hasReadableText reports whether anything is left that a person would
// read as an answer, rather than a leftover fragment of a wrapper.
func hasReadableText(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
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
