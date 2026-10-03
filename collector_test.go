package milter

import (
	"bytes"
	"errors"
	"net"
	"reflect"
	"testing"

	"github.com/emersion/go-message/textproto"
)

func startCollectorTestSession(t *testing.T, mm *MockMilter) *ClientSession {
	t.Helper()

	s := Server{
		NewMilter: func() Milter {
			return mm
		},
		Actions: OptAddHeader | OptChangeHeader | OptAddRcpt |
			OptRemoveRcpt | OptChangeBody | OptQuarantine,
	}
	t.Cleanup(func() { s.Close() })
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(local)

	cl := NewClientWithOptions("tcp", local.Addr().String(), ClientOptions{
		ActionMask: OptAddHeader | OptChangeHeader | OptAddRcpt |
			OptRemoveRcpt | OptChangeBody | OptQuarantine,
	})
	t.Cleanup(func() { cl.Close() })
	session, err := cl.Session()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })

	if _, err := session.Conn("host", FamilyInet, 25565, "172.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Helo("helo_host"); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Mail("from@example.org", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Rcpt("to@example.org", nil); err != nil {
		t.Fatal(err)
	}
	hdr := textproto.Header{}
	hdr.Add("Subject", "hello")
	hdr.Add("Subject", "hello again")
	if _, err := session.Header(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := session.BodyChunk([]byte("body")); err != nil {
		t.Fatal(err)
	}

	return session
}

func TestMilterClient_CollectorCommit(t *testing.T) {
	mm := MockMilter{
		ConnResp:      RespContinue,
		HeloResp:      RespContinue,
		MailResp:      RespContinue,
		RcptResp:      RespContinue,
		HdrResp:       RespContinue,
		HdrsResp:      RespContinue,
		BodyChunkResp: RespContinue,
		BodyResp:      RespAccept,
		BodyMod: func(m *Modifier) {
			m.AddHeader("X-Spam", "yes")
			m.ChangeHeader(2, "Subject", "***SPAM***")
			m.AddRecipient("extra@example.org")
			m.ReplaceBody([]byte("rewritten"))
			m.Quarantine("bad")
		},
	}
	session := startCollectorTestSession(t, &mm)

	collector := NewResultCollector(CollectorOptions{})
	act, err := session.EndWithOptions(&EndOptions{Collector: collector})
	if err != nil {
		t.Fatal(err)
	}
	if act.Code != ActAccept {
		t.Fatal("Unexpected terminal action:", act.Code)
	}

	res := collector.Result()
	if !res.Committed {
		t.Fatalf("Expected committed result, got %+v", res)
	}
	if res.Err != nil {
		t.Fatal("Unexpected result error:", res.Err)
	}
	if res.Final == nil || res.Final.Code != ActAccept {
		t.Fatalf("Wrong final action: %+v", res.Final)
	}

	expected := []ModifyAction{
		{Code: ActAddHeader, HeaderName: "X-Spam", HeaderValue: "yes"},
		{Code: ActChangeHeader, HeaderIndex: 2, HeaderName: "Subject", HeaderValue: "***SPAM***"},
		{Code: ActAddRcpt, Rcpt: "<extra@example.org>"},
		{Code: ActReplBody, Body: []byte("rewritten")},
		{Code: ActQuarantine, Reason: "bad"},
	}
	if !reflect.DeepEqual(res.Actions, expected) {
		t.Fatalf("Wrong collected actions, got %+v", res.Actions)
	}
}

func TestMilterClient_CollectorReject(t *testing.T) {
	mm := MockMilter{
		ConnResp:      RespContinue,
		HeloResp:      RespContinue,
		MailResp:      RespContinue,
		RcptResp:      RespContinue,
		HdrResp:       RespContinue,
		HdrsResp:      RespContinue,
		BodyChunkResp: RespContinue,
		BodyResp:      RespReject,
		BodyMod: func(m *Modifier) {
			m.AddHeader("X-Spam", "yes")
			m.ChangeHeader(1, "Subject", "***SPAM***")
		},
	}
	session := startCollectorTestSession(t, &mm)

	collector := NewResultCollector(CollectorOptions{})
	act, err := session.EndWithOptions(&EndOptions{Collector: collector})
	if err != nil {
		t.Fatal(err)
	}
	if act.Code != ActReject {
		t.Fatal("Unexpected terminal action:", act.Code)
	}

	res := collector.Result()
	if res.Committed {
		t.Fatal("Expected uncommitted result for rejected message")
	}
	if res.Actions != nil {
		t.Fatalf("Uncommitted result must not deliver actions, got %+v", res.Actions)
	}
	if res.Err != nil {
		t.Fatal("Clean rejection should not set Err:", res.Err)
	}
	if res.Final == nil || res.Final.Code != ActReject {
		t.Fatalf("Wrong final action: %+v", res.Final)
	}
}

