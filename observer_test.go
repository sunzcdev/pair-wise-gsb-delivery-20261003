package milter

import (
	"errors"
	"net"
	nettextproto "net/textproto"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-message/textproto"
)

func startObservedSession(t *testing.T, s *Server) *ClientSession {
	t.Helper()
	defer s.Close()
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(local)

	cl := NewClientWithOptions("tcp", local.Addr().String(), ClientOptions{
		ActionMask: OptAddHeader | OptChangeHeader | OptQuarantine,
	})
	defer cl.Close()
	session, err := cl.Session()
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func waitForCondition(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// loggingMilter records the order in which callbacks are invoked.
type loggingMilter struct {
	mu  *sync.Mutex
	log *[]string

	bodyMod  func(m *Modifier)
	bodyResp Response
}

func (m *loggingMilter) record(cb string) {
	m.mu.Lock()
	*m.log = append(*m.log, cb)
	m.mu.Unlock()
}

func (m *loggingMilter) Connect(host string, family string, port uint16, addr net.IP, mod *Modifier) (Response, error) {
	m.record("Connect")
	return RespContinue, nil
}

func (m *loggingMilter) Helo(name string, mod *Modifier) (Response, error) {
	m.record("Helo")
	return RespContinue, nil
}

func (m *loggingMilter) MailFrom(from string, mod *Modifier) (Response, error) {
	m.record("MailFrom")
	return RespContinue, nil
}

func (m *loggingMilter) RcptTo(rcptTo string, mod *Modifier) (Response, error) {
	m.record("RcptTo")
	return RespContinue, nil
}

func (m *loggingMilter) Header(name string, value string, mod *Modifier) (Response, error) {
	m.record("Header")
	return RespContinue, nil
}

func (m *loggingMilter) Headers(h nettextproto.MIMEHeader, mod *Modifier) (Response, error) {
	m.record("Headers")
	return RespContinue, nil
}

func (m *loggingMilter) BodyChunk(chunk []byte, mod *Modifier) (Response, error) {
	m.record("BodyChunk")
	return RespContinue, nil
}

func (m *loggingMilter) Body(mod *Modifier) (Response, error) {
	m.record("Body")
	if m.bodyMod != nil {
		m.bodyMod(mod)
	}
	if m.bodyResp != nil {
		return m.bodyResp, nil
	}
	return RespAccept, nil
}

func (m *loggingMilter) Abort(mod *Modifier) error {
	m.record("Abort")
	return nil
}

func TestObserverMetadataOnly(t *testing.T) {
	rec := NewRecorder(false)
	s := Server{
		NewMilter: func() Milter {
			return NoOpMilter{}
		},
		Actions:  OptAddHeader | OptChangeHeader,
		Observer: rec,
	}
	session := startObservedSession(t, &s)
	defer session.Close()

	assertAction := func(act *Action, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if act.Code != ActContinue {
			t.Fatal("Unexpected code:", act.Code)
		}
	}

	act, err := session.Conn("host", FamilyInet, 25565, "172.0.0.1")
	assertAction(act, err)
	act, err = session.Helo("helo_host")
	assertAction(act, err)
	act, err = session.Mail("from@example.org", nil)
	assertAction(act, err)
	act, err = session.Rcpt("to@example.org", nil)
	assertAction(act, err)
	hdr := textproto.Header{}
	hdr.Add("Subject", "hi")
	act, err = session.Header(hdr)
	assertAction(act, err)
	_, act, err = session.BodyReadFrom(strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	if act.Code != ActAccept {
		t.Fatal("Unexpected code:", act.Code)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	waitForCondition(t, func() bool {
		events := rec.Events()
		return len(events) > 0 && events[len(events)-1].Code == byte(CodeQuit)
	})

	type dirCode struct {
		dir  Direction
		code byte
	}
	expected := []dirCode{
		{DirectionInbound, 'O'}, {DirectionOutbound, 'O'},
		{DirectionInbound, 'C'}, {DirectionOutbound, 'c'},
		{DirectionInbound, 'H'}, {DirectionOutbound, 'c'},
		{DirectionInbound, 'M'}, {DirectionOutbound, 'c'},
		{DirectionInbound, 'R'}, {DirectionOutbound, 'c'},
		{DirectionInbound, 'L'}, {DirectionOutbound, 'c'},
		{DirectionInbound, 'N'}, {DirectionOutbound, 'c'},
		{DirectionInbound, 'B'}, {DirectionOutbound, 'c'},
		{DirectionInbound, 'E'}, {DirectionOutbound, 'a'},
		{DirectionInbound, 'A'}, // sent by ClientSession.Close (needAbort)
		{DirectionInbound, 'Q'},
	}

	events := rec.Events()
	if len(events) != len(expected) {
		t.Fatalf("Wrong event count: got %d, want %d (%v)", len(events), len(expected), events)
	}
	for i, ev := range events {
		if ev.Direction != expected[i].dir || ev.Code != expected[i].code {
			t.Fatalf("Event %d: got %v %q, want %v %q", i, ev.Direction, ev.Code, expected[i].dir, expected[i].code)
		}
		if ev.Data != nil {
			t.Fatalf("Event %d: payload captured in metadata-only mode: %q", i, ev.Data)
		}
		if ev.Direction == DirectionInbound && ev.Final {
			t.Fatalf("Event %d: inbound event marked final", i)
		}
	}

	// Negotiated capabilities are reported on the optneg response.
	optneg := events[1]
	if optneg.Actions != s.Actions || optneg.Protocol != s.Protocol {
		t.Fatal("Wrong negotiated capabilities:", optneg.Actions, optneg.Protocol)
	}

	// Only the final accept response is marked as terminal.
	for i, ev := range events {
		wantFinal := ev.Direction == DirectionOutbound && ev.Code == byte(ActAccept)
		if ev.Final != wantFinal {
			t.Fatalf("Event %d (%q): wrong Final flag: %v", i, ev.Code, ev.Final)
		}
	}

	// Lengths cover the command byte plus payload.
	if l := events[len(events)-1].Length; l != 1 {
		t.Fatal("Wrong QUIT length:", l)
	}
	if l := events[14].Length; l != 2 { // 'B' + "x"
		t.Fatal("Wrong body chunk length:", l)
	}
}

func TestObserverReplayAfterAbort(t *testing.T) {
	rec := NewRecorder(true)

	var liveMu sync.Mutex
	var liveLog []string
	newLoggingMilter := func(mu *sync.Mutex, log *[]string) func() Milter {
		return func() Milter {
			return &loggingMilter{
				mu:  mu,
				log: log,
				bodyMod: func(m *Modifier) {
					m.AddHeader("X-Seen", "yes")
				},
				bodyResp: RespAccept,
			}
		}
	}

	s := Server{
		NewMilter: newLoggingMilter(&liveMu, &liveLog),
		Actions:   OptAddHeader,
		Observer:  rec,
	}
	session := startObservedSession(t, &s)
	defer session.Close()

	assertAction := func(act *Action, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if act.Code != ActContinue {
			t.Fatal("Unexpected code:", act.Code)
		}
	}

	act, err := session.Conn("host", FamilyInet, 25565, "172.0.0.1")
	assertAction(act, err)
	if err := session.Macros(CodeHelo, "tls_version", "very old"); err != nil {
		t.Fatal(err)
	}
	act, err = session.Helo("helo_host")
	assertAction(act, err)

	// First message, aborted in the middle.
	act, err = session.Mail("one@example.org", nil)
	assertAction(act, err)
	act, err = session.Rcpt("two@example.org", nil)
	assertAction(act, err)
	if err := session.Abort(); err != nil {
		t.Fatal(err)
	}

	// Second message after the abort, delivered to the end.
	act, err = session.Mail("three@example.org", nil)
	assertAction(act, err)
	act, err = session.Rcpt("four@example.org", nil)
	assertAction(act, err)
	hdr := textproto.Header{}
	hdr.Add("Subject", "second")
	act, err = session.Header(hdr)
	assertAction(act, err)
	modActs, act, err := session.BodyReadFrom(strings.NewReader("hello body"))
	if err != nil {
		t.Fatal(err)
	}
	if act.Code != ActAccept {
		t.Fatal("Unexpected code:", act.Code)
	}
	if len(modActs) != 1 || modActs[0].Code != ActAddHeader || modActs[0].HeaderName != "X-Seen" {
		t.Fatalf("Unexpected modify actions: %+v", modActs)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	waitForCondition(t, func() bool {
		events := rec.Events()
		return len(events) > 0 && events[len(events)-1].Code == byte(CodeQuit)
	})

	expectedLog := []string{
		"Connect", "Helo",
		"MailFrom", "RcptTo", "Abort",
		"MailFrom", "RcptTo", "Header", "Headers", "BodyChunk", "Body",
		"Abort", // sent by ClientSession.Close (needAbort)
	}
	liveMu.Lock()
	if !reflect.DeepEqual(liveLog, expectedLog) {
		t.Fatalf("Wrong live callback order: %v", liveLog)
	}
	liveMu.Unlock()

	// Replay the full recording into a fresh state machine.
	var replayMu sync.Mutex
	var replayLog []string
	replayServer := &Server{
		NewMilter: newLoggingMilter(&replayMu, &replayLog),
		Actions:   OptAddHeader,
	}
	if diff := Replay(rec.Events(), replayServer); diff != nil {
		t.Fatal("Replay diverged:", diff)
	}

	replayMu.Lock()
	defer replayMu.Unlock()
	if !reflect.DeepEqual(replayLog, expectedLog) {
		t.Fatalf("Replay did not reproduce callback order: got %v, want %v", replayLog, expectedLog)
	}
}

type failingObserver struct {
	err   error
	panic bool
}

func (f failingObserver) CapturePayload() bool { return false }

func (f failingObserver) ObserveEvent(ev ObservedEvent) error {
	if f.panic {
		panic("observer boom")
	}
	return f.err
}

func TestObserverFailurePolicy(t *testing.T) {
	run := func(t *testing.T, obs Observer, policy ObserverFailurePolicy) error {
		s := Server{
			NewMilter: func() Milter {
				return NoOpMilter{}
			},
			Observer:              obs,
			ObserverFailurePolicy: policy,
		}
		defer s.Close()
		local, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go s.Serve(local)

		cl := NewClientWithOptions("tcp", local.Addr().String(), ClientOptions{
			ActionMask: OptAddHeader,
		})
		defer cl.Close()
		session, err := cl.Session()
		if err != nil {
			return err
		}
		defer session.Close()
		act, err := session.Conn("host", FamilyInet, 25565, "172.0.0.1")
		if err != nil {
			return err
		}
		if act.Code != ActContinue {
			t.Fatal("Unexpected code:", act.Code)
		}
		return nil
	}

	// Default policy: failures are logged, the session keeps going.
	if err := run(t, failingObserver{err: errors.New("boom")}, ObserverFailureLog); err != nil {
		t.Fatal("Session failed despite log-only policy:", err)
	}
	// Panics are treated as failures, not crashes.
	if err := run(t, failingObserver{panic: true}, ObserverFailureLog); err != nil {
		t.Fatal("Session failed despite log-only policy (panicking observer):", err)
	}
	// Close policy: the affected session is terminated.
	if err := run(t, failingObserver{err: errors.New("boom")}, ObserverFailureClose); err == nil {
		t.Fatal("Session unexpectedly survived close-on-failure policy")
	}
}

func TestReplayDetectsDivergence(t *testing.T) {
	rec := NewRecorder(true)
	s := Server{
		NewMilter: func() Milter {
			return NoOpMilter{}
		},
		Observer: rec,
	}
	session := startObservedSession(t, &s)
	defer session.Close()

	if _, err := session.Conn("host", FamilyInet, 25565, "172.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Helo("helo_host"); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	waitForCondition(t, func() bool {
		events := rec.Events()
		return len(events) > 0 && events[len(events)-1].Code == byte(CodeQuit)
	})

	// A backend that rejects HELO must diverge from the recorded continue.
	rejecting := &Server{
		NewMilter: func() Milter {
			return &MockMilter{ConnResp: RespContinue, HeloResp: RespReject}
		},
	}
	diff := Replay(rec.Events(), rejecting)
	if diff == nil {
		t.Fatal("Expected replay divergence, got nil")
	}
	if diff.Expected == nil || diff.Expected.Code != byte(ActContinue) {
		t.Fatalf("Wrong expected packet in diff: %+v", diff)
	}
	if diff.Actual == nil || diff.Actual.Code != byte(ActReject) {
		t.Fatalf("Wrong actual packet in diff: %+v", diff)
	}
	t.Log("First divergence:", diff)
}

func TestReplayRequiresPayload(t *testing.T) {
	events := []ObservedEvent{
		{Direction: DirectionInbound, Code: byte(CodeOptNeg), Length: 13},
	}
	s := &Server{
		NewMilter: func() Milter { return NoOpMilter{} },
	}
	diff := Replay(events, s)
	if diff == nil {
		t.Fatal("Expected replay to refuse metadata-only recording")
	}
	if !strings.Contains(diff.Reason, "payload not captured") {
		t.Fatal("Unexpected diff reason:", diff.Reason)
	}
}
