// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"bytes"
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestEventReaderLargeIndefiniteArraySmallReads(t *testing.T) {
	const numItems = 10000

	var buf bytes.Buffer
	buf.WriteByte(cborArrayWithIndefiniteLengthHead)
	for i := 0; i < numItems; i++ {
		b, err := Marshal(i)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
	}
	buf.WriteByte(cborBreakFlag)
	data := buf.Bytes()

	// Reader returns at most 3 bytes per read.
	er := NewEventReader(newNBytesReader(data, 3))

	ev, err := er.Next()
	if err != nil {
		t.Fatalf("Next() returned error %v", err)
	}
	if ev.Type != EventArrayBegin || ev.Length != -1 {
		t.Errorf("Next() = %v (length %d), want array begin (length -1)", ev.Type, ev.Length)
	}

	for i := 0; i < numItems; i++ {
		ev, err = er.Next()
		if err != nil {
			t.Fatalf("Next() at item %d returned error %v", i, err)
		}
		if ev.Type != EventPositiveInt || ev.Uint != uint64(i) {
			t.Fatalf("Next() at item %d = %v (%d), want positive integer %d", i, ev.Type, ev.Uint, i)
		}
	}

	ev, err = er.Next()
	if err != nil {
		t.Fatalf("Next() returned error %v", err)
	}
	if ev.Type != EventArrayEnd {
		t.Errorf("Next() = %v, want array end", ev.Type)
	}

	if _, err = er.Next(); err != io.EOF {
		t.Errorf("Next() returned error %v, want io.EOF", err)
	}
	if er.Offset() != len(data) {
		t.Errorf("Offset() = %d, want %d", er.Offset(), len(data))
	}
}

func TestEventReaderEventSequence(t *testing.T) {
	// {"a": 1, "b": [true, null, undefined, simple(32), 1.0f16, 3.1415927f32],
	//  "c": (_ "x", "y"), "d": h'627965', "e": 1(-1)}
	data := []byte{
		0xa5,
		0x61, 'a', 0x01,
		0x61, 'b', 0x86, 0xf5, 0xf6, 0xf7, 0xf8, 0x20, 0xf9, 0x3c, 0x00, 0xfa, 0x40, 0x49, 0x0f, 0xdb,
		0x61, 'c', 0x7f, 0x61, 'x', 0x61, 'y', 0xff,
		0x61, 'd', 0x43, 'b', 'y', 'e',
		0x61, 'e', 0xc1, 0x20,
	}

	want := []Event{
		{Type: EventMapBegin, Length: 5},
		{Type: EventTextString, Text: "a"},
		{Type: EventPositiveInt, Uint: 1},
		{Type: EventTextString, Text: "b"},
		{Type: EventArrayBegin, Length: 6},
		{Type: EventBool, Bool: true},
		{Type: EventNil},
		{Type: EventUndefined},
		{Type: EventSimple, Simple: SimpleValue(32)},
		{Type: EventFloat, Float: 1.0},
		{Type: EventFloat, Float: float64(math.Float32frombits(0x40490fdb))},
		{Type: EventArrayEnd},
		{Type: EventTextString, Text: "c"},
		{Type: EventTextStringBegin},
		{Type: EventTextStringChunk, Text: "x"},
		{Type: EventTextStringChunk, Text: "y"},
		{Type: EventStringEnd},
		{Type: EventTextString, Text: "d"},
		{Type: EventByteString, Bytes: []byte("bye")},
		{Type: EventTextString, Text: "e"},
		{Type: EventTag, Uint: 1},
		{Type: EventNegativeInt, Uint: 0},
		{Type: EventMapEnd},
	}

	er := NewEventReader(newNBytesReader(data, 1))
	for i, w := range want {
		ev, err := er.Next()
		if err != nil {
			t.Fatalf("Next() at event %d returned error %v", i, err)
		}
		if !reflect.DeepEqual(ev, w) {
			t.Errorf("Next() at event %d = %+v, want %+v", i, ev, w)
		}
	}
	if _, err := er.Next(); err != io.EOF {
		t.Errorf("Next() returned error %v, want io.EOF", err)
	}
	// Subsequent calls keep returning io.EOF.
	if _, err := er.Next(); err != io.EOF {
		t.Errorf("Next() returned error %v, want io.EOF", err)
	}
	if er.Offset() != len(data) {
		t.Errorf("Offset() = %d, want %d", er.Offset(), len(data))
	}
}

