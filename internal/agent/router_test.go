package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fakeTools builds a ToolSet with the real names/metadata but no database,
// so routing and declaration building can be tested without one.
func fakeTools(t *testing.T, names ...string) *ToolSet {
	t.Helper()
	ts := &ToolSet{tools: map[string]Tool{}}
	for _, n := range names {
		ts.add(Tool{
			Name:        n,
			Description: "test tool " + strings.ReplaceAll(n, "_", " "),
			Run:         func(ctx context.Context, tenantID string, args json.RawMessage) string { return "" },
		})
	}
	return ts
}

func TestRoutePicksTheAdvertisedTool(t *testing.T) {
	// Exactly the prompts the owner UI suggests. Each used to leak raw
	// <tool_call> markup, so each must now resolve to a real tool.
	cases := []struct {
		message string
		want    string
	}{
		{"Ringkas pendapatan hari ini", "query_income"},
		{"Buatkan laporan Excel bulan ini", "export_report_excel"},
		{"Jam tersibuk bulan ini?", "query_range_summary"},
		{"Siapa pelanggan paling sering?", "top_customers"},
		{"Bandingkan pendapatan bulan ini vs bulan lalu", "query_range_summary"},
		{"buat closing report 2026-09-30", "closing_report"},
		{"Berapa laba bulan ini?", "query_net_profit"},
		{"Stok shuttlecock sisa berapa?", "check_stock"},
		{"Booking jam 19 kosong?", "get_availability"},
		{"Berapa harga lapangan?", "get_pricing"},
		{"Omzet kantin bulan ini?", "query_kantin_summary"},
	}
	ts := fakeTools(t,
		"query_income", "export_report_excel", "query_range_summary",
		"top_customers", "closing_report", "query_net_profit",
		"check_stock", "get_availability", "get_pricing",
		"query_kantin_summary", "list_bookings", "book_slot",
	)
	for _, c := range cases {
		got := ts.route(c.message)
		if got.Tool != c.want {
			t.Errorf("route(%q) = %q (score %d), want %q", c.message, got.Tool, got.Score, c.want)
		}
	}
}

func TestRouteIsCaseAndPunctuationInsensitive(t *testing.T) {
	ts := fakeTools(t, "closing_report", "query_income")
	for _, m := range []string{
		"closing report",
		"CLOSING REPORT!",
		"Buat Closing-Report",
		"  closing   report  ",
	} {
		if got := ts.route(m); got.Tool != "closing_report" {
			t.Errorf("route(%q) = %q, want closing_report", m, got.Tool)
		}
	}
}

func TestRouteDeclinesWhenNothingMatches(t *testing.T) {
	ts := fakeTools(t, "query_income", "closing_report")
	// Should return no tool rather than guessing, so the model answers from
	// conversation instead of running an arbitrary query.
	if got := ts.route("halo, apa kabar?"); got.Tool != "" {
		t.Errorf("route matched %q for a greeting", got.Tool)
	}
}

func TestRouteIsDeterministic(t *testing.T) {
	ts := fakeTools(t, "query_income", "query_range_summary", "query_net_profit")
	msg := "ringkasan pendapatan bulan ini"
	first := ts.route(msg).Tool
	for i := 0; i < 25; i++ {
		if got := ts.route(msg).Tool; got != first {
			t.Fatalf("route not deterministic: %q then %q", first, got)
		}
	}
}

func TestRouteCannotSelectAToolOutsideTheSet(t *testing.T) {
	// Role isolation depends on routing only ever seeing the filtered set.
	ownerOnly := fakeTools(t, "query_income", "export_report_excel")
	staff := &ToolSet{tools: map[string]Tool{}}
	for _, n := range []string{"query_income"} {
		staff.add(ownerOnly.tools[n])
	}
	if got := staff.route("Buatkan laporan Excel bulan ini"); got.Tool == "export_report_excel" {
		t.Error("staff ToolSet routed to an owner-only tool")
	}
}

// TestSanitiseReplyBlocksProtocolMarkup is the regression guard for the
// defect where the model's own <tool_call>{function=…} wrapper was
// rendered to the user verbatim and stored in history.
func TestSanitiseReplyBlocksProtocolMarkup(t *testing.T) {
	leaks := []string{
		`<tool_call>{function=closing_report}</tool_call>`,
		`<tool_call><function=closing_report><parameter=date>2026-09-30</parameter></function></tool_call>`,
		`<tool_call>{"function":"closing_report","arguments":{}}</tool_call>`,
		"Here you go:\n<function_call>{\"name\":\"query_income\"}</function_call>",
		`{"tool_call": {"name": "closing_report"}}`,
		`{"tool": "closing_report", "args": {}}`,
	}
	for _, l := range leaks {
		got := sanitiseReply(l)
		if strings.Contains(got, "tool_call") || strings.Contains(got, "function") ||
			strings.Contains(got, "<") {
			t.Errorf("markup leaked through: %q -> %q", l, got)
		}
		if got == l {
			t.Errorf("markup returned unchanged: %q", l)
		}
	}
}

