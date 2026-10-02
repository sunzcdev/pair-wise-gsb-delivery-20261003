package milter

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// writeLen writes a four-byte big-endian length header.
func writeLen(w io.Writer, n int) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(n))
	w.Write(hdr[:])
}

// writeLenByteByByte sends the length header one byte at a time, waiting for
// each byte to be consumed before sending the next one. With net.Pipe this
// proves the reader parses the length incrementally and enforces the limit
// from the header alone.
func writeLenByteByByte(w io.Writer, n int) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(n))
	for _, b := range hdr {
		if _, err := w.Write([]byte{b}); err != nil {
			return
		}
	}
}

func asFrameError(t *testing.T, err error) *FrameError {
	t.Helper()
	var fe *FrameError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *FrameError, got %T: %v", err, err)
	}
	return fe
}

func TestReadPacket_StructuredResults(t *testing.T) {
	const maxSize = 16

	cases := []struct {
		name string
		// write frames bytes to the peer end.
		write func(w net.Conn)
		// expectErr is the expected error kind; zero means success.
		expectErr FrameErrorKind
		// consumed is the expected number of on-wire bytes consumed.
		consumed int
		// halfClose shuts the write side down after writing so a blocked
		// writer on net.Pipe can finish and the reader observes EOF.
		halfClose bool
	}{
		{
			name: "zero length",
			write: func(w net.Conn) {
				writeLen(w, 0)
			},
			expectErr: FrameZeroLength,
			consumed:  4,
		},
		{
			name: "command byte only is valid",
			write: func(w net.Conn) {
				writeLen(w, 1)
				w.Write([]byte{byte(CodeQuit)})
			},
			consumed: 0,
		},
		{
			name: "over limit",
			write: func(w net.Conn) {
				writeLen(w, maxSize+1)
			},
			expectErr: FrameTooLarge,
			consumed:  4,
		},
		{
			name: "truncated length header",
			write: func(w net.Conn) {
				w.Write([]byte{0x00, 0x00})
			},
			expectErr: FrameIOError,
			consumed:  2,
			halfClose: true,
		},
		{
			name: "truncated mid payload",
			write: func(w net.Conn) {
				writeLen(w, 5)
				w.Write([]byte{byte(CodeBody), 'a', 'b'})
			},
			expectErr: FrameIOError,
			consumed:  7,
			halfClose: true,
		},
		{
			name: "valid frame",
			write: func(w net.Conn) {
				writeLen(w, 4)
				w.Write([]byte{byte(CodeBody), 'x', 'y', 'z'})
			},
			consumed: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()

			go func() {
				tc.write(server)
				if tc.halfClose {
					if cw, ok := server.(interface{ CloseWrite() error }); ok {
						cw.CloseWrite()
					} else {
						server.Close()
					}
				}
			}()

			msg, err := readPacket(client, time.Second, maxSize)

			if tc.expectErr != 0 {
				if err == nil {
					t.Fatal("expected error, got message")
				}
				fe := asFrameError(t, err)
				if fe.Kind != tc.expectErr {
					t.Fatalf("expected kind %v, got %v", tc.expectErr, fe.Kind)
				}
				if fe.Direction != DirRecv {
					t.Fatalf("expected recv direction, got %v", fe.Direction)
				}
				if fe.Consumed != tc.consumed {
					t.Fatalf("expected %d consumed bytes, got %d", tc.consumed, fe.Consumed)
				}
				// Payload must never appear in the structured error.
				if strings.Contains(err.Error(), "xyz") {
					t.Fatalf("error string leaked payload: %v", err)
				}
				if tc.halfClose && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("expected errors.Is(io.ErrUnexpectedEOF), got %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if msg == nil {
					t.Fatal("expected message")
				}
			}
		})
	}
}

func TestReadPacket_DefaultLimit(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	go func() {
		writeLen(server, DefaultMaxPacketSize+1)
	}()

	_, err := readPacket(client, time.Second, 0)
	fe := asFrameError(t, err)
	if fe.Kind != FrameTooLarge {
		t.Fatalf("expected FrameTooLarge, got %v", fe.Kind)
	}
	if fe.MaxSize != DefaultMaxPacketSize {
		t.Fatalf("expected default max size %d, got %d", DefaultMaxPacketSize, fe.MaxSize)
	}
	if fe.Declared != DefaultMaxPacketSize+1 {
		t.Fatalf("expected declared %d, got %d", DefaultMaxPacketSize+1, fe.Declared)
	}
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("errors.Is(ErrFrameTooLarge) false for %v", err)
	}
}

