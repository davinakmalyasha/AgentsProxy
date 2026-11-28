// DONE: PRD 6.1 (OpenAI-compatible LLM client, 60s timeout, sanitize errors) / STANDARDS A, D
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

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
	// bearer is "Bearer "+APIKey, precomputed: the hot path built it per
	// request with a string alloc. The client is created once at boot and
	// the key never rotates, so caching is safe.
	bearer string
	// SessionID is sent as x-opencode-session. OpenCode Go rejects
	// requests without it; other OpenAI-compatible providers ignore the
	// unknown header. Stable for the life of this client.
	SessionID string
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
		bearer:       "Bearer " + apiKey,
		SessionID:    uuid.NewString(),
	}
}

type chatRequest struct {
	Model           string         `json:"model"`
	Messages        []Message      `json:"messages"`
	MaxTokens       int            `json:"max_tokens"`
	ReasoningEffort *string        `json:"reasoning_effort,omitempty"`
	Thinking        *thinking      `json:"thinking,omitempty"`
	Stream          bool           `json:"stream,omitempty"`
	StreamOptions   *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
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

// streamChunk is the minimal OpenAI-compatible SSE chunk parsed here:
// a content delta per choice, and optionally usage on the final chunk
// (sent when stream_options.include_usage is true).
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage,omitempty"`
}

// maxStreamLine bounds one SSE line. Deltas are words, not documents;
// 1MB is far above any real chunk and stops a malicious provider from
// growing the scanner buffer without bound.
const maxStreamLine = 1 << 20

// Stream posts like Complete but with stream:true and calls onDelta for
// each content delta as it arrives, returning the accumulated text plus
// token counts. It mirrors Complete's shaping (low-effort first try,
// thinking flag) and its "Invalid option" retry for providers that want
// the other shape. An onDelta error aborts the read: the handler treats
// a dead client as done and still meters what accumulated, so a
// disconnect mid-stream never loses metering. A truncated provider
// stream returns the partial text with fallback tokens and a wrapped
// ErrProvider so the handler meters the partial and reports the error.
func (c *Client) Stream(ctx context.Context, messages []Message, modelOverride string, onDelta func(string) error) (string, int, int, error) {
	build := func(reason string) chatRequest {
		model := c.DefaultModel
		if modelOverride != "" {
			model = modelOverride
		}
		payload := chatRequest{
			Model:         model,
			Messages:      messages,
			MaxTokens:     c.MaxTokens,
			Stream:        true,
			StreamOptions: &streamOptions{IncludeUsage: true},
		}
		payload.ReasoningEffort = &reason
		if IsReasoningModel(model) {
			payload.Thinking = &thinking{Type: "disabled"}
		}
		return payload
	}
	reasonLow := "low"
	full, inTok, outTok, retryable, err := c.streamOnce(ctx, build(reasonLow), onDelta)
	if err != nil {
		return "", 0, 0, err
	}
	if retryable {
		// Same retry as Complete: no deltas were delivered (the error
		// body is not SSE), so the second attempt cannot double-send.
		reasonNone := "none"
		full, inTok, outTok, _, err = c.streamOnce(ctx, build(reasonNone), onDelta)
		if err != nil {
			return "", 0, 0, err
		}
	}
	return full, inTok, outTok, nil
}

func (c *Client) streamOnce(ctx context.Context, payload chatRequest, onDelta func(string) error) (full string, inTok, outTok int, retryable bool, err error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", 0, 0, false, fmt.Errorf("marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, 0, false, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", c.bearer)
	if c.SessionID != "" {
		httpReq.Header.Set("x-opencode-session", c.SessionID)
	}

	// Same 60s total budget as post(): streaming improves time-to-first-
	// token, not the deadline. A stall past the budget ends the stream.
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return "", 0, 0, false, fmt.Errorf("%w: %v", ErrProvider, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Error bodies are small JSON, never SSE: bound the read like
		// Complete does before scanning for the retry shape.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if len(raw) < 512 && bytes.Contains(raw, []byte("Invalid option")) {
			return "", 0, 0, true, nil
		}
		return "", 0, 0, false, fmt.Errorf("%w: status %d", ErrProvider, resp.StatusCode)
	}

	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), maxStreamLine)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // ": keep-alive", "event:", blank lines
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var ch streamChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return sb.String(), inTok, outTok, false, fmt.Errorf("%w: decode chunk", ErrProvider)
		}
		if ch.Usage != nil {
			inTok = ch.Usage.PromptTokens
			outTok = ch.Usage.CompletionTokens
		}
		for _, choice := range ch.Choices {
			if choice.Delta.Content == "" {
				continue
			}
			sb.WriteString(choice.Delta.Content)
			if onDelta != nil {
				if derr := onDelta(choice.Delta.Content); derr != nil {
					full := sb.String()
					if outTok == 0 {
						outTok = len(full) / 4
					}
					return full, inTok, outTok, false, nil
				}
			}
		}
	}
	if serr := sc.Err(); serr != nil {
		full := sb.String()
		if outTok == 0 {
			outTok = len(full) / 4
		}
		if errors.Is(serr, context.Canceled) || errors.Is(serr, context.DeadlineExceeded) {
			// Client or deadline went away mid-stream: keep what
			// accumulated so metering still records it.
			return full, inTok, outTok, false, nil
		}
		return full, inTok, outTok, false, fmt.Errorf("%w: truncated stream", ErrProvider)
	}
	full = sb.String()
	if inTok == 0 && outTok == 0 {
		// Provider sent no usage chunk: same character fallback as
		// Complete (chat carries no tool calls, so no per-call term).
		outTok = len(full) / 4
	}
	return full, inTok, outTok, false, nil
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
	httpReq.Header.Set("Authorization", c.bearer)
	if c.SessionID != "" {
		httpReq.Header.Set("x-opencode-session", c.SessionID)
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrProvider, err)
	}
	defer resp.Body.Close()

	// Bound the read: completions are kilobytes, and the bounded error
	// scan below only needs the head. An unbounded provider body must
	// not become an unbounded alloc on our side.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: read body", ErrProvider)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return raw, resp.StatusCode, nil
	}
	return raw, 0, nil
}
