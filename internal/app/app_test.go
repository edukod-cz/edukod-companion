package app

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/edukod-cz/edukod-companion/internal/config"
	"github.com/edukod-cz/edukod-companion/internal/gateway"
	"github.com/edukod-cz/edukod-companion/internal/protocol"
)

func TestRunReconnectsWithoutReplayingAmbiguousRequest(t *testing.T) {
	localRequestStarted := make(chan struct{}, 1)
	releaseLocalRequest := make(chan struct{})
	var releaseLocalOnce sync.Once
	releaseLocal := func() { releaseLocalOnce.Do(func() { close(releaseLocalRequest) }) }
	var localRequestCount int32
	localServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/responses" {
			t.Errorf("local request = %s %s", request.Method, request.URL.Path)
		}
		atomic.AddInt32(&localRequestCount, 1)
		select {
		case localRequestStarted <- struct{}{}:
		default:
		}
		select {
		case <-request.Context().Done():
		case <-releaseLocalRequest:
		}
	}))
	defer localServer.Close()
	defer releaseLocal()

	secondConnectionChecked := make(chan struct{})
	releaseSecondConnection := make(chan struct{})
	var releaseOnce sync.Once
	releaseServer := func() { releaseOnce.Do(func() { close(releaseSecondConnection) }) }
	defer releaseServer()
	var connectionCount int32
	websocketServer := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != gateway.WebSocketPath {
			t.Errorf("WebSocket path = %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer device-token-for-reconnect-test" {
			t.Errorf("WebSocket authorization = %q", request.Header.Get("Authorization"))
		}
		connection, buffer, err := acceptTestWebSocket(writer, request)
		if err != nil {
			t.Errorf("accept WebSocket: %v", err)
			return
		}
		defer connection.Close()
		opcode, helloPayload, err := readTestClientFrame(buffer.Reader)
		if err != nil {
			t.Errorf("read hello: %v", err)
			return
		}
		var hello protocol.Hello
		if opcode != 0x1 || json.Unmarshal(helloPayload, &hello) != nil || hello.Type != "hello" || hello.DeviceID != "device-reconnect" {
			t.Errorf("invalid hello: opcode=%d payload=%s", opcode, helloPayload)
			return
		}

		switch atomic.AddInt32(&connectionCount, 1) {
		case 1:
			requestPayload, marshalErr := json.Marshal(protocol.Request{
				Type:           "request",
				ID:             "ambiguous-request",
				Method:         http.MethodPost,
				Path:           "/v1/responses",
				DeadlineUnixMS: time.Now().Add(time.Minute).UnixMilli(),
				Body:           json.RawMessage(`{"model":"qwen3","input":"hello"}`),
			})
			if marshalErr != nil {
				t.Errorf("marshal request: %v", marshalErr)
				return
			}
			if err := writeTestServerFrame(connection, 0x1, requestPayload); err != nil {
				t.Errorf("write request: %v", err)
				return
			}
			select {
			case <-localRequestStarted:
			case <-time.After(2 * time.Second):
				t.Error("ambiguous request never reached the local model")
			}
			// Closing without a response makes dispatch outcome ambiguous. The next
			// WSS session must start clean and must not replay this request.
		case 2:
			if err := connection.SetReadDeadline(time.Now().Add(400 * time.Millisecond)); err != nil {
				t.Errorf("set replay observation deadline: %v", err)
				return
			}
			if replayOpcode, replayPayload, readErr := readTestClientFrame(buffer.Reader); readErr == nil {
				t.Errorf("unexpected frame after reconnect: opcode=%d payload=%s", replayOpcode, replayPayload)
			} else if networkError, ok := readErr.(net.Error); !ok || !networkError.Timeout() {
				t.Errorf("observe reconnect session: %v", readErr)
			}
			close(secondConnectionChecked)
			<-releaseSecondConnection
		default:
			t.Error("companion opened an unexpected extra connection before shutdown")
		}
	}))
	websocketServer.EnableHTTP2 = false
	websocketServer.StartTLS()
	defer websocketServer.Close()

	certificatePath := filepath.Join(t.TempDir(), "gateway-ca.pem")
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: websocketServer.Certificate().Raw})
	if err := os.WriteFile(certificatePath, certificatePEM, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", certificatePath)

	configDirectory := filepath.Join(t.TempDir(), "config")
	store, err := config.NewStore(configDirectory)
	if err != nil {
		t.Fatal(err)
	}
	store.Keyring = nil
	websocketURL := "wss" + strings.TrimPrefix(websocketServer.URL, "https") + gateway.WebSocketPath
	if err := store.Save(config.State{
		SchoolOrigin: websocketServer.URL,
		DeviceID:     "device-reconnect",
		DeviceName:   "Reconnect test",
		WebSocketURL: websocketURL,
		LocalBaseURL: localServer.URL + "/v1",
		PairedAt:     time.Now().UTC(),
	}, "device-token-for-reconnect-test"); err != nil {
		t.Fatal(err)
	}

	application := &App{Stdout: io.Discard, Stderr: io.Discard}
	runDone := make(chan error, 1)
	go func() {
		runDone <- application.run([]string{"--config-dir", configDirectory})
	}()
	stopped := false
	defer func() {
		if !stopped {
			select {
			case <-runDone:
				stopped = true
				return
			default:
			}
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
			select {
			case <-runDone:
			case <-time.After(3 * time.Second):
			}
		}
	}()

	select {
	case <-secondConnectionChecked:
	case <-time.After(6 * time.Second):
		t.Fatal("companion did not reconnect after the ambiguous disconnect")
	}
	if calls := atomic.LoadInt32(&localRequestCount); calls != 1 {
		t.Fatalf("local request count after reconnect = %d, want 1", calls)
	}
	if connections := atomic.LoadInt32(&connectionCount); connections != 2 {
		t.Fatalf("connection count = %d, want 2", connections)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-runDone:
		stopped = true
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not stop after SIGTERM")
	}
	releaseServer()
	releaseLocal()
}

func acceptTestWebSocket(writer http.ResponseWriter, request *http.Request) (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("test server does not support hijacking")
	}
	connection, buffer, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	acceptDigest := sha1.Sum([]byte(request.Header.Get("Sec-Websocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(acceptDigest[:])
	if _, err := buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n"); err != nil {
		connection.Close()
		return nil, nil, err
	}
	if err := buffer.Flush(); err != nil {
		connection.Close()
		return nil, nil, err
	}
	return connection, buffer, nil
}

func readTestClientFrame(reader *bufio.Reader) (byte, []byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, nil, err
	}
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		extended := make([]byte, 2)
		if _, err := io.ReadFull(reader, extended); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(extended))
	case 127:
		extended := make([]byte, 8)
		if _, err := io.ReadFull(reader, extended); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(extended)
	}
	if header[1]&0x80 == 0 {
		return 0, nil, fmt.Errorf("client frame was not masked")
	}
	mask := make([]byte, 4)
	if _, err := io.ReadFull(reader, mask); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	for index := range payload {
		payload[index] ^= mask[index%len(mask)]
	}
	return header[0] & 0x0f, payload, nil
}

func writeTestServerFrame(connection net.Conn, opcode byte, payload []byte) error {
	header := []byte{0x80 | opcode}
	switch {
	case len(payload) < 126:
		header = append(header, byte(len(payload)))
	case len(payload) <= 65535:
		header = append(header, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		header = append(header, 127)
		length := make([]byte, 8)
		binary.BigEndian.PutUint64(length, uint64(len(payload)))
		header = append(header, length...)
	}
	_, err := connection.Write(append(header, payload...))
	return err
}
