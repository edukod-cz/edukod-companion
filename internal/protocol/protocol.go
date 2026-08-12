package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const Version = 1

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type TypeEnvelope struct {
	Type string `json:"type"`
}

type Hello struct {
	Type            string       `json:"type"`
	ProtocolVersion int          `json:"protocol_version"`
	DeviceID        string       `json:"device_id"`
	DeviceName      string       `json:"device_name"`
	Capabilities    Capabilities `json:"capabilities"`
}

type Capabilities struct {
	Responses        bool `json:"responses"`
	ChatCompletions  bool `json:"chat_completions"`
	Models           bool `json:"models"`
	MaxRequestBytes  int  `json:"max_request_bytes"`
	MaxResponseBytes int  `json:"max_response_bytes"`
	MaxConcurrency   int  `json:"max_concurrency"`
}

type Request struct {
	Type           string          `json:"type"`
	ID             string          `json:"id"`
	Method         string          `json:"method"`
	Path           string          `json:"path"`
	DeadlineUnixMS int64           `json:"deadline_unix_ms,omitempty"`
	Body           json.RawMessage `json:"body,omitempty"`
}

type Cancel struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type Ping struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

type Response struct {
	Type            string          `json:"type"`
	ID              string          `json:"id"`
	Status          int             `json:"status,omitempty"`
	ContentType     string          `json:"content_type,omitempty"`
	Body            json.RawMessage `json:"body,omitempty"`
	ErrorCode       string          `json:"error_code,omitempty"`
	ErrorMessage    string          `json:"error_message,omitempty"`
	DurationMS      int64           `json:"duration_ms,omitempty"`
	CompletedUnixMS int64           `json:"completed_unix_ms"`
}

func NewHello(deviceID, deviceName string, maxRequest, maxResponse, concurrency int) Hello {
	return Hello{
		Type:            "hello",
		ProtocolVersion: Version,
		DeviceID:        deviceID,
		DeviceName:      deviceName,
		Capabilities: Capabilities{
			Responses:        true,
			ChatCompletions:  true,
			Models:           true,
			MaxRequestBytes:  maxRequest,
			MaxResponseBytes: maxResponse,
			MaxConcurrency:   concurrency,
		},
	}
}

func ValidateRequest(request Request, now time.Time, maxBodyBytes int) error {
	if request.Type != "request" {
		return errors.New("message type must be request")
	}
	if !requestIDPattern.MatchString(request.ID) {
		return errors.New("request id is invalid")
	}
	if len(request.Body) > maxBodyBytes {
		return fmt.Errorf("request body exceeds %d bytes", maxBodyBytes)
	}
	if len(request.Body) > 0 && !json.Valid(request.Body) {
		return errors.New("request body must be valid JSON")
	}
	method := strings.ToUpper(strings.TrimSpace(request.Method))
	switch request.Path {
	case "/v1/models":
		if method != "GET" {
			return errors.New("models endpoint only allows GET")
		}
		if len(request.Body) != 0 && string(request.Body) != "null" {
			return errors.New("models request must not contain a body")
		}
	case "/v1/responses", "/v1/chat/completions":
		if method != "POST" {
			return errors.New("inference endpoints only allow POST")
		}
		if len(request.Body) == 0 || string(request.Body) == "null" {
			return errors.New("inference request requires a JSON body")
		}
		var options map[string]json.RawMessage
		if err := json.Unmarshal(request.Body, &options); err != nil || options == nil {
			return errors.New("inference request body must be a JSON object")
		}
		if rawStream, present := options["stream"]; present {
			var stream bool
			if err := json.Unmarshal(rawStream, &stream); err != nil {
				return errors.New("stream must be a boolean")
			}
			if stream {
				return errors.New("streaming is not supported by companion protocol v1")
			}
		}
	default:
		return errors.New("request path is not allowed")
	}
	if request.DeadlineUnixMS != 0 {
		deadline := time.UnixMilli(request.DeadlineUnixMS)
		if !deadline.After(now) {
			return errors.New("request deadline has expired")
		}
		if deadline.After(now.Add(5 * time.Minute)) {
			return errors.New("request deadline is too far in the future")
		}
	}
	return nil
}

func ValidateCancel(cancel Cancel) error {
	if cancel.Type != "cancel" || !requestIDPattern.MatchString(cancel.ID) {
		return errors.New("cancel message is invalid")
	}
	return nil
}

func ErrorResponse(id, code, message string, started time.Time) Response {
	if !requestIDPattern.MatchString(id) {
		id = "invalid"
	}
	return Response{
		Type:            "response",
		ID:              id,
		ErrorCode:       code,
		ErrorMessage:    message,
		DurationMS:      time.Since(started).Milliseconds(),
		CompletedUnixMS: time.Now().UnixMilli(),
	}
}
