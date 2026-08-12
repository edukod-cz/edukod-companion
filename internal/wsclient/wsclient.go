package wsclient

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type Dialer struct {
	HandshakeTimeout time.Duration
	ReadTimeout      time.Duration
	MaxMessageBytes  int64
	TLSConfig        *tls.Config
}

type Conn struct {
	connection  net.Conn
	reader      *bufio.Reader
	writeMu     sync.Mutex
	readTimeout time.Duration
	maxMessage  int64
	closeOnce   sync.Once
}

func (dialer Dialer) Dial(ctx context.Context, rawURL, bearerToken string) (*Conn, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse gateway WebSocket URL: %w", err)
	}
	if parsed.Scheme != "wss" || parsed.Hostname() == "" {
		return nil, errors.New("gateway WebSocket URL must use wss")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("gateway WebSocket URL must not contain credentials, query, or fragments")
	}
	if strings.TrimSpace(bearerToken) == "" || strings.ContainsAny(bearerToken, "\r\n") {
		return nil, errors.New("device credential is invalid")
	}
	timeout := dialer.HandshakeTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	maxMessage := dialer.MaxMessageBytes
	if maxMessage <= 0 {
		maxMessage = 12 << 20
	}
	readTimeout := dialer.ReadTimeout
	if readTimeout <= 0 {
		readTimeout = 75 * time.Second
	}
	port := parsed.Port()
	if port == "" {
		port = "443"
	}
	dialContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rawConnection, err := (&net.Dialer{}).DialContext(dialContext, "tcp", net.JoinHostPort(parsed.Hostname(), port))
	if err != nil {
		return nil, fmt.Errorf("connect to companion gateway: %w", err)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: parsed.Hostname()}
	if dialer.TLSConfig != nil {
		tlsConfig = dialer.TLSConfig.Clone()
		if tlsConfig.ServerName == "" {
			tlsConfig.ServerName = parsed.Hostname()
		}
		if tlsConfig.MinVersion == 0 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
	}
	connection := tls.Client(rawConnection, tlsConfig)
	if err := connection.SetDeadline(time.Now().Add(timeout)); err != nil {
		rawConnection.Close()
		return nil, err
	}
	if err := connection.HandshakeContext(dialContext); err != nil {
		rawConnection.Close()
		return nil, fmt.Errorf("secure companion gateway handshake: %w", err)
	}
	websocketKeyBytes := make([]byte, 16)
	if _, err := rand.Read(websocketKeyBytes); err != nil {
		connection.Close()
		return nil, err
	}
	websocketKey := base64.StdEncoding.EncodeToString(websocketKeyBytes)
	request := &http.Request{
		Method: "GET",
		URL:    parsed,
		Host:   parsed.Host,
		Header: http.Header{
			"Authorization":         []string{"Bearer " + bearerToken},
			"Connection":            []string{"Upgrade"},
			"Upgrade":               []string{"websocket"},
			"Sec-Websocket-Key":     []string{websocketKey},
			"Sec-Websocket-Version": []string{"13"},
		},
	}
	if err := request.Write(connection); err != nil {
		connection.Close()
		return nil, fmt.Errorf("write companion gateway handshake: %w", err)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		connection.Close()
		return nil, fmt.Errorf("read companion gateway handshake: %w", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols ||
		!headerHasToken(response.Header, "Upgrade", "websocket") ||
		!headerHasToken(response.Header, "Connection", "upgrade") ||
		response.Header.Get("Sec-Websocket-Accept") != expectedAccept(websocketKey) {
		connection.Close()
		return nil, fmt.Errorf("gateway rejected WebSocket upgrade with HTTP %d", response.StatusCode)
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		connection.Close()
		return nil, err
	}
	return &Conn{
		connection:  connection,
		reader:      reader,
		readTimeout: readTimeout,
		maxMessage:  maxMessage,
	}, nil
}

func expectedAccept(key string) string {
	digest := sha1.Sum([]byte(key + websocketGUID))
	return base64.StdEncoding.EncodeToString(digest[:])
}

func headerHasToken(header http.Header, name, expected string) bool {
	for _, value := range header.Values(name) {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), expected) {
				return true
			}
		}
	}
	return false
}

