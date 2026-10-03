package milter

import (
	"encoding/binary"
	"errors"
	"net"
	"testing"
)

// rawMilter is a minimal milter server speaking the wire protocol directly.
// It performs a fixed OPTNEG reply, consumes the MTA command stream up to EOB
// and then writes the packets supplied in eobReplies.
type rawMilter struct {
	listener net.Listener
	// replies are raw message payloads (including the command byte) returned
	// in order after EOB.
	eobReplies [][]byte
	// closeBeforeReply, when set, makes the server close the connection
	// immediately after receiving EOB without sending anything.
	closeBeforeReply bool
}

func serveRawMilter(t *testing.T, rm *rawMilter) {
	t.Helper()
	go func() {
		conn, err := rm.listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		msg, err := readPacket(conn, 0)
		if err != nil {
			return
		}
		if Code(msg.Code) != CodeOptNeg {
			return
		}
		optneg := make([]byte, 4*3)
		binary.BigEndian.PutUint32(optneg, 6)
		binary.BigEndian.PutUint32(optneg[4:], 0xffffffff)
		binary.BigEndian.PutUint32(optneg[8:], 0)
		if err := writePacket(conn, &Message{Code: byte(CodeOptNeg), Data: optneg}, 0); err != nil {
			return
		}

		for {
			msg, err := readPacket(conn, 0)
			if err != nil {
				return
			}
			switch Code(msg.Code) {
			case CodeEOB:
				if rm.closeBeforeReply {
					return
				}
				for _, p := range rm.eobReplies {
					if err := writePacket(conn, &Message{Code: p[0], Data: p[1:]}, 0); err != nil {
						return
					}
				}
				for {
					if _, err := readPacket(conn, 0); err != nil {
						return
					}
				}
			case CodeMacro:
				continue
			case CodeQuit:
				return
			default:
				if err := writePacket(conn, &Message{Code: byte(ActContinue)}, 0); err != nil {
					return
				}
			}
		}
	}()
}

