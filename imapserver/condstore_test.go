package imapserver_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

const testRawMessage = `MIME-Version: 1.0
Message-Id: <condstore@example.com>
Content-Type: text/plain

hello`

type condStoreTest struct {
	t       *testing.T
	server  *imapserver.Server
	ln      net.Listener
	stopped chan struct{}
}

type pipeListener struct {
	ch chan net.Conn
}

func newPipeListener() *pipeListener {
	return &pipeListener{ch: make(chan net.Conn)}
}

func (ln *pipeListener) Accept() (net.Conn, error) {
	conn, ok := <-ln.ch
	if !ok {
		return nil, io.EOF
	}
	return conn, nil
}

func (ln *pipeListener) Close() error {
	close(ln.ch)
	return nil
}

func (ln *pipeListener) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

func newCondStoreTest(t *testing.T) *condStoreTest {
	t.Helper()

	memServer := imapmemserver.New()
	user := imapmemserver.NewUser("test-user", "test-password")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("Create() = %v", err)
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
			imap.CapCondStore: {},
		},
	})

	ln := newPipeListener()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = server.Serve(ln)
	}()

	return &condStoreTest{t: t, server: server, ln: ln, stopped: stopped}
}

func (ct *condStoreTest) close() {
	ct.server.Close()
	<-ct.stopped
}

func (ct *condStoreTest) dial() *rawIMAPConn {
	ct.t.Helper()
	serverConn, clientConn := net.Pipe()
	ct.ln.(*pipeListener).ch <- serverConn
	c := &rawIMAPConn{t: ct.t, conn: clientConn, r: bufio.NewReader(clientConn)}
	// Greeting
	c.readUntil(regexp.MustCompile(`\* OK`))
	return c
}

type rawIMAPConn struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	mu   sync.Mutex
	tagN int
}

func (c *rawIMAPConn) close() {
	c.conn.Close()
}

func (c *rawIMAPConn) send(format string, args ...interface{}) {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	s := fmt.Sprintf(format, args...)
	if _, err := io.WriteString(c.conn, s); err != nil {
		c.t.Fatalf("write %q: %v", s, err)
	}
}

// cmd sends a tagged command and returns all lines up to and including the
// tagged completion response.
func (c *rawIMAPConn) cmd(format string, args ...interface{}) string {
	c.t.Helper()
	c.tagN++
	tag := fmt.Sprintf("a%03d", c.tagN)
	c.send(tag+" "+format+"\r\n", args...)
	return c.readTag(tag)
}

// literal performs an APPEND-like literal upload.
func (c *rawIMAPConn) appendMessage() string {
	c.t.Helper()
	c.tagN++
	tag := fmt.Sprintf("a%03d", c.tagN)
	c.send("%s APPEND INBOX {%d}\r\n", tag, len(testRawMessage))
	c.readUntil(regexp.MustCompile(`\+ `))
	c.send("%s\r\n", testRawMessage)
	return c.readTag(tag)
}

func (c *rawIMAPConn) readTag(tag string) string {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(tag) + ` (OK|NO|BAD)`)
	return c.readUntil(re)
}

// readUntil reads lines until one matches re and returns the accumulated data.
func (c *rawIMAPConn) readUntil(re *regexp.Regexp) string {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	_ = c.conn.SetReadDeadline(deadline)

	var sb strings.Builder
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			c.t.Fatalf("read until %v: %v (data so far: %q)", re.String(), err, sb.String())
		}
		sb.WriteString(line)
		if re.MatchString(strings.TrimRight(line, "\r\n")) {
			_ = c.conn.SetReadDeadline(time.Time{})
			return sb.String()
		}
	}
}

func (c *rawIMAPConn) login() {
	resp := c.cmd("LOGIN test-user test-password")
	if !strings.Contains(resp, " OK ") {
		c.t.Fatalf("LOGIN failed: %q", resp)
	}
}

