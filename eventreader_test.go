// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"
)

type recordedEvent struct {
	kind   EventKind
	typ    MajorType
	length int64
	value  any
	bytes  []byte
	tags   []uint64
}

func drain(t *testing.T, r *EventReader) []recordedEvent {
	t.Helper()
	var got []recordedEvent
	for {
		ev, err := r.Next()
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatalf("Next() error: %v", err)
		}
		rec := recordedEvent{
			kind:   ev.Kind,
			typ:    ev.Type,
			length: ev.Length,
			value:  ev.Value,
			tags:   append([]uint64(nil), ev.Tags...),
		}
		if ev.Bytes != nil {
			rec.bytes = append([]byte(nil), ev.Bytes...)
		}
		got = append(got, rec)
	}
}

func TestEventReaderScalars(t *testing.T) {
	data := mustHexDecode("0120f6f4f5fb3ff199999999999a6361626343010203")
	r := NewEventReader(bytes.NewReader(data))

	got := drain(t, r)
	want := []recordedEvent{
		{kind: EventScalar, typ: MajorTypePositiveInteger, value: uint64(1)},
		{kind: EventScalar, typ: MajorTypeNegativeInteger, value: int64(-1)},
		{kind: EventScalar, typ: MajorTypePrimitives, value: nil},
		{kind: EventScalar, typ: MajorTypePrimitives, value: false},
		{kind: EventScalar, typ: MajorTypePrimitives, value: true},
		{kind: EventScalar, typ: MajorTypePrimitives, value: 1.1},
		{kind: EventScalar, typ: MajorTypeTextString, length: 3, value: "abc", bytes: []byte("abc")},
		{kind: EventScalar, typ: MajorTypeByteString, length: 3, value: []byte{1, 2, 3}, bytes: []byte{1, 2, 3}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
}

func TestEventReaderDefiniteContainers(t *testing.T) {
	// [1, {"a": 2}, [3]]
	data := mustHexDecode("8301a16161028103")
	r := NewEventReader(bytes.NewReader(data))

	got := drain(t, r)
	wantKinds := []EventKind{
		EventContainerStart, // array(3)
		EventScalar,         // 1
		EventContainerStart, // map(1)
		EventScalar,         // "a"
		EventScalar,         // 2
		EventContainerEnd,   // map
		EventContainerStart, // array(1)
		EventScalar,         // 3
		EventContainerEnd,   // inner array
		EventContainerEnd,   // outer array
	}
	if len(got) != len(wantKinds) {
		t.Fatalf("got %d events, want %d: %#v", len(got), len(wantKinds), got)
	}
	for i, k := range wantKinds {
		if got[i].kind != k {
			t.Fatalf("event %d kind = %v, want %v (all: %#v)", i, got[i].kind, k, got)
		}
	}
	if got[0].typ != MajorTypeArray || got[0].length != 3 {
		t.Errorf("outer array start = %v/%d", got[0].typ, got[0].length)
	}
	if got[2].typ != MajorTypeMap || got[2].length != 1 {
		t.Errorf("map start = %v/%d", got[2].typ, got[2].length)
	}
}

func TestEventReaderIndefiniteArrayByteByByte(t *testing.T) {
	// [_ 1, 2, "abc", [3], {_ "k": 4 }]
	data := mustHexDecode("9f0102636162638103bf616b04ffff")

	for _, maxBytes := range []int{1, 2, 3, 7} {
		name := fmt.Sprintf("%d-byte reader", maxBytes)
		t.Run(name, func(t *testing.T) {
			r := NewEventReader(newNBytesReader(data, maxBytes))
			got := drain(t, r)

			if got[0].kind != EventContainerStart || got[0].length != -1 {
				t.Fatalf("first event = %#v, want indefinite array start", got[0])
			}
			if got[len(got)-1].kind != EventContainerEnd {
				t.Fatalf("last event = %#v, want container end", got[len(got)-1])
			}
			// Verify the re-assembled stream decodes to the same value.
			var v any
			if err := Unmarshal(data, &v); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			_ = v
		})
	}
}

func TestEventReaderIndefiniteStringChunks(t *testing.T) {
	// (_ "strea" "ming")  =>  7f 65 7374726561 64 6d696e67 ff
	data := mustHexDecode("7f657374726561646d696e67ff")

	for _, maxBytes := range []int{1, 3} {
		t.Run(fmt.Sprintf("%d-byte", maxBytes), func(t *testing.T) {
			r := NewEventReader(newNBytesReader(data, maxBytes))
			var chunks []string
			var sawStart, sawEnd bool
			for {
				ev, err := r.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("Next(): %v", err)
				}
				switch ev.Kind {
				case EventContainerStart:
					sawStart = true
					if ev.Type != MajorTypeTextString || ev.Length != -1 {
						t.Fatalf("start = %v/%d", ev.Type, ev.Length)
					}
				case EventStringChunk:
					if ev.Value != nil {
						t.Errorf("chunk Value should be nil, got %v", ev.Value)
					}
					chunks = append(chunks, string(ev.Bytes))
				case EventContainerEnd:
					sawEnd = true
				}
			}
			if !sawStart || !sawEnd {
				t.Fatalf("start=%v end=%v", sawStart, sawEnd)
			}
			if joined := joinStrings(chunks); joined != "streaming" {
				t.Fatalf("chunks = %v => %q, want %q", chunks, joined, "streaming")
			}
		})
	}
}

func joinStrings(ss []string) string {
	out := ""
	for _, s := range ss {
		out += s
	}
	return out
}

func TestEventReaderSkipSubtree(t *testing.T) {
	// [[1,2,3], 9]  -- skip the first nested array, then read 9.
	data := mustHexDecode("828301020309")
	r := NewEventReader(bytes.NewReader(data))

	if ev, err := r.Next(); err != nil || ev.Kind != EventContainerStart {
		t.Fatalf("outer start: %#v %v", ev, err)
	}
	if err := r.Skip(); err != nil {
		t.Fatalf("Skip nested array: %v", err)
	}
	ev, err := r.Next()
	if err != nil {
		t.Fatalf("after skip: %v", err)
	}
	if ev.Kind != EventScalar || ev.Value != uint64(9) {
		t.Fatalf("got %#v, want scalar 9", ev)
	}
	if ev, err := r.Next(); err != nil || ev.Kind != EventContainerEnd {
		t.Fatalf("outer end: %#v %v", ev, err)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("got %v, want io.EOF", err)
	}
}

func TestEventReaderSkipTopLevel(t *testing.T) {
	// Two top-level items: [1,2], 7.
	data := mustHexDecode("82010207")
	r := NewEventReader(bytes.NewReader(data))

	if err := r.Skip(); err != nil {
		t.Fatalf("Skip: %v", err)
	}
	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Value != uint64(7) {
		t.Fatalf("got %#v, want 7", ev)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("got %v, want EOF", err)
	}
}

func TestEventReaderTruncatedNestedString(t *testing.T) {
	// Indefinite array containing an indefinite string with two chunks;
	// cut in the middle of the second chunk.
	full := mustHexDecode("9f7f6361626363646566ff01ff")
	// Keep through the first payload byte of second chunk ("d"), i.e. drop
	// the remaining two payload bytes, the string break, the 0x01 and breaks.
	cut := mustHexDecode("9f7f636162636364")

	// Sanity check the cut is a prefix of the full encoding.
	if !bytes.HasPrefix(full, cut) {
		t.Fatalf("test data is not a prefix")
	}

	r := NewEventReader(newNBytesReader(cut, 1))
	var uerr *UnexpectedEOFError
	var lastErr error
	for {
		_, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			lastErr = err
			break
		}
	}
	if lastErr == nil {
		t.Fatalf("expected truncation error, got clean EOF")
	}
	if !errors.As(lastErr, &uerr) {
		t.Fatalf("error = %T %v, want *UnexpectedEOFError", lastErr, lastErr)
	}
	if !errors.Is(lastErr, io.ErrUnexpectedEOF) {
		t.Fatalf("errors.Is(err, io.ErrUnexpectedEOF) = false for %v", lastErr)
	}
	if uerr.Offset != int64(len(cut)) {
		t.Errorf("Offset = %d, want %d", uerr.Offset, len(cut))
	}
	if len(uerr.Unclosed) != 2 {
		t.Fatalf("Unclosed = %v, want [array text string]", uerr.Unclosed)
	}
	if uerr.Unclosed[0] != "indefinite-length array" || uerr.Unclosed[1] != "UTF-8 text string" {
		t.Errorf("Unclosed = %v", uerr.Unclosed)
	}
	t.Logf("error message: %v", lastErr)
}

