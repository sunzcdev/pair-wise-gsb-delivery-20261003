package wsutil

import (
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"testing"

	"github.com/gobwas/ws"
)

type recordedEvent struct {
	kind    string
	frame   FrameEvent
	failure error
}

type eventRecorder struct {
	events []recordedEvent
	errAt  map[string]bool // return an error for these event kinds
	err    error
}

func (e *eventRecorder) record(kind string, ev FrameEvent) error {
	e.events = append(e.events, recordedEvent{kind: kind, frame: ev})
	if e.errAt != nil && e.errAt[kind] {
		return e.err
	}
	return nil
}

func (e *eventRecorder) OnFrameHeader(ev FrameEvent) error {
	return e.record("header", ev)
}

func (e *eventRecorder) OnFramePayload(ev FrameEvent) error {
	return e.record("payload", ev)
}

func (e *eventRecorder) OnFrameFailure(ev FrameEvent, err error) error {
	e.events = append(e.events, recordedEvent{kind: "failure", frame: ev, failure: err})
	return nil
}

func mustWriteFrames(t *testing.T, buf *bytes.Buffer, frames ...ws.Frame) {
	t.Helper()
	for _, f := range frames {
		if err := ws.WriteFrame(buf, f); err != nil {
			t.Fatal(err)
		}
	}
}

func frameHeaderSize(f ws.Frame) int64 {
	n := int64(2)
	switch {
	case f.Header.Length >= 1<<16:
		n += 8
	case f.Header.Length >= 126:
		n += 2
	}
	if f.Header.Masked {
		n += 4
	}
	return n
}

