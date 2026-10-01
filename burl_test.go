package smtp_test

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/emersion/go-smtp"
)

var (
	errBurlUnauthorized = &smtp.SMTPError{
		Code:         550,
		EnhancedCode: smtp.EnhancedCode{5, 7, 1},
		Message:      "URL access denied",
	}
	errBurlReadInterrupted = errors.New("simulated read interruption")
)

// memoryURLBackend is an in-memory "message storage". It resolves BURL URLs to
// data held in memory, so the SMTP server itself performs no network access.
type memoryURLBackend struct {
	objects     map[string][]byte
	denied      map[string]bool
	interrupted map[string]bool

	messages []*burlMessage
	resets   chan struct{}
	// dataErrors receives the result of every Data call.
	dataErrors chan error
}

type burlMessage struct {
	from string
	to   []string
	data []byte
}

func newMemoryURLBackend() *memoryURLBackend {
	return &memoryURLBackend{
		objects:     map[string][]byte{},
		denied:      map[string]bool{},
		interrupted: map[string]bool{},
		resets:      make(chan struct{}, 32),
		dataErrors:  make(chan error, 16),
	}
}

func (be *memoryURLBackend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	return &memoryURLSession{backend: be}, nil
}

type memoryURLSession struct {
	backend *memoryURLBackend
	msg     *burlMessage
}

var (
	_ smtp.Session     = (*memoryURLSession)(nil)
	_ smtp.BURLSession = (*memoryURLSession)(nil)
)

func (s *memoryURLSession) Reset() {
	if s.msg != nil {
		select {
		case s.backend.resets <- struct{}{}:
		default:
		}
	}
	s.msg = &burlMessage{}
}

func (s *memoryURLSession) Logout() error { return nil }

func (s *memoryURLSession) Mail(from string, _ *smtp.MailOptions) error {
	s.msg = &burlMessage{from: from}
	return nil
}

func (s *memoryURLSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.msg.to = append(s.msg.to, to)
	return nil
}

func (s *memoryURLSession) Data(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		s.backend.dataErrors <- err
		return err
	}
	s.msg.data = data
	s.backend.messages = append(s.backend.messages, s.msg)
	s.backend.dataErrors <- nil
	return nil
}

// BurlFetch resolves and authorizes the URL entirely within the backend.
func (s *memoryURLSession) BurlFetch(url string) (io.ReadCloser, error) {
	if s.backend.denied[url] {
		return nil, errBurlUnauthorized
	}
	data, ok := s.backend.objects[url]
	if !ok {
		return nil, errors.New("URL not found")
	}
	if s.backend.interrupted[url] {
		return &interruptingReader{data: data, failAfter: len(data) / 2}, nil
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

// interruptingReader returns failAfter bytes and then errors, simulating a
// fetch whose data stream is cut off before the announced length is reached.
type interruptingReader struct {
	data      []byte
	pos       int
	failAfter int
}

func (r *interruptingReader) Read(p []byte) (int, error) {
	if r.pos >= r.failAfter {
		return 0, errBurlReadInterrupted
	}
	n := copy(p, r.data[r.pos:r.failAfter])
	r.pos += n
	return n, nil
}

func (r *interruptingReader) Close() error { return nil }

func scanLine(t *testing.T, sc *bufio.Scanner) string {
	t.Helper()
	if !sc.Scan() {
		t.Fatalf("server closed connection: %v", sc.Err())
	}
	return sc.Text()
}

func expectPrefix(t *testing.T, sc *bufio.Scanner, prefix string) string {
	t.Helper()
	line := scanLine(t, sc)
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("expected response prefix %q, got %q", prefix, line)
	}
	return line
}

func testBurlServer(t *testing.T, be *memoryURLBackend) (*smtp.Server, net.Conn, *bufio.Scanner, map[string]bool) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	s := smtp.NewServer(be)
	s.Domain = "localhost"
	t.Cleanup(func() { s.Close() })
	go s.Serve(l)

	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 4096), 1<<20)

	expectPrefix(t, sc, "220 ")
	io.WriteString(c, "EHLO localhost\r\n")
	expectPrefix(t, sc, "250-Hello localhost")

	caps := map[string]bool{}
	for {
		line := scanLine(t, sc)
		if strings.HasPrefix(line, "250 ") {
			caps[strings.TrimPrefix(line, "250 ")] = true
			break
		}
		if !strings.HasPrefix(line, "250-") {
			t.Fatal("Invalid capability response:", line)
		}
		caps[strings.TrimPrefix(line, "250-")] = true
	}

	return s, c, sc, caps
}

