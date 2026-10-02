package websocket

import (
	"bufio"
	"bytes"
	"compress/flate"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// similarJSONMessages returns n JSON messages where only a few fields change
// from message to message, typical of long lived connection push traffic.
func similarJSONMessages(n int) [][]byte {
	messages := make([][]byte, n)
	for i := 0; i < n; i++ {
		messages[i] = []byte(fmt.Sprintf(
			`{"id":%d,"type":"push","from":"alice","to":"bob","payload":"the quick brown fox jumps over the lazy dog","seq":%d}`,
			1000+i, i))
	}
	return messages
}

func compressMessages(t *testing.T, newWriter func(io.WriteCloser, int) io.WriteCloser, messages [][]byte) [][]byte {
	t.Helper()
	var payloads [][]byte
	for _, m := range messages {
		var buf bytes.Buffer
		w := newWriter(nopCloser{&buf}, defaultCompressionLevel)
		if _, err := w.Write(m); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		payloads = append(payloads, buf.Bytes())
	}
	return payloads
}

func TestContextTakeoverCompressionRatio(t *testing.T) {
	messages := similarJSONMessages(32)

	takeoverPayloads := compressMessages(t, compressContextTakeover(), messages)
	noTakeoverPayloads := compressMessages(t, compressNoContextTakeover, messages)

	takeoverSize, noTakeoverSize := 0, 0
	for i := range messages {
		takeoverSize += len(takeoverPayloads[i])
		noTakeoverSize += len(noTakeoverPayloads[i])
	}
	t.Logf("context takeover: %d bytes, no context takeover: %d bytes", takeoverSize, noTakeoverSize)
	if takeoverSize >= noTakeoverSize/2 {
		t.Errorf("context takeover size %d, want less than half of no context takeover size %d", takeoverSize, noTakeoverSize)
	}

	// An independently implemented peer can decode the messages by
	// concatenating the payloads (each terminated with the four bytes
	// stripped by the sender) into a single deflate stream.
	var stream bytes.Buffer
	for _, p := range takeoverPayloads {
		stream.Write(p)
		stream.WriteString("\x00\x00\xff\xff")
	}
	// Terminate the stream with a final block.
	stream.WriteString("\x01\x00\x00\xff\xff")
	r := flate.NewReader(&stream)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("independent decode: %v", err)
	}
	if want := bytes.Join(messages, nil); !bytes.Equal(got, want) {
		t.Errorf("independent decode mismatch\ngot:  %q\nwant: %q", got, want)
	}
}

func TestContextTakeoverRoundTrip(t *testing.T) {
	messages := similarJSONMessages(8)
	// Interleave empty messages.
	messages = append(messages[:4], append([][]byte{{}, {}}, messages[4:]...)...)

	newReader := decompressContextTakeover()
	for i, payload := range compressMessages(t, compressContextTakeover(), messages) {
		r := newReader(bytes.NewReader(payload))
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("message %d: read: %v", i, err)
		}
		// The reader is closed preemptively at EOF, so Close may return
		// io.ErrClosedPipe here.
		_ = r.Close()
		if !bytes.Equal(got, messages[i]) {
			t.Errorf("message %d: got %q, want %q", i, got, messages[i])
		}
	}
}

func TestContextTakeoverPartialRead(t *testing.T) {
	// The dictionary must stay in sync with the peer even when the
	// application does not read a message to the end.
	messages := similarJSONMessages(4)
	payloads := compressMessages(t, compressContextTakeover(), messages)

	newReader := decompressContextTakeover()
	for i, payload := range payloads {
		r := newReader(bytes.NewReader(payload))
		if i == 1 {
			// Read only part of the message, then close.
			var buf [8]byte
			if _, err := r.Read(buf[:]); err != nil {
				t.Fatalf("message %d: read: %v", i, err)
			}
			if err := r.Close(); err != nil {
				t.Fatalf("message %d: close: %v", i, err)
			}
			continue
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("message %d: read: %v", i, err)
		}
		if !bytes.Equal(got, messages[i]) {
			t.Errorf("message %d: got %q, want %q", i, got, messages[i])
		}
	}
}