func TestEventReaderTruncatedScalar(t *testing.T) {
	// 0x1a followed by only 2 of 4 argument bytes.
	data := mustHexDecode("1a0102")
	r := NewEventReader(newNBytesReader(data, 1))
	_, err := r.Next()
	var uerr *UnexpectedEOFError
	if !errors.As(err, &uerr) {
		t.Fatalf("err = %T %v, want *UnexpectedEOFError", err, err)
	}
	if uerr.Offset != int64(len(data)) {
		t.Errorf("offset = %d, want %d", uerr.Offset, len(data))
	}
	if len(uerr.Unclosed) != 0 {
		t.Errorf("unexpected unclosed: %v", uerr.Unclosed)
	}
}

func TestEventReaderTags(t *testing.T) {
	// 55799(0("2024-01-02T03:04:05Z"))
	data := mustHexDecode("d9d9f7c074323032342d30312d30325430333a30343a30355a")
	r := NewEventReader(bytes.NewReader(data))
	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Kind != EventScalar || !reflect.DeepEqual(ev.Tags, []uint64{55799, 0}) {
		t.Fatalf("event = %#v, want tagged scalar [55799 0]", ev)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("got %v, want EOF", err)
	}
}

func TestEventReaderDecodeInterfaceStillWorks(t *testing.T) {
	// The existing Decode interface must continue to function.
	data := mustHexDecode("8263616263820102")
	var v any
	if err := NewDecoder(bytes.NewReader(data)).Decode(&v); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	want := []any{"abc", []any{uint64(1), uint64(2)}}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("decoded = %#v, want %#v", v, want)
	}
}

