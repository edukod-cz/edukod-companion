package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/edukod-cz/edukod-companion/internal/protocol"
)

type fakeConnection struct {
	reads  chan []byte
	writes chan interface{}
	closed chan struct{}
	once   sync.Once
}

func (connection *fakeConnection) ReadMessage() ([]byte, error) {
	payload, ok := <-connection.reads
	if !ok {
		return nil, io.EOF
	}
	return payload, nil
}

func (connection *fakeConnection) WriteJSON(value interface{}) error {
	select {
	case connection.writes <- value:
		return nil
	case <-connection.closed:
		return errors.New("closed")
	}
}

func (connection *fakeConnection) WritePing() error { return nil }

func (connection *fakeConnection) Close() error {
	connection.once.Do(func() { close(connection.closed) })
	return nil
}

type fakeForwarder struct{}

func (fakeForwarder) Forward(_ context.Context, request protocol.Request) (int, string, json.RawMessage, error) {
	return 200, "application/json", json.RawMessage(`{"id":"local-result","path":"` + request.Path + `"}`), nil
}

type cancelForwarder struct {
	started chan struct{}
}

type phaseError struct {
	dispatched bool
}

func (err phaseError) Error() string       { return "local transport failed" }
func (err phaseError) WasDispatched() bool { return err.dispatched }

type errorForwarder struct {
	err error
}

func (forwarder errorForwarder) Forward(context.Context, protocol.Request) (int, string, json.RawMessage, error) {
	return 0, "", nil, forwarder.err
}

type blockingForwarder struct {
	started chan string
}

func (forwarder blockingForwarder) Forward(ctx context.Context, request protocol.Request) (int, string, json.RawMessage, error) {
	select {
	case forwarder.started <- request.ID:
	default:
	}
	<-ctx.Done()
	return 0, "", nil, ctx.Err()
}

func (forwarder cancelForwarder) Forward(ctx context.Context, _ protocol.Request) (int, string, json.RawMessage, error) {
	close(forwarder.started)
	<-ctx.Done()
	return 0, "", nil, ctx.Err()
}

func TestServeForwardsAllowedRequest(t *testing.T) {
	connection := &fakeConnection{
		reads:  make(chan []byte, 2),
		writes: make(chan interface{}, 4),
		closed: make(chan struct{}),
	}
	request := protocol.Request{
		Type:   "request",
		ID:     "request-1",
		Method: "POST",
		Path:   "/v1/responses",
		Body:   json.RawMessage(`{"model":"qwen3"}`),
	}
	payload, _ := json.Marshal(request)
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), connection, fakeForwarder{}, Options{
			DeviceID:   "device-1",
			DeviceName: "AI workstation",
			Heartbeat:  time.Hour,
		})
	}()
	first := <-connection.writes
	if _, ok := first.(protocol.Hello); !ok {
		t.Fatalf("first message type = %T, want protocol.Hello", first)
	}
	connection.reads <- payload
	select {
	case second := <-connection.writes:
		response, ok := second.(protocol.Response)
		if !ok || response.ID != request.ID || response.Status != 200 {
			t.Fatalf("response = %#v", second)
		}
	case <-time.After(time.Second):
		t.Fatal("forwarded response was not returned")
	}
	close(connection.reads)
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("Serve error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not stop after gateway disconnect")
	}
}

func TestServeRejectsForbiddenPathWithoutForwarding(t *testing.T) {
	connection := &fakeConnection{
		reads:  make(chan []byte, 2),
		writes: make(chan interface{}, 4),
		closed: make(chan struct{}),
	}
	payload := []byte(`{"type":"request","id":"request-2","method":"GET","path":"/api/tags"}`)
	connection.reads <- payload
	close(connection.reads)
	_ = Serve(context.Background(), connection, fakeForwarder{}, Options{
		DeviceID:   "device-1",
		DeviceName: "AI workstation",
		Heartbeat:  time.Hour,
	})
	<-connection.writes
	response := (<-connection.writes).(protocol.Response)
	if response.ErrorCode != "bad_request" {
		t.Fatalf("error code = %q", response.ErrorCode)
	}
}

