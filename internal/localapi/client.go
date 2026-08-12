package localapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/edukod-cz/edukod-companion/internal/protocol"
)

const DefaultBaseURL = "http://127.0.0.1:11434/v1"

type Client struct {
	base        *url.URL
	httpClient  *http.Client
	maxResponse int64
}

// TransportError records whether the local HTTP request reached the point at
// which net/http wrote it. Callers may safely retry/fallback only when a local
// Ollama connection failed before that point.
type TransportError struct {
	Err            error
	RequestWritten bool
}

func (err *TransportError) Error() string { return err.Err.Error() }
func (err *TransportError) Unwrap() error { return err.Err }
func (err *TransportError) WasDispatched() bool {
	return err.RequestWritten
}

func ValidateBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("parse local model URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("local model URL must use http or https")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("local model URL must not contain credentials, query, or fragment")
	}
	if parsed.Hostname() == "" {
		return nil, errors.New("local model URL requires a host")
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = "/v1"
	}
	if parsed.Path != "/v1" && parsed.Path != "/v1/" {
		return nil, errors.New("local model URL path must be /v1")
	}
	parsed.Path = "/v1"
	parsed.RawPath = ""
	resolveContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ensureLoopbackHost(resolveContext, parsed.Hostname()); err != nil {
		return nil, err
	}
	return parsed, nil
}

func ensureLoopbackHost(ctx context.Context, host string) error {
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve local model host: %w", err)
	}
	if len(addresses) == 0 {
		return errors.New("local model host has no addresses")
	}
	for _, address := range addresses {
		if !address.IP.IsLoopback() {
			return fmt.Errorf("local model host resolves outside loopback: %s", address.IP.String())
		}
	}
	return nil
}

func New(rawBaseURL string, maxResponseBytes int64) (*Client, error) {
	base, err := ValidateBaseURL(rawBaseURL)
	if err != nil {
		return nil, err
	}
	if maxResponseBytes <= 0 {
		return nil, errors.New("maximum response size must be positive")
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DisableCompression:    true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	transport.DialContext = loopbackDialer
	return &Client{
		base: base,
		httpClient: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("local model redirects are disabled")
			},
		},
		maxResponse: maxResponseBytes,
	}, nil
}

func loopbackDialer(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid local model address: %w", err)
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve local model host: %w", err)
	}
	var lastErr error
	for _, candidate := range addresses {
		if !candidate.IP.IsLoopback() {
			return nil, fmt.Errorf("refusing non-loopback local model address: %s", candidate.IP.String())
		}
		endpoint := net.JoinHostPort(candidate.IP.String(), port)
		connection, dialErr := (&net.Dialer{}).DialContext(ctx, network, endpoint)
		if dialErr == nil {
			return connection, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = errors.New("local model host has no loopback addresses")
	}
	return nil, lastErr
}

func (client *Client) Forward(ctx context.Context, request protocol.Request) (int, string, json.RawMessage, error) {
	if err := protocol.ValidateRequest(request, time.Now(), 8<<20); err != nil {
		return 0, "", nil, err
	}
	endpoint := *client.base
	endpoint.Path = request.Path
	var body io.Reader
	if len(request.Body) > 0 {
		body = strings.NewReader(string(request.Body))
	}
	httpRequest, err := http.NewRequestWithContext(ctx, strings.ToUpper(request.Method), endpoint.String(), body)
	if err != nil {
		return 0, "", nil, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	if body != nil {
		httpRequest.Header.Set("Content-Type", "application/json")
	}
	// 0 means net/http never reached request writing (for example connection
	// refused); 1 means a complete write; 2 means a write failed and may have
	// been partial. Both 1 and 2 are ambiguous for replay purposes.
	var requestWritePhase uint32
	trace := &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			atomic.StoreUint32(&requestWritePhase, 1)
		} else {
			atomic.StoreUint32(&requestWritePhase, 2)
		}
	}}
	httpRequest = httpRequest.WithContext(httptrace.WithClientTrace(httpRequest.Context(), trace))
	response, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return 0, "", nil, &TransportError{Err: err, RequestWritten: atomic.LoadUint32(&requestWritePhase) != 0}
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, client.maxResponse+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return 0, "", nil, &TransportError{Err: err, RequestWritten: true}
	}
	if int64(len(payload)) > client.maxResponse {
		return 0, "", nil, fmt.Errorf("local model response exceeds %s bytes", strconv.FormatInt(client.maxResponse, 10))
	}
	if len(payload) == 0 || !json.Valid(payload) {
		return 0, "", nil, errors.New("local model returned a non-JSON response")
	}
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	return response.StatusCode, contentType, json.RawMessage(payload), nil
}

func (client *Client) Models(ctx context.Context) (json.RawMessage, error) {
	request := protocol.Request{Type: "request", ID: "models", Method: "GET", Path: "/v1/models"}
	status, _, body, err := client.Forward(ctx, request)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("local model endpoint returned HTTP %d", status)
	}
	return body, nil
}
