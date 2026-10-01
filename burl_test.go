package smtp_test

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-smtp"
)

// burlBackend is an in-memory URL "message store" used to test the BURL
// extension. Resolving URLs and authorizing access to them is entirely
// handled by the backend, the server never fetches anything by itself.
type burlBackend struct {
	messages []*message
	urls     map[string]*burlResource

	// Errors received by the Data method, if non-nil.
	dataErrors chan error
}

type burlResource struct {
	data []byte

	// fetchErr is returned by FetchURL, simulating e.g. an authorization
	// failure.
	fetchErr error

	// readErr is returned by the content reader after readErrOffset
	// bytes, simulating an interrupted read.
	readErr       error
	readErrOffset int
}

func (be *burlBackend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	return &burlSession{backend: be}, nil
}

type burlSession struct {
	backend *burlBackend
	msg     *message
}

var _ smtp.BURLSession = (*burlSession)(nil)

func (s *burlSession) Reset() {
	s.msg = &message{}
}

func (s *burlSession) Logout() error {
	return nil
}

func (s *burlSession) Mail(from string, opts *smtp.MailOptions) error {
	s.Reset()
	s.msg.From = from
	s.msg.Opts = opts
	return nil
}

func (s *burlSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	s.msg.To = append(s.msg.To, to)
	return nil
}

func (s *burlSession) Data(r io.Reader) error {
	b, err := ioutil.ReadAll(r)
	if s.backend.dataErrors != nil {
		s.backend.dataErrors <- err
	}
	if err != nil {
		return err
	}
	s.msg.Data = b
	s.backend.messages = append(s.backend.messages, s.msg)
	return nil
}

func (s *burlSession) FetchURL(url string) (io.ReadCloser, error) {
	res, ok := s.backend.urls[url]
	if !ok {
		return nil, &smtp.SMTPError{
			Code:         554,
			EnhancedCode: smtp.EnhancedCode{5, 7, 1},
			Message:      "URL access denied",
		}
	}
	if res.fetchErr != nil {
		return nil, res.fetchErr
	}
	var r io.Reader = bytes.NewReader(res.data)
	if res.readErr != nil {
		r = io.MultiReader(
			bytes.NewReader(res.data[:res.readErrOffset]),
			&failReader{err: res.readErr},
		)
	}
	return ioutil.NopCloser(r), nil
}

type failReader struct {
	err error
}

func (r *failReader) Read([]byte) (int, error) {
	return 0, r.err
}

func testBURLServer(t *testing.T, be *burlBackend, fn ...serverConfigureFunc) (s *smtp.Server, c net.Conn, scanner *bufio.Scanner, caps map[string]bool) {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	s = smtp.NewServer(be)
	s.Domain = "localhost"
	for _, f := range fn {
		f(s)
	}

	go s.Serve(l)

	c, err = net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	scanner = bufio.NewScanner(c)

	if res := readReply(t, scanner); !strings.HasPrefix(res, "220 ") {
		t.Fatal("Invalid greeting:", res)
	}

	io.WriteString(c, "EHLO localhost\r\n")
	caps = make(map[string]bool)
	for {
		res := readReply(t, scanner)
		if strings.HasPrefix(res, "250-") {
			caps[strings.TrimPrefix(res, "250-")] = true
		} else if strings.HasPrefix(res, "250 ") {
			caps[strings.TrimPrefix(res, "250 ")] = true
			break
		} else {
			t.Fatal("Invalid EHLO response:", res)
		}
	}

	return
}

func burlMailRcpt(t *testing.T, c net.Conn, scanner *bufio.Scanner) {
	t.Helper()

	io.WriteString(c, "MAIL FROM:<root@nsa.gov>\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatal("Invalid MAIL response:", res)
	}
	io.WriteString(c, "RCPT TO:<root@gchq.gov.uk>\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatal("Invalid RCPT response:", res)
	}
}