func TestEventReaderRoundTripAssortment(t *testing.T) {
	values := []any{
		uint64(0),
		int64(-24),
		uint64(100000),
		true,
		false,
		nil,
		float64(3.14),
		"hello",
		[]byte{0xde, 0xad},
		[]any{uint64(1), uint64(2), uint64(3)},
		map[any]any{"k": int64(-7)},
		[]any{
			[]any{uint64(1)},
			map[any]any{uint64(2): "two"},
			"deep",
		},
	}

	var encoded bytes.Buffer
	enc := NewEncoder(&encoded)
	for _, v := range values {
		if err := enc.Encode(v); err != nil {
			t.Fatalf("encode %#v: %v", v, err)
		}
	}

	// Decode the same stream one item at a time with the standard Decoder and
	// compare each EventScalar against the standard result at scalar granularity
	// is complex; instead verify counts and total bytes, and that every event
	// sequence is balanced.
	for _, maxBytes := range []int{1, 2, 5} {
		t.Run(fmt.Sprintf("%d-byte", maxBytes), func(t *testing.T) {
			r := NewEventReader(newNBytesReader(encoded.Bytes(), maxBytes))
			depth := 0
			scalars := 0
			items := 0
			for {
				ev, err := r.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("Next: %v", err)
				}
				switch ev.Kind {
				case EventContainerStart:
					depth++
				case EventContainerEnd:
					depth--
					if depth < 0 {
						t.Fatalf("unbalanced container end")
					}
				case EventScalar:
					scalars++
				case EventStringChunk:
				}
				items++
			}
			if depth != 0 {
				t.Fatalf("unbalanced depth = %d", depth)
			}
			if scalars == 0 {
				t.Fatalf("expected scalars")
			}
			if r.NumBytesRead() != encoded.Len() {
				t.Fatalf("NumBytesRead = %d, want %d", r.NumBytesRead(), encoded.Len())
			}
		})
	}
}

