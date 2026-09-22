// DONE: developer-only helper (Phase 1 verify), used by scripts/verify-phase2.ps1
// mockllm is a local stand-in for the DeepSeek /chat/completions
// endpoint used during Phase 1 verification. It returns a canned
// response with realistic token counts so the full chat flow
// (auth → quota → LLM → usage → response) can be exercised without
// burning real tokens. NOT used in production.
package main

import (
	"encoding/json"
	"log"
	"net/http"
)

type chatReq struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

func main() {
	addr := ":18080"
	log.Printf("mockllm listening on %s", addr)
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", 405)
			return
		}
		var req chatReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		// Trivial reply that quotes the user's first message.
		content := "echo: ok"
		if len(req.Messages) > 0 {
			content = "echo: " + req.Messages[len(req.Messages)-1].Content
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "mockllm-1",
			"object":  "chat.completion",
			"created": 1700000000,
			"model":   req.Model,
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]string{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{
				"prompt_tokens":     12,
				"completion_tokens": 18,
				"total_tokens":      30,
			},
		})
	})
	log.Fatal(http.ListenAndServe(addr, mux))
}
