package milter

import (
	"net"
	nettextproto "net/textproto"
	"testing"
)

// strictMilter records callback invocations to verify that out-of-order
// commands never reach the business callbacks.
type strictMilter struct {
	NoOpMilter

	mailCalls      int
	rcptCalls      int
	headerCalls    int
	eohCalls       int
	bodyChunkCalls int
	bodyCalls      int
	abortCalls     int

	macrosAtMail []map[string]string
	headersAtEOH []nettextproto.MIMEHeader
}

func (m *strictMilter) MailFrom(from string, mod *Modifier) (Response, error) {
	m.mailCalls++
	macros := make(map[string]string, len(mod.Macros))
	for k, v := range mod.Macros {
		macros[k] = v
	}
	m.macrosAtMail = append(m.macrosAtMail, macros)
	return RespContinue, nil
}

func (m *strictMilter) RcptTo(rcptTo string, mod *Modifier) (Response, error) {
	m.rcptCalls++
	return RespContinue, nil
}

func (m *strictMilter) Header(name string, value string, mod *Modifier) (Response, error) {
	m.headerCalls++
	return RespContinue, nil
}

func (m *strictMilter) Headers(h nettextproto.MIMEHeader, mod *Modifier) (Response, error) {
	m.eohCalls++
	m.headersAtEOH = append(m.headersAtEOH, h)
	return RespContinue, nil
}

func (m *strictMilter) BodyChunk(chunk []byte, mod *Modifier) (Response, error) {
	m.bodyChunkCalls++
	return RespContinue, nil
}

func (m *strictMilter) Body(mod *Modifier) (Response, error) {
	m.bodyCalls++
	return RespAccept, nil
}

func (m *strictMilter) Abort(mod *Modifier) error {
	m.abortCalls++
	return nil
}

func startStrictServer(t *testing.T, mm Milter, proto OptProtocol) *ClientSession {
	t.Helper()

	s := Server{
		NewMilter: func() Milter { return mm },
		Actions:   OptAddHeader | OptChangeHeader | OptQuarantine,
		Protocol:  proto,
		Strict:    true,
	}
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(local)

	cl := NewClientWithOptions("tcp", local.Addr().String(), ClientOptions{})
	session, err := cl.Session()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		session.Close()
		cl.Close()
		s.Close()
	})
	return session
}

func assertActionCode(t *testing.T, act *Action, err error, code ActionCode) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if act.Code != code {
		t.Fatalf("Unexpected action code: %v (expected %v)", act.Code, code)
	}
}

func assertProtocolError(t *testing.T, act *Action, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if act.Code != ActReplyCode {
		t.Fatalf("Unexpected action code: %v (expected reply code)", act.Code)
	}
	if act.SMTPCode != 451 {
		t.Fatalf("Unexpected SMTP code: %v (expected 451)", act.SMTPCode)
	}
}

