// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/x448/float16"
)

// EventType identifies the kind of event returned by (*EventReader).Next.
type EventType int

const (
	// EventInvalid is the zero value and is never returned by (*EventReader).Next.
	EventInvalid EventType = iota

	// EventPositiveInt is a non-negative integer (major type 0). Event.Uint holds the value.
	EventPositiveInt

	// EventNegativeInt is a negative integer (major type 1). Event.Uint holds the encoded
	// argument; the actual value is -1-Event.Uint.
	EventNegativeInt

	// EventByteString is a complete definite-length byte string. Event.Bytes holds the content.
	EventByteString

	// EventTextString is a complete definite-length text string. Event.Text holds the content.
	EventTextString

	// EventByteStringBegin starts an indefinite-length byte string. It is followed by
	// EventByteStringChunk events and one EventStringEnd.
	EventByteStringBegin

	// EventTextStringBegin starts an indefinite-length text string. It is followed by
	// EventTextStringChunk events and one EventStringEnd.
	EventTextStringBegin

	// EventByteStringChunk is one definite-length chunk of an indefinite-length byte string.
	EventByteStringChunk

	// EventTextStringChunk is one definite-length chunk of an indefinite-length text string.
	EventTextStringChunk

	// EventStringEnd terminates an indefinite-length byte or text string.
	EventStringEnd

	// EventArrayBegin starts an array. Event.Length is the number of elements, or -1 for
	// indefinite-length arrays. Every EventArrayBegin is matched by an EventArrayEnd
	// (synthesized for definite-length arrays).
	EventArrayBegin

	// EventArrayEnd terminates an array.
	EventArrayEnd

	// EventMapBegin starts a map. Event.Length is the number of key-value pairs, or -1 for
	// indefinite-length maps. Every EventMapBegin is matched by an EventMapEnd
	// (synthesized for definite-length maps).
	EventMapBegin

	// EventMapEnd terminates a map.
	EventMapEnd

	// EventTag is a CBOR tag. Event.Uint holds the tag number; the tag content follows as
	// the next event(s).
	EventTag

	// EventBool is a boolean. Event.Bool holds the value.
	EventBool

	// EventNil is CBOR null.
	EventNil

	// EventUndefined is CBOR undefined.
	EventUndefined

	// EventFloat is a floating-point number. Event.Float holds the value.
	EventFloat

	// EventSimple is an unassigned/reserved simple value. Event.Simple holds the value.
	EventSimple
)

// String returns a human-readable name of the event type.
func (t EventType) String() string {
	switch t {
	case EventPositiveInt:
		return "positive integer"
	case EventNegativeInt:
		return "negative integer"
	case EventByteString:
		return "byte string"
	case EventTextString:
		return "text string"
	case EventByteStringBegin:
		return "byte string begin"
	case EventTextStringBegin:
		return "text string begin"
	case EventByteStringChunk:
		return "byte string chunk"
	case EventTextStringChunk:
		return "text string chunk"
	case EventStringEnd:
		return "string end"
	case EventArrayBegin:
		return "array begin"
	case EventArrayEnd:
		return "array end"
	case EventMapBegin:
		return "map begin"
	case EventMapEnd:
		return "map end"
	case EventTag:
		return "tag"
	case EventBool:
		return "bool"
	case EventNil:
		return "nil"
	case EventUndefined:
		return "undefined"
	case EventFloat:
		return "float"
	case EventSimple:
		return "simple value"
	default:
		return "invalid event"
	}
}

// Event is a single CBOR data item event produced by (*EventReader).Next.
// Which fields are set depends on Event.Type; see EventType constants.
type Event struct {
	Type EventType

	// Uint holds positive integers, negative integers in encoded form (actual value
	// is -1-Uint), and tag numbers.
	Uint uint64

	// Length is set for EventArrayBegin (element count) and EventMapBegin (pair count).
	// It is -1 for indefinite-length containers.
	Length int64

	// Bytes holds byte string content for EventByteString and EventByteStringChunk.
	Bytes []byte

	// Text holds text string content for EventTextString and EventTextStringChunk.
	Text string

	// Bool holds the value for EventBool.
	Bool bool

	// Float holds the value for EventFloat.
	Float float64

	// Simple holds the value for EventSimple.
	Simple SimpleValue
}

