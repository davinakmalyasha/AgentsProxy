package agent

import (
	"strings"
	"unicode"
)

// Routing decides which tool a message is about, in Go, before the model is
// consulted.
//
// Why not let the model choose: the configured gateway (OpenAI-compatible,
// xiaomi/mimo-v2.5 behind api.commandcode.ai) accepts a `tools` array but
// silently ignores `tool_choice: "auto"` and `"required"` — verified
// against the live endpoint, where both return prose with no tool_calls,
// while an explicitly named tool_choice returns finish_reason "tool_calls".
// So an auto-selecting agent is a coin flip on that provider.
//
// The fallback that made it look like it worked — a system prompt telling
// the model to emit {"tool": ..., "args": ...} as prose — is what leaked
// raw XML into the chat, because the model follows its own RLHF'd
// <tool_call> convention instead when no machine-readable interface is
// offered. Pinning the tool removes both the flake and the leak, and makes
// the behaviour testable: given a message and a tool set, the routing
// decision is a pure function.
//
// Role isolation is unaffected: routing only ever runs against the
// already-filtered ToolSet, so a tool the role cannot call cannot be
// selected, let alone executed.

// strongWeight is how much a decisive keyword is worth. High enough to
// outrank the accumulated generic vocabulary of a competing tool.
const strongWeight = 5

type routeResult struct {
	Tool      string
	Score     int
	Confident bool
}

// route picks the best tool for a message.
//
// Scoring is deliberately simple and explainable rather than clever: exact
// tool name, curated keywords, then description-word overlap. A model call
// to disambiguate would cost a round-trip and reintroduce the same
// non-determinism.
func (ts *ToolSet) route(message string) routeResult {
	norm := normalise(message)
	words := tokenize(norm)
	if len(words) == 0 {
		return routeResult{}
	}

	best := routeResult{}
	for _, name := range ts.Names() {
		t := ts.tools[name]
		score := scoreTool(t, norm, words)
		// Ties break on name for a stable decision.
		if score > best.Score || (score == best.Score && best.Tool != "" && name < best.Tool) {
			best = routeResult{Tool: name, Score: score}
		}
	}
	// A single keyword hit is enough to be worth pinning; two is enough to
	// call it confident. Below the threshold we let the model answer from
	// conversation rather than guessing a query.
	if best.Score <= 0 {
		return routeResult{}
	}
	best.Confident = best.Score >= 2
	return best
}

func scoreTool(t Tool, norm string, words []string) int {
	score := 0
	// 1. The user named the tool outright ("query_income", "closing report").
	snakes := strings.ReplaceAll(t.Name, "_", " ")
	if strings.Contains(norm, t.Name) || strings.Contains(norm, snakes) {
		score += 4
	}
	// 2. Strong keywords are decisive. "laba" means profit and nothing else;
	//    "kosong" is an availability question, not a request to book. These
	//    outweigh the generic vocabulary below, which is what makes
	//    "Berapa laba bulan ini?" reach query_net_profit instead of losing
	//    a tie against query_income on "berapa".
	for _, kw := range t.StrongKeywords {
		if kw != "" && strings.Contains(norm, kw) {
			score += strongWeight
		}
	}
	// 3. Ordinary keywords, weighted by length so a specific term ("excel",
	//    "stok") beats a generic one.
	for _, kw := range t.Keywords {
		if kw == "" {
			continue
		}
		if strings.Contains(norm, kw) {
			score += 1 + min(len(tokenize(kw)), 3)
		}
	}
	// 4. Description overlap, so a tool with no curated vocabulary is still
	//    reachable. Capped: prose is the least reliable signal here and must
	//    not be able to outvote a strong keyword.
	overlap := 0
	for _, w := range words {
		if len(w) < 4 {
			continue
		}
		if strings.Contains(strings.ToLower(t.Description), w) {
			overlap++
		}
	}
	score += min(overlap, 3)
	return score
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// normalise lowercases and collapses punctuation to single spaces, so
// "Jam Tersibuk?" and "jam-tersibuk" route the same.
func normalise(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := true
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prevSpace = false
			continue
		}
		if !prevSpace {
			b.WriteByte(' ')
			prevSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// tokenize splits on spaces. Callers pre-normalise.
func tokenize(s string) []string {
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