func TestContextTakeoverDecodeIndependentEncoder(t *testing.T) {
	// An independently implemented peer encodes messages with a single
	// flate.Writer, flushing (and stripping the four tail bytes) between
	// messages.
	messages := similarJSONMessages(8)
	messages = append(messages[:2], append([][]byte{{}}, messages[2:]...)...)

	var stream bytes.Buffer
	fw, err := flate.NewWriter(&stream, defaultCompressionLevel)
	if err != nil {
		t.Fatal(err)
	}
	var payloads [][]byte
	for _, m := range messages {
		if _, err := fw.Write(m); err != nil {
			t.Fatal(err)
		}
		if err := fw.Flush(); err != nil {
			t.Fatal(err)
		}
		b := stream.Bytes()
		if !bytes.HasSuffix(b, []byte("\x00\x00\xff\xff")) {
			t.Fatalf("stream does not end with sync flush marker: %x", b)
		}
		payloads = append(payloads, append([]byte(nil), b[:len(b)-4]...))
		stream.Reset()
	}

	newReader := decompressContextTakeover()
	for i, payload := range payloads {
		got, err := io.ReadAll(newReader(bytes.NewReader(payload)))
		if err != nil {
			t.Fatalf("message %d: read: %v", i, err)
		}
		if !bytes.Equal(got, messages[i]) {
			t.Errorf("message %d: got %q, want %q", i, got, messages[i])
		}
	}
}

func TestContextTakeoverFreshStatePerConnection(t *testing.T) {
	messages := similarJSONMessages(8)

	firstMessageSize := func() int {
		payloads := compressMessages(t, compressContextTakeover(), messages[:1])
		return len(payloads[0])
	}

	// Simulate a connection that compressed many messages.
	compressMessages(t, compressContextTakeover(), messages)

	// A new connection (new compressor state) must not benefit from the
	// previous connection's dictionary.
	first := firstMessageSize()
	compressMessages(t, compressContextTakeover(), messages)
	if got := firstMessageSize(); got != first {
		t.Errorf("first message size on new connection %d, want %d (fresh dictionary)", got, first)
	}
}

func newEchoServer(t *testing.T, upgrader *Upgrader) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("Upgrade: %v", err)
			return
		}
		defer c.Close()
		for {
			mt, p, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(mt, p); err != nil {
				return
			}
		}
	}))
}

func wsURL(s *httptest.Server) string {
	return "ws" + strings.TrimPrefix(s.URL, "http")
}

func TestContextTakeoverClientServer(t *testing.T) {
	upgrader := Upgrader{
		EnableCompression:                true,
		EnableCompressionContextTakeover: true,
		ReadBufferSize:                   128,
		WriteBufferSize:                  128,
	}
	server := newEchoServer(t, &upgrader)
	defer server.Close()

	dialer := Dialer{
		EnableCompression:                true,
		EnableCompressionContextTakeover: true,
		// Small buffers force messages to be fragmented into multiple
		// frames.
		ReadBufferSize:  128,
		WriteBufferSize: 128,
	}
	ws, resp, err := dialer.Dial(wsURL(server), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer ws.Close()

	if ext := resp.Header.Get("Sec-Websocket-Extensions"); ext != "permessage-deflate" {
		t.Errorf("Sec-Websocket-Extensions = %q, want %q", ext, "permessage-deflate")
	}

	messages := similarJSONMessages(8)
	// Interleave an empty message and a large message that is fragmented
	// into multiple frames.
	large := bytes.Repeat([]byte(`{"key":"value"}`), 512)
	messages = append(messages[:3], append([][]byte{{}, large}, messages[3:]...)...)

	for i, m := range messages {
		if i == 4 {
			// Interleave a ping (the peer responds with a pong).
			if err := ws.WriteControl(PingMessage, []byte("ping"), time.Now().Add(5*time.Second)); err != nil {
				t.Fatalf("ping: %v", err)
			}
		}
		if err := ws.WriteMessage(TextMessage, m); err != nil {
			t.Fatalf("message %d: write: %v", i, err)
		}
		_, got, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("message %d: read: %v", i, err)
		}
		if !bytes.Equal(got, m) {
			t.Errorf("message %d: got %d bytes, want %d bytes", i, len(got), len(m))
		}
	}
}