// UnclosedStructure describes a container or indefinite-length string that was
// still open when the input ended.
type UnclosedStructure struct {
	// Type is "array", "map", "byte string", or "text string".
	Type string

	// Offset is the absolute offset of the structure's first byte in the input stream.
	Offset int

	// Indefinite indicates the structure uses indefinite-length encoding.
	Indefinite bool
}

// IncompleteItemError is returned by (*EventReader).Next when the input ends in the
// middle of a data item. It reports the offset where more data was expected and the
// structures that were still open.
type IncompleteItemError struct {
	// Offset is the absolute offset in the input stream where more data was expected.
	Offset int

	// Unclosed lists the structures that were still open, outermost first.
	Unclosed []UnclosedStructure
}

// Error returns a description of the truncation, including the offset and any
// unclosed structures.
func (e *IncompleteItemError) Error() string {
	msg := "cbor: unexpected end of input at offset " + strconv.Itoa(e.Offset)
	if len(e.Unclosed) == 0 {
		return msg
	}
	var sb strings.Builder
	sb.WriteString(msg)
	sb.WriteString(": unclosed structure(s): ")
	for i, s := range e.Unclosed {
		if i > 0 {
			sb.WriteString(", ")
		}
		if s.Indefinite {
			sb.WriteString("indefinite-length ")
		}
		fmt.Fprintf(&sb, "%s (offset %d)", s.Type, s.Offset)
	}
	return sb.String()
}

// Unwrap returns io.ErrUnexpectedEOF so errors.Is and errors.As recognize the
// truncation as an unexpected EOF condition.
func (e *IncompleteItemError) Unwrap() error {
	return io.ErrUnexpectedEOF
}

// eventFrame tracks an open array, map, or indefinite-length string.
type eventFrame struct {
	typ        cborType // array, map, byte string, or text string
	offset     int      // absolute offset of the structure's first byte
	remaining  uint64   // definite-length containers: remaining items (maps count key and value separately)
	count      uint64   // indefinite-length containers: items seen so far
	indefinite bool
}

// EventReader reads CBOR data items from an io.Reader as a stream of events:
// container begin/end, scalars, and chunks of indefinite-length strings.
//
// EventReader reads from the underlying io.Reader incrementally as events are
// consumed; it never reads the entire input up front. This makes it suitable for
// forwarding or filtering CBOR data item by data item without materializing
// complete values.
type EventReader struct {
	r      io.Reader
	buf    []byte // read-ahead buffer; buf[off:] is unconsumed
	off    int    // read offset within buf
	base   int    // absolute offset of buf[0] in the input stream
	frames []eventFrame
	last   EventType
}

// NewEventReader returns a new EventReader that reads CBOR events from r.
// No data is read from r until Next is called.
func NewEventReader(r io.Reader) *EventReader {
	return &EventReader{r: r}
}

// Offset returns the absolute offset of the next unread byte in the input stream,
// i.e. the number of bytes consumed so far.
func (er *EventReader) Offset() int {
	return er.base + er.off
}

