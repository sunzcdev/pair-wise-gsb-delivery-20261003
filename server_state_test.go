package milter

import (
	"bytes"
	"net"
	nettextproto "net/textproto"
	"testing"

	"github.com/emersion/go-message/textproto"
)

type stateMilter struct {
	connectCalls int
	heloCalls    int
	mailCalls    int
	rcptCalls    int
	headerCalls  int
	eohCalls     int
	bodyCalls    int
	eobCalls     int
	abortCalls   int
	newCalls     int

	host  string
	helo  string
	from  string
	rcpts []string
	hdr   nettextproto.MIMEHeader

	mailResp Response
}

func (m *stateMilter) Connect(host, family string, port uint16, addr net.IP, modifier *Modifier) (Response, error) {
	m.connectCalls++
	m.host = host
	return RespContinue, nil
}

func (m *stateMilter) Helo(name string, modifier *Modifier) (Response, error) {
	m.heloCalls++
	m.helo = name
	return RespContinue, nil
}

func (m *stateMilter) MailFrom(from string, modifier *Modifier) (Response, error) {
	m.mailCalls++
	m.from = from
	if m.mailResp != nil {
		return m.mailResp, nil
	}
	return RespContinue, nil
}

func (m *stateMilter) RcptTo(rcpt string, modifier *Modifier) (Response, error) {
	m.rcptCalls++
	m.rcpts = append(m.rcpts, rcpt)
	return RespContinue, nil
}

func (m *stateMilter) Header(name, value string, modifier *Modifier) (Response, error) {
	m.headerCalls++
	return RespContinue, nil
}

func (m *stateMilter) Headers(headers nettextproto.MIMEHeader, modifier *Modifier) (Response, error) {
	m.eohCalls++
	m.hdr = headers
	return RespContinue, nil
}

func (m *stateMilter) BodyChunk(chunk []byte, modifier *Modifier) (Response, error) {
	m.bodyCalls++
	return RespContinue, nil
}

func (m *stateMilter) Body(modifier *Modifier) (Response, error) {
	m.eobCalls++
	return RespAccept, nil
}

func (m *stateMilter) Abort(modifier *Modifier) error {
	m.abortCalls++
	return nil
}

func newStrictStateSession(t *testing.T, protocol OptProtocol) (*stateMilter, *ClientSession) {
	t.Helper()

	backend := &stateMilter{}
	server := &Server{
		NewMilter: func() Milter {
			backend.newCalls++
			return backend
		},
		StrictStateMachine: true,
		Protocol:           protocol,
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })

	go server.Serve(listener)

	client := NewClientWithOptions("tcp", listener.Addr().String(), ClientOptions{
		ProtocolMask: protocol,
	})
	session, err := client.Session()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })

	return backend, session
}

func stateHeaders(values ...string) textproto.Header {
	headers := textproto.Header{}
	for i := 0; i < len(values); i += 2 {
		headers.Add(values[i], values[i+1])
	}
	return headers
}

func stateContinue(t *testing.T) func(*Action, error) {
	t.Helper()
	return func(action *Action, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if action.Code != ActContinue {
			t.Fatalf("expected continue, got %v", action.Code)
		}
	}
}

func stateAccept(t *testing.T) func(*Action, error) {
	t.Helper()
	return func(action *Action, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if action.Code != ActAccept {
			t.Fatalf("expected accept, got %v", action.Code)
		}
	}
}

func stateExpectTempFail(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected protocol error")
	}
}

func TestStrictStateMachine_PartiallySkippedStages(t *testing.T) {
	t.Run("skip RCPT only", func(t *testing.T) {
		backend, session := newStrictStateSession(t, OptNoRcptTo)

		stateContinue(t)(session.Conn("host", FamilyInet, 25, "192.0.2.1"))
		stateContinue(t)(session.Helo("helo.example"))
		stateContinue(t)(session.Mail("from@example.org", nil))
		stateContinue(t)(session.Header(stateHeaders("From", "from@example.org")))
		_, action, err := session.BodyReadFrom(bytes.NewReader(nil))
		stateAccept(t)(action, err)

		if backend.rcptCalls != 0 || backend.headerCalls != 1 ||
			backend.eohCalls != 1 || backend.eobCalls != 1 {
			t.Fatalf("unexpected callback counts: %+v", backend)
		}
	})

	t.Run("skip headers but keep EOH", func(t *testing.T) {
		backend, session := newStrictStateSession(t, OptNoHeaders)

		stateContinue(t)(session.Conn("host", FamilyInet, 25, "192.0.2.1"))
		stateContinue(t)(session.Helo("helo.example"))
		stateContinue(t)(session.Mail("from@example.org", nil))
		stateContinue(t)(session.Rcpt("to@example.org", nil))
		if _, err := session.HeaderEnd(); err != nil {
			t.Fatal(err)
		}
		_, action, err := session.BodyReadFrom(bytes.NewReader(nil))
		stateAccept(t)(action, err)

		if backend.headerCalls != 0 || backend.eohCalls != 1 || backend.eobCalls != 1 {
			t.Fatalf("unexpected callback counts: %+v", backend)
		}
	})

	t.Run("zero header fields without skipping", func(t *testing.T) {
		backend, session := newStrictStateSession(t, 0)

		stateContinue(t)(session.Conn("host", FamilyInet, 25, "192.0.2.1"))
		stateContinue(t)(session.Helo("helo.example"))
		stateContinue(t)(session.Mail("from@example.org", nil))
		stateContinue(t)(session.Rcpt("to@example.org", nil))
		if _, err := session.HeaderEnd(); err != nil {
			t.Fatal(err)
		}
		_, action, err := session.BodyReadFrom(bytes.NewReader(nil))
		stateAccept(t)(action, err)

		if backend.headerCalls != 0 || backend.eohCalls != 1 || backend.eobCalls != 1 {
			t.Fatalf("unexpected callback counts: %+v", backend)
		}
	})
}