// TestObserverByteByByte verifies that reading payload one byte at a time
// reports header once and payload completion only after the final byte.
func TestObserverByteByByte(t *testing.T) {
	msg := []byte("hello")
	f := ws.NewTextFrame(msg)

	var buf bytes.Buffer
	mustWriteFrames(t, &buf, f)
	wireTotal := int64(buf.Len())

	var rec eventRecorder
	r := Reader{
		Source:   &buf,
		Observer: &rec,
	}
	if _, err := r.NextFrame(); err != nil {
		t.Fatal(err)
	}

	one := make([]byte, 1)
	var got []byte
	for {
		n, err := r.Read(one)
		if n > 0 {
			got = append(got, one[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		// While payload bytes remain unread, no payload event must fire.
		if len(got) < len(msg) {
			for _, ev := range rec.events {
				if ev.kind == "payload" {
					t.Fatalf("payload event before full consumption at %d/%d bytes",
						len(got), len(msg))
				}
			}
		}
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("read %q; want %q", got, msg)
	}

	if len(rec.events) != 2 {
		t.Fatalf("events = %v; want header then payload", rec.events)
	}
	if rec.events[0].kind != "header" || rec.events[1].kind != "payload" {
		t.Fatalf("event kinds = %s,%s; want header,payload",
			rec.events[0].kind, rec.events[1].kind)
	}

	headerEv := rec.events[0].frame
	if headerEv.FrameSeq != 1 || headerEv.MessageSeq != 1 ||
		headerEv.OpCode != ws.OpText || headerEv.Length != int64(len(msg)) {
		t.Fatalf("unexpected header event: %+v", headerEv)
	}
	if headerEv.WireLength != frameHeaderSize(f) {
		t.Errorf("header WireLength = %d; want %d", headerEv.WireLength, frameHeaderSize(f))
	}

	payloadEv := rec.events[1].frame
	if payloadEv.FrameSeq != 1 || payloadEv.MessageSeq != 1 {
		t.Fatalf("unexpected payload event ids: %+v", payloadEv)
	}
	if payloadEv.WireLength != wireTotal {
		t.Errorf("payload WireLength = %d; want %d", payloadEv.WireLength, wireTotal)
	}
}

// TestObserverPartialReadThenDiscard verifies a partially read frame is not
// reported complete early, and Discard reports consumption of the rest.
func TestObserverPartialReadThenDiscard(t *testing.T) {
	msg := []byte("abcdefghij")
	f := ws.NewBinaryFrame(msg)

	var buf bytes.Buffer
	mustWriteFrames(t, &buf, f)
	wireTotal := int64(buf.Len())

	var rec eventRecorder
	r := Reader{
		Source:   &buf,
		Observer: &rec,
	}
	hdr, err := r.NextFrame()
	if err != nil {
		t.Fatal(err)
	}

	p := make([]byte, 3)
	n, err := r.Read(p)
	if err != nil || n != 3 {
		t.Fatalf("partial read n=%d err=%v", n, err)
	}
	if !bytes.Equal(p, msg[:3]) {
		t.Fatalf("read %q; want %q", p, msg[:3])
	}
	for _, ev := range rec.events {
		if ev.kind == "payload" {
			t.Fatal("payload event fired before Discard finished consuming")
		}
	}

	if err := r.Discard(); err != nil {
		t.Fatalf("discard: %v", err)
	}

	if len(rec.events) != 2 ||
		rec.events[0].kind != "header" || rec.events[1].kind != "payload" {
		t.Fatalf("events = %v; want header,payload", rec.events)
	}
	ev := rec.events[1].frame
	if ev.FrameSeq != 1 || ev.MessageSeq != 1 || ev.OpCode != ws.OpBinary {
		t.Fatalf("unexpected payload event: %+v", ev)
	}
	if ev.Length != int64(len(msg)) {
		t.Errorf("Length = %d; want %d", ev.Length, len(msg))
	}
	if ev.WireLength != wireTotal {
		t.Errorf("WireLength = %d; want %d (hdr=%d len=%d)",
			ev.WireLength, wireTotal, hdr.Length, len(msg))
	}
}

// TestObserverInterleavedControl verifies control frames between fragments
// are independent frames but do not open a new data message.
func TestObserverInterleavedControl(t *testing.T) {
	var buf bytes.Buffer
	mustWriteFrames(t, &buf,
		ws.NewFrame(ws.OpText, false, []byte("foo")),
		ws.NewPingFrame([]byte("p")),
		ws.NewFrame(ws.OpContinuation, true, []byte("bar")),
	)

	var rec eventRecorder
	r := Reader{
		Source:   &buf,
		Observer: &rec,
	}
	if _, err := r.NextFrame(); err != nil {
		t.Fatal(err)
	}
	if _, err := ioutil.ReadAll(&r); err != nil {
		t.Fatal(err)
	}

	type want struct {
		kind       string
		frameSeq   int
		messageSeq int
		op         ws.OpCode
	}
	exp := []want{
		{"header", 1, 1, ws.OpText},
		{"payload", 1, 1, ws.OpText},
		{"header", 2, 1, ws.OpPing},
		{"payload", 2, 1, ws.OpPing},
		{"header", 3, 1, ws.OpContinuation},
		{"payload", 3, 1, ws.OpContinuation},
	}
	if len(rec.events) != len(exp) {
		t.Fatalf("events = %v; want %d", rec.events, len(exp))
	}
	for i, w := range exp {
		got := rec.events[i]
		if got.kind != w.kind ||
			got.frame.FrameSeq != w.frameSeq ||
			got.frame.MessageSeq != w.messageSeq ||
			got.frame.OpCode != w.op {
			t.Fatalf("event %d = %+v; want %+v", i, got, w)
		}
	}
}

// TestObserverUnderlyingEOF verifies a truncated payload surfaces a failure
// event carrying bytes actually consumed and the error, with no payload event.
func TestObserverUnderlyingEOF(t *testing.T) {
	var buf bytes.Buffer
	f := ws.NewTextFrame([]byte("complete"))
	if err := ws.WriteHeader(&buf, f.Header); err != nil {
		t.Fatal(err)
	}
	buf.Write([]byte("abc")) // only 3 of 8 payload bytes

	var rec eventRecorder
	r := Reader{
		Source:   &buf,
		Observer: &rec,
	}
	if _, err := r.NextFrame(); err != nil {
		t.Fatal(err)
	}

	all, err := ioutil.ReadAll(&r)
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("ReadAll err = %v; want %v", err, io.ErrUnexpectedEOF)
	}
	if string(all) != "abc" {
		t.Fatalf("bytes = %q; want abc", all)
	}

	var sawPayload, sawFailure bool
	for _, ev := range rec.events {
		switch ev.kind {
		case "payload":
			sawPayload = true
		case "failure":
			sawFailure = true
			if ev.failure != io.ErrUnexpectedEOF {
				t.Errorf("failure cause = %v; want %v", ev.failure, io.ErrUnexpectedEOF)
			}
			if ev.frame.FrameSeq != 1 || ev.frame.MessageSeq != 1 {
				t.Errorf("failure ids = %+v", ev.frame)
			}
			if ev.frame.Length != 8 {
				t.Errorf("failure Length = %d; want 8", ev.frame.Length)
			}
			if ev.frame.WireLength != frameHeaderSize(f)+3 {
				t.Errorf("failure WireLength = %d; want %d",
					ev.frame.WireLength, frameHeaderSize(f)+3)
			}
		}
	}
	if sawPayload {
		t.Error("payload event fired despite truncation")
	}
	if !sawFailure {
		t.Error("no failure event")
	}
}

// TestObserverCleanEOF verifies a clean connection close is not a frame
// failure.
func TestObserverCleanEOF(t *testing.T) {
	r := Reader{
		Source:   bytes.NewReader(nil),
		Observer: &eventRecorder{},
	}
	rec := r.Observer.(*eventRecorder)
	if _, err := r.NextFrame(); err != io.EOF {
		t.Fatalf("NextFrame err = %v; want io.EOF", err)
	}
	if len(rec.events) != 0 {
		t.Fatalf("events = %v; want none on clean EOF", rec.events)
	}
}

// TestObserverErrorTerminates verifies an observer error terminates the
// Reader and that no stale events are delivered after reset.
func TestObserverErrorTerminates(t *testing.T) {
	obsErr := errors.New("observer boom")

	var buf bytes.Buffer
	mustWriteFrames(t, &buf,
		ws.NewTextFrame([]byte("first")),
		ws.NewTextFrame([]byte("second")),
	)

	rec := &eventRecorder{
		errAt: map[string]bool{"header": true},
		err:   obsErr,
	}
	r := Reader{
		Source:   &buf,
		Observer: rec,
	}

	if _, err := r.NextFrame(); err != obsErr {
		t.Fatalf("NextFrame err = %v; want %v", err, obsErr)
	}

	// Even after an explicit reset, the terminated Reader must not emit stale
	// events or process more frames.
	r.reset()
	if _, err := r.NextFrame(); err != ErrReaderTerminated {
		t.Fatalf("NextFrame after reset err = %v; want %v", err, ErrReaderTerminated)
	}
	if _, err := r.Read(make([]byte, 4)); err != ErrReaderTerminated {
		t.Fatalf("Read after reset err = %v; want %v", err, ErrReaderTerminated)
	}
	if err := r.Discard(); err != ErrReaderTerminated {
		t.Fatalf("Discard after reset err = %v; want %v", err, ErrReaderTerminated)
	}

	// Only the first header event was delivered; no payload, no failure,
	// nothing after reset.
	if len(rec.events) != 1 || rec.events[0].kind != "header" {
		t.Fatalf("events = %v; want single header", rec.events)
	}
}

// TestObserverFailureCallbackError verifies an error from OnFrameFailure also
// terminates and is returned to the caller.
func TestObserverFailureCallbackError(t *testing.T) {
	obsErr := errors.New("failure observer boom")
	var buf bytes.Buffer
	f := ws.NewTextFrame([]byte("xx"))
	if err := ws.WriteHeader(&buf, f.Header); err != nil {
		t.Fatal(err)
	}
	// No payload bytes: header promised 2, source is truncated.

	var kinds []string
	r := Reader{
		Source: &buf,
		Observer: FrameObserverFunc{
			Header: func(FrameEvent) error { return nil },
			Failure: func(FrameEvent, error) error {
				kinds = append(kinds, "failure")
				return obsErr
			},
		},
	}
	if _, err := r.NextFrame(); err != nil {
		t.Fatal(err)
	}
	_, err := ioutil.ReadAll(&r)
	if err != obsErr {
		t.Fatalf("err = %v; want observer error %v", err, obsErr)
	}

	// Next call must be terminated.
	_, err = r.NextFrame()
	if err != ErrReaderTerminated {
		t.Fatalf("NextFrame err = %v; want %v", err, ErrReaderTerminated)
	}
}
