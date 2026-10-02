package redcon

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// bulkCmd builds an RESP array command with the given argument payloads.
func bulkCmd(args ...[]byte) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n", len(a))
		b.Write(a)
		b.WriteString("\r\n")
	}
	return b.Bytes()
}

func payload(n int, fill byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = fill
	}
	return b
}

// startStreamServer starts a server on a free local TCP address with both a
// regular and a stream handler installed. It returns the address and a
// cleanup func.
func startStreamServer(t *testing.T, maxBulk int64,
	stream func(conn Conn, cmd *StreamCommand),
	regular func(conn Conn, cmd Command),
) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := newServer()
	s.ln = ln
	s.net = "tcp"
	s.laddr = ln.Addr().String()
	s.handler = regular
	s.SetStreamHandler(StreamHandlerFunc(stream), maxBulk)
	closed := make(chan struct{})
	go func() {
		serve(s)
		close(closed)
	}()
	cleanup := func() {
		s.Close()
		ln.Close()
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
		}
	}
	return s.laddr, cleanup
}

// readLine reads a single RESP reply line for simple assertions.
func readReply(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("reading reply: %v", err)
	}
	return line
}

// TestStreamBigArgThenPing puts a large streamed argument and the next PING
// in the same TCP stream. The handler reads the whole argument, replies, and
// the following PING must still be served on the same connection.
func TestStreamBigArgThenPing(t *testing.T) {
	const bigSize = 4 << 20 // 4 MiB
	got := make(chan int64, 1)
	addr, cleanup := startStreamServer(t, 1<<16,
		func(conn Conn, cmd *StreamCommand) {
			if strings.ToLower(string(cmd.Args[0])) != "set" {
				t.Errorf("expected set, got %q", cmd.Args[0])
			}
			if cmd.StreamIndex() != 2 {
				t.Errorf("expected stream index 2, got %d", cmd.StreamIndex())
			}
			if cmd.StreamSize() != bigSize {
				t.Errorf("expected size %d, got %d", bigSize, cmd.StreamSize())
			}
			var sum int64
			buf := make([]byte, 4096)
			for {
				n, err := cmd.Read(buf)
				sum += int64(n)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Errorf("read: %v", err)
					cmd.Close()
					return
				}
			}
			if sum != bigSize {
				t.Errorf("expected %d bytes, got %d", bigSize, sum)
			}
			more, err := cmd.Next()
			if err != nil || more {
				t.Errorf("Next: more=%v err=%v", more, err)
			}
			got <- sum
			conn.WriteString("OK")
		},
		func(conn Conn, cmd Command) {
			switch strings.ToLower(string(cmd.Args[0])) {
			case "ping":
				conn.WriteString("PONG")
			default:
				conn.WriteError("ERR unknown")
			}
		},
	)
	defer cleanup()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	big := payload(bigSize, 'x')
	// Both commands are written into one TCP stream up front.
	if _, err := c.Write(bulkCmd([]byte("SET"), []byte("k"), big)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("PING\r\n")); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(c)
	if got1 := readReply(t, br); got1 != "+OK\r\n" {
		t.Fatalf("expected +OK, got %q", got1)
	}
	if got2 := readReply(t, br); got2 != "+PONG\r\n" {
		t.Fatalf("expected +PONG, got %q", got2)
	}
	select {
	case s := <-got:
		if s != bigSize {
			t.Fatalf("handler got %d bytes", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never finished")
	}
}

// TestStreamRejectDrainContinue rejects the argument halfway through, calls
// Close to bounded-drain the rest of the command, and confirms the following
// pipelined PING is still processed on the same connection.
func TestStreamRejectDrainContinue(t *testing.T) {
	const bigSize = 2 << 20
	addr, cleanup := startStreamServer(t, 1<<16,
		func(conn Conn, cmd *StreamCommand) {
			half := make([]byte, bigSize/2)
			if _, err := io.ReadFull(cmd, half); err != nil {
				t.Errorf("half read: %v", err)
			}
			conn.WriteError("ERR rejected")
			// Bounded discard of the remaining half and complete the
			// command so the connection stays usable.
			if err := cmd.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
		},
		func(conn Conn, cmd Command) {
			if strings.ToLower(string(cmd.Args[0])) == "ping" {
				conn.WriteString("PONG")
			} else {
				conn.WriteError("ERR unknown")
			}
		},
	)
	defer cleanup()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.Write(bulkCmd([]byte("SET"), []byte("k"), payload(bigSize, 'y')))
	c.Write([]byte("PING\r\n"))

	br := bufio.NewReader(c)
	if got := readReply(t, br); got != "-ERR rejected\r\n" {
		t.Fatalf("expected error, got %q", got)
	}
	if got := readReply(t, br); got != "+PONG\r\n" {
		t.Fatalf("expected +PONG after drain, got %q", got)
	}
}

// TestStreamRejectCloseConn covers the other legal option: the handler
// rejects mid-argument and explicitly closes the connection. The server must
// not deadlock and the client must observe the closed connection.
func TestStreamRejectCloseConn(t *testing.T) {
	const bigSize = 2 << 20
	closed := make(chan error, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := newServer()
	s.ln = ln
	s.net = "tcp"
	s.laddr = ln.Addr().String()
	s.handler = func(conn Conn, cmd Command) {}
	s.closed = func(conn Conn, err error) {
		select {
		case closed <- err:
		default:
		}
	}
	s.SetStreamHandler(StreamHandlerFunc(func(conn Conn, cmd *StreamCommand) {
		buf := make([]byte, 1024)
		cmd.Read(buf)
		conn.WriteError("ERR rejected")
		conn.Close()
	}), 1<<16)
	go serve(s)
	defer s.Close()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.Write(bulkCmd([]byte("SET"), []byte("k"), payload(bigSize, 'z')))
	br := bufio.NewReader(c)
	readReply(t, br) // error reply (write happens before close)
	// Connection must be closed promptly.
	done := make(chan struct{})
	go func() {
		io.Copy(io.Discard, c)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("connection was not closed after mid-read reject")
	}
}

// TestStreamShortRead closes the write side halfway through the bulk body.
// The server must surface a short read, close the connection, and never
// execute the handler's success path or a following command.
func TestStreamShortRead(t *testing.T) {
	const bigSize = 1 << 20
	closedErr := make(chan error, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := newServer()
	s.ln = ln
	s.net = "tcp"
	s.laddr = ln.Addr().String()
	s.handler = func(conn Conn, cmd Command) {
		t.Errorf("regular handler must not run for a half command")
	}
	s.closed = func(conn Conn, err error) {
		select {
		case closedErr <- err:
		default:
		}
	}
	s.SetStreamHandler(StreamHandlerFunc(func(conn Conn, cmd *StreamCommand) {
		buf := make([]byte, 4096)
		for {
			n, rerr := cmd.Read(buf)
			_ = n
			if rerr != nil {
				// Expected: unexpected EOF on a short read.
				return
			}
		}
	}), 1<<16)
	go serve(s)
	defer s.Close()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Declare bigSize bytes but send only a fraction, then half-close.
	fmt.Fprintf(c, "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$%d\r\n", bigSize)
	c.Write(payload(bigSize/4, 'q'))
	c.(*net.TCPConn).CloseWrite()

	select {
	case err := <-closedErr:
		if err == nil {
			t.Fatal("expected a non-nil error on short read")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not close the connection on a short read")
	}
}

// TestStreamLengthOverflow sends a bulk length that cannot be satisfied;
// the server must report a protocol error and close rather than hang or
// execute a partial command.
func TestStreamLengthOverflow(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := newServer()
	s.ln = ln
	s.net = "tcp"
	s.laddr = ln.Addr().String()
	s.handler = func(conn Conn, cmd Command) {}
	s.SetStreamHandler(StreamHandlerFunc(func(conn Conn, cmd *StreamCommand) {}), 1<<16)
	go serve(s)
	defer s.Close()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Not a valid int64 (20 digits) -> protocol error.
	io.WriteString(c, "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$99999999999999999999\r\n")
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("expected protocol error reply, got read err: %v", err)
	}
	if !strings.HasPrefix(line, "-ERR Protocol error") {
		t.Fatalf("expected protocol error, got %q", line)
	}
	// Server should close after a protocol error.
	done := make(chan struct{})
	go func() { io.Copy(io.Discard, c); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("connection not closed after length overflow")
	}
}

// TestStreamOptOut verifies that without SetStreamHandler a server keeps the
// original behavior: large bulks are buffered and delivered via Command.Args.
func TestStreamOptOut(t *testing.T) {
	const bigSize = 128 << 10
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan int, 1)
	s := newServer()
	s.ln = ln
	s.net = "tcp"
	s.laddr = ln.Addr().String()
	s.handler = func(conn Conn, cmd Command) {
		if strings.ToLower(string(cmd.Args[0])) == "set" {
			got <- len(cmd.Args[2])
			conn.WriteString("OK")
		}
	}
	go serve(s)
	defer s.Close()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write(bulkCmd([]byte("SET"), []byte("k"), payload(bigSize, 'w')))
	br := bufio.NewReader(c)
	if line := readReply(t, br); line != "+OK\r\n" {
		t.Fatalf("expected +OK, got %q", line)
	}
	select {
	case n := <-got:
		if n != bigSize {
			t.Fatalf("expected %d buffered bytes, got %d", bigSize, n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("regular handler did not receive buffered command")
	}
}

// TestStreamTrailingArgs streams a big value and then reads the smaller
// arguments that follow it in the same command.
func TestStreamTrailingArgs(t *testing.T) {
	const bigSize = 200 << 10
	addr, cleanup := startStreamServer(t, 1<<16,
		func(conn Conn, cmd *StreamCommand) {
			buf := make([]byte, 64<<10)
			var total int64
			for {
				n, err := cmd.Read(buf)
				total += int64(n)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Errorf("read: %v", err)
					cmd.Close()
					return
				}
			}
			if total != bigSize {
				t.Errorf("expected %d, got %d", bigSize, total)
			}
			more, err := cmd.Next()
			if err != nil || more {
				t.Fatalf("Next: more=%v err=%v", more, err)
			}
			// Args 0 (SET), 1 (key) pre-buffered; arg 2 (big) nil; args
			// 3,4 appended by Next.
			if cmd.Total() != 5 || len(cmd.Args) != 5 {
				t.Fatalf("expected 5 args, got total=%d len=%d",
					cmd.Total(), len(cmd.Args))
			}
			if string(cmd.Args[3]) != "EX" || string(cmd.Args[4]) != "100" {
				t.Fatalf("unexpected trailing args: %q %q",
					cmd.Args[3], cmd.Args[4])
			}
			conn.WriteString("OK")
		},
		func(conn Conn, cmd Command) {
			if strings.ToLower(string(cmd.Args[0])) == "ping" {
				conn.WriteString("PONG")
			} else {
				conn.WriteError("ERR unexpected regular command")
			}
		},
	)
	defer cleanup()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write(bulkCmd(
		[]byte("SET"), []byte("k"), payload(bigSize, 'a'),
		[]byte("EX"), []byte("100"),
	))
	br := bufio.NewReader(c)
	if line := readReply(t, br); line != "+OK\r\n" {
		t.Fatalf("expected +OK, got %q", line)
	}
}

// TestStreamPipelinedOrdering sends a small command, a streaming command,
// and another small command all up front, then verifies replies come back in
// order and all commands run on the same connection.
func TestStreamPipelinedOrdering(t *testing.T) {
	const bigSize = 256 << 10
	addr, cleanup := startStreamServer(t, 1<<16,
		func(conn Conn, cmd *StreamCommand) {
			if err := cmd.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
			conn.WriteString("STREAM")
		},
		func(conn Conn, cmd Command) {
			switch strings.ToLower(string(cmd.Args[0])) {
			case "ping":
				conn.WriteString("PONG")
			default:
				conn.WriteError("ERR unknown")
			}
		},
	)
	defer cleanup()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.Write([]byte("*1\r\n$4\r\nPING\r\n"))
	c.Write(bulkCmd([]byte("SET"), []byte("k"), payload(bigSize, 'p')))
	c.Write([]byte("*1\r\n$4\r\nPING\r\n"))

	br := bufio.NewReader(c)
	want := []string{"+PONG\r\n", "+STREAM\r\n", "+PONG\r\n"}
	for i, w := range want {
		if got := readReply(t, br); got != w {
			t.Fatalf("reply %d: expected %q, got %q", i, w, got)
		}
	}
}

// TestStreamMultipleBigArgs streams more than one large argument in one
// command, reading each in turn via Next.
func TestStreamMultipleBigArgs(t *testing.T) {
	const bigSize = 300 << 10
	addr, cleanup := startStreamServer(t, 1<<16,
		func(conn Conn, cmd *StreamCommand) {
			readOne := func(fill byte) {
				var sum int64
				buf := make([]byte, 64<<10)
				for {
					n, err := cmd.Read(buf)
					for k := 0; k < n; k++ {
						if buf[k] != fill {
							t.Errorf("wrong byte in stream at %d", sum)
							return
						}
					}
					sum += int64(n)
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Errorf("read: %v", err)
						return
					}
				}
				if sum != bigSize {
					t.Errorf("expected %d, got %d", bigSize, sum)
				}
			}
			if cmd.StreamIndex() != 1 {
				t.Fatalf("expected index 1, got %d", cmd.StreamIndex())
			}
			readOne('a')
			more, err := cmd.Next()
			if err != nil || !more {
				t.Fatalf("Next: more=%v err=%v", more, err)
			}
			if cmd.StreamIndex() != 2 {
				t.Fatalf("expected index 2, got %d", cmd.StreamIndex())
			}
			readOne('b')
			more, err = cmd.Next()
			if err != nil || more {
				t.Fatalf("Next2: more=%v err=%v", more, err)
			}
			conn.WriteString("OK")
		},
		func(conn Conn, cmd Command) {
			if strings.ToLower(string(cmd.Args[0])) == "ping" {
				conn.WriteString("PONG")
			} else {
				conn.WriteError("ERR unexpected regular command")
			}
		},
	)
	defer cleanup()

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write(bulkCmd(
		[]byte("MSET"),
		payload(bigSize, 'a'),
		payload(bigSize, 'b'),
	))
	c.Write([]byte("*1\r\n$4\r\nPING\r\n"))
	br := bufio.NewReader(c)
	if line := readReply(t, br); line != "+OK\r\n" {
		t.Fatalf("expected +OK, got %q", line)
	}
	if line := readReply(t, br); line != "+PONG\r\n" {
		t.Fatalf("expected +PONG, got %q", line)
	}
}

// TestStreamThresholdBoundary checks that sizes below the threshold use the
// regular path and sizes at/above it stream.
func TestStreamThresholdBoundary(t *testing.T) {
	const threshold = 1 << 16
	streamN := make(chan int, 4)
	regularN := make(chan int, 4)
	addr, cleanup := startStreamServer(t, threshold,
		func(conn Conn, cmd *StreamCommand) {
			if err := cmd.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
			streamN <- 1
			conn.WriteString("STREAM")
		},
		func(conn Conn, cmd Command) {
			if strings.ToLower(string(cmd.Args[0])) == "set" {
				regularN <- len(cmd.Args[2])
			}
			conn.WriteString("REGULAR")
		},
	)
	defer cleanup()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)

	check := func(size int, want string) {
		c.Write(bulkCmd([]byte("SET"), []byte("k"), payload(size, 'x')))
		if got := readReply(t, br); got != "+"+want+"\r\n" {
			t.Fatalf("size %d: expected %s, got %q", size, want, got)
		}
	}
	check(threshold-1, "REGULAR")
	check(threshold, "STREAM")
	check(threshold+1, "STREAM")

	if n := <-regularN; n != threshold-1 {
		t.Fatalf("expected regular arg size %d, got %d", threshold-1, n)
	}
}

// TestReaderStreamUnit exercises the Reader streaming primitives directly,
// including chunked delivery, bounded discard, and boundary preservation for
// the following command.
func TestReaderStreamUnit(t *testing.T) {
	const bigSize = 500_000
	// Build data: big SET + inline PING.
	msg := bulkCmd([]byte("SET"), []byte("k"), payload(bigSize, 's'))
	msg = append(msg, []byte("PING\r\n")...)

	// Deliver a few bytes at a time to force repeated fills.
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for len(msg) > 0 {
			n := 3000
			if n > len(msg) {
				n = len(msg)
			}
			pw.Write(msg[:n])
			msg = msg[n:]
		}
	}()
	defer pr.Close()

	rd := NewReader(pr)
	cmds, scmd, err := rd.readCommands(nil, 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 0 || scmd == nil {
		t.Fatalf("expected one stream command, cmds=%d scmd=%v", len(cmds), scmd)
	}
	if string(scmd.Args[0]) != "SET" {
		t.Fatalf("unexpected args %q", scmd.Args[0])
	}
	// Read half, then discard half.
	half := make([]byte, bigSize/2)
	if _, err := io.ReadFull(scmd, half); err != nil {
		t.Fatal(err)
	}
	if half[0] != 's' {
		t.Fatal("bad payload")
	}
	if err := scmd.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Streaming a large body must not retain a proportionally large
	// internal buffer.
	if cap(rd.buf) > streamChunkSize*2 {
		t.Fatalf("reader buffer grew to %d while streaming", cap(rd.buf))
	}
	// The following inline PING must remain intact and parse normally.
	pcmd, err := rd.ReadCommand()
	if err != nil {
		t.Fatalf("read ping: %v", err)
	}
	if string(pcmd.Args[0]) != "PING" {
		t.Fatalf("expected PING, got %q", pcmd.Args[0])
	}
}

// TestStreamAbandonedMustClose verifies that if a handler returns while the
// streamed argument is only partially read (and Close was not called), the
// server closes the connection instead of continuing with a half command.
func TestStreamAbandonedMustClose(t *testing.T) {
	const bigSize = 1 << 20
	closeErr := make(chan error, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := newServer()
	s.ln = ln
	s.net = "tcp"
	s.laddr = ln.Addr().String()
	s.handler = func(conn Conn, cmd Command) {}
	s.closed = func(conn Conn, err error) {
		select {
		case closeErr <- err:
		default:
		}
	}
	s.SetStreamHandler(StreamHandlerFunc(func(conn Conn, cmd *StreamCommand) {
		// Read a little then return without consuming or Close.
		buf := make([]byte, 1024)
		cmd.Read(buf)
		conn.WriteError("ERR gave up")
	}), 1<<16)
	go serve(s)
	defer s.Close()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write(bulkCmd([]byte("SET"), []byte("k"), payload(bigSize, 'g')))
	br := bufio.NewReader(c)
	readReply(t, br)

	done := make(chan struct{})
	go func() { io.Copy(io.Discard, c); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("server deadlocked: half-read command did not close conn")
	}
	select {
	case err := <-closeErr:
		if err == nil {
			t.Fatal("expected non-nil close error for abandoned stream")
		}
	case <-time.After(time.Second):
		t.Fatal("closed callback not invoked")
	}
}

// TestStreamCancellation verifies that a read deadline (cancellation) while
// the client is slow does not deadlock: the read unblocks, the handler sees
// an error, and the connection is torn down.
func TestStreamCancellation(t *testing.T) {
	const bigSize = 1 << 20
	handlerErr := make(chan error, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := newServer()
	s.ln = ln
	s.net = "tcp"
	s.laddr = ln.Addr().String()
	s.handler = func(conn Conn, cmd Command) {}
	s.SetIdleClose(300 * time.Millisecond)
	s.SetStreamHandler(StreamHandlerFunc(func(conn Conn, cmd *StreamCommand) {
		buf := make([]byte, 4096)
		_, rerr := cmd.Read(buf) // blocks until the deadline fires
		handlerErr <- rerr
		// Do not call Close; server must close the connection.
	}), 1<<16)
	go serve(s)
	defer s.Close()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Announce a large arg but send nothing, so the read blocks.
	fmt.Fprintf(c, "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$%d\r\n", bigSize)

	select {
	case err := <-handlerErr:
		if err == nil {
			t.Fatal("expected a timeout error from the cancelled read")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled read deadlocked the handler")
	}
	done := make(chan struct{})
	go func() { io.Copy(io.Discard, c); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("connection not closed after cancellation")
	}
}
