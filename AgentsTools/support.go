package agentstools

import (
	"encoding/json"
	"log/slog"
	"strings"
)

// Truncate shortens s to n bytes for logs. Tool results can be kilobytes
// and the model only ever sees the full text through the tool channel;
// what lands in slog must fit on one line.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// NormaliseArguments guards against a provider sending empty or non-object
// arguments. An empty string becomes an empty object (missing args resolve
// to server-side defaults); a non-object becomes an empty object with a
// warning, because running a tool with half-parsed arguments is how a
// wrong-typed field used to silently become a default.
func NormaliseArguments(raw string) json.RawMessage {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return json.RawMessage(`{}`)
	}
	if !strings.HasPrefix(trimmed, "{") {
		slog.Warn("agent: tool arguments were not a JSON object", "args", Truncate(trimmed, 80))
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(trimmed)
}

// ErrMsg renders an error for a tool result. Tools return strings, not
// errors, so a nil error must still produce a readable acknowledgement
// rather than an empty string the model reads as silence.
func ErrMsg(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}