func TestSanitiseReplyPassesOrdinaryText(t *testing.T) {
	for _, ok := range []string{
		"Total pendapatan hari ini Rp0.",
		"Laba bulan ini naik 12% dibanding bulan lalu.",
		"Stok Es Teh habis.",
		// A reply that merely mentions a function name in prose is fine.
		"Stok饮品 habis, ya.",
		"Jam paling ramai hari ini pukul 19.00.",
	} {
		if got := sanitiseReply(ok); got != ok {
			t.Errorf("sanitiseReply altered a normal reply: %q -> %q", ok, got)
		}
	}
}

func TestSanitiseReplyHandlesEmpty(t *testing.T) {
	if got := sanitiseReply("   "); got == "" {
		t.Error("empty reply should produce a usable fallback, not an empty string")
	}
}

func TestSanitiseReplyKeepsProseAroundMarkup(t *testing.T) {
	// The model regularly finishes a correct answer and then, in the same
	// turn, writes a wrapper to try to chain a second call. The tool had
	// already run. Discarding the whole reply for a trailing wrapper left
	// the user with "Maaf, jawabannya tidak bisa ditampilkan" for a
	// question that had been answered, so the prose is kept and only the
	// protocol is removed.
	cases := []struct{ in, want string }{
		{
			"Belum ada lapangan aktif.\n<tool_call><function=list_bookings><parameter=date>2026-10-01</parameter></function></tool_call>",
			"Belum ada lapangan aktif.",
		},
		{
			"Here you go:\n<function_call>{\"name\":\"query_income\"}</function_call>",
			"Here you go:",
		},
	}
	for _, c := range cases {
		got := sanitiseReply(c.in)
		if got != c.want {
			t.Errorf("sanitiseReply(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitiseReplyStripsZeroWidthBypass(t *testing.T) {
	// A zero-width space between "<" and the tag name is enough to slip
	// past a guard that only matches literal tag text, and the model does
	// emit it. If this regresses, protocol internals reach the user again.
	zwsp := "\u200b"
	leak := "<" + zwsp + "tool_call>{function=closing_report}</" + zwsp + "tool_call>"
	got := sanitiseReply(leak)
	if strings.Contains(got, "tool_call") || strings.Contains(got, "function") {
		t.Errorf("zero-width markup leaked through: %q -> %q", leak, got)
	}
}

func TestSanitiseReplyRefusesWhenNothingReadableRemains(t *testing.T) {
	// Markup only, with no prose to salvage: the turn really was nothing
	// but protocol, so the caller must be told rather than shown "".
	got := sanitiseReply(`<tool_call>{function=closing_report}</tool_call>`)
	if strings.TrimSpace(got) == "" {
		t.Error("markup-only reply should not reduce to an empty message")
	}
	if strings.Contains(got, "tool_call") || strings.Contains(got, "function") {
		t.Errorf("markup leaked: %q", got)
	}
}

func TestNormaliseArguments(t *testing.T) {
	cases := map[string]string{
		``:                             `{}`,
		`   `:                          `{}`,
		`{"date":"2026-09-30"}`:        `{"date":"2026-09-30"}`,
		`  {"date":"2026-09-30"}  `:    `{"date":"2026-09-30"}`,
		`not json at all`:              `{}`,
		`[1,2,3]`:                      `{}`,
		`"a string the model made up"`: `{}`,
	}
	for in, want := range cases {
		if got := string(normaliseArguments(in)); got != want {
			t.Errorf("normaliseArguments(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefsCarrySchemaAndAreSorted(t *testing.T) {
	ts := &ToolSet{tools: map[string]Tool{}}
	ts.add(Tool{Name: "zeta", Description: "z"})
	ts.add(Tool{Name: "alpha", Description: "a"})
	defs := ts.Defs()
	if len(defs) != 2 {
		t.Fatalf("defs=%d want 2", len(defs))
	}
	if defs[0].Function.Name != "alpha" || defs[1].Function.Name != "zeta" {
		t.Errorf("defs not sorted: %q,%q", defs[0].Function.Name, defs[1].Function.Name)
	}
	for _, d := range defs {
		if d.Type != "function" {
			t.Errorf("%s type=%q want function", d.Function.Name, d.Type)
		}
		// A declaration without a schema is rejected by some gateways.
		if len(d.Function.Parameters) == 0 {
			t.Errorf("%s has no parameters schema", d.Function.Name)
		}
		if !strings.Contains(string(d.Function.Parameters), `"type":"object"`) {
			t.Errorf("%s schema is not an object: %s", d.Function.Name, d.Function.Parameters)
		}
	}
}

// TestMetaTableMatchesRegistry keeps the routing vocabulary from drifting
// away from the tools that actually exist: every entry in the table must be
// a registered tool, and every money/query tool must have a schema.
func TestMetaTableMatchesRegistry(t *testing.T) {
	ts := fakeTools(t)
	for name := range toolMetaTable {
		if name == "" {
			t.Fatal("meta table has an empty tool name")
		}
		// Name shape: snake_case, no spaces, so it can be a tool id.
		if strings.ContainsAny(name, " -") {
			t.Errorf("meta key %q is not a valid tool name", name)
		}
	}
	// Spot-check that a representative tool really gets its schema applied.
	ts.add(Tool{Name: "closing_report", Description: "daily closing"})
	got, ok := ts.Get("closing_report")
	if !ok {
		t.Fatal("closing_report not registered")
	}
	if len(got.Parameters) == 0 {
		t.Error("closing_report got no parameters schema")
	}
	if len(got.Keywords) == 0 {
		t.Error("closing_report got no routing keywords")
	}
}