func TestReadPacket_CommandOnlyFrameContents(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	go func() {
		writeLen(server, 1)
		server.Write([]byte{byte(CodeEOB)})
	}()

	msg, err := readPacket(client, time.Second, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if Code(msg.Code) != CodeEOB {
		t.Fatalf("expected EOB code, got %q", msg.Code)
	}
	if len(msg.Data) != 0 {
		t.Fatalf("expected empty payload, got %q", msg.Data)
	}
}

// TestProcess_MalformedFramesNoPanic feeds every malformed frame shape to the
// command processor directly and asserts none of them panics.
func TestProcess_MalformedFramesNoPanic(t *testing.T) {
	session := &milterSession{
		backend: NoOpMilter{},
	}

	cases := []*Message{
		{Code: byte(CodeConn)},
		{Code: byte(CodeConn), Data: []byte{'h', 'o', 's', 't', 0, '4'}},
		{Code: byte(CodeMacro)},
		{Code: byte(CodeMacro), Data: []byte{byte(CodeHelo)}},
		{Code: byte(CodeBody)},
		{Code: byte(CodeHeader)},
		{Code: byte(CodeHelo)},
		{Code: 0x00},
	}

	for _, msg := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Process panicked on code %q data %q: %v", msg.Code, msg.Data, r)
				}
			}()
			_, _ = session.Process(msg)
		}()
	}
}

// TestServer_OverSizeRejectedBeforeBody sends the length header one byte at
// a time over net.Pipe and declares an oversize body frame. The limit must be
// rejected as soon as the fourth header byte is parsed, without the client
// having sent a single payload byte.
func TestServer_OverSizeRejectedBeforeBody(t *testing.T) {
	mm := &MockMilter{BodyChunkResp: RespContinue}
	ln := netPipeListener(t)

	s := &Server{
		NewMilter:     func() Milter { return mm },
		Actions:       OptAddHeader,
		MaxPacketSize: 16,
	}
	defer s.Close()
	go s.Serve(ln)

	conn, err := ln.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := clientHandshake(t, conn); err != nil {
		t.Fatal(err)
	}

	// Declare an oversize BODY frame, pushing the length header across one
	// byte at a time. Each Write on net.Pipe blocks until the server consumes
	// that byte, so returning from the fourth byte means the server has
	// parsed the full length.
	writeLenByteByByte(conn, 17)

	// The server rejects immediately on the length header, so it never reads
	// the payload. net.Pipe fails the write once the server closes its end.
	n, werr := conn.Write(bytes.Repeat([]byte{'Z'}, 16))
	if werr == nil {
		t.Fatal("expected payload write to fail: server consumed oversize body")
	}
	if n != 0 {
		t.Fatalf("expected 0 payload bytes consumed, %d were read", n)
	}

	// The server closes the connection after a frame error.
	buf := make([]byte, 16)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected connection to be closed after oversize frame")
	}

	if len(mm.Chunks) != 0 {
		t.Fatalf("backend received %d body chunks from oversize frame", len(mm.Chunks))
	}
}

// TestServer_MalformedFramesNoPanic drives the full server loop over
// net.Pipe with every malformed frame shape and asserts the connection is
// torn down cleanly without a panic.
func TestServer_MalformedFramesNoPanic(t *testing.T) {
	cases := []struct {
		name  string
		frame func(w net.Conn)
		// closeConn means the peer fully closes after writing; halfClose
		// means only the write side is shut down so EOF is signalled while
		// reads stay possible. expectResponse indicates the server answers
		// with a frame instead of dropping the connection.
		halfClose      bool
		closeConn      bool
		expectResponse bool
	}{
		{"zero length", func(w net.Conn) { writeLen(w, 0) }, false, true, false},
		{"truncated header", func(w net.Conn) {
			w.Write([]byte{0x00})
		}, true, false, false},
		{"truncated payload", func(w net.Conn) {
			writeLen(w, 4)
			w.Write([]byte{byte(CodeBody), 'a'})
		}, true, false, false},
		{"command-only connect", func(w net.Conn) {
			writeLen(w, 1)
			w.Write([]byte{byte(CodeConn)})
		}, false, false, true},
		{"command-only macro", func(w net.Conn) {
			writeLen(w, 1)
			w.Write([]byte{byte(CodeMacro)})
			writeLen(w, 1)
			w.Write([]byte{byte(CodeQuit)})
		}, false, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mm := &MockMilter{}
			ln := netPipeListener(t)
			s := &Server{
				NewMilter:     func() Milter { return mm },
				Actions:       0,
				MaxPacketSize: 16,
			}
			defer s.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.Serve(ln)
			}()

			conn, err := ln.Dial()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := clientHandshake(t, conn); err != nil {
				t.Fatal(err)
			}

			if tc.halfClose {
				// Write from a goroutine: on net.Pipe the final Write blocks
				// until the server reads it, after which the goroutine can
				// half-close and unblock the server's next read with EOF.
				go func() {
					tc.frame(conn)
					if cw, ok := conn.(interface{ CloseWrite() error }); ok {
						cw.CloseWrite()
					} else {
						conn.Close()
					}
				}()
			} else {
				tc.frame(conn)
				if tc.closeConn {
					conn.Close()
				}
			}

			if tc.expectResponse {
				// A command-only CONNECT is malformed content, not a frame
				// error: the server answers TEMPFAIL and keeps going.
				var hdr [4]byte
				if _, err := io.ReadFull(conn, hdr[:]); err != nil {
					t.Fatalf("expected response frame: %v", err)
				}
				n := int(binary.BigEndian.Uint32(hdr[:]))
				resp := make([]byte, n)
				if _, err := io.ReadFull(conn, resp); err != nil {
					t.Fatalf("expected response payload: %v", err)
				}
				if ActionCode(resp[0]) != ActTempFail {
					t.Fatalf("expected tempfail, got %q", resp[0])
				}
			} else {
				// The server must close the connection rather than hang.
				buf := make([]byte, 8)
				if _, err := conn.Read(buf); err == nil {
					t.Fatal("expected server to close connection on malformed frame")
				}
			}
			conn.Close()

			// If Serve panicked, closing the listener below can still
			// succeed; the real panic guarantee is checked separately in
			// TestProcess_MalformedFramesNoPanic. Ensure no chunk landed.
			if len(mm.Chunks) != 0 {
				t.Fatalf("backend received chunks: %d", len(mm.Chunks))
			}
			s.Close()
			<-done
		})
	}
}

