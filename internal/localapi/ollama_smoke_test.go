package localapi

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/edukod-cz/edukod-companion/internal/protocol"
)

// TestRealOllamaOpenAICompatibility is an opt-in release/canary smoke test.
// It deliberately exercises both OpenAI-compatible surfaces through the same
// loopback-only client used by the daemon. Normal unit and CI runs skip it so a
// model download is never triggered implicitly.
func TestRealOllamaOpenAICompatibility(t *testing.T) {
	model := os.Getenv("EDUKOD_OLLAMA_SMOKE_MODEL")
	if model == "" {
		t.Skip("set EDUKOD_OLLAMA_SMOKE_MODEL to a locally installed Ollama model")
	}
	baseURL := os.Getenv("EDUKOD_OLLAMA_SMOKE_URL")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	client, err := New(baseURL, 16<<20)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	models, err := client.Models(ctx)
	if err != nil {
		t.Fatalf("list Ollama models: %v", err)
	}
	var modelList struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(models, &modelList); err != nil || len(modelList.Data) == 0 {
		t.Fatalf("Ollama /v1/models returned no model data: %s", models)
	}

	responsesBody, err := json.Marshal(map[string]any{
		"model":  model,
		"input":  "Reply with exactly the word OK.",
		"stream": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertOllamaSurface(t, ctx, client, "ollama-responses-smoke", "/v1/responses", responsesBody, "output")

	chatBody, err := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "Reply with exactly the word OK."}},
		"stream":   false,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertOllamaSurface(t, ctx, client, "ollama-chat-smoke", "/v1/chat/completions", chatBody, "choices")
}

func assertOllamaSurface(
	t *testing.T,
	ctx context.Context,
	client *Client,
	requestID string,
	path string,
	body json.RawMessage,
	resultField string,
) {
	t.Helper()
	status, _, responseBody, err := client.Forward(ctx, protocol.Request{
		Type: "request", ID: requestID, Method: "POST", Path: path, Body: body,
	})
	if err != nil {
		t.Fatalf("%s request failed: %v", path, err)
	}
	if status < 200 || status >= 300 {
		t.Fatalf("%s returned HTTP %d: %s", path, status, responseBody)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		t.Fatalf("decode %s response: %v", path, err)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(envelope[resultField], &items); err != nil || len(items) == 0 {
		t.Fatalf("%s response has no %s entries: %s", path, resultField, responseBody)
	}
}
