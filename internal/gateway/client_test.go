package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestValidateSchoolOrigin(t *testing.T) {
	valid, err := ValidateSchoolOrigin("https://school.example.test/")
	if err != nil || valid.String() != "https://school.example.test" {
		t.Fatalf("valid origin = %v, %v", valid, err)
	}
	for _, candidate := range []string{
		"http://school.example.test",
		"https://school.example.test/admin",
		"https://user@school.example.test",
		"https://school.example.test?redirect=evil",
	} {
		if _, err := ValidateSchoolOrigin(candidate); err == nil {
			t.Fatalf("accepted unsafe origin %q", candidate)
		}
	}
}

func TestEnrollUsesOneTimeCodeAndDerivesWSSURL(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != EnrollmentPath {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		var enrollment EnrollmentRequest
		if err := json.NewDecoder(request.Body).Decode(&enrollment); err != nil {
			t.Fatal(err)
		}
		if enrollment.PairingCode != "one-time-code" || enrollment.ProtocolVersion != 1 {
			t.Fatalf("enrollment = %#v", enrollment)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"device_id":"device-001","device_token":"12345678901234567890123456789012"}`))
	}))
	defer server.Close()
	httpClient := server.Client()
	httpClient.Timeout = 5 * time.Second
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	client := &Client{httpClient: httpClient}
	response, err := client.Enroll(context.Background(), server.URL, "one-time-code", "AI workstation")
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse(server.URL)
	want := "wss://" + origin.Host + WebSocketPath
	if response.WebSocketURL != want {
		t.Fatalf("WebSocketURL = %q, want %q", response.WebSocketURL, want)
	}
}

func TestValidateWebSocketURLRequiresExactPublicRoute(t *testing.T) {
	origin, _ := url.Parse("https://school.example.test")
	if _, err := ValidateWebSocketURL("wss://school.example.test"+WebSocketPath, origin); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{
		"ws://school.example.test" + WebSocketPath,
		"wss://other.example.test" + WebSocketPath,
		"wss://school.example.test/ws",
		"wss://school.example.test" + WebSocketPath + "?token=secret",
	} {
		if _, err := ValidateWebSocketURL(candidate, origin); err == nil {
			t.Fatalf("accepted unsafe WebSocket URL %q", candidate)
		}
	}
}

func TestDeviceIdentityRejectsControlCharacters(t *testing.T) {
	if !containsControl("workstation\x1b[2J") {
		t.Fatal("terminal control character was accepted")
	}
	for _, candidate := range []string{"short", "-starts-wrong", "device id", "device/one"} {
		if validDeviceID(candidate) {
			t.Fatalf("invalid device id %q was accepted", candidate)
		}
	}
	if !validDeviceID("device-01:primary") {
		t.Fatal("valid device id was rejected")
	}
}