// TestServer_SameLimitForOptNegMacroAndBody verifies option negotiation,
// macro definition and body frames all share the single read limit: a frame
// over the limit is rejected regardless of its command code.
func TestServer_SameLimitForOptNegMacroAndBody(t *testing.T) {
	// A limit below the 13-byte option negotiation frame rejects optneg too.
	ln := netPipeListener(t)
	s := &Server{
		NewMilter:     func() Milter { return &MockMilter{} },
		MaxPacketSize: 8,
	}
	defer s.Close()
	go s.Serve(ln)

	conn, err := ln.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	errc := make(chan error, 1)
	go func() {
		writeLen(conn, 13)
		_, err := conn.Write(bytes.Repeat([]byte{0}, 13))
		errc <- err
	}()

	buf := make([]byte, 16)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected connection closed for oversize option-negotiation frame")
	}
	<-errc
}

// rawOptNegRequest writes a client option-negotiation frame: command byte 'O'
// followed by three zero u32 values (13 bytes on the wire).
func rawOptNegRequest(w io.Writer) error {
	payload := make([]byte, 13)
	payload[0] = byte(CodeOptNeg)
	writeLen(w, 13)
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return nil
}

func clientHandshake(t *testing.T, conn net.Conn) (int, error) {
	t.Helper()
	// Client option negotiation: version 2, no actions/protocols.
	if err := rawOptNegRequest(conn); err != nil {
		return 0, err
	}
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return 0, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if _, err := io.ReadFull(conn, make([]byte, n)); err != nil {
		return 0, err
	}
	if n < 1 {
		return 0, errInvalidOptNeg
	}
	return int(n), nil
}

var errInvalidOptNeg = errors.New("test: empty option-negotiation response")

// pipeListener is an in-memory net.Listener producing net.Pipe connections.
type pipeListener struct {
	ch   chan net.Conn
	done chan struct{}
}

func netPipeListener(t *testing.T) *pipeListener {
	t.Helper()
	return &pipeListener{
		ch:   make(chan net.Conn),
		done: make(chan struct{}),
	}
}

// Dial creates a new in-memory connection pair and offers one end to the
// server's Accept loop.
func (l *pipeListener) Dial() (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.ch <- server:
		return client, nil
	case <-l.done:
		client.Close()
		server.Close()
		return nil, ErrServerClosed
	}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, ErrServerClosed
	}
}

func (l *pipeListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

// pipeDialer returns a pre-created connection on Dial, used to connect a
// Client to a hand-rolled peer over net.Pipe.
type pipeDialer struct {
	conn net.Conn
}

func (d pipeDialer) Dial(network, addr string) (net.Conn, error) {
	return d.conn, nil
}

// readFrame consumes one frame from the peer, used by the hand-rolled milter
// server in client tests.
func readFrame(t *testing.T, r io.Reader) []byte {
	t.Helper()
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		t.Fatalf("peer: reading frame header: %v", err)
	}
	n := int(binary.BigEndian.Uint32(hdr[:]))
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("peer: reading frame payload: %v", err)
	}
	return buf
}

func writeOptNegResponse(w io.Writer) {
	msg := make([]byte, 13)
	msg[0] = byte(CodeOptNeg)
	binary.BigEndian.PutUint32(msg[1:], 2)
	writeLen(w, 13)
	w.Write(msg)
}

