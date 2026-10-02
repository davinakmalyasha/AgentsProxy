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
)

const (
	chatCompletionsPath = "/chat/completions"
)

// Message is one turn of the conversation.
//
// ToolCalls/ToolCallID are what make a real tool round-trip possible.
// Without them the only way to return a result to the model is to fake it
// as a user turn, which teaches the model that tool output is something
// the user said â€” and leaves no id to correlate, so a model that emits two
// calls cannot be answered unambiguously.
type Message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID is set on role:"tool" messages and names the call this
	// message answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
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
}

func New(baseURL, apiKey, model string, maxTokens int) *Client {
	return &Client{
		BaseURL:      strings.TrimRight(baseURL, "/"),
		APIKey:       apiKey,
		DefaultModel: model,
		MaxTokens:    maxTokens,
		HTTP:         &http.Client{Timeout: 60 * time.Second},
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

// Completion is one model turn: either tool calls, or prose, or both.
type Completion struct {
	Content   string
	ToolCalls []ToolCall
	// FinishReason is the provider's own verdict ("stop",
	// "tool_calls", "length"). Useful for telling "the model ignored the
	// tool" apart from "the model ran out of tokens".
	FinishReason  string
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
	if isReasoningModel(model) {
		payload.Thinking = &thinking{Type: "disabled"}
	}

	raw, status, err := c.post(ctx, payload)
	if err != nil {
		return nil, err
	}
	if status != 0 && strings.Contains(raw, "Invalid option") {
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
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
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
		ToolCalls:     choice0.Message.ToolCalls,
		FinishReason:  choice0.FinishReason,
		PromptTokens:  out.Usage.PromptTokens,
		CompletionTok: out.Usage.CompletionTokens,
	}
	if comp.PromptTokens == 0 && comp.CompletionTok == 0 {
		// Fallback estimate when the provider reports no usage.
		comp.CompletionTok = len(comp.Content)/4 + len(choice0.Message.ToolCalls)*16
	}
	return comp, nil
}

func isReasoningModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "mimo") || strings.Contains(m, "deepseek")
}

// post sends one payload and returns the body plus a non-2xx status (0 when
// the request itself failed, in which case err is set).
func (c *Client) post(ctx context.Context, payload chatRequest) (string, int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", 0, fmt.Errorf("marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+chatCompletionsPath, bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %v", ErrProvider, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("%w: read body", ErrProvider)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return string(raw), resp.StatusCode, nil
	}
	return string(raw), 0, nil
}
