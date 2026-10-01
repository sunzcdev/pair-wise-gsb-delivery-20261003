package imapserver_test

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

const condStoreTestMessage = "Subject: hello\r\n\r\nbody\r\n"

func newCondStoreTestListener(t *testing.T) net.Listener {
	memServer := imapmemserver.New()

	user := imapmemserver.NewUser("user", "pass")
	user.Create("INBOX", nil)
	for i := 0; i < 3; i++ {
		_, err := user.Append("INBOX", strings.NewReader(condStoreTestMessage), &imap.AppendOptions{})
		if err != nil {
			t.Fatalf("Append() = %v", err)
		}
	}
	memServer.AddUser(user)

	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapIMAP4rev2: {},
		},
	})
	t.Cleanup(func() { server.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	go func() {
		server.Serve(ln)
	}()
	t.Cleanup(func() { ln.Close() })

	return ln
}

type rawClient struct {
	net.Conn
	r *bufio.Reader
}

func newRawClient(t *testing.T, ln net.Listener) *rawClient {
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial() = %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	c := &rawClient{Conn: conn, r: bufio.NewReader(conn)}
	if line := c.readLine(t); !strings.HasPrefix(line, "* OK ") {
		t.Fatalf("unexpected greeting: %q", line)
	}
	return c
}

func (c *rawClient) send(t *testing.T, format string, args ...interface{}) {
	c.Conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(c.Conn, format+"\r\n", args...); err != nil {
		t.Fatalf("write failed: %v", err)
	}
}

func (c *rawClient) readLine(t *testing.T) string {
	c.Conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := c.r.ReadString('\n')
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

// readCmd reads untagged responses until the tagged response for tag.
func (c *rawClient) readCmd(t *testing.T, tag string) []string {
	var lines []string
	for {
		line := c.readLine(t)
		lines = append(lines, line)
		if strings.HasPrefix(line, tag+" ") {
			return lines
		}
	}
}

func requireLine(t *testing.T, lines []string, want string) {
	t.Helper()
	for _, line := range lines {
		if line == want {
			return
		}
	}
	t.Errorf("missing line %q in %q", want, lines)
}

func rejectLineContaining(t *testing.T, lines []string, substr string) {
	t.Helper()
	for _, line := range lines {
		if strings.Contains(line, substr) {
			t.Errorf("unexpected line containing %q: %q", substr, line)
		}
	}
}

func countLinesContaining(lines []string, substr string) int {
	var n int
	for _, line := range lines {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// TestCondStore exercises CONDSTORE with two connections modifying flags in
// the same mailbox.
func TestCondStore(t *testing.T) {
	ln := newCondStoreTestListener(t)

	clientA := newRawClient(t, ln)
	clientB := newRawClient(t, ln)

	clientA.send(t, "a1 LOGIN user pass")
	lines := clientA.readCmd(t, "a1")
	if !strings.HasPrefix(lines[len(lines)-1], "a1 OK ") {
		t.Fatalf("LOGIN failed: %q", lines)
	}

	clientA.send(t, "a2 CAPABILITY")
	caps := clientA.readCmd(t, "a2")
	if countLinesContaining(caps, "CONDSTORE") == 0 {
		t.Errorf("CAPABILITY response is missing CONDSTORE: %q", caps)
	}

	clientA.send(t, "a3 SELECT INBOX (CONDSTORE)")
	requireLine(t, clientA.readCmd(t, "a3"), "* OK [HIGHESTMODSEQ 4] Highest mailbox mod-sequence")

	clientB.send(t, "b1 LOGIN user pass")
	lines = clientB.readCmd(t, "b1")
	if !strings.HasPrefix(lines[len(lines)-1], "b1 OK ") {
		t.Fatalf("LOGIN failed: %q", lines)
	}
	clientB.send(t, "b2 SELECT INBOX (CONDSTORE)")
	requireLine(t, clientB.readCmd(t, "b2"), "* OK [HIGHESTMODSEQ 4] Highest mailbox mod-sequence")

	// Successful conditional STORE: bumps the mod-sequence
	clientB.send(t, "b3 STORE 2 (UNCHANGEDSINCE 4) +FLAGS (\\Flagged)")
	lines = clientB.readCmd(t, "b3")
	requireLine(t, lines, "* 2 FETCH (UID 2 FLAGS (\\flagged) MODSEQ (5))")
	rejectLineContaining(t, lines, "MODIFIED")

	// The other connection is asynchronously notified, with the new
	// mod-sequence
	clientA.send(t, "a4 NOOP")
	requireLine(t, clientA.readCmd(t, "a4"), "* 2 FETCH (UID 2 FLAGS (\\flagged) MODSEQ (5))")

	// Partially failing conditional UID STORE: message 1 is updated, message
	// 3 is too recent and is reported in the MODIFIED response code
	clientB.send(t, "b4 UID STORE 1,3 (UNCHANGEDSINCE 2) +FLAGS.SILENT (\\Seen)")
	lines = clientB.readCmd(t, "b4")
	requireLine(t, lines, "* OK [MODIFIED 3] Conditional STORE failed for some messages")
	rejectLineContaining(t, lines, "FETCH")

	// Same, with sequence numbers: message 1 now fails, message 3 succeeds
	clientB.send(t, "b5 STORE 1,3 (UNCHANGEDSINCE 5) +FLAGS.SILENT (\\Deleted)")
	lines = clientB.readCmd(t, "b5")
	requireLine(t, lines, "* OK [MODIFIED 1] Conditional STORE failed for some messages")

	// A no-op STORE must not bump the mod-sequence
	clientB.send(t, "b6 STORE 2 (UNCHANGEDSINCE 100) +FLAGS.SILENT (\\Flagged)")
	lines = clientB.readCmd(t, "b6")
	rejectLineContaining(t, lines, "MODIFIED")

	clientB.send(t, "b7 STATUS INBOX (HIGHESTMODSEQ)")
	requireLine(t, clientB.readCmd(t, "b7"), "* STATUS INBOX (HIGHESTMODSEQ 7)")

	// CHANGEDSINCE only returns messages modified after the given
	// mod-sequence, and implicitly includes MODSEQ
	clientB.send(t, "b8 FETCH 1:* (FLAGS) (CHANGEDSINCE 5)")
	lines = clientB.readCmd(t, "b8")
	if n := countLinesContaining(lines, " FETCH ("); n != 2 {
		t.Errorf("FETCH CHANGEDSINCE returned %v messages, want 2: %q", n, lines)
	}
	requireLine(t, lines, "* 1 FETCH (UID 1 FLAGS (\\seen) MODSEQ (6))")
	requireLine(t, lines, "* 3 FETCH (UID 3 FLAGS (\\deleted) MODSEQ (7))")

	// SEARCH with a MODSEQ criterion
	clientB.send(t, "b9 SEARCH MODSEQ 6")
	requireLine(t, clientB.readCmd(t, "b9"), "* SEARCH 1 3 (MODSEQ 7)")

	// Re-selecting the mailbox reports the up-to-date HIGHESTMODSEQ
	clientA.send(t, "a5 SELECT INBOX (CONDSTORE)")
	requireLine(t, clientA.readCmd(t, "a5"), "* OK [HIGHESTMODSEQ 7] Highest mailbox mod-sequence")
}
