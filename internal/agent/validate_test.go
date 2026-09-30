// Runtime argument validation.
//
// The tools advertised JSON Schemas to the provider but never checked the
// model's arguments against them. Run unmarshals into a struct, which is a
// no-op for a wrong-typed field, so bad arguments silently ran the tool
// with zero values and reported success.
package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustRaw(t *testing.T, s string) json.RawMessage {
	t.Helper()
	b := json.RawMessage(s)
	if !json.Valid(b) {
		t.Fatalf("test fixture is not valid JSON: %s", s)
	}
	return b
}

const dateSchema = `{"type":"object","properties":{"from":{"type":"string","format":"date"}}}`
const limitSchema = `{"type":"object","properties":{"limit":{"type":"integer","minimum":1}}}`
const enumSchema = `{"type":"object","properties":{"type":{"type":"string","enum":["lapangan","kantin","semua"]}}}`

func TestValidateAcceptsWhatTheSchemaAllows(t *testing.T) {
	cases := []struct {
		schema, args string
	}{
		{dateSchema, `{"from":"2026-10-01"}`},
		{dateSchema, `{}`}, // every property is optional today
		{limitSchema, `{"limit":5}`},
		{limitSchema, `{"limit":"7"}`}, // numeric string: tolerated on purpose
		{enumSchema, `{"type":"semua"}`},
		{dateSchema, `{"from":"2026-10-01","extra":"ignored"}`}, // extras tolerated
		{dateSchema, `null`},
	}
	for i, c := range cases {
		if err := validateArgs(mustRaw(t, c.schema), mustRaw(t, c.args)); err != nil {
			t.Errorf("case %d: unexpected error: %v", i, err)
		}
	}
}

func TestValidateRejectsMalformedDates(t *testing.T) {
	// The reason this matters: the tools do time.Parse on these and fall
	// back to "this month" when it fails, so a bad date used to become a
	// confident answer about the wrong period rather than an error.
	for _, bad := range []string{`"15-10-2026"`, `"2026-13-01"`, `"2026-02-30"`, `"oktober"`, `""`, `20261001`} {
		args := `{"from":` + bad + `}`
		if err := validateArgs(mustRaw(t, dateSchema), mustRaw(t, args)); err == nil {
			t.Errorf("date %s should have been rejected", bad)
		}
	}
}

func TestValidateAcceptsRealLeapDay(t *testing.T) {
	// Guards against a validator that only checks the shape.
	if err := validateArgs(mustRaw(t, dateSchema), mustRaw(t, `{"from":"2028-02-29"}`)); err != nil {
		t.Errorf("2028 is a leap year, should be accepted: %v", err)
	}
}

func TestValidateRejectsWrongTypes(t *testing.T) {
	// The bug this replaces: {"limit":"abc"} reached Run, Left Limit at 0,
	// and the tool used its default while reporting success.
	if err := validateArgs(mustRaw(t, limitSchema), mustRaw(t, `{"limit":"abc"}`)); err == nil {
		t.Error("a non-numeric limit should be rejected")
	}
	// A fractional value where a whole number is declared.
	if err := validateArgs(mustRaw(t, limitSchema), mustRaw(t, `{"limit":1.5}`)); err == nil {
		t.Error("a fractional limit should be rejected")
	}
	// An object where a string is declared.
	if err := validateArgs(mustRaw(t, dateSchema), mustRaw(t, `{"from":{"y":2026}}`)); err == nil {
		t.Error("an object where a date string is declared should be rejected")
	}
}

func TestValidateEnforcesMinimum(t *testing.T) {
	if err := validateArgs(mustRaw(t, limitSchema), mustRaw(t, `{"limit":0}`)); err == nil {
		t.Error("limit 0 violates minimum 1 and should be rejected")
	}
}

func TestValidateEnforcesEnum(t *testing.T) {
	err := validateArgs(mustRaw(t, enumSchema), mustRaw(t, `{"type":"sewa"}`))
	if err == nil {
		t.Fatal("a value outside the enum should be rejected")
	}
	// The message has to name the allowed values, or the model cannot
	// correct itself on the retry round.
	if !strings.Contains(err.Error(), "lapangan") || !strings.Contains(err.Error(), "kantin") {
		t.Errorf("enum error should list the allowed values, got %q", err)
	}
}

func TestValidateEnforcesRequired(t *testing.T) {
	schema := `{"type":"object","required":["booking_id"],"properties":{"booking_id":{"type":"string"}}}`
	if err := validateArgs(mustRaw(t, schema), mustRaw(t, `{}`)); err == nil {
		t.Error("a missing required argument should be rejected")
	}
	if err := validateArgs(mustRaw(t, schema), mustRaw(t, `{"booking_id":"x"}`)); err != nil {
		t.Errorf("present required argument should pass: %v", err)
	}
}

func TestValidateRejectsNonObjectArguments(t *testing.T) {
	for _, bad := range []string{`[]`, `"text"`, `42`, `true`} {
		if err := validateArgs(mustRaw(t, dateSchema), mustRaw(t, bad)); err == nil {
			t.Errorf("arguments %s should be rejected", bad)
		}
	}
}

func TestValidateToleratesMissingOrBrokenSchema(t *testing.T) {
	// A schema we cannot parse is our bug, not the caller's. Blocking a
	// working tool over it would turn a typo in meta.go into an outage.
	if err := validateArgs(nil, mustRaw(t, `{"limit":0}`)); err != nil {
		t.Errorf("nil schema should skip validation, got %v", err)
	}
	// Deliberately not valid JSON, so it bypasses mustRaw.
	if err := validateArgs(json.RawMessage(`not json`), mustRaw(t, `{"limit":0}`)); err != nil {
		t.Errorf("unparseable schema should skip validation, got %v", err)
	}
}

// Every real tool's schema must survive the validator's own parser. A
// schema written in a shape the validator mishandles would either reject
// every call or, worse, silently accept anything.
func TestEveryRegistrySchemaIsValidJSONObject(t *testing.T) {
	ts := NewToolSet(nil)
	for _, name := range ts.Names() {
		tool, ok := ts.Get(name)
		if !ok {
			t.Fatalf("Names() listed %s but Get() does not have it", name)
		}
		if len(tool.Parameters) == 0 {
			continue // noArgs() covers this at Defs() time
		}
		if !json.Valid(tool.Parameters) {
			t.Errorf("%s: parameters are not valid JSON: %s", name, tool.Parameters)
			continue
		}
		var s struct {
			Type       string                    `json:"type"`
			Properties map[string]map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(tool.Parameters, &s); err != nil {
			t.Errorf("%s: parameters do not fit the expected shape: %v", name, err)
			continue
		}
		if s.Type != "object" {
			t.Errorf("%s: schema type is %q, want \"object\"", name, s.Type)
		}
		// Every declared property must be checkable, or validation is a
		// no-op for it.
		for prop, def := range s.Properties {
			if def == nil {
				t.Errorf("%s: property %q has a null schema", name, prop)
				continue
			}
			if _, ok := def["type"]; !ok {
				t.Errorf("%s: property %q declares no type, so it is never checked", name, prop)
			}
		}
	}
}
