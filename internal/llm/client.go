// DONE: PRD 6.1 (OpenAI-compatible LLM client, 60s timeout, sanitize errors) / STANDARDS A, D
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/davinakmalyasha/agents-proxy/internal/httpx"
)

const (
	chatCompletionsPath = "/chat/completions"
)

// Message is one turn of the conversation.
//
// ToolCalls carries the provider's function-call request on responses.
// This client never performs a tool round-trip itself (the ADK loop
// does), so nothing here sets them on requests.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls carries the provider's function-call request on the
	// response path. The request path never sets it.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall is one function invocation requested by the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON text, not an object
}

type Client struct {
	BaseURL      string
	APIKey       string
	DefaultModel string
	MaxTokens    int
	HTTP         *http.Client
	// chatURL is BaseURL + chatCompletionsPath, precomputed: the hot path
	// concatenated it per request.
	chatURL string
}

func New(baseURL, apiKey, model string, maxTokens int) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	return &Client{
		BaseURL:      baseURL,
		APIKey:       apiKey,
		DefaultModel: model,
		MaxTokens:    maxTokens,
		HTTP:         httpx.NewClient(60 * time.Second),
		chatURL:      baseURL + chatCompletionsPath,
	}
}

type chatRequest struct {
	Model           string    `json:"model"`
	Messages        []Message `json:"messages"`
	MaxTokens       int       `json:"max_tokens"`
	ReasoningEffort *string   `json:"reasoning_effort,omitempty"`
	Thinking        *thinking `json:"thinking,omitempty"`
}

type thinking struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// Completion is one model turn: prose plus token counts. Tool calls
// ride on Message.ToolCalls (read by the caller from the raw choice);
// this struct carries only what the quota/metering path consumes.
type Completion struct {
	Content       string
	PromptTokens  int
	CompletionTok int
}

// ErrProvider is returned for any non-2xx provider response. The
// underlying error string is sanitized; do not echo it to clients.
var ErrProvider = errors.New("provider error")

// Chat posts messages to the provider's OpenAI-compatible endpoint and
// returns the assistant reply and token counts. Never logs prompt or
// response bodies.
func (c *Client) Chat(ctx context.Context, messages []Message, modelOverride string) (string, int, int, error) {
	comp, err := c.Complete(ctx, messages, modelOverride)
	if err != nil {
		return "", 0, 0, err
	}
	return comp.Content, comp.PromptTokens, comp.CompletionTok, nil
}

// Complete performs one model turn and returns whatever it produced: prose,
// tool calls, or both.
func (c *Client) Complete(
	ctx context.Context,
	messages []Message,
	modelOverride string,
) (*Completion, error) {
	model := c.DefaultModel
	if modelOverride != "" {
		model = modelOverride
	}
	payload := chatRequest{
		Model:     model,
		Messages:  messages,
		MaxTokens: c.MaxTokens,
	}
	// Non-thinking for efficiency: the client landing and the agent both
	// only need retrieve-then-answer.
	//
	// This used to send "none" for MiMo/DeepSeek and retry with "low" when
	// the provider answered 400 "Invalid option" — so on the configured
	// gateway EVERY call cost two round-trips, and the retry was the only
	// reason the agent worked at all. That gateway accepts only
	// low|medium|high|xhigh|max, so "low" goes first now and the retry is
	// kept purely as a fallback for a provider that wants the other shape.
	reasonLow := "low"
	payload.ReasoningEffort = &reasonLow
	if IsReasoningModel(model) {
		payload.Thinking = &thinking{Type: "disabled"}
	}

	raw, status, err := c.post(ctx, payload)
	if err != nil {
		return nil, err
	}
	// Scan only small error bodies: the "Invalid option" shape is a ~100
	// byte provider 400. A megabytes-large body is never it, and
	// scanning the whole thing per error is pure CPU.
	if status != 0 && len(raw) < 512 && bytes.Contains(raw, []byte("Invalid option")) {
		// Provider dislikes "low"; try the explicit-thinking shape instead.
		reasonNone := "none"
		payload.ReasoningEffort = &reasonNone
		raw, status, err = c.post(ctx, payload)
		if err != nil {
			return nil, err
		}
	}
	if status != 0 {
		return nil, fmt.Errorf("%w: status %d", ErrProvider, status)
	}

	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: decode", ErrProvider)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("%w: %s", ErrProvider, out.Error.Type)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("%w: no choices", ErrProvider)
	}
	choice0 := out.Choices[0]
	comp := &Completion{
		Content:       choice0.Message.Content,
		PromptTokens:  out.Usage.PromptTokens,
		CompletionTok: out.Usage.CompletionTokens,
	}
	if comp.PromptTokens == 0 && comp.CompletionTok == 0 {
		// Fallback estimate when the provider reports no usage.
		comp.CompletionTok = len(comp.Content)/4 + len(choice0.Message.ToolCalls)*16
	}
	return comp, nil
}

// IsReasoningModel reports whether the model thinks unless told not to,
// in which case thinking tokens eat the max_tokens budget tool calls
// need. Shared with adkrunner, which sends the same shaping through
// ADK's openaimodel: one matcher, one place to update when providers
// change their thinking protocol.
func IsReasoningModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "mimo") || strings.Contains(m, "deepseek")
}

// post sends one payload and returns the body plus a non-2xx status (0 when
// the request itself failed, in which case err is set). The body is
// returned as bytes: callers unmarshal from it directly, so one .xlsx of
// a completion is never copied through string conversions.
func (c *Client) post(ctx context.Context, payload chatRequest) ([]byte, int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatURL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrProvider, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: read body", ErrProvider)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return raw, resp.StatusCode, nil
	}
	return raw, 0, nil
}