func beginBurlTransaction(t *testing.T, c net.Conn, sc *bufio.Scanner) {
	t.Helper()
	io.WriteString(c, "MAIL FROM:<root@nsa.gov>\r\n")
	expectPrefix(t, sc, "250 ")
	io.WriteString(c, "RCPT TO:<root@gchq.gov.uk>\r\n")
	expectPrefix(t, sc, "250 ")
}

// The BURL extension is advertised only because the backend implements it.
func TestBurl_AdvertisedWhenBackendSupports(t *testing.T) {
	be := newMemoryURLBackend()
	_, _, _, caps := testBurlServer(t, be)

	if !caps["BURL"] {
		t.Fatal("BURL capability is missing even though the backend supports it")
	}
}

// A plain session (no BURLSession) must not advertise BURL and must reject the
// command; the server must not fetch anything itself.
func TestBurl_NotAdvertisedForPlainSession(t *testing.T) {
	be, s, c, scanner, caps := testServerEhlo(t)
	defer s.Close()
	defer c.Close()

	if caps["BURL"] {
		t.Fatal("BURL capability must not be advertised without backend support")
	}

	io.WriteString(c, "MAIL FROM:<root@nsa.gov>\r\n")
	scanner.Scan()
	io.WriteString(c, "RCPT TO:<root@gchq.gov.uk>\r\n")
	scanner.Scan()

	io.WriteString(c, "BURL <imap://example.com/x> 5 LAST\r\n")
	if line := expectPrefix(t, scanner, "502 "); !strings.Contains(line, "BURL") {
		t.Fatal("expected BURL unsupported error, got:", line)
	}

	if len(be.messages) != 0 || len(be.anonmsgs) != 0 {
		t.Fatal("no message must have been delivered:", be.messages, be.anonmsgs)
	}
}

// Successful BURL LAST delivers exactly the referenced bytes.
func TestBurl_Success(t *testing.T) {
	be := newMemoryURLBackend()
	body := "Subject: hello\r\n\r\nBURL body\r\n"
	be.objects["imap://example.com/msg/1"] = []byte(body)

	_, c, sc, _ := testBurlServer(t, be)
	beginBurlTransaction(t, c, sc)

	io.WriteString(c, "BURL <imap://example.com/msg/1> "+strconv.Itoa(len(body))+" LAST\r\n")
	expectPrefix(t, sc, "250 ")

	if len(be.messages) != 1 {
		t.Fatalf("expected exactly one delivered message, got %d", len(be.messages))
	}
	got := string(be.messages[0].data)
	if got != body {
		t.Fatalf("delivered bytes mismatch:\nwant %q\n got %q", body, got)
	}
}

// BDAT chunks and BURL references may be interleaved; the backend sees them in
// the exact order they were submitted, all counting towards one message.
func TestBurl_InterleavedWithBdat(t *testing.T) {
	be := newMemoryURLBackend()
	be.objects["mem://part1"] = []byte("one-")
	be.objects["mem://part2"] = []byte("three-")

	_, c, sc, _ := testBurlServer(t, be)
	beginBurlTransaction(t, c, sc)

	io.WriteString(c, "BDAT 6\r\n")
	io.WriteString(c, "ZERO\r\n")
	expectPrefix(t, sc, "250 ")

	io.WriteString(c, "BURL <mem://part1> 4\r\n")
	expectPrefix(t, sc, "250 ")

	io.WriteString(c, "BDAT 4\r\n")
	io.WriteString(c, "TWO-")
	expectPrefix(t, sc, "250 ")

	io.WriteString(c, "BURL <mem://part2> 6 LAST\r\n")
	expectPrefix(t, sc, "250 ")

	if len(be.messages) != 1 {
		t.Fatalf("expected exactly one delivered message, got %d", len(be.messages))
	}

	want := "ZERO\r\none-TWO-three-"
	got := string(be.messages[0].data)
	if got != want {
		t.Fatalf("delivered byte order mismatch:\nwant %q\n got %q", want, got)
	}
}

