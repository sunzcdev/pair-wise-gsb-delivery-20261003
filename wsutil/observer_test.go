package wsutil

import (
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"reflect"
	"testing"

	"github.com/gobwas/ws"
)

type recObserver struct {
	events []FrameEvent
	err    error
	failOn FrameEventKind
}

func (o *recObserver) ObserveFrame(e FrameEvent) error {
	o.events = append(o.events, e)
	if o.failOn != 0 && e.Kind == o.failOn {
		return o.err
	}
	return nil
}

func writeFrames(t *testing.T, frames ...ws.Frame) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	for _, f := range frames {
		if err := ws.WriteFrame(&buf, f); err != nil {
			t.Fatal(err)
		}
	}
	return &buf
}

func wireLen(f ws.Frame) int64 {
	return int64(ws.HeaderSize(f.Header)) + int64(len(f.Payload))
}

func event(kind FrameEventKind, frameIdx, msgIdx int64, f ws.Frame) FrameEvent {
	return FrameEvent{
		Kind:         kind,
		FrameIndex:   frameIdx,
		MessageIndex: msgIdx,
		Header:       f.Header,
		WireLength:   wireLen(f),
	}
}

// TestObserverByteByByte verifies the event sequence when every payload byte
// is read separately: consumed must be reported exactly once, only after the
// final byte was actually taken.
func TestObserverByteByByte(t *testing.T) {
	msg := []byte("hello")
	f := ws.NewTextFrame(msg)

	var obs recObserver
	r := Reader{Source: writeFrames(t, f), Observer: &obs}

	hdr, err := r.NextFrame()
	if err != nil {
		t.Fatal(err)
	}

	p := make([]byte, 1)
	var got []byte
	for {
		n, err := r.Read(p)
		got = append(got, p[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		// No completion event before the very last byte.
		if len(got) < len(msg) {
			for _, e := range obs.events {
				if e.Kind == EventPayloadConsumed {
					t.Fatalf("premature consumed event after %d bytes", len(got))
				}
			}
		}
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("payload: %q; want %q", got, msg)
	}

	exp := []FrameEvent{
		event(EventHeaderChecked, 0, 0, f),
		event(EventPayloadConsumed, 0, 0, f),
	}
	if hdr.OpCode != ws.OpText {
		t.Fatalf("unexpected header opcode: %v", hdr.OpCode)
	}
	if !reflect.DeepEqual(obs.events, exp) {
		t.Fatalf("events:\nact: %+v\nexp: %+v", obs.events, exp)
	}
}

// TestObserverFragmentedWithControl verifies that control frames interleaved
// between fragments are independent frames, but do not open a new data
// message: they report the in-flight message index.
func TestObserverFragmentedWithControl(t *testing.T) {
	f1 := ws.NewFrame(ws.OpText, false, []byte("foo"))
	ping := ws.NewPingFrame([]byte("p"))
	f2 := ws.NewFrame(ws.OpContinuation, false, []byte("bar"))
	f3 := ws.NewFrame(ws.OpContinuation, true, []byte("baz"))
	standalone := ws.NewPongFrame([]byte("q"))
	f4 := ws.NewBinaryFrame([]byte("next"))

	var obs recObserver
	r := Reader{
		Source:          writeFrames(t, f1, ping, f2, f3, standalone, f4),
		SkipHeaderCheck: true,
		Observer:        &obs,
	}

	// First fragmented message.
	if _, err := r.NextFrame(); err != nil {
		t.Fatal(err)
	}
	if bts, err := ioutil.ReadAll(&r); err != nil || string(bts) != "foobarbaz" {
		t.Fatalf("read: %q %v", bts, err)
	}

	// Standalone control frame between data messages.
	if _, err := r.NextFrame(); err != nil {
		t.Fatal(err)
	}
	// Its payload must be consumed before the next frame advance.
	if bts, err := ioutil.ReadAll(&r); err != nil || string(bts) != "q" {
		t.Fatalf("standalone control read: %q %v", bts, err)
	}

	// Next data message.
	if _, err := r.NextFrame(); err != nil {
		t.Fatal(err)
	}
	if _, err := ioutil.ReadAll(&r); err != nil {
		t.Fatal(err)
	}

	exp := []FrameEvent{
		event(EventHeaderChecked, 0, 0, f1),
		event(EventPayloadConsumed, 0, 0, f1),
		event(EventHeaderChecked, 1, 0, ping),
		event(EventPayloadConsumed, 1, 0, ping),
		event(EventHeaderChecked, 2, 0, f2),
		event(EventPayloadConsumed, 2, 0, f2),
		event(EventHeaderChecked, 3, 0, f3),
		event(EventPayloadConsumed, 3, 0, f3),
		event(EventHeaderChecked, 4, -1, standalone),
		event(EventPayloadConsumed, 4, -1, standalone),
		event(EventHeaderChecked, 5, 1, f4),
		event(EventPayloadConsumed, 5, 1, f4),
	}
	if !reflect.DeepEqual(obs.events, exp) {
		t.Fatalf("events:\nact:\n%+v\nexp:\n%+v", obs.events, exp)
	}
}

// TestObserverDiscard verifies Discard reports actual payload consumption and
// walks every fragment of a fragmented message.
func TestObserverDiscard(t *testing.T) {
	f1 := ws.NewFrame(ws.OpBinary, false, []byte("foo"))
	ping := ws.NewPingFrame(nil)
	f2 := ws.NewFrame(ws.OpContinuation, true, []byte("barbaz"))

	var obs recObserver
	r := Reader{Source: writeFrames(t, f1, ping, f2), Observer: &obs}

	if _, err := r.NextFrame(); err != nil {
		t.Fatal(err)
	}
	// Read only part of the first fragment, then discard the rest.
	p := make([]byte, 2)
	if n, err := r.Read(p); err != nil || n != 2 {
		t.Fatalf("partial read: n=%d err=%v", n, err)
	}
	if err := r.Discard(); err != nil {
		t.Fatal(err)
	}

	exp := []FrameEvent{
		event(EventHeaderChecked, 0, 0, f1),
		event(EventPayloadConsumed, 0, 0, f1),
		event(EventHeaderChecked, 1, 0, ping),
		event(EventPayloadConsumed, 1, 0, ping),
		event(EventHeaderChecked, 2, 0, f2),
		event(EventPayloadConsumed, 2, 0, f2),
	}
	if !reflect.DeepEqual(obs.events, exp) {
		t.Fatalf("events:\nact:\n%+v\nexp:\n%+v", obs.events, exp)
	}
}

// TestObserverEOFInPayload verifies a mid-payload EOF reports a failure with
// the frame's identity and expected wire length, and no consumed event.
func TestObserverEOFInPayload(t *testing.T) {
	f := ws.NewTextFrame([]byte("abcdef"))
	var raw bytes.Buffer
	if err := ws.WriteHeader(&raw, f.Header); err != nil {
		t.Fatal(err)
	}
	raw.WriteString("abc")

	var obs recObserver
	r := Reader{Source: &raw, Observer: &obs}
	hdr, err := r.NextFrame()
	if err != nil {
		t.Fatal(err)
	}

	_, rerr := ioutil.ReadAll(&r)
	if rerr != io.ErrUnexpectedEOF {
		t.Fatalf("read error: %v; want %v", rerr, io.ErrUnexpectedEOF)
	}

	exp := []FrameEvent{
		event(EventHeaderChecked, 0, 0, f),
		{
			Kind:         EventFailed,
			FrameIndex:   0,
			MessageIndex: 0,
			Header:       hdr,
			WireLength:   wireLen(f),
			Err:          io.ErrUnexpectedEOF,
		},
	}
	if !reflect.DeepEqual(obs.events, exp) {
		t.Fatalf("events:\nact:\n%+v\nexp:\n%+v", obs.events, exp)
	}
}

// TestObserverEOFInHeader verifies a truncated header reports a failure for
// the missing frame and no header-checked event.
func TestObserverEOFInHeader(t *testing.T) {
	f := ws.NewFrame(ws.OpText, true, make([]byte, 300))
	raw := ws.MustCompileFrame(f)
	// Cut in the middle of the 16-bit length + payload area.
	src := bytes.NewReader(raw[:3])

	var obs recObserver
	r := Reader{Source: src, Observer: &obs}
	if _, err := r.NextFrame(); err != io.ErrUnexpectedEOF {
		t.Fatalf("NextFrame error: %v; want %v", err, io.ErrUnexpectedEOF)
	}

	if len(obs.events) != 1 {
		t.Fatalf("events: %+v; want single failure", obs.events)
	}
	e := obs.events[0]
	if e.Kind != EventFailed || e.FrameIndex != 0 || e.MessageIndex != 0 {
		t.Fatalf("unexpected event: %+v", e)
	}
	if e.WireLength != 0 {
		t.Fatalf("wire length: %d; want 0 for unparsed header", e.WireLength)
	}
	if e.Err != io.ErrUnexpectedEOF {
		t.Fatalf("event err: %v; want %v", e.Err, io.ErrUnexpectedEOF)
	}
}

// TestObserverCleanEOF verifies a clean EOF between frames reports no event.
func TestObserverCleanEOF(t *testing.T) {
	var obs recObserver
	r := Reader{Source: bytes.NewReader(nil), Observer: &obs}
	if _, err := r.NextFrame(); err != io.EOF {
		t.Fatalf("NextFrame error: %v; want %v", err, io.EOF)
	}
	if len(obs.events) != 0 {
		t.Fatalf("unexpected events on clean EOF: %+v", obs.events)
	}
}

// TestObserverTermination verifies the explicit termination rule: after the
// observer errors once the Reader returns ErrObserverTerminated for all
// further operations and delivers no more events, even when a new message is
// attempted; after Reset numbering restarts and events flow again.
func TestObserverTermination(t *testing.T) {
	observerErr := errors.New("boom")
	f1 := ws.NewTextFrame([]byte("a"))
	f2 := ws.NewTextFrame([]byte("bb"))

	obs := recObserver{err: observerErr, failOn: EventHeaderChecked}
	r := Reader{Source: writeFrames(t, f1, f2), Observer: &obs}

	_, err := r.NextFrame()
	if !errors.Is(err, ErrObserverTerminated) {
		t.Fatalf("NextFrame error: %v; want wrap of ErrObserverTerminated", err)
	}
	if r.ObserverError() != observerErr {
		t.Fatalf("observer error not preserved: %v", r.ObserverError())
	}
	if r.observerErr != observerErr {
		t.Fatalf("stored observer error: %v; want %v", r.observerErr, observerErr)
	}

	// Every subsequent operation terminates, and no more events are emitted.
	nEvents := len(obs.events)
	if _, err := r.NextFrame(); !errors.Is(err, ErrObserverTerminated) {
		t.Fatalf("second NextFrame: %v", err)
	}
	if _, err := r.Read(make([]byte, 4)); !errors.Is(err, ErrObserverTerminated) {
		t.Fatalf("Read after termination: %v", err)
	}
	if err := r.Discard(); !errors.Is(err, ErrObserverTerminated) {
		t.Fatalf("Discard after termination: %v", err)
	}
	if len(obs.events) != nEvents {
		t.Fatalf("events delivered after termination: %+v", obs.events[nEvents:])
	}

	// After reset no stale events arrive and numbering starts from scratch.
	r.Source = writeFrames(t, f1)
	r.Reset()
	obs.failOn = 0
	if _, err := r.NextFrame(); err != nil {
		t.Fatal(err)
	}
	if _, err := ioutil.ReadAll(&r); err != nil {
		t.Fatal(err)
	}
	exp := []FrameEvent{
		event(EventHeaderChecked, 0, 0, f1),
		event(EventPayloadConsumed, 0, 0, f1),
	}
	got := obs.events[nEvents:]
	if !reflect.DeepEqual(got, exp) {
		t.Fatalf("post-reset events:\nact:\n%+v\nexp:\n%+v", got, exp)
	}
}

// TestObserverWireLengthMasked verifies wire length includes masking header
// bytes and payload length.
func TestObserverWireLengthMasked(t *testing.T) {
	f := ws.MaskFrame(ws.NewPingFrame([]byte("1234")))
	var obs recObserver
	r := Reader{Source: writeFrames(t, f), Observer: &obs}
	if _, err := r.NextFrame(); err != nil {
		t.Fatal(err)
	}
	exp := int64(2 + 4 + 4) // base header + mask + payload
	if got := obs.events[0].WireLength; got != exp {
		t.Fatalf("wire length: %d; want %d", got, exp)
	}
}

// TestObserverDiscardWithoutFrame verifies Discard before any NextFrame
// neither reports events nor errors.
func TestObserverDiscardWithoutFrame(t *testing.T) {
	var obs recObserver
	r := Reader{Observer: &obs}
	if err := r.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if len(obs.events) != 0 {
		t.Fatalf("unexpected events: %+v", obs.events)
	}
}

// TestObserverTerminatesOnConsumed verifies an observer error from the
// consumed event terminates the Reader as well.
func TestObserverTerminatesOnConsumed(t *testing.T) {
	f := ws.NewTextFrame([]byte("abc"))
	obs := recObserver{err: errors.New("stop"), failOn: EventPayloadConsumed}
	r := Reader{Source: writeFrames(t, f), Observer: &obs}
	if _, err := r.NextFrame(); err != nil {
		t.Fatal(err)
	}
	if _, err := ioutil.ReadAll(&r); !errors.Is(err, ErrObserverTerminated) {
		t.Fatalf("ReadAll: %v; want ErrObserverTerminated", err)
	}
	if _, err := r.NextFrame(); !errors.Is(err, ErrObserverTerminated) {
		t.Fatalf("NextFrame after terminate: %v", err)
	}
}
