package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edukod-cz/edukod-companion/internal/protocol"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestValidateBaseURLRequiresLoopbackV1(t *testing.T) {
	for _, candidate := range []string{
		"http://127.0.0.1:11434/v1",
		"http://[::1]:11434/v1",
		"http://localhost:11434/v1",
	} {
		if _, err := ValidateBaseURL(candidate); err != nil {
			t.Errorf("valid URL %q rejected: %v", candidate, err)
		}
	}
	for _, candidate := range []string{
		"http://192.168.1.20:11434/v1",
		"https://example.com/v1",
		"http://127.0.0.1:11434/api",
		"http://user:secret@127.0.0.1:11434/v1",
		"file:///tmp/ollama.sock",
	} {
		if _, err := ValidateBaseURL(candidate); err == nil {
			t.Errorf("unsafe URL %q was accepted", candidate)
		}
	}
}

func TestForwardUsesFixedPathAndNoCredentialHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" || request.Method != http.MethodPost {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "" {
			t.Fatal("companion leaked an authorization header to the local endpoint")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"response-1"}`))
	}))
	defer server.Close()
	client, err := New(server.URL+"/v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	status, contentType, body, err := client.Forward(context.Background(), protocol.Request{
		Type:   "request",
		ID:     "request-1",
		Method: "POST",
		Path:   "/v1/responses",
		Body:   json.RawMessage(`{"model":"qwen3"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != 200 || !strings.HasPrefix(contentType, "application/json") || string(body) != `{"id":"response-1"}` {
		t.Fatalf("response = %d %q %s", status, contentType, body)
	}
}

func TestForwardRejectsRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", "http://127.0.0.1:9/v1/models")
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, err := New(server.URL+"/v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = client.Forward(context.Background(), protocol.Request{Type: "request", ID: "models-redirect", Method: "GET", Path: "/v1/models"})
	if err == nil {
		t.Fatal("redirect was followed")
	}
}

func TestForwardReportsOfflineLoopbackEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	client, err := New("http://127.0.0.1:"+strconv.Itoa(port)+"/v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, _, err = client.Forward(ctx, protocol.Request{Type: "request", ID: "offline-models", Method: "GET", Path: "/v1/models"})
	if err == nil {
		t.Fatal("offline loopback endpoint unexpectedly accepted a request")
	}
	var phaseError *TransportError
	if !errors.As(err, &phaseError) || phaseError.WasDispatched() {
		t.Fatalf("offline endpoint error must be definite pre-dispatch, got %T: %v", err, err)
	}
}

func TestForwardFailedRequestWriteIsAmbiguous(t *testing.T) {
	client, err := New("http://127.0.0.1:11434/v1", 4096)
	if err != nil {
		t.Fatal(err)
	}
	writeErr := errors.New("request write failed")
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		trace := httptrace.ContextClientTrace(request.Context())
		if trace == nil || trace.WroteRequest == nil {
			t.Fatal("request write trace was not installed")
		}
		trace.WroteRequest(httptrace.WroteRequestInfo{Err: writeErr})
		return nil, writeErr
	})
	_, _, _, err = client.Forward(context.Background(), protocol.Request{
		Type: "request", ID: "failed-write", Method: "POST", Path: "/v1/responses", Body: json.RawMessage(`{}`),
	})
	var phaseError *TransportError
	if !errors.As(err, &phaseError) || !phaseError.WasDispatched() {
		t.Fatalf("failed request write may be partial and must remain ambiguous, got %T: %v", err, err)
	}
}

func TestForwardEnforcesResponseLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"` + strings.Repeat("x", 256) + `"}`))
	}))
	defer server.Close()
	client, err := New(server.URL+"/v1", 128)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = client.Forward(context.Background(), protocol.Request{Type: "request", ID: "limited-models", Method: "GET", Path: "/v1/models"})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized response error = %v", err)
	}
}