// TestClient_OverSizeResponseRejectedBeforeBody verifies the client rejects a
// declared-oversize response from the four-byte header alone. The peer sends
// the header one byte at a time over net.Pipe and never sends any payload;
// the client must still report a structured FrameTooLarge.
func TestClient_OverSizeResponseRejectedBeforeBody(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	cl := NewClientWithOptions("pipe", "", ClientOptions{
		Dialer:        pipeDialer{conn: clientConn},
		ReadTimeout:   time.Second,
		WriteTimeout:  time.Second,
		MaxPacketSize: 16,
	})

	rejected := make(chan struct{})
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		// Option negotiation.
		readFrame(t, serverConn)
		writeOptNegResponse(serverConn)
		// Wait for EOB.
		readFrame(t, serverConn)
		// Declare an oversize response, byte by byte, send no payload.
		writeLenByteByByte(serverConn, 100000)
		select {
		case <-rejected:
		case <-time.After(3 * time.Second):
			t.Error("peer: client did not reject oversize header on its own")
		}
	}()

	session, err := cl.Session()
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = session.End()
	fe := asFrameError(t, err)
	if fe.Kind != FrameTooLarge {
		t.Fatalf("expected FrameTooLarge, got %v", fe.Kind)
	}
	if fe.Direction != DirRecv {
		t.Fatalf("expected recv direction, got %v", fe.Direction)
	}
	if fe.Consumed != 4 {
		t.Fatalf("expected 4 consumed bytes (header only), got %d", fe.Consumed)
	}
	if fe.Declared != 100000 {
		t.Fatalf("expected declared 100000, got %d", fe.Declared)
	}

	close(rejected)
	clientConn.Close()
	<-peerDone
}

// TestClient_TruncatedResponseNoLeak verifies a mid-payload truncation yields
// a structured I/O error whose text never contains payload (which may be
// message body).
func TestClient_TruncatedResponseNoLeak(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	cl := NewClientWithOptions("pipe", "", ClientOptions{
		Dialer:       pipeDialer{conn: clientConn},
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	})

	go func() {
		readFrame(t, serverConn)
		writeOptNegResponse(serverConn)
		readFrame(t, serverConn)
		// Declare 10 bytes, send only 3 carrying a secret, then EOF.
		writeLen(serverConn, 10)
		serverConn.Write([]byte("SECRET"[:3]))
		if cw, ok := serverConn.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			serverConn.Close()
		}
	}()

	session, err := cl.Session()
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = session.End()
	fe := asFrameError(t, err)
	if fe.Kind != FrameIOError {
		t.Fatalf("expected FrameIOError, got %v", fe.Kind)
	}
	if fe.Direction != DirRecv {
		t.Fatalf("expected recv direction, got %v", fe.Direction)
	}
	if fe.Consumed != 4+3 {
		t.Fatalf("expected 7 consumed bytes, got %d", fe.Consumed)
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "SEC") {
		t.Fatalf("error string leaked payload: %v", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected errors.Is(io.ErrUnexpectedEOF), got %v", err)
	}
}

// TestClient_BodyChunkCapIndependent confirms the protocol-level body chunk
// cap stays enforced independently of the frame size limit.
func TestClient_BodyChunkCapIndependent(t *testing.T) {
	if DefaultMaxPacketSize != MaxBodyChunk+1 {
		t.Fatalf("default packet size %d must fit a maximal body chunk %d", DefaultMaxPacketSize, MaxBodyChunk+1)
	}

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	cl := NewClientWithOptions("pipe", "", ClientOptions{
		Dialer:        pipeDialer{conn: clientConn},
		ReadTimeout:   5 * time.Second,
		WriteTimeout:  5 * time.Second,
		MaxPacketSize: DefaultMaxPacketSize,
	})

	go func() {
		readFrame(t, serverConn)
		writeOptNegResponse(serverConn)
		// Accept a maximal-size body chunk and reply CONTINUE.
		chunk := readFrame(t, serverConn)
		if len(chunk) != MaxBodyChunk+1 {
			t.Errorf("expected %d-byte frame, got %d", MaxBodyChunk+1, len(chunk))
		}
		writeLen(serverConn, 1)
		serverConn.Write([]byte{byte(ActContinue)})
	}()

	session, err := cl.Session()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := session.BodyChunk(make([]byte, MaxBodyChunk+1)); err == nil {
		t.Fatal("expected error for chunk above MaxBodyChunk")
	}

	if act, err := session.BodyChunk(make([]byte, MaxBodyChunk)); err != nil {
		t.Fatalf("maximal body chunk rejected: %v", err)
	} else if act.Code != ActContinue {
		t.Fatalf("expected continue, got %q", act.Code)
	}
}