// Next returns the next event. It returns io.EOF after the last complete data item
// when no more input remains. If the input ends in the middle of a data item, Next
// returns *IncompleteItemError describing the offset and unclosed structures.
func (er *EventReader) Next() (Event, error) { //nolint:gocyclo
	// Inside an indefinite-length string, the next input is a chunk or the break flag.
	if f := er.top(); f != nil && (f.typ == cborTypeByteString || f.typ == cborTypeTextString) {
		return er.nextStringChunk()
	}

	// Close definite-length containers whose items have all been consumed.
	// Tag frames complete silently (no end event) because a tag and its content
	// together form a single data item.
	for {
		f := er.top()
		if f == nil || f.indefinite || f.remaining > 0 {
			break
		}
		typ := er.pop().typ
		er.finishItem()
		if typ == cborTypeTag {
			continue
		}
		return er.emit(Event{Type: endEventType(typ)}), nil
	}

	itemOffset := er.Offset()

	b, err := er.readByte()
	if err != nil {
		if err == io.EOF && len(er.frames) == 0 {
			er.last = EventInvalid
			return Event{}, io.EOF
		}
		return Event{}, er.translateReadError(err)
	}

	if isBreakFlag(b) {
		f := er.top()
		if f == nil || !f.indefinite {
			return Event{}, &SyntaxError{msg: "cbor: unexpected \"break\" code"}
		}
		if f.typ == cborTypeMap && f.count%2 != 0 {
			return Event{}, &SyntaxError{msg: "cbor: unexpected \"break\" code"}
		}
		typ := er.pop().typ
		er.finishItem()
		return er.emit(Event{Type: endEventType(typ)}), nil
	}

	t, ai, val, err := er.readHead(b)
	if err != nil {
		return Event{}, err
	}

	switch t {
	case cborTypePositiveInt:
		er.finishItem()
		return er.emit(Event{Type: EventPositiveInt, Uint: val}), nil

	case cborTypeNegativeInt:
		er.finishItem()
		return er.emit(Event{Type: EventNegativeInt, Uint: val}), nil

	case cborTypeByteString, cborTypeTextString:
		if additionalInformation(ai).isIndefiniteLength() {
			er.push(eventFrame{typ: t, offset: itemOffset, indefinite: true})
			if t == cborTypeByteString {
				return er.emit(Event{Type: EventByteStringBegin}), nil
			}
			return er.emit(Event{Type: EventTextStringBegin}), nil
		}
		content, err := er.readContent(t, val)
		if err != nil {
			return Event{}, err
		}
		er.finishItem()
		if t == cborTypeByteString {
			return er.emit(Event{Type: EventByteString, Bytes: content}), nil
		}
		return er.emit(Event{Type: EventTextString, Text: string(content)}), nil

	case cborTypeArray, cborTypeMap:
		f := eventFrame{typ: t, offset: itemOffset}
		length := int64(-1)
		if additionalInformation(ai).isIndefiniteLength() {
			f.indefinite = true
		} else {
			if val > uint64(math.MaxInt64) {
				return Event{}, errors.New("cbor: " + t.String() + " length " + strconv.FormatUint(val, 10) + " is too large, it would cause integer overflow")
			}
			f.remaining = val
			if t == cborTypeMap {
				f.remaining = 2 * val // cannot overflow: val <= math.MaxInt64
			}
			length = int64(val)
		}
		er.push(f)
		if t == cborTypeArray {
			return er.emit(Event{Type: EventArrayBegin, Length: length}), nil
		}
		return er.emit(Event{Type: EventMapBegin, Length: length}), nil

	case cborTypeTag:
		// A tag and its content form a single data item, so the tag is tracked
		// as a frame that completes when its content item completes.
		er.push(eventFrame{typ: t, offset: itemOffset, remaining: 1})
		return er.emit(Event{Type: EventTag, Uint: val}), nil

	case cborTypePrimitives:
		return er.primitiveEvent(ai, val)
	}

	// Unreachable: cborType covers all 3-bit major types.
	return Event{}, &SyntaxError{msg: "cbor: invalid type " + t.String()}
}

// Skip discards the remainder of the current data item. If the most recent event
// began an array, map, indefinite-length string, or tag, Skip consumes events
// through the end of that item. If the most recent event was a chunk of an
// indefinite-length string, Skip consumes the remaining chunks and the string end.
// Otherwise the current item is already complete and Skip does nothing.
func (er *EventReader) Skip() error {
	switch er.last {
	case EventByteStringChunk, EventTextStringChunk:
		// Finish the current indefinite-length string.
		for {
			ev, err := er.Next()
			if err != nil {
				return err
			}
			if ev.Type == EventStringEnd {
				return nil
			}
		}

	case EventByteStringBegin, EventTextStringBegin, EventArrayBegin, EventMapBegin:
		// The begin event was already delivered; consume through the matching end.
		return er.skipContainerBody()

	case EventTag:
		// Consume the tag content (one complete item).
		pending := 1
		for pending > 0 {
			ev, err := er.Next()
			if err != nil {
				return err
			}
			switch ev.Type {
			case EventTag:
				// Content of a nested tag is still one item.
			case EventByteStringBegin, EventTextStringBegin, EventArrayBegin, EventMapBegin:
				if err := er.skipContainerBody(); err != nil {
					return err
				}
				pending--
			default:
				pending--
			}
		}
		return nil

	default:
		return nil
	}
}

// skipContainerBody consumes events through the end of the container whose begin
// event was just delivered.
func (er *EventReader) skipContainerBody() error {
	depth := 1
	for depth > 0 {
		ev, err := er.Next()
		if err != nil {
			return err
		}
		switch ev.Type {
		case EventByteStringBegin, EventTextStringBegin, EventArrayBegin, EventMapBegin:
			depth++
		case EventStringEnd, EventArrayEnd, EventMapEnd:
			depth--
		}
	}
	return nil
}

