package redcon

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
)

// TestStreamStress repeatedly streams a value followed by a PING over the
// same connection with small read buffers, exercising chunk boundaries,
// drain, and continuation many times.
func TestStreamStress(t *testing.T) {
	const (
		bigSize = 700_000
		rounds  = 20
	)
	addr, cleanup := startStreamServer(t, 1<<15,
		func(conn Conn, cmd *StreamCommand) {
			buf := make([]byte, 1234) // deliberately awkward size
			var sum int64
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
				t.Errorf("size mismatch %d", sum)
			}
			if err := cmd.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
			conn.WriteString("S")
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

	go func() {
		p := payload(bigSize, 0xAB)
		for i := 0; i < rounds; i++ {
			c.Write(bulkCmd([]byte("SET"), []byte("k"), p))
		}
	}()

	br := bufio.NewReader(c)
	for i := 0; i < rounds; i++ {
		if got := readReply(t, br); got != "+S\r\n" {
			t.Fatalf("round %d: got %q", i, got)
		}
	}
}