func TestServerStrict_SequenceEnforced(t *testing.T) {
	mm := &strictMilter{}
	session := startStrictServer(t, mm, 0)

	act, err := session.Conn("host", FamilyInet, 25565, "172.0.0.1")
	assertActionCode(t, act, err, ActContinue)
	act, err = session.Helo("helo_host")
	assertActionCode(t, act, err, ActContinue)

	// BODY before MAIL is rejected and does not reach the callback.
	act, err = session.BodyChunk([]byte("hello"))
	assertProtocolError(t, act, err)
	// RCPT before MAIL is rejected.
	act, err = session.Rcpt("to@example.org", nil)
	assertProtocolError(t, act, err)
	// HEADER before MAIL is rejected.
	act, err = session.HeaderField("X-Early", "1")
	assertProtocolError(t, act, err)
	// EOB without a message in progress is rejected.
	_, act, err = session.End()
	assertProtocolError(t, act, err)

	if mm.bodyChunkCalls != 0 || mm.rcptCalls != 0 || mm.headerCalls != 0 || mm.bodyCalls != 0 {
		t.Fatal("Out-of-order commands reached milter callbacks")
	}

	// The session is still usable after protocol errors.
	if err := session.Macros(CodeHelo, "tls_version", "very old"); err != nil {
		t.Fatal(err)
	}
	act, err = session.Mail("from@example.org", nil)
	assertActionCode(t, act, err, ActContinue)

	// Duplicate MAIL while a message is in progress is rejected.
	act, err = session.Mail("other@example.org", nil)
	assertProtocolError(t, act, err)
	if mm.mailCalls != 1 {
		t.Fatal("Duplicate MAIL reached MailFrom callback")
	}

	// HEADER before RCPT is rejected.
	act, err = session.HeaderField("X-Early", "1")
	assertProtocolError(t, act, err)
	if mm.headerCalls != 0 {
		t.Fatal("Early HEADER reached Header callback")
	}

	act, err = session.Rcpt("to1@example.org", nil)
	assertActionCode(t, act, err, ActContinue)
	// Repeated RCPT within its own phase is fine.
	act, err = session.Rcpt("to2@example.org", nil)
	assertActionCode(t, act, err, ActContinue)

	// BODY before HEADER/EOH is rejected.
	act, err = session.BodyChunk([]byte("hello"))
	assertProtocolError(t, act, err)
	if mm.bodyChunkCalls != 0 {
		t.Fatal("Early BODY reached BodyChunk callback")
	}

	act, err = session.HeaderField("X-First", "1")
	assertActionCode(t, act, err, ActContinue)
	act, err = session.HeaderEnd()
	assertActionCode(t, act, err, ActContinue)
	act, err = session.BodyChunk([]byte("hello"))
	assertActionCode(t, act, err, ActContinue)
	_, act, err = session.End()
	assertActionCode(t, act, err, ActAccept)
	if mm.bodyCalls != 1 {
		t.Fatal("Body callback not called exactly once")
	}

	// Duplicate EOB after the message ended is rejected.
	_, act, err = session.End()
	assertProtocolError(t, act, err)
	// Modifications after the message ended are rejected.
	act, err = session.HeaderField("X-Late", "1")
	assertProtocolError(t, act, err)
	act, err = session.BodyChunk([]byte("late"))
	assertProtocolError(t, act, err)
	if mm.bodyCalls != 1 || mm.headerCalls != 1 || mm.bodyChunkCalls != 1 {
		t.Fatal("Commands after EOB reached milter callbacks")
	}

	// A second message on the same connection starts from a clean state.
	act, err = session.Mail("from2@example.org", nil)
	assertActionCode(t, act, err, ActContinue)
	act, err = session.Rcpt("to@example.org", nil)
	assertActionCode(t, act, err, ActContinue)
	act, err = session.HeaderField("X-Second", "2")
	assertActionCode(t, act, err, ActContinue)
	act, err = session.HeaderEnd()
	assertActionCode(t, act, err, ActContinue)
	_, act, err = session.End()
	assertActionCode(t, act, err, ActAccept)

	// The second message must not inherit headers from the first one.
	if len(mm.headersAtEOH) != 2 {
		t.Fatal("Unexpected Headers callback count:", len(mm.headersAtEOH))
	}
	if val := mm.headersAtEOH[1].Get("X-First"); val != "" {
		t.Fatal("Second message inherited X-First header:", val)
	}
	if val := mm.headersAtEOH[1].Get("X-Second"); val != "2" {
		t.Fatal("Second message lost X-Second header:", val)
	}
	// HELO-stage macros are connection state and survive the message end.
	if len(mm.macrosAtMail) != 2 {
		t.Fatal("Unexpected MailFrom callback count:", len(mm.macrosAtMail))
	}
	if val := mm.macrosAtMail[1]["tls_version"]; val != "very old" {
		t.Fatal("Second message lost HELO-stage macro:", val)
	}
}

