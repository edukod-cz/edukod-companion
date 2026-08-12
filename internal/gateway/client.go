package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/edukod-cz/edukod-companion/internal/protocol"
)

const (
	EnrollmentPath = "/api/ai/companion/v1/enroll"
	WebSocketPath  = "/api/ai/companion/v1/ws"
	DevicePath     = "/api/ai/companion/v1/device"
)

type Client struct {
	httpClient *http.Client
}

type EnrollmentRequest struct {
	PairingCode     string                `json:"pairing_code"`
	DeviceName      string                `json:"device_name"`
	ProtocolVersion int                   `json:"protocol_version"`
	Capabilities    protocol.Capabilities `json:"capabilities"`
}

type EnrollmentResponse struct {
	DeviceID     string `json:"device_id"`
	DeviceToken  string `json:"device_token"`
	WebSocketURL string `json:"websocket_url,omitempty"`
}

func New() *Client {
	return &Client{httpClient: &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil,
			DisableCompression:    true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("gateway redirects are disabled")
		},
	}}
}

func ValidateSchoolOrigin(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("parse school URL: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Hostname() == "" {
		return nil, errors.New("school URL must be an https origin")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("school URL must not contain credentials, query, or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, errors.New("school URL must not contain a path")
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return parsed, nil
}

func ValidateWebSocketURL(raw string, schoolOrigin *url.URL) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("parse gateway WebSocket URL: %w", err)
	}
	if parsed.Scheme != "wss" || parsed.Host != schoolOrigin.Host {
		return nil, errors.New("gateway WebSocket URL must use the exact school origin over wss")
	}
	if parsed.Path != WebSocketPath || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("gateway returned an unexpected WebSocket URL")
	}
	return parsed, nil
}

func (client *Client) Enroll(ctx context.Context, schoolOrigin, pairingCode, deviceName string) (EnrollmentResponse, error) {
	var enrollment EnrollmentResponse
	origin, err := ValidateSchoolOrigin(schoolOrigin)
	if err != nil {
		return enrollment, err
	}
	pairingCode = strings.TrimSpace(pairingCode)
	deviceName = strings.TrimSpace(deviceName)
	if len(pairingCode) < 8 || len(pairingCode) > 256 || strings.ContainsAny(pairingCode, " \t\r\n") {
		return enrollment, errors.New("pairing code must be 8-256 non-whitespace characters")
	}
	if len(deviceName) == 0 || len(deviceName) > 80 || containsControl(deviceName) {
		return enrollment, errors.New("device name must be 1-80 printable characters")
	}
	payload, err := json.Marshal(EnrollmentRequest{
		PairingCode:     pairingCode,
		DeviceName:      deviceName,
		ProtocolVersion: protocol.Version,
		Capabilities: protocol.Capabilities{
			Responses:        true,
			ChatCompletions:  true,
			Models:           true,
			MaxRequestBytes:  8 << 20,
			MaxResponseBytes: 16 << 20,
			MaxConcurrency:   4,
		},
	})
	if err != nil {
		return enrollment, err
	}
	endpoint := *origin
	endpoint.Path = EnrollmentPath
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return enrollment, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return enrollment, fmt.Errorf("enroll companion: %w", err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, 65537)
	body, err := io.ReadAll(limited)
	if err != nil {
		return enrollment, err
	}
	if len(body) > 65536 {
		return enrollment, errors.New("gateway enrollment response is too large")
	}
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return enrollment, fmt.Errorf("gateway rejected pairing with HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&enrollment); err != nil {
		return enrollment, errors.New("gateway returned an invalid enrollment response")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return enrollment, errors.New("gateway returned trailing enrollment data")
	}
	if !validDeviceID(enrollment.DeviceID) || len(enrollment.DeviceToken) < 32 || len(enrollment.DeviceToken) > 4096 {
		return enrollment, errors.New("gateway returned incomplete device credentials")
	}
	if enrollment.WebSocketURL == "" {
		websocket := *origin
		websocket.Scheme = "wss"
		websocket.Path = WebSocketPath
		enrollment.WebSocketURL = websocket.String()
	}
	if _, err := ValidateWebSocketURL(enrollment.WebSocketURL, origin); err != nil {
		return EnrollmentResponse{}, err
	}
	return enrollment, nil
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func validDeviceID(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || (index > 0 && strings.ContainsRune("._:-", character)) {
			continue
		}
		return false
	}
	return true
}

func (client *Client) Revoke(ctx context.Context, schoolOrigin, deviceToken string) error {
	origin, err := ValidateSchoolOrigin(schoolOrigin)
	if err != nil {
		return err
	}
	if strings.TrimSpace(deviceToken) == "" || strings.ContainsAny(deviceToken, "\r\n") {
		return errors.New("device credential is invalid")
	}
	endpoint := *origin
	endpoint.Path = DevicePath
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+deviceToken)
	response, err := client.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusNotFound {
		return fmt.Errorf("gateway rejected unpair with HTTP %d", response.StatusCode)
	}
	return nil
}