// A BURL LAST can finish a message that started with BDAT chunks.
func TestBurl_LastAfterBdatChunks(t *testing.T) {
	be := newMemoryURLBackend()
	be.objects["mem://tail"] = []byte("tail")

	_, c, sc, _ := testBurlServer(t, be)
	beginBurlTransaction(t, c, sc)

	io.WriteString(c, "BDAT 4\r\n")
	io.WriteString(c, "head")
	expectPrefix(t, sc, "250 ")

	io.WriteString(c, "BURL <mem://tail> 4 LAST\r\n")
	expectPrefix(t, sc, "250 ")

	if len(be.messages) != 1 {
		t.Fatalf("expected exactly one delivered message, got %d", len(be.messages))
	}
	if got := string(be.messages[0].data); got != "headtail" {
		t.Fatalf("delivered byte order mismatch: %q", got)
	}
}

// A non-final BURL followed by a final BDAT LAST is also valid.
func TestBurl_ThenBdatLast(t *testing.T) {
	be := newMemoryURLBackend()
	be.objects["mem://head"] = []byte("head")

	_, c, sc, _ := testBurlServer(t, be)
	beginBurlTransaction(t, c, sc)

	io.WriteString(c, "BURL <mem://head> 4\r\n")
	expectPrefix(t, sc, "250 ")

	io.WriteString(c, "BDAT 4 LAST\r\n")
	io.WriteString(c, "tail")
	expectPrefix(t, sc, "250 ")

	if len(be.messages) != 1 {
		t.Fatalf("expected exactly one delivered message, got %d", len(be.messages))
	}
	if got := string(be.messages[0].data); got != "headtail" {
		t.Fatalf("delivered byte order mismatch: %q", got)
	}
}

// Authorization refusal aborts the transaction and delivers nothing.
func TestBurl_AuthorizationDenied(t *testing.T) {
	be := newMemoryURLBackend()
	be.denied["mem://secret"] = true

	_, c, sc, _ := testBurlServer(t, be)
	beginBurlTransaction(t, c, sc)

	io.WriteString(c, "BURL <mem://secret> 5 LAST\r\n")
	line := expectPrefix(t, sc, "550 5.7.1")
	if !strings.Contains(line, "denied") {
		t.Fatal("expected authorization failure message, got:", line)
	}

	if len(be.messages) != 0 {
		t.Fatalf("no message must be delivered on authorization failure, got %d", len(be.messages))
	}

	// The transaction was reset: a new MAIL/RCPT/BURL sequence is required.
	io.WriteString(c, "RCPT TO:<root@gchq.gov.uk>\r\n")
	expectPrefix(t, sc, "502 ")
}

// A stream that dies before the announced length aborts and delivers nothing.
func TestBurl_ReadInterrupted(t *testing.T) {
	be := newMemoryURLBackend()
	body := "0123456789"
	be.objects["mem://broken"] = []byte(body)
	be.interrupted["mem://broken"] = true

	_, c, sc, _ := testBurlServer(t, be)
	beginBurlTransaction(t, c, sc)

	io.WriteString(c, "BURL <mem://broken> 10 LAST\r\n")
	expectPrefix(t, sc, "554 ")

	if len(be.messages) != 0 {
		t.Fatalf("no message must be delivered when the fetch is interrupted, got %d", len(be.messages))
	}
}

// Announcing more octets than the reference provides is a length mismatch.
func TestBurl_LengthMismatchShort(t *testing.T) {
	be := newMemoryURLBackend()
	be.objects["mem://x"] = []byte("abc")

	_, c, sc, _ := testBurlServer(t, be)
	beginBurlTransaction(t, c, sc)

	io.WriteString(c, "BURL <mem://x> 10 LAST\r\n")
	expectPrefix(t, sc, "554 5.6.0")

	if len(be.messages) != 0 {
		t.Fatalf("no message must be delivered on a length mismatch, got %d", len(be.messages))
	}
}

// Announcing fewer octets than the reference provides is also a mismatch.
func TestBurl_LengthMismatchLong(t *testing.T) {
	be := newMemoryURLBackend()
	be.objects["mem://x"] = []byte("0123456789")

	_, c, sc, _ := testBurlServer(t, be)
	beginBurlTransaction(t, c, sc)

	io.WriteString(c, "BURL <mem://x> 3 LAST\r\n")
	expectPrefix(t, sc, "554 5.6.0")

	if len(be.messages) != 0 {
		t.Fatalf("no message must be delivered on a length mismatch, got %d", len(be.messages))
	}
}

