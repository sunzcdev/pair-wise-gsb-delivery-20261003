package milter

import (
	"bytes"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

// startTestServer runs a milter server with the given backend and actions on
// a random local port and returns a client session connected to it.
func startTestServer(t *testing.T, mm Milter, actions OptAction, clientMask OptAction) *ClientSession {
	t.Helper()

	s := &Server{
		NewMilter: func() Milter { return mm },
		Actions:   actions,
	}
	t.Cleanup(func() { s.Close() })

	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(local)

	cl := NewClientWithOptions("tcp", local.Addr().String(), ClientOptions{
		ActionMask: clientMask,
	})
	t.Cleanup(func() { cl.Close() })

	session, err := cl.Session()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// runMessageFlow drives a minimal SMTP transaction up to end of message and
// returns the modify actions sent by the milter.
func runMessageFlow(t *testing.T, session *ClientSession) []ModifyAction {
	t.Helper()

	for _, step := range []func() error{
		func() error { _, err := session.Conn("host", FamilyInet, 25565, "172.0.0.1"); return err },
		func() error { _, err := session.Helo("helo_host"); return err },
		func() error { _, err := session.Mail("from@example.org", nil); return err },
		func() error { _, err := session.Rcpt("to@example.org", nil); return err },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}

	modifyActs, _, err := session.BodyReadFrom(strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	return modifyActs
}

// TestModifierNegotiatedSubset checks that modification actions are only
// written when both peers negotiated the required capability, and that
// failures name the action and the missing capability.
func TestModifierNegotiatedSubset(t *testing.T) {
	var (
		canAddHeader, canChangeFrom bool
		addHeaderErr                error
		changeFromErr               error
		replaceBodyErr              error
	)
	mm := MockMilter{
		ConnResp:      RespContinue,
		HeloResp:      RespContinue,
		MailResp:      RespContinue,
		RcptResp:      RespContinue,
		BodyChunkResp: RespContinue,
		BodyResp:      RespContinue,
		BodyMod: func(m *Modifier) {
			canAddHeader = m.ActionOption(OptAddHeader)
			canChangeFrom = m.ActionOption(OptChangeFrom)
			addHeaderErr = m.AddHeader("X-Test", "yes")
			changeFromErr = m.ChangeFrom("new@example.org")
			replaceBodyErr = m.ReplaceBody([]byte("new body"))
		},
	}

	// The server offers OptChangeFrom which the client does not accept, the
	// client offers OptChangeBody which the server does not support. Only
	// OptAddHeader is negotiated by both peers.
	session := startTestServer(t, &mm, OptAddHeader|OptChangeFrom,
		OptAddHeader|OptChangeBody)
	modifyActs := runMessageFlow(t, session)

	if !canAddHeader {
		t.Error("ActionOption(OptAddHeader) = false, want true")
	}
	if canChangeFrom {
		t.Error("ActionOption(OptChangeFrom) = true, want false")
	}
	if addHeaderErr != nil {
		t.Error("AddHeader failed:", addHeaderErr)
	}
	if changeFromErr == nil ||
		!strings.Contains(changeFromErr.Error(), "ChangeFrom") ||
		!strings.Contains(changeFromErr.Error(), "OptChangeFrom") {
		t.Error("ChangeFrom error does not name action and capability:", changeFromErr)
	}
	if replaceBodyErr == nil ||
		!strings.Contains(replaceBodyErr.Error(), "ReplaceBody") ||
		!strings.Contains(replaceBodyErr.Error(), "OptChangeBody") {
		t.Error("ReplaceBody error does not name action and capability:", replaceBodyErr)
	}

	// Only the negotiated action may have produced a packet.
	expected := []ModifyAction{
		{Code: ActAddHeader, HeaderName: "X-Test", HeaderValue: "yes"},
	}
	if !reflect.DeepEqual(modifyActs, expected) {
		t.Fatalf("Wrong modify actions, got %+v", modifyActs)
	}
}

// TestModifierExpiredAfterCallback checks that a Modifier saved by the
// backend becomes useless once its callback has returned and does not emit
// any packets anymore.
func TestModifierExpiredAfterCallback(t *testing.T) {
	var saved *Modifier
	var expiredErr, currentErr error
	mm := MockMilter{
		ConnResp:      RespContinue,
		HeloResp:      RespContinue,
		MailResp:      RespContinue,
		RcptResp:      RespContinue,
		BodyChunkResp: RespContinue,
		BodyResp:      RespContinue,
		MailMod: func(m *Modifier) {
			saved = m
		},
		BodyMod: func(m *Modifier) {
			expiredErr = saved.AddHeader("X-Old", "1")
			currentErr = m.AddHeader("X-New", "2")
		},
	}

	session := startTestServer(t, &mm, OptAddHeader, OptAddHeader)
	modifyActs := runMessageFlow(t, session)

	if expiredErr == nil || !strings.Contains(expiredErr.Error(), "AddHeader") {
		t.Error("Expected error naming AddHeader for expired modifier, got:", expiredErr)
	}
	if currentErr != nil {
		t.Error("AddHeader on current modifier failed:", currentErr)
	}

	// The expired modifier must not have produced any packet.
	expected := []ModifyAction{
		{Code: ActAddHeader, HeaderName: "X-New", HeaderValue: "2"},
	}
	if !reflect.DeepEqual(modifyActs, expected) {
		t.Fatalf("Wrong modify actions, got %+v", modifyActs)
	}
}

// failAfterConn is a net.Conn that starts failing writes after failAfter
// successful ones and counts all write attempts.
type failAfterConn struct {
	writes    int
	failAfter int
}

func (c *failAfterConn) Write(b []byte) (int, error) {
	c.writes++
	if c.writes > c.failAfter {
		return 0, errors.New("connection broken")
	}
	return len(b), nil
}

func (c *failAfterConn) Read(b []byte) (int, error)         { return 0, io.EOF }
func (c *failAfterConn) Close() error                       { return nil }
func (c *failAfterConn) LocalAddr() net.Addr                { return nil }
func (c *failAfterConn) RemoteAddr() net.Addr               { return nil }
func (c *failAfterConn) SetDeadline(t time.Time) error      { return nil }
func (c *failAfterConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *failAfterConn) SetWriteDeadline(t time.Time) error { return nil }

// TestModifierWriteFailureSticky checks that once a packet write fails, the
// failing action reports the error and subsequent actions fail without
// attempting to send more packets.
func TestModifierWriteFailureSticky(t *testing.T) {
	conn := &failAfterConn{failAfter: 1}
	s := &milterSession{
		actions: OptAddHeader,
		stage:   StageEOM,
		conn:    conn,
	}
	mod := newModifier(s)
	s.modifier = mod

	if err := mod.AddHeader("X-One", "1"); err != nil {
		t.Fatal("First AddHeader failed:", err)
	}
	err := mod.AddHeader("X-Two", "2")
	if err == nil || !strings.Contains(err.Error(), "AddHeader") {
		t.Fatal("Expected error naming AddHeader, got:", err)
	}

	writesAfterFailure := conn.writes
	stickyErr := mod.AddHeader("X-Three", "3")
	if stickyErr == nil {
		t.Fatal("Expected sticky error after write failure")
	}
	if conn.writes != writesAfterFailure {
		t.Fatal("Packets were silently sent after the last successful action")
	}

	// Once the callback is over the modifier must not send anything either.
	s.modifier = nil
	if err := mod.AddHeader("X-Four", "4"); err == nil {
		t.Fatal("Expected error for expired modifier")
	}
	if conn.writes != writesAfterFailure {
		t.Fatal("Expired modifier sent packets")
	}
}

// TestModifierReplaceBodyChunking checks that large bodies are split into
// MaxBodyChunk sized packets and that CRLF pairs straddling a chunk boundary
// are converted exactly once.
func TestModifierReplaceBodyChunking(t *testing.T) {
	var body bytes.Buffer
	body.Write(bytes.Repeat([]byte{'A'}, MaxBodyChunk-1))
	// CRLF straddles the first chunk boundary.
	body.WriteString("\r\n")
	body.Write(bytes.Repeat([]byte{'B'}, MaxBodyChunk))
	// More CRLFs after the boundary.
	body.WriteString("line1\r\nline2\r\n")

	var replaceErr error
	mm := MockMilter{
		ConnResp:      RespContinue,
		HeloResp:      RespContinue,
		MailResp:      RespContinue,
		RcptResp:      RespContinue,
		BodyChunkResp: RespContinue,
		BodyResp:      RespContinue,
		BodyMod: func(m *Modifier) {
			replaceErr = m.ReplaceBody(body.Bytes())
		},
	}

	session := startTestServer(t, &mm, OptChangeBody, OptChangeBody)
	modifyActs := runMessageFlow(t, session)

	if replaceErr != nil {
		t.Fatal("ReplaceBody failed:", replaceErr)
	}

	var reassembled []byte
	for _, act := range modifyActs {
		if act.Code != ActReplBody {
			t.Fatalf("Unexpected modify action: %+v", act)
		}
		if len(act.Body) > MaxBodyChunk {
			t.Fatal("Chunk bigger than MaxBodyChunk:", len(act.Body))
		}
		reassembled = append(reassembled, act.Body...)
	}
	if len(modifyActs) < 2 {
		t.Fatal("Body was not split into multiple chunks")
	}

	expected := crlfToLF(body.Bytes())
	if !bytes.Equal(reassembled, expected) {
		t.Fatalf("Reassembled body does not match, got %d bytes, want %d",
			len(reassembled), len(expected))
	}
	if bytes.Contains(reassembled, []byte{'\r'}) {
		t.Fatal("Reassembled body still contains CR characters")
	}
}