func TestMilterClient_CollectorActionBudget(t *testing.T) {
	mm := MockMilter{
		ConnResp:      RespContinue,
		HeloResp:      RespContinue,
		MailResp:      RespContinue,
		RcptResp:      RespContinue,
		HdrResp:       RespContinue,
		HdrsResp:      RespContinue,
		BodyChunkResp: RespContinue,
		BodyResp:      RespAccept,
		BodyMod: func(m *Modifier) {
			m.AddHeader("X-One", "1")
			m.AddHeader("X-Two", "2")
			m.AddHeader("X-Three", "3")
		},
	}
	session := startCollectorTestSession(t, &mm)

	collector := NewResultCollector(CollectorOptions{MaxActions: 2})
	act, err := session.EndWithOptions(&EndOptions{Collector: collector})
	// The connection must stay in sync: remaining actions are drained and
	// the terminal action is still read.
	if err != nil {
		t.Fatal(err)
	}
	if act.Code != ActAccept {
		t.Fatal("Unexpected terminal action:", act.Code)
	}

	res := collector.Result()
	if res.Committed {
		t.Fatal("Expected uncommitted result on budget overflow")
	}
	if res.Actions != nil {
		t.Fatalf("Uncommitted result must not deliver actions, got %+v", res.Actions)
	}
	if !errors.Is(res.Err, ErrTooManyActions) {
		t.Fatal("Expected ErrTooManyActions, got:", res.Err)
	}
}

func TestMilterClient_CollectorByteBudget(t *testing.T) {
	mm := MockMilter{
		ConnResp:      RespContinue,
		HeloResp:      RespContinue,
		MailResp:      RespContinue,
		RcptResp:      RespContinue,
		HdrResp:       RespContinue,
		HdrsResp:      RespContinue,
		BodyChunkResp: RespContinue,
		BodyResp:      RespAccept,
		BodyMod: func(m *Modifier) {
			m.ReplaceBody(bytes.Repeat([]byte{'A'}, 4096))
		},
	}
	session := startCollectorTestSession(t, &mm)

	collector := NewResultCollector(CollectorOptions{MaxBytes: 1024})
	act, err := session.EndWithOptions(&EndOptions{Collector: collector})
	if err != nil {
		t.Fatal(err)
	}
	if act.Code != ActAccept {
		t.Fatal("Unexpected terminal action:", act.Code)
	}

	res := collector.Result()
	if res.Committed {
		t.Fatal("Expected uncommitted result on budget overflow")
	}
	if res.Actions != nil {
		t.Fatalf("Uncommitted result must not deliver actions, got %+v", res.Actions)
	}
	if !errors.Is(res.Err, ErrTooManyBytes) {
		t.Fatal("Expected ErrTooManyBytes, got:", res.Err)
	}
}

func TestMilterClient_CollectorDisconnect(t *testing.T) {
	mm := MockMilter{
		ConnResp:      RespContinue,
		HeloResp:      RespContinue,
		MailResp:      RespContinue,
		RcptResp:      RespContinue,
		HdrResp:       RespContinue,
		HdrsResp:      RespContinue,
		BodyChunkResp: RespContinue,
		BodyMod: func(m *Modifier) {
			m.AddHeader("X-Spam", "yes")
		},
		// Returning an error makes the server close the connection after
		// the modify action above was already sent.
		BodyErr: errors.New("boom"),
	}
	session := startCollectorTestSession(t, &mm)

	collector := NewResultCollector(CollectorOptions{})
	_, err := session.EndWithOptions(&EndOptions{Collector: collector})
	if err == nil {
		t.Fatal("Expected error on connection loss")
	}

	res := collector.Result()
	if res.Committed {
		t.Fatal("Expected uncommitted result on connection loss")
	}
	if res.Actions != nil {
		t.Fatalf("Uncommitted result must not deliver actions, got %+v", res.Actions)
	}
	if res.Err == nil {
		t.Fatal("Expected Err to explain the failure")
	}
	if res.Final != nil {
		t.Fatalf("No terminal action should be recorded, got %+v", res.Final)
	}
}

func TestMilterClient_StreamingCallback(t *testing.T) {
	mm := MockMilter{
		ConnResp:      RespContinue,
		HeloResp:      RespContinue,
		MailResp:      RespContinue,
		RcptResp:      RespContinue,
		HdrResp:       RespContinue,
		HdrsResp:      RespContinue,
		BodyChunkResp: RespContinue,
		BodyResp:      RespReject,
		BodyMod: func(m *Modifier) {
			m.AddHeader("X-Spam", "yes")
			m.AddRecipient("extra@example.org")
		},
	}
	session := startCollectorTestSession(t, &mm)

	var streamed []ModifyAction
	act, err := session.EndWithOptions(&EndOptions{
		OnModifyAction: func(modifyAct ModifyAction) error {
			streamed = append(streamed, modifyAct)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if act.Code != ActReject {
		t.Fatal("Unexpected terminal action:", act.Code)
	}

	// Streaming mode delivers actions as they arrive, even though the
	// message was ultimately rejected.
	expected := []ModifyAction{
		{Code: ActAddHeader, HeaderName: "X-Spam", HeaderValue: "yes"},
		{Code: ActAddRcpt, Rcpt: "<extra@example.org>"},
	}
	if !reflect.DeepEqual(streamed, expected) {
		t.Fatalf("Wrong streamed actions, got %+v", streamed)
	}
}