func newRawClientSession(t *testing.T, rm *rawMilter) *ClientSession {
	t.Helper()
	var err error
	rm.listener, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveRawMilter(t, rm)

	cl := NewClientWithOptions("tcp", rm.listener.Addr().String(), ClientOptions{})
	session, err := cl.Session()
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func pktAddHeader(name, value string) []byte {
	p := []byte{byte(ActAddHeader)}
	p = appendCString(p, name)
	p = appendCString(p, value)
	return p
}

func pktChangeHeader(index uint32, name, value string) []byte {
	p := []byte{byte(ActChangeHeader)}
	var idx [4]byte
	binary.BigEndian.PutUint32(idx[:], index)
	p = append(p, idx[:]...)
	p = appendCString(p, name)
	p = appendCString(p, value)
	return p
}

func pktAddRcpt(rcpt string) []byte {
	p := []byte{byte(ActAddRcpt)}
	p = appendCString(p, rcpt)
	return p
}

func pktReplBody(body []byte) []byte {
	return append([]byte{byte(ActReplBody)}, body...)
}

func pkt(code byte) []byte {
	return []byte{code}
}

func TestEndWithCollector_AcceptDelivers(t *testing.T) {
	rm := &rawMilter{
		eobReplies: [][]byte{
			pktAddHeader("X-A", "1"),
			pktChangeHeader(2, "Received", "replaced"),
			pktAddRcpt("extra@example.org"),
			pktReplBody([]byte("new body")),
			pkt(byte(ActAccept)),
		},
	}
	session := newRawClientSession(t, rm)
	defer session.Close()

	c := &ResultCollector{}
	act, err := session.EndWithCollector(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if act.Code != ActAccept {
		t.Fatalf("expected accept, got %v", act.Code)
	}

	if len(c.Actions) != 4 {
		t.Fatalf("expected 4 actions, got %d", len(c.Actions))
	}
	if c.Actions[0].Code != ActAddHeader || c.Actions[0].HeaderName != "X-A" {
		t.Fatalf("wrong first action: %+v", c.Actions[0])
	}
	if c.Actions[1].Code != ActChangeHeader || c.Actions[1].HeaderIndex != 2 ||
		c.Actions[1].HeaderName != "Received" || c.Actions[1].HeaderValue != "replaced" {
		t.Fatalf("wrong second action: %+v", c.Actions[1])
	}
	if c.Actions[2].Code != ActAddRcpt || c.Actions[2].Rcpt != "extra@example.org" {
		t.Fatalf("wrong third action: %+v", c.Actions[2])
	}
	if c.Actions[3].Code != ActReplBody || string(c.Actions[3].Body) != "new body" {
		t.Fatalf("wrong fourth action: %+v", c.Actions[3])
	}

	wantBytes := 0
	for _, p := range rm.eobReplies[:len(rm.eobReplies)-1] {
		wantBytes += len(p)
	}
	if c.TotalBytes() != wantBytes {
		t.Fatalf("expected %d total bytes, got %d", wantBytes, c.TotalBytes())
	}
}

func TestEndWithCollector_OrderPreserved(t *testing.T) {
	rm := &rawMilter{
		eobReplies: [][]byte{
			pktAddRcpt("a@example.org"),
			pktAddHeader("X-A", "1"),
			pktAddRcpt("b@example.org"),
			pktAddHeader("X-B", "2"),
			pkt(byte(ActDiscard)),
		},
	}
	session := newRawClientSession(t, rm)
	defer session.Close()

	c := &ResultCollector{}
	act, err := session.EndWithCollector(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if act.Code != ActDiscard {
		t.Fatalf("expected discard (success terminal), got %v", act.Code)
	}

	codes := make([]ModifyActCode, len(c.Actions))
	for i, a := range c.Actions {
		codes[i] = a.Code
	}
	want := []ModifyActCode{ActAddRcpt, ActAddHeader, ActAddRcpt, ActAddHeader}
	for i := range want {
		if codes[i] != want[i] {
			t.Fatalf("order not preserved: got %v want %v", codes, want)
		}
	}
}

func TestEndWithCollector_RejectNotCommitted(t *testing.T) {
	for _, terminal := range []byte{byte(ActReject), byte(ActTempFail)} {
		rm := &rawMilter{
			eobReplies: [][]byte{
				pktAddHeader("X-A", "1"),
				pktAddRcpt("extra@example.org"),
				pkt(terminal),
			},
		}
		session := newRawClientSession(t, rm)

		c := &ResultCollector{}
		act, err := session.EndWithCollector(c)
		if !errors.Is(err, ErrResultNotCommitted) {
			t.Fatalf("expected ErrResultNotCommitted, got %v", err)
		}
		if act == nil || act.Code != ActionCode(terminal) {
			t.Fatalf("expected terminal action to be returned, got %+v", act)
		}
		if len(c.Actions) != 0 {
			t.Fatalf("buffered actions must not be delivered on rejection, got %d", len(c.Actions))
		}
		session.Close()
	}
}

func TestEndWithCollector_ByteBudget(t *testing.T) {
	first := pktAddHeader("X-A", "1")
	second := pktAddRcpt("extra@example.org")

	rm := &rawMilter{
		eobReplies: [][]byte{
			first,
			second,
			pkt(byte(ActAccept)),
		},
	}
	session := newRawClientSession(t, rm)
	defer session.Close()

	c := &ResultCollector{MaxBytes: len(first)}
	_, err := session.EndWithCollector(c)
	if !errors.Is(err, ErrResultTooManyBytes) {
		t.Fatalf("expected ErrResultTooManyBytes, got %v", err)
	}
	if !errors.Is(err, ErrResultNotCommitted) {
		t.Fatalf("error must wrap ErrResultNotCommitted, got %v", err)
	}
	if len(c.Actions) != 0 {
		t.Fatalf("no actions must be delivered on budget overflow, got %d", len(c.Actions))
	}
	if c.TotalBytes() != 0 {
		t.Fatalf("collector must be reset on overflow, got %d bytes", c.TotalBytes())
	}
}

func TestEndWithCollector_ActionBudget(t *testing.T) {
	rm := &rawMilter{
		eobReplies: [][]byte{
			pktAddHeader("X-A", "1"),
			pktAddHeader("X-B", "2"),
			pkt(byte(ActAccept)),
		},
	}
	session := newRawClientSession(t, rm)
	defer session.Close()

	c := &ResultCollector{MaxActions: 1}
	_, err := session.EndWithCollector(c)
	if !errors.Is(err, ErrResultTooManyActions) {
		t.Fatalf("expected ErrResultTooManyActions, got %v", err)
	}
	if len(c.Actions) != 0 {
		t.Fatalf("no actions must be delivered on count overflow, got %d", len(c.Actions))
	}
}

func TestEndWithCollector_BudgetExactFit(t *testing.T) {
	first := pktAddHeader("X-A", "1")
	second := pktAddRcpt("extra@example.org")
	total := len(first) + len(second)

	rm := &rawMilter{
		eobReplies: [][]byte{
			first,
			second,
			pkt(byte(ActAccept)),
		},
	}
	session := newRawClientSession(t, rm)
	defer session.Close()

	c := &ResultCollector{MaxBytes: total, MaxActions: 2}
	if _, err := session.EndWithCollector(c); err != nil {
		t.Fatalf("exact-fit budget must succeed, got %v", err)
	}
	if len(c.Actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(c.Actions))
	}
}

func TestEndWithCollector_IllegalChangeHeaderIndexZero(t *testing.T) {
	rm := &rawMilter{
		eobReplies: [][]byte{
			pktAddHeader("X-A", "1"),
			pktChangeHeader(0, "Subject", "spam"),
			pkt(byte(ActAccept)),
		},
	}
	session := newRawClientSession(t, rm)
	defer session.Close()

	c := &ResultCollector{}
	_, err := session.EndWithCollector(c)
	if !errors.Is(err, ErrResultNotCommitted) {
		t.Fatalf("expected ErrResultNotCommitted for index 0, got %v", err)
	}
	if len(c.Actions) != 0 {
		t.Fatalf("illegal action must not commit earlier actions, got %d", len(c.Actions))
	}
}

func TestEndWithCollector_DisconnectNotCommitted(t *testing.T) {
	rm := &rawMilter{closeBeforeReply: true}
	session := newRawClientSession(t, rm)
	defer session.Close()

	c := &ResultCollector{}
	_, err := session.EndWithCollector(c)
	if !errors.Is(err, ErrResultNotCommitted) {
		t.Fatalf("expected ErrResultNotCommitted on disconnect, got %v", err)
	}
	if len(c.Actions) != 0 {
		t.Fatalf("disconnect must not deliver actions, got %d", len(c.Actions))
	}
}

func TestEndStream_CalledInOrder(t *testing.T) {
	rm := &rawMilter{
		eobReplies: [][]byte{
			pktAddHeader("X-A", "1"),
			pktAddRcpt("extra@example.org"),
			pkt(byte(ActAccept)),
		},
	}
	session := newRawClientSession(t, rm)
	defer session.Close()

	var got []ModifyActCode
	act, err := session.EndStream(func(a ModifyAction) error {
		got = append(got, a.Code)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if act.Code != ActAccept {
		t.Fatalf("expected accept, got %v", act.Code)
	}
	want := []ModifyActCode{ActAddHeader, ActAddRcpt}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("streaming callback order wrong: got %v want %v", got, want)
		}
	}
}

// TestEndStream_SeesActionsEvenOnReject documents the streaming-mode side
// effect boundary: callbacks fire before the terminal verdict is known.
func TestEndStream_SeesActionsEvenOnReject(t *testing.T) {
	rm := &rawMilter{
		eobReplies: [][]byte{
			pktAddHeader("X-A", "1"),
			pkt(byte(ActReject)),
		},
	}
	session := newRawClientSession(t, rm)
	defer session.Close()

	count := 0
	act, err := session.EndStream(func(a ModifyAction) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if act.Code != ActReject {
		t.Fatalf("expected reject, got %v", act.Code)
	}
	if count != 1 {
		t.Fatalf("streaming mode must deliver the action despite rejection, got %d", count)
	}
}

// TestEnd_StreamingModeUnbuffered documents the legacy End behavior: all
// actions are returned regardless of the terminal verdict and without budget
// enforcement.
func TestEnd_StreamingModeUnbuffered(t *testing.T) {
	rm := &rawMilter{
		eobReplies: [][]byte{
			pktAddHeader("X-A", "1"),
			pkt(byte(ActTempFail)),
		},
	}
	session := newRawClientSession(t, rm)
	defer session.Close()

	acts, act, err := session.End()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if act.Code != ActTempFail {
		t.Fatalf("expected tempfail, got %v", act.Code)
	}
	if len(acts) != 1 || acts[0].Code != ActAddHeader {
		t.Fatalf("streaming End must return action despite tempfail, got %+v", acts)
	}
}

func TestEndWithCollector_MalformedPacketNotCommitted(t *testing.T) {
	// ActAddHeader requires a NUL-delimited name and value; here there is no
	// NUL at all, so the parser cannot split name from value.
	malformed := []byte{byte(ActAddHeader), 'X', 'Y'}

	rm := &rawMilter{
		eobReplies: [][]byte{
			malformed,
			pkt(byte(ActAccept)),
		},
	}
	session := newRawClientSession(t, rm)
	defer session.Close()

	c := &ResultCollector{}
	_, err := session.EndWithCollector(c)
	if !errors.Is(err, ErrResultNotCommitted) {
		t.Fatalf("expected ErrResultNotCommitted for malformed action, got %v", err)
	}
	if len(c.Actions) != 0 {
		t.Fatalf("malformed action must not deliver results, got %d", len(c.Actions))
	}
}

func TestEndWithCollector_ContinueAtEOBCommits(t *testing.T) {
	rm := &rawMilter{
		eobReplies: [][]byte{
			pktAddHeader("X-A", "1"),
			pkt(byte(ActContinue)),
		},
	}
	session := newRawClientSession(t, rm)
	defer session.Close()

	c := &ResultCollector{}
	act, err := session.EndWithCollector(c)
	if err != nil {
		t.Fatalf("continue at EOB functions as accept, got %v", err)
	}
	if act.Code != ActContinue {
		t.Fatalf("expected continue, got %v", act.Code)
	}
	if len(c.Actions) != 1 {
		t.Fatalf("expected action to be committed on continue, got %d", len(c.Actions))
	}
}

func TestEndStream_CallbackErrorStops(t *testing.T) {
	stopErr := errors.New("caller stopped")
	rm := &rawMilter{
		eobReplies: [][]byte{
			pktAddHeader("X-A", "1"),
			pktAddHeader("X-B", "2"),
			pkt(byte(ActAccept)),
		},
	}
	session := newRawClientSession(t, rm)
	defer session.Close()

	count := 0
	_, err := session.EndStream(func(a ModifyAction) error {
		count++
		return stopErr
	})
	if !errors.Is(err, stopErr) {
		t.Fatalf("expected caller error to propagate, got %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one callback before the error, got %d", count)
	}
}