func TestEventReaderTruncatedNestedIndefiniteString(t *testing.T) {
	// [_ [_ (_ "abc"  -- input ends after the chunk, nothing is closed.
	data := []byte{0x9f, 0x9f, 0x7f, 0x63, 'a', 'b', 'c'}

	er := NewEventReader(newNBytesReader(data, 2))

	wantTypes := []EventType{
		EventArrayBegin,
		EventArrayBegin,
		EventTextStringBegin,
		EventTextStringChunk,
	}
	for i, w := range wantTypes {
		ev, err := er.Next()
		if err != nil {
			t.Fatalf("Next() at event %d returned error %v", i, err)
		}
		if ev.Type != w {
			t.Fatalf("Next() at event %d = %v, want %v", i, ev.Type, w)
		}
	}

	_, err := er.Next()
	if err == nil {
		t.Fatal("Next() didn't return error for truncated input")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("Next() error %v is not io.ErrUnexpectedEOF", err)
	}

	var incErr *IncompleteItemError
	if !errors.As(err, &incErr) {
		t.Fatalf("Next() error %v (%T) is not *IncompleteItemError", err, err)
	}
	if incErr.Offset != len(data) {
		t.Errorf("IncompleteItemError.Offset = %d, want %d", incErr.Offset, len(data))
	}
	wantUnclosed := []UnclosedStructure{
		{Type: "array", Offset: 0, Indefinite: true},
		{Type: "array", Offset: 1, Indefinite: true},
		{Type: "text string", Offset: 2, Indefinite: true},
	}
	if !reflect.DeepEqual(incErr.Unclosed, wantUnclosed) {
		t.Errorf("IncompleteItemError.Unclosed = %+v, want %+v", incErr.Unclosed, wantUnclosed)
	}

	errMsg := err.Error()
	for _, want := range []string{"offset 7", "unclosed", "indefinite-length array", "indefinite-length text string"} {
		if !strings.Contains(errMsg, want) {
			t.Errorf("error message %q doesn't contain %q", errMsg, want)
		}
	}
}

func TestEventReaderTruncatedDefiniteStringContent(t *testing.T) {
	// [1, "hello... -- text string of length 5 truncated after 1 content byte.
	data := []byte{0x82, 0x01, 0x65, 'h'}

	er := NewEventReader(newNBytesReader(data, 3))

	for _, w := range []EventType{EventArrayBegin, EventPositiveInt} {
		ev, err := er.Next()
		if err != nil {
			t.Fatalf("Next() returned error %v", err)
		}
		if ev.Type != w {
			t.Fatalf("Next() = %v, want %v", ev.Type, w)
		}
	}

	_, err := er.Next()
	var incErr *IncompleteItemError
	if !errors.As(err, &incErr) {
		t.Fatalf("Next() error %v (%T) is not *IncompleteItemError", err, err)
	}
	if incErr.Offset != len(data) {
		t.Errorf("IncompleteItemError.Offset = %d, want %d", incErr.Offset, len(data))
	}
	wantUnclosed := []UnclosedStructure{
		{Type: "array", Offset: 0, Indefinite: false},
	}
	if !reflect.DeepEqual(incErr.Unclosed, wantUnclosed) {
		t.Errorf("IncompleteItemError.Unclosed = %+v, want %+v", incErr.Unclosed, wantUnclosed)
	}
	if !strings.Contains(err.Error(), "offset 4") {
		t.Errorf("error message %q doesn't contain %q", err.Error(), "offset 4")
	}
}

func TestEventReaderTruncatedTopLevelItem(t *testing.T) {
	// Truncated head of a top-level item: no unclosed structures.
	data := []byte{0x1a, 0x01}

	er := NewEventReader(bytes.NewReader(data))
	_, err := er.Next()

	var incErr *IncompleteItemError
	if !errors.As(err, &incErr) {
		t.Fatalf("Next() error %v (%T) is not *IncompleteItemError", err, err)
	}
	if incErr.Offset != 2 {
		t.Errorf("IncompleteItemError.Offset = %d, want 2", incErr.Offset)
	}
	if len(incErr.Unclosed) != 0 {
		t.Errorf("IncompleteItemError.Unclosed = %+v, want empty", incErr.Unclosed)
	}
}

