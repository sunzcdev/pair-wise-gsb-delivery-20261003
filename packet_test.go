package milter

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

const testMaxPacketSize uint32 = 16

// pipePair returns both ends of a net.Pipe.
func pipePair(t *testing.T) (client net.Conn, server net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() {
		a.Close()
		b.Close()
	})
	return a, b
}

// writeLenHeader sends the 4-byte big-endian length header one byte at a
// time so oversized frames can be rejected before the payload is sent.
func writeLenHeader(t *testing.T, w net.Conn, length uint32) {
	t.Helper()
	for shift := 24; shift >= 0; shift -= 8 {
		if _, err := w.Write([]byte{byte(length >> uint(shift))}); err != nil {
			t.Fatalf("write length header byte: %v", err)
		}
	}
}

func assertPacketError(t *testing.T, err error, dir Direction, kind PacketErrorKind, declared uint32, consumed int) {
	t.Helper()
	var pe *PacketError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *PacketError, got %T: %v", err, err)
	}
	if pe.Direction != dir {
		t.Errorf("wrong direction: got %v, want %v", pe.Direction, dir)
	}
	if pe.Kind != kind {
		t.Errorf("wrong kind: got %v, want %v", pe.Kind, kind)
	}
	if pe.DeclaredLength != declared {
		t.Errorf("wrong declared length: got %d, want %d", pe.DeclaredLength, declared)
	}
	if pe.Consumed != consumed {
		t.Errorf("wrong consumed bytes: got %d, want %d", pe.Consumed, consumed)
	}
	if bytes.Contains([]byte(pe.Error()), []byte("secret")) {
		t.Errorf("error message must not contain packet payload: %v", pe)
	}
}

// TestReadPacket_FrameCases verifies the four structurally distinct frame
// outcomes: zero length, command-only, over-limit and truncation. Cases are
// exercised for both frame directions.
func TestReadPacket_FrameCases(t *testing.T) {
	directions := []struct {
		name string
		dir  Direction
	}{
		{"MTA->milter", DirectionMTA},
		{"milter->MTA", DirectionMilter},
	}

	for _, d := range directions {
		t.Run(d.name, func(t *testing.T) {
			t.Run("zero length", func(t *testing.T) {
				reader, writer := pipePair(t)
				errc := make(chan error, 1)
				go func() {
					_, err := readPacket(reader, 0, testMaxPacketSize, d.dir)
					errc <- err
				}()

				writeLenHeader(t, writer, 0)
				select {
				case err := <-errc:
					assertPacketError(t, err, d.dir, PacketLengthZero, 0, 4)
				case <-time.After(time.Second):
					t.Fatal("zero-length frame was not rejected")
				}
			})

			t.Run("command only", func(t *testing.T) {
				reader, writer := pipePair(t)
				msgc := make(chan *Message, 1)
				errc := make(chan error, 1)
				go func() {
					msg, err := readPacket(reader, 0, testMaxPacketSize, d.dir)
					msgc <- msg
					errc <- err
				}()

				writeLenHeader(t, writer, 1)
				if _, err := writer.Write([]byte{byte(CodeQuit)}); err != nil {
					t.Fatal(err)
				}
				select {
				case msg := <-msgc:
					if err := <-errc; err != nil {
						t.Fatal(err)
					}
					if msg.Code != byte(CodeQuit) {
						t.Errorf("wrong code: %c", msg.Code)
					}
					if !msg.CommandOnly() {
						t.Errorf("expected command-only message")
					}
				case <-time.After(time.Second):
					t.Fatal("command-only frame not read")
				}
			})

			t.Run("truncated header", func(t *testing.T) {
				reader, writer := pipePair(t)
				errc := make(chan error, 1)
				go func() {
					_, err := readPacket(reader, 0, testMaxPacketSize, d.dir)
					errc <- err
				}()

				// Send two of four header bytes, then close.
				for _, b := range []byte{0x00, 0x01} {
					if _, err := writer.Write([]byte{b}); err != nil {
						t.Fatal(err)
					}
				}
				writer.Close()
				select {
				case err := <-errc:
					assertPacketError(t, err, d.dir, PacketTruncated, 0, 2)
				case <-time.After(time.Second):
					t.Fatal("truncated header not detected")
				}
			})

			t.Run("truncated payload", func(t *testing.T) {
				reader, writer := pipePair(t)
				errc := make(chan error, 1)
				go func() {
					_, err := readPacket(reader, 0, testMaxPacketSize, d.dir)
					errc <- err
				}()

				// Advertise a 6-byte packet but send only command + 1
				// payload byte before closing.
				writeLenHeader(t, writer, 6)
				if _, err := writer.Write([]byte{byte(CodeBody), 's'}); err != nil {
					t.Fatal(err)
				}
				writer.Close()
				select {
				case err := <-errc:
					assertPacketError(t, err, d.dir, PacketTruncated, 6, 6)
				case <-time.After(time.Second):
					t.Fatal("truncated payload not detected")
				}
			})

			t.Run("oversized rejected without payload", func(t *testing.T) {
				reader, writer := pipePair(t)
				errc := make(chan error, 1)
				go func() {
					_, err := readPacket(reader, 0, testMaxPacketSize, d.dir)
					errc <- err
				}()

				// Send the length header byte by byte only; the reader
				// must reject without receiving a single payload byte.
				writeLenHeader(t, writer, testMaxPacketSize+1)
				select {
				case err := <-errc:
					assertPacketError(t, err, d.dir, PacketTooLarge, testMaxPacketSize+1, 4)
				case <-time.After(time.Second):
					t.Fatal("oversized frame not rejected after header")
				}

				// A payload write must not be consumed by the reader; the
				// connection is expected to have been abandoned/closed.
				done := make(chan struct{})
				go func() {
					writer.Write([]byte("PAYLOAD-NEVER-READ"))
					close(done)
				}()
				select {
				case <-done:
					// Write completed because the peer closed the
					// connection; either way the payload was never read.
				case <-time.After(time.Second):
				}
			})
		})
	}
}