// selectInbox selects INBOX with CONDSTORE and returns the parsed
// HIGHESTMODSEQ value.
func (c *rawIMAPConn) selectInbox() uint64 {
	resp := c.cmd("SELECT INBOX (CONDSTORE)")
	re := regexp.MustCompile(`HIGHESTMODSEQ (\d+)`)
	m := re.FindStringSubmatch(resp)
	if m == nil {
		c.t.Fatalf("SELECT missing HIGHESTMODSEQ: %q", resp)
	}
	return parseModSeq(c.t, m[1])
}

func parseModSeq(t *testing.T, s string) uint64 {
	t.Helper()
	var v uint64
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		t.Fatalf("parse modseq %q: %v", s, err)
	}
	return v
}

func TestCondStoreSelectHighestModSeq(t *testing.T) {
	ct := newCondStoreTest(t)
	defer ct.close()

	c := ct.dial()
	defer c.close()
	c.login()

	if resp := c.appendMessage(); !strings.Contains(resp, " OK ") {
		t.Fatalf("APPEND failed: %q", resp)
	}

	// SELECT without a CONDSTORE parameter still advertises the mailbox
	// HIGHESTMODSEQ because the server supports persistent mod-sequences.
	resp := c.cmd("SELECT INBOX")
	if !regexp.MustCompile(`HIGHESTMODSEQ \d+`).MatchString(resp) {
		t.Fatalf("SELECT missing HIGHESTMODSEQ: %q", resp)
	}

	// STATUS must report HIGHESTMODSEQ too.
	resp = c.cmd("STATUS INBOX (HIGHESTMODSEQ)")
	if !regexp.MustCompile(`STATUS INBOX \(HIGHESTMODSEQ \d+\)`).MatchString(resp) {
		t.Fatalf("STATUS missing HIGHESTMODSEQ: %q", resp)
	}
}

func TestCondStoreFetchModSeq(t *testing.T) {
	ct := newCondStoreTest(t)
	defer ct.close()

	c := ct.dial()
	defer c.close()
	c.login()
	c.appendMessage()
	c.selectInbox()

	// FETCH MODSEQ on a fresh message returns modseq 0.
	resp := c.cmd("FETCH 1 (UID FLAGS MODSEQ)")
	if !regexp.MustCompile(`MODSEQ \(0\)`).MatchString(resp) {
		t.Fatalf("FETCH missing MODSEQ (0): %q", resp)
	}

	// A real flag change advances the mod-sequence.
	resp = c.cmd("STORE 1 +FLAGS (\\Flagged)")
	m := regexp.MustCompile(`MODSEQ \((\d+)\)`).FindStringSubmatch(resp)
	if m == nil {
		t.Fatalf("STORE response missing MODSEQ: %q", resp)
	}
	modSeq := parseModSeq(t, m[1])
	if modSeq != 2 {
		t.Fatalf("modseq after change = %v, want 2", modSeq)
	}
	if !regexp.MustCompile(`HIGHESTMODSEQ 2`).MatchString(resp) {
		t.Fatalf("STORE OK missing HIGHESTMODSEQ 2: %q", resp)
	}

	// A no-op STORE (adding an already present flag) must not advance the
	// mod-sequence and must not include HIGHESTMODSEQ.
	resp = c.cmd("STORE 1 +FLAGS (\\Flagged)")
	if regexp.MustCompile(`MODSEQ \(\d+\)`).MatchString(resp) {
		t.Fatalf("no-op STORE should not produce a FETCH response: %q", resp)
	}
	if strings.Contains(resp, "HIGHESTMODSEQ") {
		t.Fatalf("no-op STORE should not include HIGHESTMODSEQ: %q", resp)
	}

	// CHANGEDSINCE limits the result set.
	resp = c.cmd("UID FETCH 1:* (FLAGS) (CHANGEDSINCE 5)")
	if regexp.MustCompile(`\* \d+ FETCH`).MatchString(resp) {
		t.Fatalf("CHANGEDSINCE 5 should not match the message: %q", resp)
	}
	resp = c.cmd("UID FETCH 1:* (FLAGS MODSEQ) (CHANGEDSINCE 1)")
	if !regexp.MustCompile(`MODSEQ \(2\)`).MatchString(resp) {
		t.Fatalf("CHANGEDSINCE 1 should match the message: %q", resp)
	}
}

