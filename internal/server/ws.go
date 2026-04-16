package server

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
)

// wsHandler upgrades the connection to WebSocket (RFC 6455, no external dep)
// and calls fn with a context and a send function.
// fn should write lines until ctx is done.
func wsHandler(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, send func(string))) {
	key := r.Header.Get("Sec-Websocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-Websocket-Key", 400)
		return
	}

	// Compute accept key per RFC 6455.
	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", 500)
		return
	}

	conn, rw, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, "hijack failed", 500)
		return
	}
	defer conn.Close()

	// Send 101 Switching Protocols.
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\n")
	fmt.Fprintf(rw, "Upgrade: websocket\r\n")
	fmt.Fprintf(rw, "Connection: Upgrade\r\n")
	fmt.Fprintf(rw, "Sec-WebSocket-Accept: %s\r\n", accept)
	fmt.Fprintf(rw, "\r\n")
	rw.Flush()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Cancel when client disconnects.
	go func() {
		buf := make([]byte, 1)
		conn.Read(buf) //nolint:errcheck
		cancel()
	}()

	send := func(line string) {
		writeWSTextFrame(conn, rw, line)
	}

	fn(ctx, send)
}

// writeWSTextFrame writes a single WebSocket text frame containing msg.
func writeWSTextFrame(conn net.Conn, rw *bufio.ReadWriter, msg string) {
	payload := []byte(msg)
	n := len(payload)

	var header []byte
	header = append(header, 0x81) // FIN + text opcode

	if n < 126 {
		header = append(header, byte(n))
	} else if n < 65536 {
		header = append(header, 126, byte(n>>8), byte(n))
	} else {
		header = append(header, 127,
			byte(n>>56), byte(n>>48), byte(n>>40), byte(n>>32),
			byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}

	rw.Write(header)
	rw.Write(payload)
	rw.Flush()
}