func startFrameTestServer(t *testing.T, maxPacketSize uint32) string {
	t.Helper()
	s := &Server{
		NewMilter:     func() Milter { return NoOpMilter{} },
		MaxPacketSize: maxPacketSize,
	}
	t.Cleanup(func() { s.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	return ln.Addr().String()
}

// sendRawFrame dials the server without negotiating and writes a single raw
// frame. The connection is closed afterwards; malformed frames must simply
// result in the server dropping the connection (never panicking).
func sendRawFrame(t *testing.T, addr string, frame []byte) error {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(frame); err != nil {
		return err
	}
	return nil
}

// TestServer_MalformedFrames verifies illegal frames neither panic the
// server goroutine nor tear down the listener: a later connection still
// completes negotiation.
func TestServer_MalformedFrames(t *testing.T) {
	addr := startFrameTestServer(t, testMaxPacketSize)

	frames := map[string][]byte{
		"zero length":         {0x00, 0x00, 0x00, 0x00},
		"oversized header":    {0x00, 0x00, 0x00, byte(testMaxPacketSize + 1)},
		"truncated header":    {0x00, 0x00},
		"truncated payload":   {0x00, 0x00, 0x00, 0x06, byte(CodeBody), 's'},
		"command only quit":   {0x00, 0x00, 0x00, 0x01, byte(CodeQuit)},
		"short connect":       {0x00, 0x00, 0x00, 0x01, byte(CodeConn)},
		"connect missing fam": {0x00, 0x00, 0x00, 0x03, byte(CodeConn), 'h', 0x00},
	}

	for name, frame := range frames {
		t.Run(name, func(t *testing.T) {
			if err := sendRawFrame(t, addr, frame); err != nil {
				t.Fatalf("raw frame exchange: %v", err)
			}
		})
	}

	// The server must still negotiate a clean connection afterwards.
	cl := NewClientWithOptions("tcp", addr, ClientOptions{
		ActionMask:    OptAddHeader,
		MaxPacketSize: testMaxPacketSize,
	})
	defer cl.Close()
	session, err := cl.Session()
	if err != nil {
		t.Fatalf("server did not survive malformed frames: %v", err)
	}
	defer session.Close()
}

type pipeDialer struct{ conn net.Conn }

func (d *pipeDialer) Dial(network, addr string) (net.Conn, error) {
	return d.conn, nil
}

// TestClient_OversizedOptNeg verifies the client side enforces the same
// limit when the milter advertises an oversized option negotiation packet;
// the client rejects it after reading only the length header.
func TestClient_OversizedOptNeg(t *testing.T) {
	clientConn, milterConn := pipePair(t)

	cl := NewClientWithOptions("pipe", "x", ClientOptions{
		Dialer:        &pipeDialer{conn: clientConn},
		ActionMask:    OptAddHeader,
		ReadTimeout:   time.Second,
		WriteTimeout:  time.Second,
		MaxPacketSize: testMaxPacketSize,
	})

	sessionErr := make(chan error, 1)
	go func() {
		_, err := cl.Session()
		sessionErr <- err
	}()

	// Read the client's OPTNEG frame.
	milterConn.SetDeadline(time.Now().Add(2 * time.Second))
	header := make([]byte, 4)
	if _, err := io.ReadFull(milterConn, header); err != nil {
		t.Fatal(err)
	}
	clientLen := int(header[0])<<24 | int(header[1])<<16 | int(header[2])<<8 | int(header[3])
	if _, err := io.ReadFull(milterConn, make([]byte, clientLen)); err != nil {
		t.Fatal(err)
	}

	// Reply with an oversized length header, one byte at a time, no body.
	writeLenHeader(t, milterConn, testMaxPacketSize+1)

	select {
	case err := <-sessionErr:
		if err == nil {
			t.Fatal("expected negotiate error for oversized response")
		}
		var pe *PacketError
		if !errors.As(err, &pe) {
			t.Fatalf("expected *PacketError wrapped in negotiate error, got %v", err)
		}
		if pe.Direction != DirectionMilter {
			t.Errorf("wrong direction: %v", pe.Direction)
		}
		if pe.Kind != PacketTooLarge || pe.Consumed != 4 || pe.DeclaredLength != testMaxPacketSize+1 {
			t.Errorf("wrong packet error: %+v", pe)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("oversized optneg response not rejected")
	}
}

// TestClient_BodyChunkBusinessLimit verifies the business-level per-chunk
// body limit stays independent from the framing packet size limit: chunks
// larger than MaxBodyChunk are refused without any network I/O.
func TestClient_BodyChunkBusinessLimit(t *testing.T) {
	clientConn, milterConn := pipePair(t)
	session := &ClientSession{
		conn:          clientConn,
		readTimeout:   time.Second,
		writeTimeout:  time.Second,
		maxPacketSize: DefaultMaxPacketSize,
	}

	if _, err := session.BodyChunk(make([]byte, MaxBodyChunk+1)); err == nil {
		t.Fatal("expected error for over-sized body chunk")
	}

	// A chunk at the business limit passes the local check. Drain the frame
	// on the fake-milter side and close so the read fails with clean EOF
	// instead of blocking forever.
	go func() {
		hdr := make([]byte, 4)
		if _, err := io.ReadFull(milterConn, hdr); err != nil {
			return
		}
		n := int(hdr[0])<<24 | int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
		io.ReadFull(milterConn, make([]byte, n))
		milterConn.Close()
	}()

	if _, err := session.BodyChunk(make([]byte, MaxBodyChunk)); !errors.Is(err, io.EOF) {
		t.Fatalf("expected chunk at business limit to pass local check, got %v", err)
	}
}