// Referenced octets count towards the message size limit together with BDAT.
func TestBurl_MessageSizeLimit(t *testing.T) {
	be := newMemoryURLBackend()
	be.objects["mem://x"] = []byte("0123456789")

	s, c, sc, _ := testBurlServer(t, be)
	s.MaxMessageBytes = 6
	beginBurlTransaction(t, c, sc)

	io.WriteString(c, "BURL <mem://x> 10 LAST\r\n")
	expectPrefix(t, sc, "552 ")

	if len(be.messages) != 0 {
		t.Fatalf("no message must be delivered over the size limit, got %d", len(be.messages))
	}

	// Combined size of an earlier BDAT chunk plus the reference is enforced.
	beginBurlTransaction(t, c, sc)
	io.WriteString(c, "BDAT 4\r\n")
	io.WriteString(c, "head")
	expectPrefix(t, sc, "250 ")
	io.WriteString(c, "BURL <mem://x> 10 LAST\r\n")
	expectPrefix(t, sc, "552 ")

	if len(be.messages) != 0 {
		t.Fatalf("no message must be delivered over the combined size limit, got %d", len(be.messages))
	}
}

// RSET while references/chunks are in flight aborts the delivery and resets
// the transaction without committing a partial message.
func TestBurl_Reset(t *testing.T) {
	be := newMemoryURLBackend()
	be.objects["mem://x"] = []byte("partial")

	_, c, sc, _ := testBurlServer(t, be)
	beginBurlTransaction(t, c, sc)

	io.WriteString(c, "BURL <mem://x> 7\r\n")
	expectPrefix(t, sc, "250 ")

	io.WriteString(c, "RSET\r\n")
	expectPrefix(t, sc, "250 ")
	<-be.resets

	if len(be.messages) != 0 {
		t.Fatalf("no message must be delivered after RSET, got %d", len(be.messages))
	}

	// After reset the envelope is gone.
	io.WriteString(c, "MAIL FROM:<root@nsa.gov>\r\n")
	expectPrefix(t, sc, "250 ")
}

// A non-final BURL reference followed by a client disconnect must abort the
// in-flight delivery with ErrDataReset and never commit a partial message.
func TestBurl_ClientDisconnect(t *testing.T) {
	be := newMemoryURLBackend()
	be.objects["mem://x"] = []byte("partial")

	_, c, sc, _ := testBurlServer(t, be)
	beginBurlTransaction(t, c, sc)

	io.WriteString(c, "BURL <mem://x> 7\r\n")
	expectPrefix(t, sc, "250 ")

	// Client vanishes instead of sending the final chunk/reference.
	c.Close()

	if err := <-be.dataErrors; err != smtp.ErrDataReset {
		t.Fatalf("backend reader got %v, want smtp.ErrDataReset", err)
	}

	if len(be.messages) != 0 {
		t.Fatalf("no message must be delivered after a client disconnect, got %d", len(be.messages))
	}
}

// BURL works over LMTP, producing one per-recipient final status.
func TestBurl_LMTP(t *testing.T) {
	be := newMemoryURLBackend()
	be.objects["mem://x"] = []byte("lmtp-body")

	s, c, sc, _ := testBurlServer(t, be)
	s.LMTP = true

	io.WriteString(c, "LHLO localhost\r\n")
	expectPrefix(t, sc, "250-Hello localhost")
	for {
		line := scanLine(t, sc)
		if strings.HasPrefix(line, "250 ") {
			break
		}
	}

	io.WriteString(c, "MAIL FROM:<root@nsa.gov>\r\n")
	expectPrefix(t, sc, "250 ")
	io.WriteString(c, "RCPT TO:<a@example.org>\r\n")
	expectPrefix(t, sc, "250 ")
	io.WriteString(c, "RCPT TO:<b@example.org>\r\n")
	expectPrefix(t, sc, "250 ")

	io.WriteString(c, "BURL <mem://x> 9 LAST\r\n")
	expectPrefix(t, sc, "250 2.0.0 <a@example.org>")
	expectPrefix(t, sc, "250 2.0.0 <b@example.org>")

	if len(be.messages) != 1 {
		t.Fatalf("expected one delivered message, got %d", len(be.messages))
	}
	if got := string(be.messages[0].data); got != "lmtp-body" {
		t.Fatalf("delivered bytes mismatch: %q", got)
	}
	if len(be.messages[0].to) != 2 {
		t.Fatalf("expected two recipients, got %v", be.messages[0].to)
	}
}