func TestServer_BURL(t *testing.T) {
	be := &burlBackend{urls: map[string]*burlResource{
		"mem:msg1": {data: []byte("Subject: test\r\n\r\nHey <3\r\n")},
	}}
	s, c, scanner, caps := testBURLServer(t, be)
	defer s.Close()
	defer c.Close()

	if !caps["BURL"] {
		t.Fatal("BURL capability is not advertised")
	}

	burlMailRcpt(t, c, scanner)

	io.WriteString(c, "BURL mem:msg1 LAST\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatal("Invalid BURL response:", res)
	}

	if len(be.messages) != 1 {
		t.Fatal("Invalid number of sent messages:", len(be.messages))
	}
	msg := be.messages[0]
	if msg.From != "root@nsa.gov" {
		t.Fatal("Invalid mail sender:", msg.From)
	}
	if want := "Subject: test\r\n\r\nHey <3\r\n"; string(msg.Data) != want {
		t.Fatal("Invalid mail data:", string(msg.Data))
	}
}

func TestServer_BURL_CombinedWithBDAT(t *testing.T) {
	be := &burlBackend{urls: map[string]*burlResource{
		"mem:part": {data: []byte("world")},
	}}
	s, c, scanner, _ := testBURLServer(t, be)
	defer s.Close()
	defer c.Close()

	// BDAT chunk, then BURL chunk, then final BDAT chunk.
	burlMailRcpt(t, c, scanner)

	io.WriteString(c, "BDAT 7\r\n")
	io.WriteString(c, "Hello, ")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatal("Invalid BDAT response:", res)
	}
	io.WriteString(c, "BURL mem:part\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatal("Invalid BURL response:", res)
	}
	io.WriteString(c, "BDAT 2 LAST\r\n")
	io.WriteString(c, "\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatal("Invalid BDAT response:", res)
	}

	if len(be.messages) != 1 {
		t.Fatal("Invalid number of sent messages:", len(be.messages))
	}
	if want := "Hello, world\r\n"; string(be.messages[0].Data) != want {
		t.Fatal("Invalid mail data:", string(be.messages[0].Data))
	}

	// BURL chunk first, then a final BDAT chunk.
	burlMailRcpt(t, c, scanner)

	io.WriteString(c, "BURL mem:part\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatal("Invalid BURL response:", res)
	}
	io.WriteString(c, "BDAT 6 LAST\r\n")
	io.WriteString(c, "again\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatal("Invalid BDAT response:", res)
	}

	if len(be.messages) != 2 {
		t.Fatal("Invalid number of sent messages:", len(be.messages))
	}
	if want := "worldagain\n"; string(be.messages[1].Data) != want {
		t.Fatal("Invalid mail data:", string(be.messages[1].Data))
	}
}

func TestServer_BURL_NotSupported(t *testing.T) {
	// The default test backend does not implement BURLSession.
	_, s, c, scanner, caps := testServerEhlo(t)
	defer s.Close()
	defer c.Close()

	if caps["BURL"] {
		t.Fatal("BURL capability advertised without backend support")
	}

	io.WriteString(c, "MAIL FROM:<root@nsa.gov>\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatal("Invalid MAIL response:", res)
	}
	io.WriteString(c, "RCPT TO:<root@gchq.gov.uk>\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatal("Invalid RCPT response:", res)
	}
	io.WriteString(c, "BURL mem:msg1 LAST\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "502 ") {
		t.Fatal("Invalid BURL response:", res)
	}
}

func TestServer_BURL_BadSyntax(t *testing.T) {
	be := &burlBackend{urls: map[string]*burlResource{}}
	s, c, scanner, _ := testBURLServer(t, be)
	defer s.Close()
	defer c.Close()

	// BURL before MAIL/RCPT is rejected.
	io.WriteString(c, "BURL mem:msg1 LAST\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "502 ") {
		t.Fatal("Invalid BURL response:", res)
	}

	burlMailRcpt(t, c, scanner)

	io.WriteString(c, "BURL\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "501 ") {
		t.Fatal("Invalid BURL response:", res)
	}
	io.WriteString(c, "BURL mem:msg1 FOO\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "501 ") {
		t.Fatal("Invalid BURL response:", res)
	}
	io.WriteString(c, "BURL mem:msg1 LAST EXTRA\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "501 ") {
		t.Fatal("Invalid BURL response:", res)
	}
}