func TestCondStoreConditionalStoreMixedSeq(t *testing.T) {
	ct := newCondStoreTest(t)
	defer ct.close()

	c := ct.dial()
	defer c.close()
	c.login()
	c.appendMessage()
	c.appendMessage()
	c.appendMessage()
	c.selectInbox() // 3 messages, all modseq 0

	// Bump message 2 to modseq 5.
	c.cmd("STORE 2 +FLAGS (\\Flagged)")  // -> 2
	c.cmd("STORE 2 +FLAGS (\\Answered)") // -> 3
	c.cmd("STORE 2 +FLAGS (\\Draft)")    // -> 4
	c.cmd("STORE 2 +FLAGS (\\Seen)")     // -> 5

	// Conditional STORE with sequence numbers: only message 2 fails the
	// condition; 1 and 3 are updated together with a single new modseq.
	resp := c.cmd("STORE 1:3 (UNCHANGEDSINCE 4) +FLAGS (\\Deleted)")
	if !regexp.MustCompile(`\* 1 MODIFIED 2\b`).MatchString(resp) {
		t.Fatalf("missing MODIFIED 2: %q", resp)
	}
	// The modified message must not be included in the FETCH responses.
	fetchLines := regexp.MustCompile(`\* \d+ FETCH`).FindAllString(resp, -1)
	for _, line := range fetchLines {
		if line == "* 2 FETCH" {
			t.Fatalf("message 2 should not be FETCHed: %q", resp)
		}
	}
	if !regexp.MustCompile(`(?i)\* 1 FETCH.*\\deleted`).MatchString(resp) {
		t.Fatalf("message 1 should be updated: %q", resp)
	}
	if !regexp.MustCompile(`(?i)\* 3 FETCH.*\\deleted`).MatchString(resp) {
		t.Fatalf("message 3 should be updated: %q", resp)
	}
	if !regexp.MustCompile(`MODSEQ \(6\)`).MatchString(resp) {
		t.Fatalf("updated messages should share modseq 6: %q", resp)
	}
	if !regexp.MustCompile(`HIGHESTMODSEQ 6`).MatchString(resp) {
		t.Fatalf("tagged OK should carry HIGHESTMODSEQ 6: %q", resp)
	}

	// With UNCHANGEDSINCE 4 every message is newer than the threshold
	// (modseqs are 6/5/6), so MODIFIED covers the whole set with no FETCH
	// and no HIGHESTMODSEQ.
	resp = c.cmd("STORE 1:3 (UNCHANGEDSINCE 4) +FLAGS (\\Answered)")
	if !regexp.MustCompile(`\* 1 MODIFIED 1:3\b`).MatchString(resp) {
		t.Fatalf("missing MODIFIED 1:3: %q", resp)
	}
	if strings.Contains(resp, "FETCH") {
		t.Fatalf("all-modified STORE should not produce FETCH: %q", resp)
	}
	if strings.Contains(resp, "HIGHESTMODSEQ") {
		t.Fatalf("all-modified STORE should not include HIGHESTMODSEQ: %q", resp)
	}
}

