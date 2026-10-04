package milter

import (
	"errors"
	"net"
	"net/textproto"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func waitForLastEvent(t *testing.T, r *Recorder, pred func(Event) bool) []Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		events := r.Events()
		if len(events) > 0 && pred(events[len(events)-1]) {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for event; got %d events", len(events))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func isQuit(e Event) bool {
	return e.Direction == DirectionInbound && Code(e.Code) == CodeQuit
}

func TestObserver_MetadataOnly(t *testing.T) {
	mm := MockMilter{
		ConnResp:      RespContinue,
		HeloResp:      RespContinue,
		MailResp:      RespContinue,
		RcptResp:      RespContinue,
		HdrResp:       RespContinue,
		HdrsResp:      RespContinue,
		BodyChunkResp: RespContinue,
		BodyResp:      RespAccept,
	}
	recorder := &Recorder{}
	s := Server{
		NewMilter: func() Milter { return &mm },
		Actions:   OptAddHeader,
		Observer:  recorder,
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
		t.Fatal(err)
	}

	if _, err := session.Conn("host", FamilyInet, 25565, "172.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Helo("helo_host"); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Mail("from@example.org", nil); err != nil {
		t.Fatal(err)
	}
	chunk := []byte("some body chunk")
	if _, err := session.BodyChunk(chunk); err != nil {
		t.Fatal(err)
	}
	if _, _, err := session.End(); err != nil {
		t.Fatal(err)
	}
	session.Close()

	events := waitForLastEvent(t, recorder, isQuit)

	// Default recording keeps no payload data: no bodies, addresses or
	// macro values.
	for i, e := range events {
		if e.Data != nil {
			t.Fatalf("event %d keeps payload data in metadata mode: %q", i, e.Data)
		}
		if e.Length == 0 {
			t.Fatalf("event %d has no length", i)
		}
	}

	// Negotiated capabilities are recorded for both OPTNEG packets.
	if len(events) < 2 {
		t.Fatal("not enough events recorded")
	}
	req, resp := events[0], events[1]
	if req.Direction != DirectionInbound || Code(req.Code) != CodeOptNeg {
		t.Fatalf("unexpected first event: %v %q", req.Direction, req.Code)
	}
	if req.Version != 6 || req.Actions != OptAddHeader {
		t.Fatalf("wrong client capabilities: version=%d actions=%v", req.Version, req.Actions)
	}
	if resp.Direction != DirectionOutbound || Code(resp.Code) != CodeOptNeg {
		t.Fatalf("unexpected second event: %v %q", resp.Direction, resp.Code)
	}
	if resp.Version != serverProtocolVersion || resp.Actions != OptAddHeader || resp.Protocol != 0 {
		t.Fatalf("wrong server capabilities: version=%d actions=%v protocol=%v",
			resp.Version, resp.Actions, resp.Protocol)
	}

	// Response final state is recorded.
	var sawAccept, sawContinue bool
	for _, e := range events {
		if e.Direction != DirectionOutbound {
			continue
		}
		switch ActionCode(e.Code) {
		case ActAccept:
			sawAccept = true
			if !e.Final {
				t.Fatal("accept response not marked final")
			}
		case ActContinue:
			sawContinue = true
			if e.Final {
				t.Fatal("continue response marked final")
			}
		}
	}
	if !sawAccept || !sawContinue {
		t.Fatalf("missing responses in recording: accept=%t continue=%t", sawAccept, sawContinue)
	}

	// Packet lengths are recorded.
	var sawBody bool
	for _, e := range events {
		if e.Direction == DirectionInbound && Code(e.Code) == CodeBody {
			sawBody = true
			if e.Length != uint32(len(chunk))+1 {
				t.Fatalf("wrong body chunk length: %d", e.Length)
			}
		}
	}
	if !sawBody {
		t.Fatal("body chunk not recorded")
	}
}

// callbackLogMilter records the callback invocation order.
type callbackLogMilter struct {
	NoOpMilter
	log *[]string
}

func (m *callbackLogMilter) record(name string) {
	*m.log = append(*m.log, name)
}

func (m *callbackLogMilter) Connect(host string, family string, port uint16, addr net.IP, mod *Modifier) (Response, error) {
	m.record("Connect")
	return RespContinue, nil
}

func (m *callbackLogMilter) Helo(name string, mod *Modifier) (Response, error) {
	m.record("Helo")
	return RespContinue, nil
}

func (m *callbackLogMilter) MailFrom(from string, mod *Modifier) (Response, error) {
	m.record("MailFrom")
	return RespContinue, nil
}

func (m *callbackLogMilter) RcptTo(rcptTo string, mod *Modifier) (Response, error) {
	m.record("RcptTo")
	return RespContinue, nil
}

func (m *callbackLogMilter) Header(name string, value string, mod *Modifier) (Response, error) {
	m.record("Header")
	return RespContinue, nil
}

func (m *callbackLogMilter) Headers(h textproto.MIMEHeader, mod *Modifier) (Response, error) {
	m.record("Headers")
	return RespContinue, nil
}

func (m *callbackLogMilter) BodyChunk(chunk []byte, mod *Modifier) (Response, error) {
	m.record("BodyChunk")
	return RespContinue, nil
}

func (m *callbackLogMilter) Body(mod *Modifier) (Response, error) {
	m.record("Body")
	if err := mod.AddHeader("X-Seen", "yes"); err != nil {
		return nil, err
	}
	if err := mod.Quarantine("suspicious"); err != nil {
		return nil, err
	}
	return RespAccept, nil
}

func (m *callbackLogMilter) Abort(mod *Modifier) error {
	m.record("Abort")
	return nil
}

func TestObserver_ReplayAbortThenSecondMessage(t *testing.T) {
	newBackend := func(log *[]string) func() Milter {
		return func() Milter {
			return &callbackLogMilter{log: log}
		}
	}

	var liveLog []string
	recorder := &Recorder{Full: true}
	s := Server{
		NewMilter: newBackend(&liveLog),
		Actions:   OptAddHeader | OptQuarantine,
		Observer:  recorder,
	}
	defer s.Close()
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(local)

	cl := NewClientWithOptions("tcp", local.Addr().String(), ClientOptions{
		ActionMask: OptAddHeader | OptQuarantine,
	})
	defer cl.Close()
	session, err := cl.Session()
	if err != nil {
		t.Fatal(err)
	}

	assertContinue := func(act *Action, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if act.Code != ActContinue {
			t.Fatal("unexpected action:", act.Code)
		}
	}

	act, err := session.Conn("host", FamilyInet, 25565, "172.0.0.1")
	assertContinue(act, err)
	if err := session.Macros(CodeHelo, "tls_version", "very old"); err != nil {
		t.Fatal(err)
	}
	act, err = session.Helo("helo_host")
	assertContinue(act, err)

	// First message, aborted after the body started.
	act, err = session.Mail("from@example.org", nil)
	assertContinue(act, err)
	act, err = session.Rcpt("to@example.org", nil)
	assertContinue(act, err)
	act, err = session.HeaderField("Subject", "first")
	assertContinue(act, err)
	act, err = session.HeaderEnd()
	assertContinue(act, err)
	act, err = session.BodyChunk([]byte("first body"))
	assertContinue(act, err)
	if err := session.Abort(); err != nil {
		t.Fatal(err)
	}

	// Second message on the same session, delivered to the end.
	act, err = session.Mail("from2@example.org", nil)
	assertContinue(act, err)
	act, err = session.Rcpt("to2@example.org", nil)
	assertContinue(act, err)
	act, err = session.HeaderField("Subject", "second")
	assertContinue(act, err)
	act, err = session.HeaderEnd()
	assertContinue(act, err)
	modifyActs, act, err := session.BodyReadFrom(strings.NewReader("second body"))
	if err != nil {
		t.Fatal(err)
	}
	if act.Code != ActAccept {
		t.Fatal("unexpected action:", act.Code)
	}
	expectedModify := []ModifyAction{
		{Code: ActAddHeader, HeaderName: "X-Seen", HeaderValue: "yes"},
		{Code: ActQuarantine, Reason: "suspicious"},
	}
	if !reflect.DeepEqual(modifyActs, expectedModify) {
		t.Fatalf("wrong modify actions: %+v", modifyActs)
	}
	session.Close()

	liveEvents := waitForLastEvent(t, recorder, isQuit)

	// Replay the full recording against a fresh state machine.
	var replayLog []string
	replayRecorder := &Recorder{Full: true}
	replayServer := &Server{
		NewMilter: newBackend(&replayLog),
		Actions:   s.Actions,
		Observer:  replayRecorder,
	}
	if err := recorder.Replay(replayServer); err != nil {
		t.Fatal("replay diverged:", err)
	}

	// The replay reproduces the callback order...
	if !reflect.DeepEqual(liveLog, replayLog) {
		t.Fatalf("callback order differs:\nlive:   %v\nreplay: %v", liveLog, replayLog)
	}
	wantCallbacks := []string{
		"Connect", "Helo",
		"MailFrom", "RcptTo", "Header", "Headers", "BodyChunk", "Abort",
		"MailFrom", "RcptTo", "Header", "Headers", "BodyChunk", "Body",
		"Abort", // session close aborts the fresh milter
	}
	if !reflect.DeepEqual(replayLog, wantCallbacks) {
		t.Fatalf("unexpected callbacks:\nwant: %v\ngot:  %v", wantCallbacks, replayLog)
	}

	// ...and the exact output actions, byte for byte.
	if !reflect.DeepEqual(liveEvents, replayRecorder.Events()) {
		t.Fatal("replayed event stream differs from the recording")
	}
}

func TestObserver_ReplayRejectsMetadataOnly(t *testing.T) {
	recorder := &Recorder{}
	if err := recorder.Replay(&Server{}); err == nil {
		t.Fatal("expected an error replaying a metadata-only recording")
	}
}

func TestObserver_FailurePolicy(t *testing.T) {
	mm := MockMilter{
		ConnResp: RespContinue,
		HeloResp: RespContinue,
		MailResp: RespContinue,
	}

	var mu sync.Mutex
	var errs []error
	observer := ObserverFunc(func(e Event) error {
		// Try to corrupt the wire bytes: must have no effect.
		for i := range e.Data {
			e.Data[i] ^= 0xff
		}
		if e.Direction == DirectionInbound && Code(e.Code) == CodeHelo {
			panic("boom")
		}
		return errors.New("observer failure")
	})

	s := Server{
		NewMilter: func() Milter { return &mm },
		Observer:  observer,
		ObserverErrorHandler: func(err error) {
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		},
	}
	defer s.Close()
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(local)

	cl := NewClientWithOptions("tcp", local.Addr().String(), ClientOptions{})
	defer cl.Close()
	session, err := cl.Session()
	if err != nil {
		t.Fatal(err)
	}

	// The session is unaffected by observer failures and by any mutation of
	// the observed data.
	act, err := session.Conn("host", FamilyInet, 25565, "172.0.0.1")
	if err != nil || act.Code != ActContinue {
		t.Fatalf("conn failed: %v %v", act, err)
	}
	act, err = session.Helo("helo_host")
	if err != nil || act.Code != ActContinue {
		t.Fatalf("helo failed: %v %v", act, err)
	}
	act, err = session.Mail("from@example.org", nil)
	if err != nil || act.Code != ActContinue {
		t.Fatalf("mail failed: %v %v", act, err)
	}
	session.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(errs)
		mu.Unlock()
		if n > 0 && mm.From == "from@example.org" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session affected by observer: from=%q errors=%d", mm.From, n)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if mm.HeloValue != "helo_host" {
		t.Fatal("observer corrupted helo data:", mm.HeloValue)
	}

	mu.Lock()
	defer mu.Unlock()
	var sawError, sawPanic bool
	for _, err := range errs {
		if err.Error() == "observer failure" {
			sawError = true
		}
		if strings.Contains(err.Error(), "observer panic: boom") {
			sawPanic = true
		}
	}
	if !sawError {
		t.Fatal("observer errors not reported to the handler")
	}
	if !sawPanic {
		t.Fatal("observer panic not recovered and reported")
	}
}
