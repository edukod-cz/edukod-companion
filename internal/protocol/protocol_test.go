package protocol

import (
	"encoding/json"
	"testing"
	"time"
)

func TestValidateRequestAllowsOnlyOpenAICompatibilityPaths(t *testing.T) {
	now := time.Now()
	valid := []Request{
		{Type: "request", ID: "models-1", Method: "GET", Path: "/v1/models"},
		{Type: "request", ID: "response-1", Method: "POST", Path: "/v1/responses", Body: json.RawMessage(`{"model":"qwen3"}`)},
		{Type: "request", ID: "chat-1", Method: "POST", Path: "/v1/chat/completions", Body: json.RawMessage(`{"model":"qwen3"}`)},
	}
	for _, request := range valid {
		if err := ValidateRequest(request, now, 1024); err != nil {
			t.Errorf("valid request %#v rejected: %v", request, err)
		}
	}
	invalid := []Request{
		{Type: "request", ID: "tags-1", Method: "GET", Path: "/api/tags"},
		{Type: "request", ID: "path-1", Method: "POST", Path: "/v1/responses/extra", Body: json.RawMessage(`{}`)},
		{Type: "request", ID: "query-1", Method: "GET", Path: "/v1/models?secret=x"},
		{Type: "request", ID: "method-1", Method: "DELETE", Path: "/v1/models"},
		{Type: "request", ID: "body-1", Method: "POST", Path: "/v1/responses", Body: json.RawMessage(`not-json`)},
		{Type: "request", ID: "stream-1", Method: "POST", Path: "/v1/responses", Body: json.RawMessage(`{"stream":true}`)},
		{Type: "request", ID: "expired-1", Method: "GET", Path: "/v1/models", DeadlineUnixMS: now.Add(-time.Second).UnixMilli()},
	}
	for _, request := range invalid {
		if err := ValidateRequest(request, now, 1024); err == nil {
			t.Errorf("invalid request %#v was accepted", request)
		}
	}
}

func TestValidateRequestEnforcesBodyLimit(t *testing.T) {
	request := Request{
		Type:   "request",
		ID:     "large-1",
		Method: "POST",
		Path:   "/v1/responses",
		Body:   json.RawMessage(`{"payload":"too large"}`),
	}
	if err := ValidateRequest(request, time.Now(), 4); err == nil {
		t.Fatal("oversized request was accepted")
	}
}