func TestCondStoreConditionalStoreMixedUID(t *testing.T) {
	ct := newCondStoreTest(t)
	defer ct.close()

	c := ct.dial()
	defer c.close()
	c.login()
	c.appendMessage()
	c.appendMessage()
	c.selectInbox() // UIDs 1 and 2, modseq 0

	c.cmd("STORE 2 +FLAGS (\\Flagged)") // uid 2 -> modseq 2

	// UID form of MODIFIED must report a UID set.
	resp := c.cmd("UID STORE 1:2 (UNCHANGEDSINCE 1) +FLAGS (\\Deleted)")
	if !regexp.MustCompile(`\* 1 MODIFIED 2\b`).MatchString(resp) {
		t.Fatalf("missing UID MODIFIED 2: %q", resp)
	}
	if !regexp.MustCompile(`(?i)\* 1 FETCH.*UID 1.*\\deleted`).MatchString(resp) {
		t.Fatalf("UID 1 should be updated: %q", resp)
	}
	if strings.Contains(resp, "* UID FETCH") {
		t.Fatalf("MODIFIED must not use UID prefix: %q", resp)
	}
	if !regexp.MustCompile(`HIGHESTMODSEQ 3`).MatchString(resp) {
		t.Fatalf("expected HIGHESTMODSEQ 3: %q", resp)
	}

	// SILENT conditional STORE only reports MODIFIED: both messages now
	// have a mod-sequence above 1.
	resp = c.cmd("UID STORE 1:2 (UNCHANGEDSINCE 1) +FLAGS.SILENT (\\Seen)")
	if !regexp.MustCompile(`\* 1 MODIFIED 1:2\b`).MatchString(resp) {
		t.Fatalf("silent STORE missing MODIFIED 1:2: %q", resp)
	}
	if strings.Contains(resp, "FETCH") {
		t.Fatalf("SILENT STORE should not produce FETCH: %q", resp)
	}
}

func TestCondStoreSearchModSeq(t *testing.T) {
	ct := newCondStoreTest(t)
	defer ct.close()

	c := ct.dial()
	defer c.close()
	c.login()
	c.appendMessage()
	c.appendMessage()
	c.selectInbox()

	c.cmd("STORE 2 +FLAGS (\\Flagged)") // -> modseq 2

	// Classic SEARCH with MODSEQ key.
	resp := c.cmd("UID SEARCH MODSEQ 1")
	if !regexp.MustCompile(`\* SEARCH 2\b`).MatchString(resp) {
		t.Fatalf("SEARCH MODSEQ 1 should find uid 2: %q", resp)
	}
	if !regexp.MustCompile(`\* SEARCH MODSEQ 2\b`).MatchString(resp) {
		t.Fatalf("SEARCH should report mailbox MODSEQ 2: %q", resp)
	}

	// MODSEQ 0 matches all messages.
	resp = c.cmd("UID SEARCH RETURN (ALL) MODSEQ 0")
	if !regexp.MustCompile(`ESEARCH.*UID ALL 1:2`).MatchString(resp) {
		t.Fatalf("ESEARCH MODSEQ 0 should match all: %q", resp)
	}
	if !regexp.MustCompile(`ESEARCH.*MODSEQ 2`).MatchString(resp) {
		t.Fatalf("ESEARCH should report MODSEQ: %q", resp)
	}
}

func TestCondStoreCrossSession(t *testing.T) {
	ct := newCondStoreTest(t)
	defer ct.close()

	// Both sessions select the same shared mailbox with CONDSTORE.
	c1 := ct.dial()
	defer c1.close()
	c1.login()
	c1.appendMessage()
	c1.appendMessage()
	initial := c1.selectInbox()
	if initial != 1 {
		t.Fatalf("initial HIGHESTMODSEQ = %v, want 1", initial)
	}

	c2 := ct.dial()
	defer c2.close()
	c2.login()
	c2.selectInbox()

	// Session 2 goes idle; session 1 changes a flag.
	c2.send("a001 IDLE\r\n")
	c2.readUntil(regexp.MustCompile(`\+ idling`))

	// Give the server a moment to finish IDLE setup before changing the
	// mailbox from another session.
	time.Sleep(100 * time.Millisecond)

	resp := c1.cmd("STORE 1 +FLAGS (\\Flagged)")
	if !regexp.MustCompile(`HIGHESTMODSEQ 2`).MatchString(resp) {
		t.Fatalf("session 1 STORE should bump modseq to 2: %q", resp)
	}

	// Session 2 must asynchronously receive the flag change together with
	// its new MODSEQ.
	notify := c2.readUntil(regexp.MustCompile(`(?i)FETCH.*\\flagged`))
	if !regexp.MustCompile(`(?i)\* 1 FETCH \(UID 1 FLAGS \(\\flagged\) MODSEQ \(2\)\)`).MatchString(notify) {
		t.Fatalf("session 2 async FETCH missing FLAGS/MODSEQ: %q", notify)
	}

	// End IDLE.
	c2.send("DONE\r\n")
	c2.readTag("a001")

	// Session 2's conditional STORE with the stale value must fail for the
	// changed message but succeed on the untouched one.
	resp = c2.cmd("UID STORE 1:2 (UNCHANGEDSINCE 1) +FLAGS (\\Deleted)")
	if !regexp.MustCompile(`\* 1 MODIFIED 1\b`).MatchString(resp) {
		t.Fatalf("session 2 should see MODIFIED 1: %q", resp)
	}
	if !regexp.MustCompile(`(?i)\* 2 FETCH.*\\deleted`).MatchString(resp) {
		t.Fatalf("session 2 should update uid 2: %q", resp)
	}

	// A fresh SELECT must report the advanced mailbox version.
	resp = c2.cmd("SELECT INBOX (CONDSTORE)")
	if !regexp.MustCompile(`HIGHESTMODSEQ 3`).MatchString(resp) {
		t.Fatalf("re-SELECT should report HIGHESTMODSEQ 3: %q", resp)
	}
}