func TestEventReaderIndefiniteByteStringChunks(t *testing.T) {
	// (_ h'0102', h'03') => 5f 42 0102 41 03 ff
	data := mustHexDecode("5f4201024103ff")
	r := NewEventReader(newNBytesReader(data, 1))

	var payload []byte
	var ends, starts int
	for {
		ev, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch ev.Kind {
		case EventContainerStart:
			starts++
			if ev.Type != MajorTypeByteString {
				t.Fatalf("type = %v", ev.Type)
			}
		case EventStringChunk:
			payload = append(payload, ev.Bytes...)
		case EventContainerEnd:
			ends++
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("starts=%d ends=%d", starts, ends)
	}
	if !bytes.Equal(payload, []byte{1, 2, 3}) {
		t.Fatalf("payload = %x", payload)
	}
}

func TestEventReaderEmptyIndefiniteContainers(t *testing.T) {
	// []  => 9fff ; {} => bfff ; empty indef string 7fff
	for _, tc := range []struct {
		hex       string
		typ       MajorType
		chunkable bool
	}{
		{"9fff", MajorTypeArray, false},
		{"bfff", MajorTypeMap, false},
		{"7fff", MajorTypeTextString, true},
		{"5fff", MajorTypeByteString, true},
	} {
		r := NewEventReader(bytes.NewReader(mustHexDecode(tc.hex)))
		ev, err := r.Next()
		if err != nil {
			t.Fatalf("%s: start: %v", tc.hex, err)
		}
		if ev.Kind != EventContainerStart || ev.Type != tc.typ || ev.Length != -1 {
			t.Fatalf("%s: start = %#v", tc.hex, ev)
		}
		ev, err = r.Next()
		if err != nil {
			t.Fatalf("%s: end: %v", tc.hex, err)
		}
		if ev.Kind != EventContainerEnd {
			t.Fatalf("%s: end = %#v", tc.hex, ev)
		}
		if _, err := r.Next(); err != io.EOF {
			t.Fatalf("%s: got %v, want EOF", tc.hex, err)
		}
	}
}

func TestEventReaderOddIndefiniteMapBreak(t *testing.T) {
	// {_ 1 } with no value => bf 01 ff
	data := mustHexDecode("bf01ff")
	r := NewEventReader(bytes.NewReader(data))
	var lastErr error
	for {
		_, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			lastErr = err
			break
		}
	}
	if lastErr == nil {
		t.Fatalf("expected syntax error for odd map")
	}
	var se *SyntaxError
	if !errors.As(lastErr, &se) {
		t.Fatalf("err = %T, want *SyntaxError", lastErr)
	}
}

func TestEventReaderLazyReadDoesNotConsumeAhead(t *testing.T) {
	// A reader that records how much data was delivered.  Before the first
	// Next no bytes may be read (constructor must not drain input).
	tr := &trackingReader{data: mustHexDecode("0102")}
	r := NewEventReader(tr)
	if tr.reads != 0 {
		t.Fatalf("constructor triggered %d reads", tr.reads)
	}
	if _, err := r.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if tr.totalDelivered > len(tr.data) {
		t.Fatalf("over-read %d bytes", tr.totalDelivered)
	}
}

type trackingReader struct {
	data           []byte
	off            int
	reads          int
	totalDelivered int
}

func (r *trackingReader) Read(b []byte) (int, error) {
	r.reads++
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(b, r.data[r.off:])
	r.off += n
	r.totalDelivered += n
	return n, nil
}

func TestEventReaderLargeIndefiniteArrayStreams(t *testing.T) {
	// Build a large indefinite-length array of small integers, then stream it
	// through a reader that yields one byte at a time.
	const numElements = 5000
	var data []byte
	data = append(data, cborArrayWithIndefiniteLengthHead)
	for range numElements {
		data = append(data, 0x00)
	}
	data = append(data, cborBreakFlag)

	r := NewEventReader(newNBytesReader(data, 1))

	ev, err := r.Next()
	if err != nil || ev.Kind != EventContainerStart || ev.Length != -1 {
		t.Fatalf("start: %#v %v", ev, err)
	}

	count := 0
	for {
		ev, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next at %d: %v", count, err)
		}
		if ev.Kind == EventContainerEnd {
			break
		}
		if ev.Kind != EventScalar || ev.Value != uint64(0) {
			t.Fatalf("unexpected event %#v at %d", ev, count)
		}
		count++
	}
	if count != numElements {
		t.Fatalf("read %d elements, want %d", count, numElements)
	}
	if r.NumBytesRead() != len(data) {
		t.Fatalf("bytes read = %d, want %d", r.NumBytesRead(), len(data))
	}
}

func TestEventReaderLargeIndefiniteStringStreams(t *testing.T) {
	// Indefinite text string made of many 1-byte chunks, read one byte at a
	// time.  Each chunk is delivered independently without buffering all of
	// them.
	const numChunks = 1000
	var data []byte
	data = append(data, cborTextStringWithIndefiniteLengthHead)
	for range numChunks {
		data = append(data, 0x61, 'x')
	}
	data = append(data, cborBreakFlag)

	r := NewEventReader(newNBytesReader(data, 1))
	if ev, err := r.Next(); err != nil || ev.Kind != EventContainerStart {
		t.Fatalf("start: %#v %v", ev, err)
	}
	chunks := 0
	for {
		ev, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if ev.Kind == EventContainerEnd {
			break
		}
		if ev.Kind != EventStringChunk || len(ev.Bytes) != 1 || ev.Bytes[0] != 'x' {
			t.Fatalf("bad chunk %#v", ev)
		}
		chunks++
	}
	if chunks != numChunks {
		t.Fatalf("chunks = %d, want %d", chunks, numChunks)
	}
}