func (connection *Conn) ReadMessage() ([]byte, error) {
	var messageOpcode byte
	var message []byte
	for {
		if err := connection.connection.SetReadDeadline(time.Now().Add(connection.readTimeout)); err != nil {
			return nil, err
		}
		final, opcode, payload, err := connection.readFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case 0x0:
			if messageOpcode == 0 {
				return nil, errors.New("unexpected WebSocket continuation frame")
			}
			if int64(len(message))+int64(len(payload)) > connection.maxMessage {
				return nil, fmt.Errorf("gateway WebSocket message exceeds %d bytes", connection.maxMessage)
			}
			message = append(message, payload...)
			if final {
				if messageOpcode != 0x1 {
					return nil, errors.New("gateway WebSocket messages must be text")
				}
				return message, nil
			}
		case 0x1:
			if messageOpcode != 0 {
				return nil, errors.New("gateway started a new WebSocket message before completing the previous one")
			}
			if final {
				return payload, nil
			}
			messageOpcode = opcode
			message = append(message, payload...)
		case 0x2:
			return nil, errors.New("gateway WebSocket messages must be text")
		case 0x8:
			return nil, io.EOF
		case 0x9:
			if err := connection.writeFrame(0xA, payload); err != nil {
				return nil, err
			}
		case 0xA:
			continue
		default:
			return nil, fmt.Errorf("unsupported gateway WebSocket opcode %d", opcode)
		}
	}
}

func (connection *Conn) readFrame() (bool, byte, []byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(connection.reader, header); err != nil {
		return false, 0, nil, err
	}
	if header[0]&0x70 != 0 {
		return false, 0, nil, errors.New("extended WebSocket frames are not supported")
	}
	if header[1]&0x80 != 0 {
		return false, 0, nil, errors.New("gateway WebSocket frames must not be masked")
	}
	final := header[0]&0x80 != 0
	opcode := header[0] & 0x0f
	length := int64(header[1] & 0x7f)
	switch length {
	case 126:
		extended := make([]byte, 2)
		if _, err := io.ReadFull(connection.reader, extended); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(extended))
	case 127:
		extended := make([]byte, 8)
		if _, err := io.ReadFull(connection.reader, extended); err != nil {
			return false, 0, nil, err
		}
		unsignedLength := binary.BigEndian.Uint64(extended)
		if unsignedLength > uint64(^uint(0)>>1) {
			return false, 0, nil, errors.New("gateway WebSocket frame is too large")
		}
		length = int64(unsignedLength)
	}
	if opcode >= 0x8 && (!final || length > 125) {
		return false, 0, nil, errors.New("gateway WebSocket control frame is invalid")
	}
	if length < 0 || length > connection.maxMessage {
		return false, 0, nil, fmt.Errorf("gateway WebSocket message exceeds %d bytes", connection.maxMessage)
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(connection.reader, payload); err != nil {
		return false, 0, nil, err
	}
	return final, opcode, payload, nil
}

func (connection *Conn) WriteJSON(value interface{}) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return connection.writeFrame(0x1, payload)
}

func (connection *Conn) WritePing() error {
	return connection.writeFrame(0x9, []byte("edukod"))
}

func (connection *Conn) writeFrame(opcode byte, payload []byte) error {
	if opcode >= 0x8 && len(payload) > 125 {
		return errors.New("WebSocket control payload is too large")
	}
	if int64(len(payload)) > connection.maxMessage {
		return fmt.Errorf("WebSocket message exceeds %d bytes", connection.maxMessage)
	}
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	header := []byte{0x80 | opcode}
	length := len(payload)
	switch {
	case length < 126:
		header = append(header, 0x80|byte(length))
	case length <= 65535:
		header = append(header, 0x80|126, byte(length>>8), byte(length))
	default:
		header = append(header, 0x80|127)
		encodedLength := make([]byte, 8)
		binary.BigEndian.PutUint64(encodedLength, uint64(length))
		header = append(header, encodedLength...)
	}
	header = append(header, mask...)
	maskedPayload := make([]byte, len(payload))
	for index := range payload {
		maskedPayload[index] = payload[index] ^ mask[index%4]
	}
	if err := connection.connection.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return err
	}
	if _, err := connection.connection.Write(append(header, maskedPayload...)); err != nil {
		return err
	}
	return connection.connection.SetWriteDeadline(time.Time{})
}

func (connection *Conn) Close() error {
	var err error
	connection.closeOnce.Do(func() {
		_ = connection.writeFrame(0x8, []byte{0x03, 0xE8})
		err = connection.connection.Close()
	})
	return err
}