func TestServerStrict_AbortResetsOnlyMessage(t *testing.T) {
	mm := &strictMilter{}
	session := startStrictServer(t, mm, 0)

	act, err := session.Conn("host", FamilyInet, 25565, "172.0.0.1")
	assertActionCode(t, act, err, ActContinue)
	act, err = session.Helo("helo_host")
	assertActionCode(t, act, err, ActContinue)
	if err := session.Macros(CodeHelo, "tls_version", "very old"); err != nil {
		t.Fatal(err)
	}

	act, err = session.Mail("from@example.org", nil)
	assertActionCode(t, act, err, ActContinue)
	act, err = session.Rcpt("to@example.org", nil)
	assertActionCode(t, act, err, ActContinue)
	act, err = session.HeaderField("X-Aborted", "1")
	assertActionCode(t, act, err, ActContinue)

	if err := session.Abort(); err != nil {
		t.Fatal(err)
	}

	// After abort a new message can be started right away.
	act, err = session.Mail("from2@example.org", nil)
	assertActionCode(t, act, err, ActContinue)
	act, err = session.Rcpt("to@example.org", nil)
	assertActionCode(t, act, err, ActContinue)
	act, err = session.HeaderField("X-Retried", "2")
	assertActionCode(t, act, err, ActContinue)
	act, err = session.HeaderEnd()
	assertActionCode(t, act, err, ActContinue)
	_, act, err = session.End()
	assertActionCode(t, act, err, ActAccept)

	if mm.abortCalls != 1 {
		t.Fatal("Abort callback not called exactly once")
	}
	// Connection and HELO information is preserved across the abort.
	if len(mm.macrosAtMail) != 2 {
		t.Fatal("Unexpected MailFrom callback count:", len(mm.macrosAtMail))
	}
	if val := mm.macrosAtMail[1]["tls_version"]; val != "very old" {
		t.Fatal("HELO-stage macro lost after abort:", val)
	}
	// The retried message does not inherit headers from the aborted one.
	if len(mm.headersAtEOH) != 1 {
		t.Fatal("Unexpected Headers callback count:", len(mm.headersAtEOH))
	}
	if val := mm.headersAtEOH[0].Get("X-Aborted"); val != "" {
		t.Fatal("Retried message inherited aborted header:", val)
	}
	if val := mm.headersAtEOH[0].Get("X-Retried"); val != "2" {
		t.Fatal("Retried message lost its header:", val)
	}
}

func TestServerStrict_SkippedPhasesAllowed(t *testing.T) {
	// Headers, EOH and body phases are negotiated away, so the MTA only
	// sends MAIL, RCPT and EOB. The strict state machine must accept this
	// shortened sequence.
	mm := &strictMilter{}
	session := startStrictServer(t, mm, OptNoHeaders|OptNoEOH|OptNoBody)

	act, err := session.Conn("host", FamilyInet, 25565, "172.0.0.1")
	assertActionCode(t, act, err, ActContinue)
	act, err = session.Helo("helo_host")
	assertActionCode(t, act, err, ActContinue)
	act, err = session.Mail("from@example.org", nil)
	assertActionCode(t, act, err, ActContinue)
	act, err = session.Rcpt("to@example.org", nil)
	assertActionCode(t, act, err, ActContinue)
	_, act, err = session.End()
	assertActionCode(t, act, err, ActAccept)

	if mm.mailCalls != 1 || mm.rcptCalls != 1 || mm.bodyCalls != 1 {
		t.Fatal("Unexpected callback counts:",
			mm.mailCalls, mm.rcptCalls, mm.bodyCalls)
	}
	if mm.headerCalls != 0 || mm.eohCalls != 0 || mm.bodyChunkCalls != 0 {
		t.Fatal("Negotiated-away phases reached callbacks")
	}
}

func TestServerStrict_SkippedEnvelopeAllowed(t *testing.T) {
	// The envelope phases are negotiated away, so the MTA only sends
	// headers, body and EOB.
	mm := &strictMilter{}
	session := startStrictServer(t, mm, OptNoMailFrom|OptNoRcptTo)

	act, err := session.Conn("host", FamilyInet, 25565, "172.0.0.1")
	assertActionCode(t, act, err, ActContinue)
	act, err = session.Helo("helo_host")
	assertActionCode(t, act, err, ActContinue)
	act, err = session.HeaderField("X-Test", "1")
	assertActionCode(t, act, err, ActContinue)
	act, err = session.HeaderEnd()
	assertActionCode(t, act, err, ActContinue)
	act, err = session.BodyChunk([]byte("hello"))
	assertActionCode(t, act, err, ActContinue)
	_, act, err = session.End()
	assertActionCode(t, act, err, ActAccept)

	if mm.mailCalls != 0 || mm.rcptCalls != 0 {
		t.Fatal("Negotiated-away phases reached callbacks")
	}
	if mm.headerCalls != 1 || mm.eohCalls != 1 || mm.bodyChunkCalls != 1 || mm.bodyCalls != 1 {
		t.Fatal("Unexpected callback counts:",
			mm.headerCalls, mm.eohCalls, mm.bodyChunkCalls, mm.bodyCalls)
	}
}
