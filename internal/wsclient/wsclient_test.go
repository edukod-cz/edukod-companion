package wsclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDialAndExchangeJSON(t *testing.T) {
	authorized := make(chan bool, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorized <- request.Header.Get("Authorization") == "Bearer device-secret"
		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		connection, buffer, err := hijacker.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		key := request.Header.Get("Sec-Websocket-Key")
		_, _ = buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + expectedAccept(key) + "\r\n\r\n")
		if err := buffer.Flush(); err != nil {
			t.Error(err)
			return
		}
		payload, err := readClientFrame(connection)
		if err != nil {
			t.Error(err)
			return
		}
		var sent map[string]string
		if err := json.Unmarshal(payload, &sent); err != nil || sent["type"] != "hello" {
			t.Errorf("unexpected client payload: %s (%v)", payload, err)
			return
		}
		if err := writeServerText(connection, []byte(`{"type":"ping","id":"p1"}`)); err != nil {
			t.Error(err)
		}
	}))
	server.EnableHTTP2 = false
	server.StartTLS()
	defer server.Close()
	websocketURL := "wss" + strings.TrimPrefix(server.URL, "https")
	connection, err := (Dialer{
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, // Test-only server certificate.
	}).Dial(context.Background(), websocketURL, "device-secret")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.WriteJSON(map[string]string{"type": "hello"}); err != nil {
		t.Fatal(err)
	}
	payload, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != `{"type":"ping","id":"p1"}` {
		t.Fatalf("payload = %s", payload)
	}
	select {
	case ok := <-authorized:
		if !ok {
			t.Fatal("device bearer token was not sent")
		}
	case <-time.After(time.Second):
		t.Fatal("server did not observe authorization")
	}
}

func TestReadMessageReassemblesBoundedFragments(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()
	connection := &Conn{
		connection:  clientSide,
		reader:      bufio.NewReader(clientSide),
		readTimeout: time.Second,
		maxMessage:  1024,
	}
	writeDone := make(chan error, 1)
	go func() {
		if err := writeServerFrame(serverSide, false, 0x1, []byte(`{"type":"request",`)); err != nil {
			writeDone <- err
			return
		}
		writeDone <- writeServerFrame(serverSide, true, 0x0, []byte(`"id":"fragmented"}`))
	}()
	payload, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != `{"type":"request","id":"fragmented"}` {
		t.Fatalf("payload = %s", payload)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
}

func readClientFrame(connection net.Conn) ([]byte, error) {
	reader := bufio.NewReader(connection)
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	length := int(header[1] & 0x7f)
	if length == 126 {
		extended := make([]byte, 2)
		if _, err := io.ReadFull(reader, extended); err != nil {
			return nil, err
		}
		length = int(binary.BigEndian.Uint16(extended))
	}
	mask := make([]byte, 4)
	if _, err := io.ReadFull(reader, mask); err != nil {
		return nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	for index := range payload {
		payload[index] ^= mask[index%4]
	}
	return payload, nil
}

func writeServerText(connection net.Conn, payload []byte) error {
	return writeServerFrame(connection, true, 0x1, payload)
}

func writeServerFrame(connection net.Conn, final bool, opcode byte, payload []byte) error {
	first := opcode
	if final {
		first |= 0x80
	}
	header := []byte{first}
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