// nextStringChunk reads the next chunk (or the terminating break flag) of the
// innermost open indefinite-length string.
func (er *EventReader) nextStringChunk() (Event, error) {
	typ := er.top().typ

	b, err := er.readByte()
	if err != nil {
		return Event{}, er.translateReadError(err)
	}

	if isBreakFlag(b) {
		er.pop()
		er.finishItem()
		return er.emit(Event{Type: EventStringEnd}), nil
	}

	t, ai := parseInitialByte(b)
	if t != typ {
		return Event{}, &SyntaxError{msg: "cbor: wrong element type " + t.String() + " for indefinite-length " + typ.String()}
	}
	if additionalInformation(ai).isIndefiniteLength() {
		return Event{}, &SyntaxError{msg: "cbor: indefinite-length " + typ.String() + " chunk is not definite-length"}
	}

	_, _, val, err := er.readHead(b)
	if err != nil {
		return Event{}, err
	}

	content, err := er.readContent(typ, val)
	if err != nil {
		return Event{}, err
	}

	if typ == cborTypeByteString {
		return er.emit(Event{Type: EventByteStringChunk, Bytes: content}), nil
	}
	return er.emit(Event{Type: EventTextStringChunk, Text: string(content)}), nil
}

// primitiveEvent converts major type 7 heads (simple values, floats, bool, nil,
// undefined) to events.
func (er *EventReader) primitiveEvent(ai byte, val uint64) (Event, error) {
	var ev Event
	switch ai {
	case additionalInformationAsFalse:
		ev = Event{Type: EventBool}
	case additionalInformationAsTrue:
		ev = Event{Type: EventBool, Bool: true}
	case additionalInformationAsNull:
		ev = Event{Type: EventNil}
	case additionalInformationAsUndefined:
		ev = Event{Type: EventUndefined}
	case additionalInformationAsFloat16:
		ev = Event{Type: EventFloat, Float: float64(float16.Frombits(uint16(val)).Float32())} //nolint:gosec
	case additionalInformationAsFloat32:
		ev = Event{Type: EventFloat, Float: float64(math.Float32frombits(uint32(val)))} //nolint:gosec
	case additionalInformationAsFloat64:
		ev = Event{Type: EventFloat, Float: math.Float64frombits(val)}
	default:
		// ai <= 19 (simple value in additional information) or ai == 24 (simple value
		// in 1-byte argument, already validated to be >= 32 by readHead).
		ev = Event{Type: EventSimple, Simple: SimpleValue(val)} //nolint:gosec
	}
	er.finishItem()
	return er.emit(ev), nil
}

// readHead parses a data item head whose initial byte b was already consumed and
// returns the major type, additional information, and argument value.
func (er *EventReader) readHead(b byte) (t cborType, ai byte, val uint64, err error) {
	t, ai = parseInitialByte(b)

	if ai <= maxAdditionalInformationWithoutArgument {
		return t, ai, uint64(ai), nil
	}

	var argumentSize int
	switch ai {
	case additionalInformationWith1ByteArgument:
		argumentSize = 1
	case additionalInformationWith2ByteArgument:
		argumentSize = 2
	case additionalInformationWith4ByteArgument:
		argumentSize = 4
	case additionalInformationWith8ByteArgument:
		argumentSize = 8
	case additionalInformationAsIndefiniteLengthFlag:
		switch t {
		case cborTypeByteString, cborTypeTextString, cborTypeArray, cborTypeMap:
			return t, ai, 0, nil
		}
		return 0, 0, 0, &SyntaxError{msg: "cbor: invalid additional information " + strconv.Itoa(int(ai)) + " for type " + t.String()}
	default:
		// ai == 28, 29, 30
		return 0, 0, 0, &SyntaxError{msg: "cbor: invalid additional information " + strconv.Itoa(int(ai)) + " for type " + t.String()}
	}

	var argument [8]byte
	if err := er.readFull(argument[:argumentSize]); err != nil {
		return 0, 0, 0, er.translateReadError(err)
	}
	switch argumentSize {
	case 1:
		val = uint64(argument[0])
	case 2:
		val = uint64(binary.BigEndian.Uint16(argument[:2]))
	case 4:
		val = uint64(binary.BigEndian.Uint32(argument[:4]))
	case 8:
		val = binary.BigEndian.Uint64(argument[:8])
	}

	if t == cborTypePrimitives && ai == additionalInformationWith1ByteArgument && val < 32 {
		return 0, 0, 0, &SyntaxError{msg: "cbor: invalid simple value " + strconv.Itoa(int(val)) + " for type " + t.String()}
	}

	return t, ai, val, nil
}

