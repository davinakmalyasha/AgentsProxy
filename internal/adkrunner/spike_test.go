// Package adkrunner spike: the ADK Go migration gate, kept next to the
// engine it proved.
//
// These tests need the live gateway (LLM_API_KEY / LLM_BASE_URL) because
// the question they answer -- do our custom reasoning fields survive
// ADK's request builder, and does the model actually call tools through
// the runner -- cannot be answered against a mock. They were the
// precondition for the whole cutover; they stay as the tripwire for
// future ADK or gateway upgrades.
package adkrunner

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model/openaimodel"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/openai/openai-go/v3/option"
)

// captureTripper records every request body that passes through it, so the
// spike proves what actually went over the wire rather than asserting on the
// config that asked for it.
type captureTripper struct {
	next   http.RoundTripper
	mu     sync.Mutex
	bodies []string
}

func (c *captureTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		c.mu.Lock()
		c.bodies = append(c.bodies, string(b))
		c.mu.Unlock()
	}
	return c.next.RoundTrip(r)
}

func (c *captureTripper) bodiesCopy() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies...)
}

type spikeBookingsArgs struct {
	Date string `json:"date,omitempty"`
}

type spikeBookingsResult struct {
	Items []string `json:"items"`
}

type spikeIncomeArgs struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

type spikeIncomeResult struct {
	Total float64 `json:"total"`
}

// spikeEnv carries the live gateway coordinates. The spike needs the real
// endpoint because the question it answers -- do our custom fields survive
// ADK's request builder -- cannot be answered against a mock.
func spikeEnv(t *testing.T) (key, base, modelName string) {
	t.Helper()
	key = os.Getenv("LLM_API_KEY")
	base = os.Getenv("LLM_BASE_URL")
	if key == "" || base == "" {
		t.Skip("LLM_API_KEY / LLM_BASE_URL not set; the spike needs the live gateway")
	}
	modelName = os.Getenv("LLM_MODEL")
	if modelName == "" {
		modelName = "xiaomi/mimo-v2.5"
	}
	return key, base, modelName
}

// buildSpikeAgent wires the smallest possible ADK agent against the live
// gateway: two stub tools with canned data, a chat-completions model, and an
// in-memory runner. It returns the runner plus the captured wire bodies.
func buildSpikeAgent(t *testing.T, called *sync.Map) (*runner.Runner, *captureTripper) {
	t.Helper()
	key, base, modelName := spikeEnv(t)

	trip := &captureTripper{next: http.DefaultTransport}
	httpCli := &http.Client{Transport: trip, Timeout: 90 * time.Second}

	model, err := openaimodel.NewModel(context.Background(), modelName, &openaimodel.ClientConfig{
		APIKey:     key,
		BaseURL:    base,
		API:        openaimodel.APIChatCompletions,
		HTTPClient: httpCli,
		// The gateway's two custom fields. openai-go has no first-class
		// spelling for either, so they go in as raw body fields. If a future
		// ADK or openai-go upgrade stops honouring Options, the wire test
		// below fails here -- loudly, at the request -- instead of as
		// truncated tool calls in production.
		Options: []option.RequestOption{
			option.WithJSONSet("reasoning_effort", "low"),
			option.WithJSONSet("thinking", map[string]string{"type": "disabled"}),
		},
	})
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}

	mkTool := func(name, desc string, items []string) tool.Tool {
		tl, err := functiontool.New(functiontool.Config{Name: name, Description: desc},
			func(ctx agent.Context, args spikeBookingsArgs) (spikeBookingsResult, error) {
				called.Store(name, true)
				return spikeBookingsResult{Items: items}, nil
			})
		if err != nil {
			t.Fatalf("%s tool: %v", name, err)
		}
		return tl
	}

	ag, err := llmagent.New(llmagent.Config{
		Name:        "spike",
		Description: "spike agent for the ADK migration gate",
		Model:       model,
		Instruction: "Use tools for any real number. Never invent data.",
		Tools: []tool.Tool{
			mkTool("list_bookings", "List bookings for a day, default today",
				[]string{"Pak Budi 19:00 Lapangan Satu"}),
			mkTool("query_income", "Income received for a period",
				[]string{"total 150000"}),
		},
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	r, err := runner.NewInMemory("spike", ag)
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	return r, trip
}

// TestSpike_GatewayCarriesReasoningFields is the gate for the whole ADK
// migration.
//
// The proxy's gateway needs two non-OpenAI fields on every request:
// reasoning_effort and thinking. If either is dropped, thinking turns back
// on, reasoning tokens eat the max_tokens budget, and tool calls get
// truncated -- which surfaces as the exact "could not fetch data" failure
// this migration is meant to fix. This test proves both fields reach the
// wire through ADK's openaimodel, using a captured HTTP body.
func TestSpike_GatewayCarriesReasoningFields(t *testing.T) {
	var called sync.Map
	r, trip := buildSpikeAgent(t, &called)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	msg := &genai.Content{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromText("hari ini ada pesanan?")},
	}
	for ev, err := range r.Run(ctx, "spike-user", "spike-session", msg, agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev == nil {
			continue
		}
	}

	foundReasoning, foundThinking := false, false
	for _, b := range trip.bodiesCopy() {
		if strings.Contains(b, `"reasoning_effort"`) {
			foundReasoning = true
		}
		if strings.Contains(b, `"thinking"`) {
			foundThinking = true
		}
	}
	if !foundReasoning {
		t.Error("reasoning_effort never reached the wire; thinking will turn on and eat the token budget")
	}
	if !foundThinking {
		t.Error("thinking never reached the wire; the gateway will reason and truncate tool calls")
	}
}

// TestSpike_AgentCallsATool is the behavioural half: through ADK's runner,
// the reported question gets a tool call rather than prose.
func TestSpike_AgentCallsATool(t *testing.T) {
	var called sync.Map
	r, _ := buildSpikeAgent(t, &called)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	msg := &genai.Content{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{genai.NewPartFromText("hari ini ada pesanan?")},
	}
	var sawFinal bool
	for ev, err := range r.Run(ctx, "spike-user", "spike-session", msg, agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev == nil {
			continue
		}
		sawFinal = sawFinal || ev.IsFinalResponse()
	}
	if !sawFinal {
		t.Error("no final response event from the runner")
	}
	anyCalled := false
	called.Range(func(_, _ any) bool { anyCalled = true; return false })
	if !anyCalled {
		t.Error("no tool ran for a question that needs data; the agent answered from memory or refused")
	}
}
