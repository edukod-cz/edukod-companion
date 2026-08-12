package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/edukod-cz/edukod-companion/internal/protocol"
)

const (
	DefaultMaxRequestBytes  = 8 << 20
	DefaultMaxResponseBytes = 16 << 20
	DefaultConcurrency      = 4
	DefaultQueueSize        = 16
)

type Connection interface {
	ReadMessage() ([]byte, error)
	WriteJSON(value interface{}) error
	WritePing() error
	Close() error
}

type Forwarder interface {
	Forward(ctx context.Context, request protocol.Request) (status int, contentType string, body json.RawMessage, err error)
}

type Options struct {
	DeviceID         string
	DeviceName       string
	MaxRequestBytes  int
	MaxResponseBytes int
	Concurrency      int
	QueueSize        int
	Heartbeat        time.Duration
	DefaultTimeout   time.Duration
}

type job struct {
	request protocol.Request
	ctx     context.Context
	cancel  context.CancelFunc
	started time.Time
}

func (options *Options) defaults() {
	if options.MaxRequestBytes <= 0 {
		options.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if options.MaxResponseBytes <= 0 {
		options.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if options.Concurrency <= 0 {
		options.Concurrency = DefaultConcurrency
	}
	if options.QueueSize <= 0 {
		options.QueueSize = DefaultQueueSize
	}
	if options.Heartbeat <= 0 {
		options.Heartbeat = 20 * time.Second
	}
	if options.DefaultTimeout <= 0 {
		options.DefaultTimeout = 2 * time.Minute
	}
}

func Serve(ctx context.Context, connection Connection, forwarder Forwarder, options Options) error {
	options.defaults()
	if options.DeviceID == "" || options.DeviceName == "" {
		return errors.New("device identity is required")
	}
	serveCtx, stop := context.WithCancel(ctx)
	defer stop()
	defer connection.Close()
	if err := connection.WriteJSON(protocol.NewHello(
		options.DeviceID,
		options.DeviceName,
		options.MaxRequestBytes,
		options.MaxResponseBytes,
		options.Concurrency,
	)); err != nil {
		return err
	}

	jobs := make(chan job, options.QueueSize)
	writeErrors := make(chan error, 1)
	var workers sync.WaitGroup
	var requestsMu sync.Mutex
	cancellations := make(map[string]context.CancelFunc)

	write := func(value interface{}) bool {
		if err := connection.WriteJSON(value); err != nil {
			select {
			case writeErrors <- err:
			default:
			}
			stop()
			return false
		}
		return true
	}

	for index := 0; index < options.Concurrency; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for work := range jobs {
				response := execute(work, forwarder)
				requestsMu.Lock()
				delete(cancellations, work.request.ID)
				requestsMu.Unlock()
				write(response)
				work.cancel()
			}
		}()
	}

	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(options.Heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-serveCtx.Done():
				return
			case <-ticker.C:
				if err := connection.WritePing(); err != nil {
					select {
					case writeErrors <- err:
					default:
					}
					stop()
					return
				}
			}
		}
	}()

	readErrors := make(chan error, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			select {
			case <-serveCtx.Done():
				return
			default:
			}
			payload, err := connection.ReadMessage()
			if err != nil {
				readErrors <- err
				return
			}
			if len(payload) > options.MaxRequestBytes+4096 {
				readErrors <- errors.New("gateway protocol message is too large")
				return
			}
			var envelope protocol.TypeEnvelope
			if err := json.Unmarshal(payload, &envelope); err != nil {
				readErrors <- errors.New("gateway sent invalid JSON")
				return
			}
			switch envelope.Type {
			case "request":
				var request protocol.Request
				if err := json.Unmarshal(payload, &request); err != nil {
					readErrors <- errors.New("gateway sent an invalid request")
					return
				}
				started := time.Now()
				if err := protocol.ValidateRequest(request, started, options.MaxRequestBytes); err != nil {
					write(protocol.ErrorResponse(request.ID, "bad_request", err.Error(), started))
					continue
				}
				requestsMu.Lock()
				_, duplicate := cancellations[request.ID]
				requestsMu.Unlock()
				if duplicate {
					write(protocol.ErrorResponse(request.ID, "duplicate_request", "request id is already active", started))
					continue
				}
				deadline := started.Add(options.DefaultTimeout)
				if request.DeadlineUnixMS != 0 {
					deadline = time.UnixMilli(request.DeadlineUnixMS)
				}
				requestCtx, cancel := context.WithDeadline(serveCtx, deadline)
				requestsMu.Lock()
				cancellations[request.ID] = cancel
				requestsMu.Unlock()
				select {
				case jobs <- job{request: request, ctx: requestCtx, cancel: cancel, started: started}:
				case <-serveCtx.Done():
					cancel()
					return
				default:
					requestsMu.Lock()
					delete(cancellations, request.ID)
					requestsMu.Unlock()
					cancel()
					write(protocol.ErrorResponse(request.ID, "busy", "local companion queue is full", started))
				}
			case "cancel":
				var cancelMessage protocol.Cancel
				if err := json.Unmarshal(payload, &cancelMessage); err != nil || protocol.ValidateCancel(cancelMessage) != nil {
					readErrors <- errors.New("gateway sent an invalid cancel message")
					return
				}
				requestsMu.Lock()
				cancel := cancellations[cancelMessage.ID]
				requestsMu.Unlock()
				if cancel != nil {
					cancel()
				}
			case "ping":
				var ping protocol.Ping
				if err := json.Unmarshal(payload, &ping); err != nil {
					readErrors <- errors.New("gateway sent an invalid ping")
					return
				}
				write(protocol.Ping{Type: "pong", ID: ping.ID})
			default:
				readErrors <- fmt.Errorf("gateway sent unsupported message type %q", envelope.Type)
				return
			}
		}
	}()

	var result error
	select {
	case <-ctx.Done():
		result = ctx.Err()
	case result = <-readErrors:
	case result = <-writeErrors:
	case <-serveCtx.Done():
		result = serveCtx.Err()
	}
	stop()
	_ = connection.Close()
	<-readDone
	requestsMu.Lock()
	for _, cancel := range cancellations {
		cancel()
	}
	requestsMu.Unlock()
	close(jobs)
	workers.Wait()
	<-heartbeatDone
	return result
}

func execute(work job, forwarder Forwarder) protocol.Response {
	status, contentType, body, err := forwarder.Forward(work.ctx, work.request)
	if err != nil {
		code := "local_unavailable"
		message := "local model endpoint is unavailable"
		var phaseError interface{ WasDispatched() bool }
		if errors.Is(err, context.Canceled) {
			code = "canceled"
			message = "request was canceled"
		} else if errors.As(err, &phaseError) && !phaseError.WasDispatched() {
			code = "local_predispatch_unavailable"
			message = "local model endpoint is not reachable"
		} else if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			code = "timeout"
			message = "local model request timed out"
		} else if strings.Contains(err.Error(), "non-JSON") || strings.Contains(err.Error(), "exceeds") {
			code = "local_invalid_response"
			message = err.Error()
		}
		return protocol.ErrorResponse(work.request.ID, code, message, work.started)
	}
	if !strings.HasPrefix(strings.ToLower(contentType), "application/json") {
		contentType = "application/json"
	}
	return protocol.Response{
		Type:            "response",
		ID:              work.request.ID,
		Status:          status,
		ContentType:     contentType,
		Body:            body,
		DurationMS:      time.Since(work.started).Milliseconds(),
		CompletedUnixMS: time.Now().UnixMilli(),
	}
}

func isTimeout(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

func IsDisconnect(err error) bool {
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF)
}