func TestStrictStateMachine_CompleteFlow(t *testing.T) {
	backend, session := newStrictStateSession(t, 0)

	stateContinue(t)(session.Conn("host", FamilyInet, 25, "192.0.2.1"))
	stateContinue(t)(session.Helo("helo.example"))
	stateContinue(t)(session.Mail("from@example.org", nil))
	stateContinue(t)(session.Rcpt("to@example.org", nil))
	stateContinue(t)(session.Header(stateHeaders("From", "from@example.org")))
	_, action, err := session.BodyReadFrom(bytes.NewReader(nil))
	stateAccept(t)(action, err)

	if backend.connectCalls != 1 || backend.heloCalls != 1 || backend.mailCalls != 1 ||
		backend.rcptCalls != 1 || backend.headerCalls != 1 || backend.eohCalls != 1 ||
		backend.bodyCalls != 0 || backend.eobCalls != 1 {
		t.Fatalf("unexpected callback counts: %+v", backend)
	}
}

func TestStrictStateMachine_SkippedStages(t *testing.T) {
	tests := []struct {
		name     string
		protocol OptProtocol
		body     bool
	}{
		{
			name:     "skip envelope and headers",
			protocol: OptNoMailFrom | OptNoRcptTo | OptNoHeaders | OptNoEOH,
			body:     true,
		},
		{
			name:     "skip all message callbacks",
			protocol: OptNoMailFrom | OptNoRcptTo | OptNoHeaders | OptNoEOH | OptNoBody,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend, session := newStrictStateSession(t, test.protocol)

			stateContinue(t)(session.Conn("host", FamilyInet, 25, "192.0.2.1"))
			stateContinue(t)(session.Helo("helo.example"))

			if test.body {
				stateContinue(t)(session.BodyChunk([]byte("body")))
			}
			_, action, err := session.End()
			stateAccept(t)(action, err)

			if backend.mailCalls != 0 || backend.rcptCalls != 0 ||
				backend.headerCalls != 0 || backend.eohCalls != 0 {
				t.Fatalf("skipped callbacks were invoked: %+v", backend)
			}
			if test.body && backend.bodyCalls != 1 {
				t.Fatalf("expected body callback, got %+v", backend)
			}
			if backend.eobCalls != 1 {
				t.Fatalf("expected EOB callback, got %+v", backend)
			}
		})
	}
}

func TestStrictStateMachine_BodyBeforeMailRejected(t *testing.T) {
	backend, session := newStrictStateSession(t, 0)

	stateContinue(t)(session.Conn("host", FamilyInet, 25, "192.0.2.1"))
	stateContinue(t)(session.Helo("helo.example"))

	if err := writePacket(session.conn, &Message{Code: byte(CodeBody), Data: []byte("body")}, 0); err != nil {
		t.Fatal(err)
	}
	msg, err := readPacket(session.conn, 0)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Code != byte(ActTempFail) {
		t.Fatalf("expected tempfail, got %q", msg.Code)
	}
	if backend.bodyCalls != 0 {
		t.Fatalf("body callback invoked for illegal command: %+v", backend)
	}
}

func TestStrictStateMachine_RejectedMailAllowsRetry(t *testing.T) {
	backend, session := newStrictStateSession(t, 0)

	stateContinue(t)(session.Conn("host", FamilyInet, 25, "192.0.2.1"))
	stateContinue(t)(session.Helo("helo.example"))

	backend.mailResp = RespTempFail
	action, err := session.Mail("rejected@example.org", nil)
	if err != nil {
		t.Fatal(err)
	}
	if action.Code != ActTempFail {
		t.Fatalf("expected tempfail, got %v", action.Code)
	}
	backend.mailResp = RespContinue

	stateContinue(t)(session.Mail("accepted@example.org", nil))
	stateContinue(t)(session.Rcpt("to@example.org", nil))
	stateContinue(t)(session.Header(stateHeaders("From", "accepted@example.org")))
	_, action, err = session.BodyReadFrom(bytes.NewReader(nil))
	stateAccept(t)(action, err)

	if backend.mailCalls != 2 || backend.eobCalls != 1 {
		t.Fatalf("unexpected callback counts: %+v", backend)
	}
}

