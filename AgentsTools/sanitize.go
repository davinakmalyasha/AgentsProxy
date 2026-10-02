package agentstools

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"unicode"
)

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
// (“{"tool":...,"args":...}“) and {"answer":...}).
//
// They are kept as separate patterns rather than one alternation so a
// suppression can name which shape appeared â€” see firstMarkupHit.
//
// This is defence in depth. The primary fix is that a tool call now travels
// as a real tool_calls field, so the model has no reason to improvise. But
// the guard is what guarantees a regression cannot put protocol internals
// in front of a user or into stored history â€” which is the failure that
// shipped raw <tool_call>{function=â€¦} markup into the chat and then
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

// markupBlock is a whole <tool_call>â€¦</tool_call> region, which is what
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
// already correct â€” throwing the whole reply away because of a trailing
// wrapper left the user with "Maaf, jawabannya tidak bisa ditampilkan" for
// a question that had been answered. So: remove the protocol, keep the
// prose. If nothing readable is left, the turn really was nothing but
// protocol, and the caller is told so.
func SanitiseReply(text string) string {
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
			"pattern", hit.pattern, "at", hit.at, "text", Truncate(hit.snippet, 160))
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