func TestServePropagatesGatewayCancellation(t *testing.T) {
	connection := &fakeConnection{
		reads:  make(chan []byte, 3),
		writes: make(chan interface{}, 4),
		closed: make(chan struct{}),
	}
	forwarder := cancelForwarder{started: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), connection, forwarder, Options{
			DeviceID:   "device-1",
			DeviceName: "AI workstation",
			Heartbeat:  time.Hour,
		})
	}()
	<-connection.writes // hello
	connection.reads <- []byte(`{"type":"request","id":"cancel-me","method":"POST","path":"/v1/responses","body":{"model":"qwen3"}}`)
	select {
	case <-forwarder.started:
	case <-time.After(time.Second):
		t.Fatal("request did not reach local forwarder")
	}
	connection.reads <- []byte(`{"type":"cancel","id":"cancel-me"}`)
	select {
	case message := <-connection.writes:
		response, ok := message.(protocol.Response)
		if !ok || response.ID != "cancel-me" || response.ErrorCode != "canceled" {
			t.Fatalf("canceled response = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not produce a response")
	}
	close(connection.reads)
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("Serve error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not stop after gateway disconnect")
	}
}

func TestExecuteDistinguishesPredispatchAndAmbiguousLocalFailures(t *testing.T) {
	request := protocol.Request{Type: "request", ID: "phase", Method: "POST", Path: "/v1/responses"}
	work := job{request: request, ctx: context.Background(), started: time.Now()}
	predispatch := execute(work, errorForwarder{err: phaseError{dispatched: false}})
	if predispatch.ErrorCode != "local_predispatch_unavailable" {
		t.Fatalf("predispatch error code = %q", predispatch.ErrorCode)
	}
	ambiguous := execute(work, errorForwarder{err: phaseError{dispatched: true}})
	if ambiguous.ErrorCode != "local_unavailable" {
		t.Fatalf("post-write error code = %q", ambiguous.ErrorCode)
	}
}

func TestServeAppliesBoundedQueueBackpressure(t *testing.T) {
	connection := &fakeConnection{
		reads:  make(chan []byte, 4),
		writes: make(chan interface{}, 8),
		closed: make(chan struct{}),
	}
	forwarder := blockingForwarder{started: make(chan string, 2)}
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), connection, forwarder, Options{
			DeviceID:    "device-1",
			DeviceName:  "AI workstation",
			Concurrency: 1,
			QueueSize:   1,
			Heartbeat:   time.Hour,
		})
	}()
	<-connection.writes // hello

	connection.reads <- []byte(`{"type":"request","id":"running","method":"POST","path":"/v1/responses","body":{"model":"qwen3"}}`)
	select {
	case id := <-forwarder.started:
		if id != "running" {
			t.Fatalf("first forwarded request = %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("first request did not occupy the only worker")
	}
	connection.reads <- []byte(`{"type":"request","id":"queued","method":"POST","path":"/v1/responses","body":{"model":"qwen3"}}`)
	connection.reads <- []byte(`{"type":"request","id":"overflow","method":"POST","path":"/v1/responses","body":{"model":"qwen3"}}`)

	select {
	case message := <-connection.writes:
		response, ok := message.(protocol.Response)
		if !ok || response.ID != "overflow" || response.ErrorCode != "busy" {
			t.Fatalf("backpressure response = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("queue overflow did not produce a busy response")
	}

	close(connection.reads)
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("Serve error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not stop after the saturated connection closed")
	}
}

func TestServeIgnoresCancelAfterCompletedResponse(t *testing.T) {
	connection := &fakeConnection{
		reads:  make(chan []byte, 4),
		writes: make(chan interface{}, 8),
		closed: make(chan struct{}),
	}
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), connection, fakeForwarder{}, Options{
			DeviceID:   "device-1",
			DeviceName: "AI workstation",
			Heartbeat:  time.Hour,
		})
	}()
	<-connection.writes // hello
	connection.reads <- []byte(`{"type":"request","id":"already-complete","method":"POST","path":"/v1/responses","body":{"model":"qwen3"}}`)
	select {
	case message := <-connection.writes:
		response, ok := message.(protocol.Response)
		if !ok || response.ID != "already-complete" || response.Status != 200 {
			t.Fatalf("completed response = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("request did not complete")
	}

	// The following ping is an ordering barrier: seeing its pong proves the read
	// loop already processed the late cancel. The cancel must not create a second
	// response for the completed request.
	connection.reads <- []byte(`{"type":"cancel","id":"already-complete"}`)
	connection.reads <- []byte(`{"type":"ping","id":"after-late-cancel"}`)
	select {
	case message := <-connection.writes:
		pong, ok := message.(protocol.Ping)
		if !ok || pong.Type != "pong" || pong.ID != "after-late-cancel" {
			t.Fatalf("message after late cancel = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("late cancel ordering barrier was not acknowledged")
	}

	close(connection.reads)
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("Serve error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not stop after the connection closed")
	}
}
