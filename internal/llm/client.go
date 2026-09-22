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

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
		Message Message `json:"message"`
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

// ErrProvider is returned for any non-2xx provider response. The
// underlying error string is sanitized; do not echo it to clients.
var ErrProvider = errors.New("provider error")

// Chat posts messages to the provider's OpenAI-compatible endpoint and
// returns the assistant reply and token counts. Never logs prompt or
// response bodies.
func (c *Client) Chat(ctx context.Context, messages []Message, modelOverride string) (string, int, int, error) {
	model := c.DefaultModel
	if modelOverride != "" {
		model = modelOverride
	}
	// Non-thinking for efficiency: client/landing only needs retrieve-and-answer.
	// Mimo v2.5 and DeepSeek Flash both benefit from low/no reasoning.
	// Command Code currently rejects "none" for DeepSeek (see #697), so try "none" then fallback to "low".
	// We send both shapes the provider might honor; server picks one.
	reasonLow := "low"
	reasonNone := "none"
	payload := chatRequest{
		Model:           model,
		Messages:        messages,
		MaxTokens:       c.MaxTokens,
		ReasoningEffort: &reasonLow,
	}
	// For models that support explicit thinking disable, also send it
	if strings.Contains(strings.ToLower(model), "mimo") || strings.Contains(strings.ToLower(model), "deepseek") {
		payload.Thinking = &thinking{Type: "disabled"}
		// Prefer none over low when provider adds support
		payload.ReasoningEffort = &reasonNone
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", 0, 0, fmt.Errorf("marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+chatCompletionsPath, bytes.NewReader(body))
	if err != nil {
		return "", 0, 0, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return "", 0, 0, fmt.Errorf("%w: %v", ErrProvider, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, 0, fmt.Errorf("%w: read body", ErrProvider)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// If we tried "none" and provider says invalid, retry with "low" (see #697)
		if strings.Contains(string(raw), "Invalid option") && payload.ReasoningEffort != nil && *payload.ReasoningEffort == "none" {
			low := "low"
			payload.ReasoningEffort = &low
			// keep Thinking disabled as well
			body2, _ := json.Marshal(payload)
			req2, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+chatCompletionsPath, bytes.NewReader(body2))
			req2.Header.Set("Content-Type", "application/json")
			req2.Header.Set("Authorization", "Bearer "+c.APIKey)
			if resp2, err2 := c.HTTP.Do(req2); err2 == nil {
				defer resp2.Body.Close()
				raw2, _ := io.ReadAll(resp2.Body)
				if resp2.StatusCode >= 200 && resp2.StatusCode < 300 {
					raw = raw2
					resp = resp2
				} else {
					// still error, fall through to generic
					return "", 0, 0, fmt.Errorf("%w: status %d", ErrProvider, resp2.StatusCode)
				}
			} else {
				return "", 0, 0, fmt.Errorf("%w: status %d", ErrProvider, resp.StatusCode)
			}
		} else {
			return "", 0, 0, fmt.Errorf("%w: status %d", ErrProvider, resp.StatusCode)
		}
	}
	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", 0, 0, fmt.Errorf("%w: decode", ErrProvider)
	}
	if out.Error != nil {
		return "", 0, 0, fmt.Errorf("%w: %s", ErrProvider, out.Error.Type)
	}
	if len(out.Choices) == 0 {
		return "", 0, 0, fmt.Errorf("%w: no choices", ErrProvider)
	}
	reply := out.Choices[0].Message.Content
	inTok := out.Usage.PromptTokens
	outTok := out.Usage.CompletionTokens
	if inTok == 0 && outTok == 0 {
		// Fallback estimate.
		outTok = len(reply) / 4
	}
	return reply, inTok, outTok, nil
}