func TestEventReaderSkip(t *testing.T) {
	testCases := []struct {
		name string
		data []byte
		// Number of events to consume before calling Skip.
		consume int
		// Event types expected after Skip.
		wantAfter []EventType
	}{
		{
			name:      "nested definite containers",
			data:      []byte{0x83, 0x01, 0x83, 0x02, 0x03, 0x81, 0x04, 0x05}, // [1, [2, 3, [4]], 5]
			consume:   3,                                                      // array begin, 1, inner array begin
			wantAfter: []EventType{EventPositiveInt, EventArrayEnd},
		},
		{
			name: "indefinite map with indefinite string key",
			// [_ 1, {_ (_ "a"): 1}, 2]
			data:      []byte{0x9f, 0x01, 0xbf, 0x7f, 0x61, 0x61, 0xff, 0x01, 0xff, 0x02, 0xff},
			consume:   3, // array begin, 1, map begin
			wantAfter: []EventType{EventPositiveInt, EventArrayEnd},
		},
		{
			name:      "skip remaining chunks of indefinite string",
			data:      []byte{0x7f, 0x61, 'a', 0x61, 'b', 0x61, 'c', 0xff, 0x01},
			consume:   2, // text string begin, chunk "a"
			wantAfter: []EventType{EventPositiveInt},
		},
		{
			name:      "skip scalar is no-op",
			data:      []byte{0x82, 0x01, 0x02}, // [1, 2]
			consume:   2,                        // array begin, 1
			wantAfter: []EventType{EventPositiveInt, EventArrayEnd},
		},
		{
			name:      "skip tag content",
			data:      []byte{0x83, 0x01, 0xd9, 0x03, 0xe8, 0x82, 0x01, 0x02, 0x03}, // [1, 1000([1, 2]), 3]
			consume:   3,                                                            // array begin, 1, tag 1000
			wantAfter: []EventType{EventPositiveInt, EventArrayEnd},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			er := NewEventReader(newNBytesReader(tc.data, 2))

			for i := 0; i < tc.consume; i++ {
				if _, err := er.Next(); err != nil {
					t.Fatalf("Next() at event %d returned error %v", i, err)
				}
			}

			if err := er.Skip(); err != nil {
				t.Fatalf("Skip() returned error %v", err)
			}

			for i, w := range tc.wantAfter {
				ev, err := er.Next()
				if err != nil {
					t.Fatalf("Next() after Skip at event %d returned error %v", i, err)
				}
				if ev.Type != w {
					t.Errorf("Next() after Skip at event %d = %v, want %v", i, ev.Type, w)
				}
			}

			if _, err := er.Next(); err != io.EOF {
				t.Errorf("Next() returned error %v, want io.EOF", err)
			}
			if er.Offset() != len(tc.data) {
				t.Errorf("Offset() = %d, want %d", er.Offset(), len(tc.data))
			}
		})
	}
}

func TestEventReaderForwarding(t *testing.T) {
	// Gateway use case: forward a CBOR data item event by event without
	// materializing the complete value.
	value := map[string]any{
		"list":   []any{uint64(1), "two", 3.5, true, nil, []byte{1, 2, 3}},
		"nested": []any{[]any{uint64(1), uint64(2)}, map[string]any{"k": "v"}},
		"text":   "hello",
	}
	data, err := Marshal(value)
	if err != nil {
		t.Fatal(err)
	}

	var forwarded bytes.Buffer
	enc := NewEncoder(&forwarded)
	er := NewEventReader(newNBytesReader(data, 2))

	for {
		ev, err := er.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next() returned error %v", err)
		}

		switch ev.Type {
		case EventArrayBegin:
			if err := enc.StartIndefiniteArray(); err != nil {
				t.Fatal(err)
			}
		case EventMapBegin:
			if err := enc.StartIndefiniteMap(); err != nil {
				t.Fatal(err)
			}
		case EventArrayEnd, EventMapEnd, EventStringEnd:
			if err := enc.EndIndefinite(); err != nil {
				t.Fatal(err)
			}
		case EventByteStringBegin:
			if err := enc.StartIndefiniteByteString(); err != nil {
				t.Fatal(err)
			}
		case EventTextStringBegin:
			if err := enc.StartIndefiniteTextString(); err != nil {
				t.Fatal(err)
			}
		case EventByteStringChunk, EventByteString:
			if err := enc.Encode(ev.Bytes); err != nil {
				t.Fatal(err)
			}
		case EventTextStringChunk, EventTextString:
			if err := enc.Encode(ev.Text); err != nil {
				t.Fatal(err)
			}
		case EventPositiveInt:
			if err := enc.Encode(ev.Uint); err != nil {
				t.Fatal(err)
			}
		case EventNegativeInt:
			if err := enc.Encode(-1 - int64(ev.Uint)); err != nil {
				t.Fatal(err)
			}
		case EventBool:
			if err := enc.Encode(ev.Bool); err != nil {
				t.Fatal(err)
			}
		case EventNil:
			if err := enc.Encode(nil); err != nil {
				t.Fatal(err)
			}
		case EventFloat:
			if err := enc.Encode(ev.Float); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unhandled event type %v", ev.Type)
		}
	}

	var got, want any
	if err := Unmarshal(forwarded.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal(forwarded) returned error %v", err)
	}
	if err := Unmarshal(data, &want); err != nil {
		t.Fatalf("Unmarshal(original) returned error %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("forwarded value = %v, want %v", got, want)
	}
}