func TestServer_BURL_FetchDenied(t *testing.T) {
	be := &burlBackend{urls: map[string]*burlResource{
		"mem:ok": {data: []byte("real content\r\n")},
	}}
	s, c, scanner, _ := testBURLServer(t, be)
	defer s.Close()
	defer c.Close()

	burlMailRcpt(t, c, scanner)

	// The backend denies access to the URL.
	io.WriteString(c, "BURL mem:secret LAST\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "554 5.7.1 ") {
		t.Fatal("Invalid BURL response:", res)
	}

	// The transaction is left intact and can be completed with
	// another BURL command.
	io.WriteString(c, "BURL mem:ok LAST\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatal("Invalid BURL response:", res)
	}

	if len(be.messages) != 1 {
		t.Fatal("Invalid number of sent messages:", len(be.messages))
	}
	if want := "real content\r\n"; string(be.messages[0].Data) != want {
		t.Fatal("Invalid mail data:", string(be.messages[0].Data))
	}
}

func TestServer_BURL_ReadError(t *testing.T) {
	be := &burlBackend{
		urls: map[string]*burlResource{
			"mem:broken": {
				data:          []byte("partial content"),
				readErr:       errors.New("read interrupted"),
				readErrOffset: 7,
			},
		},
		dataErrors: make(chan error, 1),
	}
	s, c, scanner, _ := testBURLServer(t, be)
	defer s.Close()
	defer c.Close()

	burlMailRcpt(t, c, scanner)

	io.WriteString(c, "BURL mem:broken LAST\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "554 ") {
		t.Fatal("Invalid BURL response:", res)
	}

	// The transaction is aborted, the backend must not commit
	// a truncated message.
	if err := <-be.dataErrors; err != smtp.ErrDataReset {
		t.Fatal("Backend received a different error:", err)
	}
	if len(be.messages) != 0 {
		t.Fatal("Truncated message was committed")
	}

	// The connection stays usable.
	io.WriteString(c, "RSET\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatal("Invalid RSET response:", res)
	}
}

func TestServer_BURL_SizeLimit(t *testing.T) {
	be := &burlBackend{urls: map[string]*burlResource{
		"mem:big": {data: []byte("0123456789")},
	}}
	s, c, scanner, _ := testBURLServer(t, be, func(s *smtp.Server) {
		s.MaxMessageBytes = 12
	})
	defer s.Close()
	defer c.Close()

	burlMailRcpt(t, c, scanner)

	// BDAT and BURL content share the same message size limit.
	io.WriteString(c, "BDAT 5\r\n")
	io.WriteString(c, "Hello")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatal("Invalid BDAT response:", res)
	}
	io.WriteString(c, "BURL mem:big LAST\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "552 ") {
		t.Fatal("Invalid BURL response:", res)
	}

	if len(be.messages) != 0 {
		t.Fatal("Too big message was committed")
	}

	io.WriteString(c, "RSET\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatal("Invalid RSET response:", res)
	}
}

func TestServer_BURL_Reset(t *testing.T) {
	be := &burlBackend{
		urls: map[string]*burlResource{
			"mem:part": {data: []byte("world")},
		},
		dataErrors: make(chan error, 1),
	}
	s, c, scanner, _ := testBURLServer(t, be)
	defer s.Close()
	defer c.Close()

	burlMailRcpt(t, c, scanner)

	io.WriteString(c, "BDAT 7\r\n")
	io.WriteString(c, "Hello, ")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatal("Invalid BDAT response:", res)
	}
	io.WriteString(c, "BURL mem:part\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatal("Invalid BURL response:", res)
	}

	// The client changes its mind, the partial message must be discarded.
	io.WriteString(c, "RSET\r\n")
	if res := readReply(t, scanner); !strings.HasPrefix(res, "250 ") {
		t.Fatal("Invalid RSET response:", res)
	}

	if err := <-be.dataErrors; err != smtp.ErrDataReset {
		t.Fatal("Backend received a different error:", err)
	}
	if len(be.messages) != 0 {
		t.Fatal("Partial message was committed")
	}
}