// readContent reads length bytes of string content.
func (er *EventReader) readContent(t cborType, length uint64) ([]byte, error) {
	valInt := int(length) //nolint:gosec
	if valInt < 0 {
		return nil, errors.New("cbor: " + t.String() + " length " + strconv.FormatUint(length, 10) + " is too large, causing integer overflow")
	}
	content := make([]byte, valInt)
	if err := er.readFull(content); err != nil {
		return nil, er.translateReadError(err)
	}
	return content, nil
}

// readByte consumes and returns the next input byte.
func (er *EventReader) readByte() (byte, error) {
	if err := er.fill(); err != nil {
		return 0, err
	}
	b := er.buf[er.off]
	er.off++
	return b, nil
}

// readFull consumes exactly len(p) bytes into p.
func (er *EventReader) readFull(p []byte) error {
	for len(p) > 0 {
		if er.off < len(er.buf) {
			n := copy(p, er.buf[er.off:])
			er.off += n
			p = p[n:]
			continue
		}
		if len(p) >= cap(er.buf) {
			// Read directly into p to avoid an extra copy.
			er.base += er.off
			er.buf = er.buf[:0]
			er.off = 0
			n, err := io.ReadFull(er.r, p)
			er.base += n
			return err
		}
		if err := er.fill(); err != nil {
			return err
		}
	}
	return nil
}

// fill ensures at least one unconsumed byte is buffered, reading from the
// underlying reader only when the buffer is exhausted.
func (er *EventReader) fill() error {
	for er.off == len(er.buf) {
		er.base += er.off
		er.buf = er.buf[:0]
		er.off = 0
		if cap(er.buf) == 0 {
			er.buf = make([]byte, 0, 512)
		}
		n, err := er.r.Read(er.buf[:cap(er.buf)])
		er.buf = er.buf[:n]
		if n == 0 {
			if err != nil {
				return err
			}
			// Reader returned no progress; retry.
		}
	}
	return nil
}

// translateReadError converts EOF conditions encountered in the middle of a data
// item to *IncompleteItemError.
func (er *EventReader) translateReadError(err error) error {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return er.incompleteError()
	}
	return err
}

// incompleteError builds an *IncompleteItemError for the current position,
// listing all structures that are still open.
func (er *EventReader) incompleteError() error {
	e := &IncompleteItemError{Offset: er.Offset()}
	for _, f := range er.frames {
		e.Unclosed = append(e.Unclosed, UnclosedStructure{
			Type:       structureTypeName(f.typ),
			Offset:     f.offset,
			Indefinite: f.indefinite,
		})
	}
	return e
}

// finishItem records completion of one item in the innermost open container.
func (er *EventReader) finishItem() {
	f := er.top()
	if f == nil {
		return
	}
	if f.indefinite {
		f.count++
	} else if f.remaining > 0 {
		f.remaining--
	}
}

func (er *EventReader) push(f eventFrame) {
	er.frames = append(er.frames, f)
}

func (er *EventReader) pop() eventFrame {
	f := er.frames[len(er.frames)-1]
	er.frames = er.frames[:len(er.frames)-1]
	return f
}

func (er *EventReader) top() *eventFrame {
	if len(er.frames) == 0 {
		return nil
	}
	return &er.frames[len(er.frames)-1]
}

func (er *EventReader) emit(ev Event) Event {
	er.last = ev.Type
	return ev
}

// endEventType returns the end event type matching an open structure type.
func endEventType(t cborType) EventType {
	switch t {
	case cborTypeArray:
		return EventArrayEnd
	case cborTypeMap:
		return EventMapEnd
	default:
		return EventStringEnd
	}
}

// structureTypeName returns a short human-readable name for an open structure type.
func structureTypeName(t cborType) string {
	switch t {
	case cborTypeArray:
		return "array"
	case cborTypeMap:
		return "map"
	case cborTypeByteString:
		return "byte string"
	case cborTypeTextString:
		return "text string"
	default:
		return t.String()
	}
}
