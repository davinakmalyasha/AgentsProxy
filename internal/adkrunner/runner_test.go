package adkrunner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"
	"google.golang.org/adk/v2/session"

	"github.com/jackc/pgx/v5/pgxpool"

	agentstools "github.com/davinakmalyasha/agents-proxy/AgentsTools"
	"github.com/davinakmalyasha/agents-proxy/internal/agentconfig"
	"github.com/davinakmalyasha/agents-proxy/internal/tenanthealth"
)

// stubTripper replays canned chat-completions bodies in order (repeating
// the last), standing in for the gateway. No network, no key.
type stubTripper struct {
	mu     sync.Mutex
	bodies []string
	calls  int
}

func (s *stubTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	if i >= len(s.bodies) {
		i = len(s.bodies) - 1
	}
	s.calls++
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(s.bodies[i])),
	}, nil
}

func (s *stubTripper) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func toolCallBody(tool, args string) string {
	return `{"id":"chatcmpl-test","object":"chat.completion","created":1700000000,"model":"test-model",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"id":"call_1","type":"function","function":{"name":` + jsonString(tool) +
		`,"arguments":` + jsonString(args) + `}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
}

func finalBody(text string) string {
	return `{"id":"chatcmpl-test","object":"chat.completion","created":1700000000,"model":"test-model",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":` + jsonString(text) +
		`},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// testCtxKey is a test-local context key proving caller values reach the
// tool handler. It replaces the removed Telegram-era recipient plumbing;
// the property under test is propagation, not delivery.
type testCtxKey struct{}

// TestCtxReachesTool is the proof the whole cutover rests on: values the
// caller puts in ctx must be visible inside the tool handler, the tenant
// must arrive from the closure (never the model), and the tool's result
// must flow into the final reply.
func TestCtxReachesTool(t *testing.T) {
	trip := &stubTripper{bodies: []string{
		toolCallBody("probe_tool", `{"q":"lapangan satu"}`),
		finalBody("Ada, lapangan satu kosong jam 19."),
	}}
	var sawTenant, sawRecipient, sawArgs string
	var runs int
	probe := agentstools.Tool{
		Name:        "probe_tool",
		Description: "probe",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			runs++
			sawTenant = tenantID
			sawRecipient, _ = ctx.Value(testCtxKey{}).(string)
			sawArgs = string(args)
			return "PROBE-OK"
		},
	}
	r := testRunner(t, trip, probe)

	ctx := context.WithValue(context.Background(), testCtxKey{}, "123")
	reply, err := r.RunWithToolsRole(ctx, "t1", "sports", "user1", "owner", "owner", "halo")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if runs != 1 {
		t.Fatalf("probe ran %d times, want 1", runs)
	}
	if sawTenant != "t1" {
		t.Errorf("tenant = %q, want t1 (must come from the closure, not the model)", sawTenant)
	}
	if sawRecipient != "123" {
		t.Errorf("ctx value = %q, want 123 (ctx did not reach the tool)", sawRecipient)
	}
	if !strings.Contains(sawArgs, "lapangan satu") {
		t.Errorf("args = %s, want the model's q", sawArgs)
	}
	if reply != "Ada, lapangan satu kosong jam 19." {
		t.Errorf("reply = %q", reply)
	}
}

// TestBracesInPromptSurvive proves tenant content with {{PLACEHOLDERS}}
// reaches the model instead of tripping ADK's {key} instruction
// templating ("state key does not exist"). Regression test for the first
// live 502 on the cutover path.
func TestBracesInPromptSurvive(t *testing.T) {
	trip := &stubTripper{bodies: []string{
		finalBody("Halo!"),
	}}
	r := testRunner(t, trip)
	r.UpdateProject(&agentconfig.Project{
		Slug:         "t1",
		Display:      "T Satu",
		Vertical:     "sports",
		SystemPrompt: "Today's date is {{DATE}} (WIB). {{TOOLS}}",
	})

	reply, err := r.RunWithToolsRole(context.Background(), "t1", "sports", "user1", "owner", "owner", "halo")
	if err != nil {
		t.Fatalf("run: %v (braces in tenant content must not break the run)", err)
	}
	if reply != "Halo!" {
		t.Errorf("reply = %q", reply)
	}
}

// TestMaxRoundsAbort bounds a model that chains tool calls forever. The
// stub never stops emitting calls; the guard must stop the run and the
// user must get the processing-limit message, not an error.
func TestMaxRoundsAbort(t *testing.T) {
	trip := &stubTripper{bodies: []string{
		toolCallBody("probe_tool", `{"q":"x"}`),
	}}
	var runs int
	probe := agentstools.Tool{
		Name:        "probe_tool",
		Description: "probe",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			runs++
			return "more"
		},
	}
	r := testRunner(t, trip, probe)

	reply, err := r.RunWithToolsRole(context.Background(), "t1", "sports", "user1", "owner", "owner", "halo")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if reply != maxRoundsMessage {
		t.Errorf("reply = %q, want the processing-limit message", reply)
	}
	if runs != maxModelCalls {
		t.Errorf("probe ran %d times, want exactly the %d-call budget", runs, maxModelCalls)
	}
}

// TestAdapterRejectsBadArgs proves validation runs before execution: the
// model omits the required field, the tool body must NOT run, and the run
// still completes with the model's own final text.
func TestAdapterRejectsBadArgs(t *testing.T) {
	trip := &stubTripper{bodies: []string{
		toolCallBody("probe_tool", `{}`),
		finalBody("Maaf, kurang jelas."),
	}}
	ran := false
	probe := agentstools.Tool{
		Name:        "probe_tool",
		Description: "probe",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
		Run: func(ctx context.Context, tenantID string, args json.RawMessage) string {
			ran = true
			return "MUST-NOT-RUN"
		},
	}
	r := testRunner(t, trip, probe)

	reply, err := r.RunWithToolsRole(context.Background(), "t1", "sports", "user1", "owner", "owner", "halo")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if ran {
		t.Error("tool body ran with invalid args; validation must run first")
	}
	if reply != "Maaf, kurang jelas." {
		t.Errorf("reply = %q", reply)
	}
}
// TestQuarantinedTenantNeverReachesModel: a fenced tenant fails closed
// with the sentinel error before any model spend (trip.calls stays 0).
// A healthy tenant proceeds to the model normally: the fence is scoped.
func TestQuarantinedTenantNeverReachesModel(t *testing.T) {
	trip := &stubTripper{bodies: []string{finalBody("Halo!")}}
	r := testRunner(t, trip)
	tenanthealth.Quarantine("t1", "test fence")
	defer tenanthealth.Release("t1")
	_, err := r.RunWithToolsRole(context.Background(), "t1", "sports", "u", "owner", "owner", "halo")
	if !errors.Is(err, tenanthealth.ErrQuarantined) {
		t.Fatalf("err = %v, want ErrQuarantined", err)
	}
	if trip.calls != 0 {
		t.Errorf("fenced tenant caused %d model calls, want 0", trip.calls)
	}
	_, err2 := r.RunWithToolsRole(context.Background(), "t2", "sports", "u", "owner", "owner", "halo")
	if errors.Is(err2, tenanthealth.ErrQuarantined) {
		t.Fatal("healthy tenant fenced")
	}
	if trip.calls == 0 {
		t.Error("healthy tenant made no model call")
	}
}

func testRunner(t *testing.T, trip *stubTripper, tools ...agentstools.Tool) *Runner {
	t.Helper()
	m, err := openaimodel.NewModel(context.Background(), "test-model", &openaimodel.ClientConfig{
		APIKey:     "sk-test",
		BaseURL:    "http://stub.local/v1",
		API:        openaimodel.APIChatCompletions,
		HTTPClient: &http.Client{Transport: trip},
	})
	if err != nil {
		t.Fatalf("test model: %v", err)
	}
	var _ model.LLM = m
	return &Runner{
		cfg:   Config{Sessions: session.InMemoryService(), MaxTokens: 512},
		model: m,
		newToolSet: func(_ *pgxpool.Pool, _, _ string) *agentstools.ToolSet {
			return agentstools.NewToolSetFrom(nil, tools...)
		},
	}
}