func TestContextTakeoverNotEnabledWithoutNegotiation(t *testing.T) {
	messages := similarJSONMessages(4)

	run := func(t *testing.T, upgrader *Upgrader, dialer *Dialer, wantExt string) {
		server := newEchoServer(t, upgrader)
		defer server.Close()
		ws, resp, err := dialer.Dial(wsURL(server), nil)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer ws.Close()
		if ext := resp.Header.Get("Sec-Websocket-Extensions"); ext != wantExt {
			t.Errorf("Sec-Websocket-Extensions = %q, want %q", ext, wantExt)
		}
		for i, m := range messages {
			if err := ws.WriteMessage(TextMessage, m); err != nil {
				t.Fatalf("message %d: write: %v", i, err)
			}
			if _, got, err := ws.ReadMessage(); err != nil {
				t.Fatalf("message %d: read: %v", i, err)
			} else if !bytes.Equal(got, m) {
				t.Errorf("message %d: got %q, want %q", i, got, m)
			}
		}
	}

	const noTakeoverExt = "permessage-deflate; server_no_context_takeover; client_no_context_takeover"

	t.Run("ClientTakeoverServerWithout", func(t *testing.T) {
		// The server does not support context takeover: both directions
		// must fall back to no context takeover.
		run(t,
			&Upgrader{EnableCompression: true},
			&Dialer{EnableCompression: true, EnableCompressionContextTakeover: true},
			noTakeoverExt)
	})

	t.Run("ServerTakeoverClientWithout", func(t *testing.T) {
		// The client does not support context takeover: the server must
		// honor the client's "no context takeover" request.
		run(t,
			&Upgrader{EnableCompression: true, EnableCompressionContextTakeover: true},
			&Dialer{EnableCompression: true},
			noTakeoverExt)
	})
}

func TestContextTakeoverServerNegotiation(t *testing.T) {
	handshake := func(t *testing.T, upgrader *Upgrader, offer string) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Logf("Upgrade: %v", err)
				return
			}
			c.Close()
		}))
		defer server.Close()

		conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Extensions: %s\r\n\r\n",
			strings.TrimPrefix(server.URL, "http://"), offer)
		resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
		}
		return resp.Header.Get("Sec-Websocket-Extensions")
	}

	takeoverUpgrader := &Upgrader{EnableCompression: true, EnableCompressionContextTakeover: true}
	for _, tc := range []struct {
		offer string
		want  string
	}{
		{"permessage-deflate", "permessage-deflate"},
		{"permessage-deflate; client_no_context_takeover", "permessage-deflate; client_no_context_takeover"},
		{"permessage-deflate; server_no_context_takeover", "permessage-deflate; server_no_context_takeover"},
		{"permessage-deflate; server_no_context_takeover; client_no_context_takeover",
			"permessage-deflate; server_no_context_takeover; client_no_context_takeover"},
	} {
		if got := handshake(t, takeoverUpgrader, tc.offer); got != tc.want {
			t.Errorf("offer %q: response %q, want %q", tc.offer, got, tc.want)
		}
	}

	// Without EnableCompressionContextTakeover the server must always
	// respond with both "no context takeover" parameters.
	noTakeoverUpgrader := &Upgrader{EnableCompression: true}
	const both = "permessage-deflate; server_no_context_takeover; client_no_context_takeover"
	if got := handshake(t, noTakeoverUpgrader, "permessage-deflate"); got != both {
		t.Errorf("default upgrader: response %q, want %q", got, both)
	}
}

func TestContextTakeoverReconnect(t *testing.T) {
	upgrader := Upgrader{
		EnableCompression:                true,
		EnableCompressionContextTakeover: true,
	}
	server := newEchoServer(t, &upgrader)
	defer server.Close()

	dialer := Dialer{
		EnableCompression:                true,
		EnableCompressionContextTakeover: true,
	}
	messages := similarJSONMessages(6)

	// The dictionary of a closed connection must not leak into the next
	// connection.
	for connIdx := 0; connIdx < 2; connIdx++ {
		ws, resp, err := dialer.Dial(wsURL(server), nil)
		if err != nil {
			t.Fatalf("connection %d: Dial: %v", connIdx, err)
		}
		if ext := resp.Header.Get("Sec-Websocket-Extensions"); ext != "permessage-deflate" {
			t.Errorf("connection %d: Sec-Websocket-Extensions = %q, want %q", connIdx, ext, "permessage-deflate")
		}
		for i, m := range messages {
			if err := ws.WriteMessage(TextMessage, m); err != nil {
				t.Fatalf("connection %d message %d: write: %v", connIdx, i, err)
			}
			if _, got, err := ws.ReadMessage(); err != nil {
				t.Fatalf("connection %d message %d: read: %v", connIdx, i, err)
			} else if !bytes.Equal(got, m) {
				t.Errorf("connection %d message %d: got %q, want %q", connIdx, i, got, m)
			}
		}
		ws.Close()
	}
}