func TestStrictStateMachine_CommandsAfterEOBRejected(t *testing.T) {
	tests := []struct {
		name string
		code Code
		data []byte
	}{
		{name: "repeated EOB", code: CodeEOB},
		{name: "header", code: CodeHeader, data: []byte("X-Test\x00value\x00")},
		{name: "body", code: CodeBody, data: []byte("body")},
		{name: "recipient", code: CodeRcpt, data: []byte("to@example.org\x00")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend, session := newStrictStateSession(t, 0)

			stateContinue(t)(session.Conn("host", FamilyInet, 25, "192.0.2.1"))
			stateContinue(t)(session.Helo("helo.example"))
			stateContinue(t)(session.Mail("from@example.org", nil))
			stateContinue(t)(session.Rcpt("to@example.org", nil))
			stateContinue(t)(session.Header(stateHeaders("From", "from@example.org")))
			_, action, err := session.BodyReadFrom(bytes.NewReader(nil))
			stateAccept(t)(action, err)

			if err := writePacket(session.conn, &Message{Code: byte(test.code), Data: test.data}, 0); err != nil {
				t.Fatal(err)
			}
			msg, err := readPacket(session.conn, 0)
			if err != nil {
				t.Fatal(err)
			}
			if msg.Code != byte(ActTempFail) {
				t.Fatalf("expected tempfail, got %q", msg.Code)
			}

			if backend.eobCalls != 1 || backend.headerCalls != 1 || backend.rcptCalls != 1 {
				t.Fatalf("illegal callback was invoked: %+v", backend)
			}

			stateContinue(t)(session.Mail("second@example.org", nil))
		})
	}
}

func TestStrictStateMachine_EOBIsolatesMessages(t *testing.T) {
	backend, session := newStrictStateSession(t, 0)

	stateContinue(t)(session.Conn("host", FamilyInet, 25, "192.0.2.1"))
	stateContinue(t)(session.Helo("helo.example"))
	stateContinue(t)(session.Mail("first@example.org", nil))
	stateContinue(t)(session.Rcpt("first-to@example.org", nil))
	stateContinue(t)(session.Header(stateHeaders(
		"X-One", "one",
		"X-Two", "two",
	)))
	_, action, err := session.BodyReadFrom(bytes.NewReader(nil))
	stateAccept(t)(action, err)

	stateContinue(t)(session.Mail("second@example.org", nil))
	stateContinue(t)(session.Rcpt("second-to@example.org", nil))
	stateContinue(t)(session.Header(stateHeaders("X-Second", "second")))
	_, action, err = session.BodyReadFrom(bytes.NewReader(nil))
	stateAccept(t)(action, err)

	if backend.host != "host" || backend.helo != "helo.example" {
		t.Fatalf("connection state was not retained: %+v", backend)
	}
	if backend.from != "second@example.org" {
		t.Fatalf("wrong current sender: %q", backend.from)
	}
	if len(backend.rcpts) != 2 || backend.rcpts[1] != "second-to@example.org" {
		t.Fatalf("recipients leaked between messages: %v", backend.rcpts)
	}
	if len(backend.hdr) != 1 {
		t.Fatalf("headers leaked between messages: %v", backend.hdr)
	}
	if backend.newCalls != 1 {
		t.Fatalf("backend was reset between messages: %d", backend.newCalls)
	}
}

func TestStrictStateMachine_AbortIsolatesMessage(t *testing.T) {
	backend, session := newStrictStateSession(t, 0)

	stateContinue(t)(session.Conn("host", FamilyInet, 25, "192.0.2.1"))
	stateContinue(t)(session.Helo("helo.example"))
	stateContinue(t)(session.Mail("first@example.org", nil))
	stateContinue(t)(session.Rcpt("first-to@example.org", nil))
	stateContinue(t)(session.Header(stateHeaders("X-One", "one")))
	stateContinue(t)(session.BodyChunk([]byte("body")))

	if err := session.Abort(); err != nil {
		t.Fatal(err)
	}

	stateContinue(t)(session.Mail("second@example.org", nil))

	if backend.abortCalls != 1 {
		t.Fatalf("expected one abort callback, got %d", backend.abortCalls)
	}
	if backend.host != "host" || backend.helo != "helo.example" {
		t.Fatalf("connection state was not retained: %+v", backend)
	}
	stateContinue(t)(session.Rcpt("second-to@example.org", nil))
	stateContinue(t)(session.Header(stateHeaders("X-Second", "second")))
	_, action, err := session.BodyReadFrom(bytes.NewReader(nil))
	stateAccept(t)(action, err)

	if len(backend.hdr) != 1 {
		t.Fatalf("headers leaked across abort: %v", backend.hdr)
	}
	if len(backend.rcpts) != 2 || backend.rcpts[1] != "second-to@example.org" {
		t.Fatalf("recipients leaked across abort: %v", backend.rcpts)
	}
	if backend.newCalls != 1 {
		t.Fatalf("backend was reset by abort: %d", backend.newCalls)
	}
}