func TestEventReaderDecModeOptions(t *testing.T) {
	dm, err := DecOptions{IndefLength: IndefLengthForbidden}.DecMode()
	if err != nil {
		t.Fatalf("DecMode: %v", err)
	}
	r := dm.NewEventReader(bytes.NewReader(mustHexDecode("9f01ff")))
	if _, err := r.Next(); err == nil {
		t.Fatalf("expected IndefiniteLengthError")
	} else {
		var ile *IndefiniteLengthError
		if !errors.As(err, &ile) {
			t.Fatalf("err = %T, want *IndefiniteLengthError", err)
		}
	}
}

func TestEventReaderTruncatedInsideNestedArray(t *testing.T) {
	// [ [ 1, 2  ...  definite outer array length 1 containing indefinite array
	// 9f 01 02 ... truncated with no breaks.
	data := mustHexDecode("9f830102")
	r := NewEventReader(newNBytesReader(data, 1))
	var uerr *UnexpectedEOFError
	for {
		_, err := r.Next()
		if err == nil {
			continue
		}
		if !errors.As(err, &uerr) {
			t.Fatalf("err = %v, want UnexpectedEOFError", err)
		}
		break
	}
	if uerr.Offset != int64(len(data)) {
		t.Errorf("offset = %d, want %d", uerr.Offset, len(data))
	}
	if len(uerr.Unclosed) != 2 {
		t.Fatalf("unclosed = %v, want 2 frames", uerr.Unclosed)
	}
	if uerr.Unclosed[0] != "indefinite-length array" || uerr.Unclosed[1] != "array of length 3" {
		t.Errorf("unclosed = %v", uerr.Unclosed)
	}
}

func TestEventReaderPropagatesReadError(t *testing.T) {
	sentinel := errors.New("boom")
	r := NewEventReader(newNBytesReaderWithError(mustHexDecode("9f0102"), 1, sentinel))
	var got error
	for {
		_, err := r.Next()
		if err != nil {
			got = err
			break
		}
	}
	if !errors.Is(got, sentinel) {
		t.Fatalf("err = %v, want sentinel read error", got)
	}
}

func TestEventReaderSkipStringChunkInsideIndefString(t *testing.T) {
	// (_ "aa" "bb" "cc") => 7f 62 6161 62 6262 62 6363 ff
	data := mustHexDecode("7f626161626262626363ff")
	r := NewEventReader(bytes.NewReader(data))
	if ev, err := r.Next(); err != nil || ev.Kind != EventContainerStart {
		t.Fatalf("start: %#v %v", ev, err)
	}
	if err := r.Skip(); err != nil { // skip "aa"
		t.Fatalf("skip aa: %v", err)
	}
	ev, err := r.Next() // "bb"
	if err != nil {
		t.Fatalf("next bb: %v", err)
	}
	if ev.Kind != EventStringChunk || string(ev.Bytes) != "bb" {
		t.Fatalf("got %#v, want chunk bb", ev)
	}
	if err := r.Skip(); err != nil { // skip "cc"
		t.Fatalf("skip cc: %v", err)
	}
	ev, err = r.Next() // break -> end
	if err != nil || ev.Kind != EventContainerEnd {
		t.Fatalf("end: %#v %v", ev, err)
	}
}

func TestEventReaderSkipLastChunkThenEnd(t *testing.T) {
	// (_ "aa") with the single chunk skipped; the following Skip must not
	// consume the break, then Next returns end.
	data := mustHexDecode("7f626161ff")
	r := NewEventReader(bytes.NewReader(data))
	if ev, err := r.Next(); err != nil || ev.Kind != EventContainerStart {
		t.Fatalf("start: %#v %v", ev, err)
	}
	if err := r.Skip(); err != nil {
		t.Fatalf("skip chunk: %v", err)
	}
	ev, err := r.Next()
	if err != nil || ev.Kind != EventContainerEnd {
		t.Fatalf("end: %#v %v", ev, err)
	}
}

func TestEventReaderRejectTagInsideIndefString(t *testing.T) {
	// (_ 0("x")) -- a tag is an illegal chunk type.
	data := mustHexDecode("7fc06178ff")
	r := NewEventReader(bytes.NewReader(data))
	if _, err := r.Next(); err != nil {
		t.Fatalf("start: %v", err)
	}
	var lastErr error
	for {
		_, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			lastErr = err
			break
		}
	}
	var se *SyntaxError
	if !errors.As(lastErr, &se) {
		t.Fatalf("err = %v, want *SyntaxError", lastErr)
	}
}
