package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Runtime validation of tool arguments against the JSON Schema that was
// already being sent to the provider.
//
// Every tool declares a schema in meta.go and it goes out to the model,
// but nothing ever checked the model's answer against it. A tool's Run
// then json.Unmarshals into a struct, and that is a no-op for a field of
// the wrong type: `{"limit": "abc"}` leaves Limit at its zero value, and
// top_customers turned that into "abc" being ignored and the default
// limit of 5 being used with no indication anything was wrong. The schema
// was documentation the server never held itself to.
//
// This validates the subset the schemas actually use, with no new
// dependency: type, required, enum, minimum, maximum, and format:"date".
// Unknown keywords are ignored rather than rejected, so adding one to a
// schema cannot start failing calls.

// validateArgs checks decoded arguments against a schema. It returns an
// error phrased for the model, because the caller feeds it back as the
// tool result so the model can correct itself on the next round.
func validateArgs(schema, args json.RawMessage) error {
	if len(schema) == 0 {
		return nil
	}
	var s struct {
		Type       string                    `json:"type"`
		Required   []string                  `json:"required"`
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		// A schema we cannot parse is our bug, not the caller's. Do not
		// block a working tool over it.
		slog.Warn("agent: tool schema was unparseable; skipping validation")
		return nil
	}
	var obj map[string]json.RawMessage
	if len(args) > 0 {
		if err := json.Unmarshal(args, &obj); err != nil {
			return fmt.Errorf("arguments must be a JSON object")
		}
	}
	if obj == nil {
		obj = map[string]json.RawMessage{}
	}
	if s.Type == "object" && obj == nil {
		return fmt.Errorf("arguments must be a JSON object")
	}
	for _, name := range s.Required {
		if _, ok := obj[name]; !ok {
			return fmt.Errorf("missing required argument %q", name)
		}
	}
	for name, raw := range obj {
		prop, ok := s.Properties[name]
		if !ok {
			// Extra properties are tolerated. The model sometimes echoes a
			// field the tool ignores, and rejecting it would turn a
			// harmless extra into a failed call.
			continue
		}
		if err := validateValue(name, prop, raw); err != nil {
			return err
		}
	}
	return nil
}

// validateValue checks one property against its schema fragment.
func validateValue(name string, prop map[string]any, raw json.RawMessage) error {
	declared, _ := prop["type"].(string)

	// format:"date" is checked as a real calendar date. The tools parse
	// these with time.Parse and silently fall back to "this month" when
	// parsing fails, so a malformed date turned into a plausible answer
	// about the wrong period.
	if format, _ := prop["format"].(string); format == "date" {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("%s must be a YYYY-MM-DD date string", name)
		}
		if _, err := time.Parse("2006-01-02", strings.TrimSpace(s)); err != nil {
			return fmt.Errorf("%s must be a YYYY-MM-DD date, got %q", name, truncate(s, 32))
		}
		return nil
	}

	if enum, ok := prop["enum"].([]any); ok && len(enum) > 0 {
		var got string
		if err := json.Unmarshal(raw, &got); err != nil {
			return fmt.Errorf("%s must be one of the allowed values", name)
		}
		allowed := make([]string, 0, len(enum))
		match := false
		for _, e := range enum {
			s, _ := e.(string)
			allowed = append(allowed, s)
			if s == got {
				match = true
			}
		}
		if !match {
			return fmt.Errorf("%s must be one of [%s], got %q", name, strings.Join(allowed, ", "), truncate(got, 32))
		}
		return nil
	}

	switch declared {
	case "string", "":
		// An undeclared type is accepted as-is rather than guessed at.
		if declared == "" {
			return nil
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("%s must be a string", name)
		}
		if err := checkLength(name, prop, len(s)); err != nil {
			return err
		}
	case "integer":
		n, err := asNumber(raw)
		if err != nil {
			return fmt.Errorf("%s must be a whole number", name)
		}
		if n != float64(int64(n)) {
			return fmt.Errorf("%s must be a whole number", name)
		}
		return checkRange(name, prop, n)
	case "number":
		n, err := asNumber(raw)
		if err != nil {
			return fmt.Errorf("%s must be a number", name)
		}
		return checkRange(name, prop, n)
	case "boolean":
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return fmt.Errorf("%s must be true or false", name)
		}
	case "array":
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return fmt.Errorf("%s must be a list", name)
		}
	}
	return nil
}

func asNumber(raw json.RawMessage) (float64, error) {
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		// A provider may send "5" as a JSON string. Accept it rather than
		// failing a call the model clearly meant, but nothing looser.
		var s string
		if err2 := json.Unmarshal(raw, &s); err2 == nil {
			return strconv.ParseFloat(strings.TrimSpace(s), 64)
		}
		return 0, err
	}
	return n, nil
}

func checkRange(name string, prop map[string]any, n float64) error {
	if min, ok := prop["minimum"].(float64); ok && n < min {
		return fmt.Errorf("%s must be at least %s", name, trimNum(min))
	}
	if max, ok := prop["maximum"].(float64); ok && n > max {
		return fmt.Errorf("%s must be at most %s", name, trimNum(max))
	}
	return nil
}

func checkLength(name string, prop map[string]any, n int) error {
	if min, ok := prop["minLength"].(float64); ok && float64(n) < min {
		return fmt.Errorf("%s must be at least %d characters", name, int(min))
	}
	if max, ok := prop["maxLength"].(float64); ok && float64(n) > max {
		return fmt.Errorf("%s must be at most %d characters", name, int(max))
	}
	return nil
}

func trimNum(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