func TestEventReaderSyntaxErrors(t *testing.T) {
	testCases := []struct {
		name         string
		data         []byte
		eventsBefore int
		wantErrorMsg string
	}{
		{
			name:         "break outside indefinite-length value",
			data:         []byte{0xff},
			eventsBefore: 0,
			wantErrorMsg: "cbor: unexpected \"break\" code",
		},
		{
			name:         "break in definite-length array",
			data:         []byte{0x81, 0xff},
			eventsBefore: 1,
			wantErrorMsg: "cbor: unexpected \"break\" code",
		},
		{
			name:         "break with odd item count in indefinite-length map",
			data:         []byte{0xbf, 0x01, 0xff},
			eventsBefore: 2,
			wantErrorMsg: "cbor: unexpected \"break\" code",
		},
		{
			name:         "wrong chunk type in indefinite-length text string",
			data:         []byte{0x7f, 0x41, 0x00, 0xff},
			eventsBefore: 1,
			wantErrorMsg: "cbor: wrong element type byte string for indefinite-length UTF-8 text string",
		},
		{
			name:         "indefinite-length chunk in indefinite-length byte string",
			data:         []byte{0x5f, 0x5f, 0xff, 0xff},
			eventsBefore: 1,
			wantErrorMsg: "cbor: indefinite-length byte string chunk is not definite-length",
		},
		{
			name:         "invalid additional information",
			data:         []byte{0x1c},
			eventsBefore: 0,
			wantErrorMsg: "cbor: invalid additional information 28 for type positive integer",
		},
		{
			name:         "invalid simple value",
			data:         []byte{0xf8, 0x10},
			eventsBefore: 0,
			wantErrorMsg: "cbor: invalid simple value 16 for type primitives",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			er := NewEventReader(newNBytesReader(tc.data, 1))
			for i := 0; i < tc.eventsBefore; i++ {
				if _, err := er.Next(); err != nil {
					t.Fatalf("Next() at event %d returned error %v", i, err)
				}
			}
			_, err := er.Next()
			if err == nil {
				t.Fatal("Next() didn't return error")
			}
			if err.Error() != tc.wantErrorMsg {
				t.Errorf("Next() returned error %q, want %q", err.Error(), tc.wantErrorMsg)
			}
		})
	}
}

func TestEventReaderEmptyIndefiniteContainers(t *testing.T) {
	// [_ ] {_ : } -- wait, maps need pairs; use empty array and empty map.
	data := []byte{0x9f, 0xff, 0xbf, 0xff, 0x80, 0xa0}
	want := []EventType{
		EventArrayBegin, EventArrayEnd,
		EventMapBegin, EventMapEnd,
		EventArrayBegin, EventArrayEnd,
		EventMapBegin, EventMapEnd,
	}

	er := NewEventReader(newNBytesReader(data, 1))
	for i, w := range want {
		ev, err := er.Next()
		if err != nil {
			t.Fatalf("Next() at event %d returned error %v", i, err)
		}
		if ev.Type != w {
			t.Errorf("Next() at event %d = %v, want %v", i, ev.Type, w)
		}
	}
	if _, err := er.Next(); err != io.EOF {
		t.Errorf("Next() returned error %v, want io.EOF", err)
	}
}