func TestCondStoreEnable(t *testing.T) {
	ct := newCondStoreTest(t)
	defer ct.close()

	c := ct.dial()
	defer c.close()
	c.login()
	c.appendMessage()

	resp := c.cmd("ENABLE CONDSTORE")
	if !strings.Contains(resp, "* ENABLED CONDSTORE") {
		t.Fatalf("ENABLE CONDSTORE not acknowledged: %q", resp)
	}

	resp = c.cmd("SELECT INBOX")
	if !regexp.MustCompile(`HIGHESTMODSEQ \d+`).MatchString(resp) {
		t.Fatalf("SELECT after ENABLE missing HIGHESTMODSEQ: %q", resp)
	}

	resp = c.cmd("STORE 1 +FLAGS (\\Flagged)")
	if !regexp.MustCompile(`\* 1 FETCH.*MODSEQ \(2\)`).MatchString(resp) {
		t.Fatalf("enabled session should see MODSEQ in STORE FETCH: %q", resp)
	}
}

func TestCondStoreStoreSetSemantics(t *testing.T) {
	ct := newCondStoreTest(t)
	defer ct.close()

	c := ct.dial()
	defer c.close()
	c.login()
	c.appendMessage()
	c.selectInbox()

	// FLAGS () on a message without flags is a no-op: no FETCH, no
	// HIGHESTMODSEQ.
	resp := c.cmd("STORE 1 FLAGS ()")
	if strings.Contains(resp, "FETCH") || strings.Contains(resp, "HIGHESTMODSEQ") {
		t.Fatalf("empty FLAGS SET with no existing flags should be a no-op: %q", resp)
	}

	// Setting an actual flag bumps modseq.
	resp = c.cmd("STORE 1 FLAGS (\\Flagged)")
	if !regexp.MustCompile(`HIGHESTMODSEQ 2`).MatchString(resp) {
		t.Fatalf("FLAGS SET should bump modseq: %q", resp)
	}

	// FLAGS () clears the flags and bumps modseq again.
	resp = c.cmd("STORE 1 FLAGS ()")
	if !regexp.MustCompile(`\* 1 FETCH \(UID 1 FLAGS \(\) MODSEQ \(3\)\)`).MatchString(resp) {
		t.Fatalf("FLAGS () should clear and report empty flags: %q", resp)
	}
	if !regexp.MustCompile(`HIGHESTMODSEQ 3`).MatchString(resp) {
		t.Fatalf("clearing flags should bump modseq to 3: %q", resp)
	}

	// Setting FLAGS () again is now a no-op.
	resp = c.cmd("STORE 1 FLAGS ()")
	if strings.Contains(resp, "FETCH") || strings.Contains(resp, "HIGHESTMODSEQ") {
		t.Fatalf("second FLAGS () should be a no-op: %q", resp)
	}
}
