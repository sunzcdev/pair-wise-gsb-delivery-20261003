// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"encoding/binary"
	"io"
	"math"
	"strconv"
	"unicode/utf8"

	"github.com/x448/float16"
)

// MajorType identifies the CBOR major type of an Event.
type MajorType = cborType

const (
	// MajorTypePositiveInteger is CBOR major type 0.
	MajorTypePositiveInteger MajorType = cborTypePositiveInt
	// MajorTypeNegativeInteger is CBOR major type 1.
	MajorTypeNegativeInteger MajorType = cborTypeNegativeInt
	// MajorTypeByteString is CBOR major type 2.
	MajorTypeByteString MajorType = cborTypeByteString
	// MajorTypeTextString is CBOR major type 3.
	MajorTypeTextString MajorType = cborTypeTextString
	// MajorTypeArray is CBOR major type 4.
	MajorTypeArray MajorType = cborTypeArray
	// MajorTypeMap is CBOR major type 5.
	MajorTypeMap MajorType = cborTypeMap
	// MajorTypeTag is CBOR major type 6.
	MajorTypeTag MajorType = cborTypeTag
	// MajorTypePrimitives is CBOR major type 7.
	MajorTypePrimitives MajorType = cborTypePrimitives
)

// EventKind classifies an Event returned by EventReader.Next.
type EventKind uint8

const (
	// EventScalar is an atomic (non-container) data item: an integer, float,
	// simple value, or a definite-length byte/text string.  The decoded value
	// is available in Event.Value; for byte/text strings the raw payload is
	// also available in Event.Bytes.
	EventScalar EventKind = iota

	// EventContainerStart marks the beginning of an array, map, or
	// indefinite-length byte/text string.  Event.Length is the number of
	// elements or pairs for definite-length containers, or -1 for
	// indefinite-length containers.
	EventContainerStart

	// EventContainerEnd marks the end of a container previously reported with
	// EventContainerStart.
	EventContainerEnd

	// EventStringChunk is one definite-length chunk of an indefinite-length
	// byte/text string.  Event.Bytes holds the chunk payload and Event.Length
	// is its length; Event.Value is not populated.
	EventStringChunk
)

// Event is a single streaming CBOR data item returned by EventReader.Next.
//
// Bytes is populated only for definite-length byte/text string scalars and
// indefinite-length string chunks.  It aliases the reader's internal buffer
// and is valid only until the next call to Next or Skip.
//
// Tags, when non-empty, lists the chain of CBOR tag numbers (outermost first)
// that precede the data item the event describes.
type Event struct {
	// Kind is the kind of this event.
	Kind EventKind

	// Type is the CBOR major type of the event's data item.
	Type MajorType

	// Length is the declared size of a container start (number of array
	// elements or map pairs, or -1 for indefinite length) or the number of
	// payload bytes of a string scalar or chunk.  It is 0 for other scalars.
	Length int64

	// Value holds the decoded value of an EventScalar, using the same default
	// Go type mapping as Decoder.Decode into interface{}.  It is nil for
	// container and chunk events.
	Value any

	// Bytes holds the payload of a string scalar or chunk.  It is valid only
	// until the next call to Next or Skip.
	Bytes []byte

	// Tags lists the tag numbers wrapping this data item, outermost first.
	Tags []uint64
}

// UnexpectedEOFError reports that the underlying input ended before the
// current CBOR data item was complete.
type UnexpectedEOFError struct {
	// Offset is the absolute byte position in the input at which more data
	// was expected, i.e. the number of bytes consumed so far.
	Offset int64

	// Unclosed lists the data items that were left open, outermost first.
	Unclosed []string
}

func (e *UnexpectedEOFError) Error() string {
	msg := "cbor: unexpected end of input at offset " + strconv.FormatInt(e.Offset, 10)
	if len(e.Unclosed) > 0 {
		msg += "; unclosed: " + joinUnclosed(e.Unclosed)
	}
	return msg
}

// Unwrap lets errors.Is(err, io.ErrUnexpectedEOF) match truncated streams.
func (e *UnexpectedEOFError) Unwrap() error { return io.ErrUnexpectedEOF }

func joinUnclosed(items []string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += " > "
		}
		out += item
	}
	return out
}

// EventReader incrementally reads CBOR data items from an underlying
// io.Reader without buffering an entire top-level data item up front.
//
// Arrays, maps, and indefinite-length strings are reported as
// EventContainerStart and EventContainerEnd events; atomic values are
// EventScalar events; and chunks of indefinite-length byte/text strings are
// EventStringChunk events.  Only the bytes needed to produce the next event
// are read from r, so a Reader that returns a few bytes at a time works
// directly even for very large indefinite-length arrays.
type EventReader struct {
	r  io.Reader
	dm *decMode

	buf       []byte
	off       int // index of first unconsumed byte in buf
	bytesRead int // absolute bytes consumed from r

	frames []eventFrame

	// rootActive is true while a top-level data item is being read and false
	// between complete top-level items.
	rootActive bool

	d decoder

	err error
}

type eventFrame struct {
	typ cborType

	// length is the declared number of array elements or map pairs (a map pair
	// counts once here), or -1 for indefinite-length containers.
	length int64

	// indefinite is true for indefinite-length containers.
	indefinite bool

	// remaining is the number of direct child items still expected for a
	// definite-length array/map (a map pair counts as two).  It is -1 for
	// indefinite-length containers.
	remaining int64

	// delivered counts direct child items already emitted for an
	// indefinite-length array/map (a map pair counts as two).
	delivered int64

	// tags wrapped this container, reported on its ContainerEnd.
	tags []uint64
}

// NewEventReader returns a new streaming reader that reads CBOR data items
// from r using the default decoding options.
func NewEventReader(r io.Reader) *EventReader {
	return defaultDecMode.NewEventReader(r)
}

// NumBytesRead returns the number of bytes consumed from the underlying reader.
func (er *EventReader) NumBytesRead() int {
	return int(er.inputOffset())
}

// Next returns the next streaming event.  After a complete top-level data item
// has been delivered it returns io.EOF.  If the input ends while a data item is
// incomplete it returns *UnexpectedEOFError reporting the byte offset and any
// unclosed containers.
func (er *EventReader) Next() (Event, error) {
	if er.err != nil {
		return Event{}, er.err
	}
	ev, err := er.next()
	if err != nil {
		er.err = err
	}
	return ev, err
}

func (er *EventReader) next() (Event, error) {
	// Auto-close a completed definite-length array/map.
	if n := len(er.frames); n > 0 {
		top := &er.frames[n-1]
		if !top.indefinite && top.remaining == 0 {
			return er.closeFrame(), nil
		}
	}

	if len(er.frames) > 0 && isStringType(er.frames[len(er.frames)-1].typ) {
		return er.readChunk()
	}

	if err := er.ensure(1); err != nil {
		return Event{}, err
	}
	if len(er.frames) > 0 && isBreakFlag(er.buf[er.off]) {
		top := er.frames[len(er.frames)-1]
		if top.typ == cborTypeMap && top.delivered%2 == 1 {
			return Event{}, er.syntaxError(`unexpected "break" code`)
		}
		if top.typ == cborTypeArray && top.delivered > int64(er.dm.maxArrayElements) {
			return Event{}, &MaxArrayElementsError{er.dm.maxArrayElements}
		}
		if top.typ == cborTypeMap && top.delivered/2 > int64(er.dm.maxMapPairs) {
			return Event{}, &MaxMapPairsError{er.dm.maxMapPairs}
		}
		er.off++
		return er.closeFrame(), nil
	}
	return er.readItem()
}

// Skip advances past the data item at the current position without returning
// its events.  At a container boundary it skips the whole container including
// children; inside an indefinite-length byte/text string it skips the next
// single chunk.
func (er *EventReader) Skip() error {
	if er.err != nil {
		return er.err
	}
	err := er.skip()
	if err != nil {
		er.err = err
	}
	return err
}

func (er *EventReader) skip() error {
	if n := len(er.frames); n > 0 {
		top := &er.frames[n-1]
		if !top.indefinite && top.remaining == 0 {
			er.closeFrame()
			return nil
		}
	}

	// Inside an indefinite-length string, Skip skips exactly one chunk (or
	// closes the container on a break code), mirroring readChunk.
	if len(er.frames) > 0 && isStringType(er.frames[len(er.frames)-1].typ) {
		return er.skipChunk()
	}

	if len(er.frames) == 0 {
		if err := er.ensure(1); err != nil {
			return err
		}
	}
	return er.skipItem(er.containerDepth())
}

// skipChunk consumes the next chunk of an open indefinite-length string, or
// closes the string when the next byte is the break code.
func (er *EventReader) skipChunk() error {
	if err := er.ensure(1); err != nil {
		return err
	}
	parent := er.frames[len(er.frames)-1].typ
	if isBreakFlag(er.buf[er.off]) {
		er.off++
		er.closeFrame()
		return nil
	}
	t, ai, val, err := er.parseHead()
	if err != nil {
		return err
	}
	if t != parent {
		return er.syntaxError("wrong element type " + t.String() +
			" for indefinite-length " + parent.String())
	}
	if additionalInformation(ai).isIndefiniteLength() {
		return er.syntaxError("indefinite-length " + t.String() +
			" chunk is not definite-length")
	}
	if err := er.discardBytes(int64(val)); err != nil {
		return err
	}
	er.afterChild()
	return nil
}

func (er *EventReader) readItem() (Event, error) {
	er.rootActive = true
	tags, t, ai, val, err := er.readTagsAndHead()
	if err != nil {
		return Event{}, err
	}

	switch t {
	case cborTypeByteString, cborTypeTextString:
		if additionalInformation(ai).isIndefiniteLength() {
			if er.dm.indefLength == IndefLengthForbidden {
				return Event{}, &IndefiniteLengthError{t}
			}
			return er.openFrame(t, -1, tags), nil
		}

		length := int64(val)
		if length < 0 {
			return Event{}, er.syntaxError(t.String() + " length " + strconv.FormatUint(val, 10) +
				" is too large, causing integer overflow")
		}
		// Buffer the whole definite string and let the standard decoder parse
		// the complete (possibly tagged) item so tag-based decoding and UTF-8
		// handling match Decoder.Decode.
		if err := er.ensureBytes(length); err != nil {
			return Event{}, err
		}
		raw, err := er.readBytes(length)
		if err != nil {
			return Event{}, err
		}
		value, err := er.decodeStringValue(t, tags, raw)
		if err != nil {
			return Event{}, err
		}
		ev := Event{
			Kind:   EventScalar,
			Type:   t,
			Length: length,
			Value:  value,
			Bytes:  raw,
			Tags:   tags,
		}
		er.afterChild()
		return ev, nil

	case cborTypeArray, cborTypeMap:
		if additionalInformation(ai).isIndefiniteLength() {
			if er.dm.indefLength == IndefLengthForbidden {
				return Event{}, &IndefiniteLengthError{t}
			}
			if er.containerDepth()+len(tags)+1 > er.dm.maxNestedLevels {
				return Event{}, &MaxNestedLevelError{er.dm.maxNestedLevels}
			}
			return er.openFrame(t, -1, tags), nil
		}

		length := int64(val)
		if length < 0 {
			return Event{}, er.syntaxError(t.String() + " length " + strconv.FormatUint(val, 10) +
				" is too large, it would cause integer overflow")
		}
		if t == cborTypeArray {
			if length > int64(er.dm.maxArrayElements) {
				return Event{}, &MaxArrayElementsError{er.dm.maxArrayElements}
			}
		} else {
			if length > int64(er.dm.maxMapPairs) {
				return Event{}, &MaxMapPairsError{er.dm.maxMapPairs}
			}
		}
		if er.containerDepth()+len(tags)+1 > er.dm.maxNestedLevels {
			return Event{}, &MaxNestedLevelError{er.dm.maxNestedLevels}
		}
		children := length
		if t == cborTypeMap {
			children = length * 2
		}
		return er.openDefiniteFrame(t, length, children, tags), nil

	default:
		// Atomic items (major type 0, 1, 7) carry their value in the head,
		// which was already consumed; reconstruct it for standard parsing.
		encoded := make([]byte, 0, len(tags)*9+9)
		for _, tagNum := range tags {
			encoded = appendHead(encoded, byte(cborTypeTag), tagNum)
		}
		encoded = appendHead(encoded, byte(t), val)
		value, _, err := er.parseBuffered(encoded)
		if err != nil {
			return Event{}, err
		}
		ev := Event{
			Kind:  EventScalar,
			Type:  t,
			Value: value,
			Tags:  tags,
		}
		er.afterChild()
		return ev, nil
	}
}

func (er *EventReader) readChunk() (Event, error) {
	if err := er.ensure(1); err != nil {
		return Event{}, err
	}
	parent := er.frames[len(er.frames)-1].typ

	if isBreakFlag(er.buf[er.off]) {
		er.off++
		return er.closeFrame(), nil
	}

	t, ai, val, err := er.parseHead()
	if err != nil {
		return Event{}, err
	}
	if t != parent {
		return Event{}, er.syntaxError("wrong element type " + t.String() +
			" for indefinite-length " + parent.String())
	}
	if additionalInformation(ai).isIndefiniteLength() {
		return Event{}, er.syntaxError("indefinite-length " + t.String() +
			" chunk is not definite-length")
	}

	payload, err := er.readBytes(int64(val))
	if err != nil {
		return Event{}, err
	}
	ev := Event{
		Kind:   EventStringChunk,
		Type:   t,
		Length: int64(len(payload)),
		Bytes:  payload,
	}
	er.afterChild()
	return ev, nil
}

// readTagsAndHead consumes a chain of zero or more CBOR tags followed by the
// head of the tagged content.
func (er *EventReader) readTagsAndHead() (
	tags []uint64,
	t cborType,
	ai byte,
	val uint64,
	err error,
) {
	for {
		if err = er.ensure(1); err != nil {
			return nil, 0, 0, 0, err
		}
		if getType(er.buf[er.off]) != cborTypeTag {
			break
		}
		if er.dm.tagsMd == TagsForbidden {
			return nil, 0, 0, 0, &TagsMdError{}
		}
		var tagNum uint64
		if _, _, tagNum, err = er.parseHead(); err != nil {
			return nil, 0, 0, 0, err
		}
		tags = append(tags, tagNum)
		if er.containerDepth()+len(tags) > er.dm.maxNestedLevels {
			return nil, 0, 0, 0, &MaxNestedLevelError{er.dm.maxNestedLevels}
		}
	}

	if err = er.ensure(1); err != nil {
		return nil, 0, 0, 0, err
	}
	t, ai, val, err = er.parseHead()
	if err != nil {
		return nil, 0, 0, 0, err
	}
	return tags, t, ai, val, nil
}

// parseBuffered decodes one complete buffered data item (including any
// leading tags) using decoder.parse and returns the value and number of bytes
// consumed.  It does not advance er.off.
func (er *EventReader) parseBuffered(data []byte) (any, int, error) {
	er.d.dm = er.dm
	er.d.reset(data)
	value, err := er.d.parse(false)
	if err != nil {
		return nil, 0, err
	}
	return value, er.d.off, nil
}

// decodeStringValue decodes a definite-length string scalar whose head has
// already been consumed.  It reconstructs the complete (possibly tagged)
// encoded item so decoder.parse applies tag semantics and UTF-8 handling.
func (er *EventReader) decodeStringValue(t cborType, tags []uint64, payload []byte) (any, error) {
	if len(tags) == 0 {
		switch t {
		case cborTypeTextString:
			if er.dm.utf8 == UTF8RejectInvalid && !utf8.Valid(payload) {
				return nil, &SemanticError{"cbor: invalid UTF-8 string"}
			}
			return string(payload), nil
		case cborTypeByteString:
			clone := make([]byte, len(payload))
			copy(clone, payload)
			return clone, nil
		}
	}
	encoded := er.buildTaggedString(t, uint64(len(payload)), tags, payload)
	value, _, err := er.parseBuffered(encoded)
	return value, err
}

// buildTaggedString assembles tags + string head + payload.
func (er *EventReader) buildTaggedString(t cborType, length uint64, tags []uint64, payload []byte) []byte {
	buf := make([]byte, 0, len(tags)*9+9+len(payload))
	for _, tagNum := range tags {
		buf = appendHead(buf, byte(cborTypeTag), tagNum)
	}
	buf = appendHead(buf, byte(t), length)
	buf = append(buf, payload...)
	return buf
}

func (er *EventReader) openFrame(typ cborType, length int64, tags []uint64) Event {
	er.frames = append(er.frames, eventFrame{
		typ:        typ,
		length:     length,
		indefinite: true,
		remaining:  -1,
		tags:       tags,
	})
	return Event{
		Kind:   EventContainerStart,
		Type:   typ,
		Length: length,
		Tags:   tags,
	}
}

func (er *EventReader) openDefiniteFrame(typ cborType, length, children int64, tags []uint64) Event {
	er.frames = append(er.frames, eventFrame{
		typ:        typ,
		length:     length,
		indefinite: false,
		remaining:  children,
		tags:       tags,
	})
	return Event{
		Kind:   EventContainerStart,
		Type:   typ,
		Length: length,
		Tags:   tags,
	}
}

func (er *EventReader) closeFrame() Event {
	n := len(er.frames)
	frame := er.frames[n-1]
	er.frames = er.frames[:n-1]

	ev := Event{
		Kind:   EventContainerEnd,
		Type:   frame.typ,
		Length: frame.length,
		Tags:   frame.tags,
	}

	if len(er.frames) == 0 {
		er.rootActive = false
	} else {
		er.afterChild()
	}
	return ev
}

// afterChild records that one direct child item of the current container was
// consumed (a scalar, a complete container, or a string chunk).  A chunk
// inside an indefinite-length string is not a container child.  At the top
// level it ends the current root item.
func (er *EventReader) afterChild() {
	n := len(er.frames)
	if n == 0 {
		er.rootActive = false
		return
	}
	top := &er.frames[n-1]
	if isStringType(top.typ) {
		return
	}
	if top.indefinite {
		top.delivered++
		return
	}
	top.remaining--
}

// parseHead reads one CBOR head at the current position and advances past it.
// It validates additional information like the byte-level well-formedness
// checker.
func (er *EventReader) parseHead() (t cborType, ai byte, val uint64, err error) {
	headOff := er.inputOffset()
	er.d.dm = er.dm
	if err = er.ensure(1); err != nil {
		return 0, 0, 0, err
	}
	b := er.buf[er.off]
	t, ai = parseInitialByte(b)
	val = uint64(ai)
	er.off++

	if ai <= maxAdditionalInformationWithoutArgument {
		return t, ai, val, nil
	}

	var argSize int
	switch ai {
	case additionalInformationWith1ByteArgument:
		argSize = 1
	case additionalInformationWith2ByteArgument:
		argSize = 2
	case additionalInformationWith4ByteArgument:
		argSize = 4
	case additionalInformationWith8ByteArgument:
		argSize = 8
	default:
		if additionalInformation(ai).isIndefiniteLength() {
			switch t {
			case cborTypePositiveInt, cborTypeNegativeInt, cborTypeTag:
				return 0, 0, 0, er.syntaxErrorAt(headOff, "invalid additional information "+
					strconv.Itoa(int(ai))+" for type "+t.String())
			case cborTypePrimitives:
				return 0, 0, 0, er.syntaxErrorAt(headOff, `unexpected "break" code`)
			}
			return t, ai, val, nil
		}
		return 0, 0, 0, er.syntaxErrorAt(headOff, "invalid additional information "+
			strconv.Itoa(int(ai))+" for type "+t.String())
	}

	if err = er.ensure(argSize); err != nil {
		return 0, 0, 0, err
	}
	arg := er.buf[er.off : er.off+argSize]
	switch argSize {
	case 1:
		val = uint64(arg[0])
	case 2:
		val = uint64(binary.BigEndian.Uint16(arg))
	case 4:
		val = uint64(binary.BigEndian.Uint32(arg))
	case 8:
		val = binary.BigEndian.Uint64(arg)
	}
	er.off += argSize

	if t == cborTypePrimitives {
		switch ai {
		case additionalInformationWith1ByteArgument:
			if val < minSimpleValueIn1ByteArgument {
				return 0, 0, 0, er.syntaxErrorAt(headOff, "invalid simple value "+
					strconv.FormatUint(val, 10)+" for type "+t.String())
			}
		case additionalInformationWith2ByteArgument:
			if err = er.d.acceptableFloat(float64(float16.Frombits(uint16(val)).Float32())); err != nil {
				return 0, 0, 0, err
			}
		case additionalInformationWith4ByteArgument:
			if err = er.d.acceptableFloat(float64(math.Float32frombits(uint32(val)))); err != nil {
				return 0, 0, 0, err
			}
		case additionalInformationWith8ByteArgument:
			if err = er.d.acceptableFloat(math.Float64frombits(val)); err != nil {
				return 0, 0, 0, err
			}
		}
	}

	return t, ai, val, nil
}

// skipItem consumes exactly one complete data item at the current position
// and records it on the enclosing frame (or ends the root item).  Nested
// descent uses skipOne, which performs no frame bookkeeping.
func (er *EventReader) skipItem(baseDepth int) error {
	if err := er.skipOne(baseDepth); err != nil {
		return err
	}
	er.afterChild()
	return nil
}

// skipOne consumes exactly one complete data item at the current position
// without updating frame bookkeeping.  baseDepth is the array/map nesting
// depth already established by ancestor frames.
func (er *EventReader) skipOne(baseDepth int) error {
	depth := baseDepth

	var numTags int
	for {
		if err := er.ensure(1); err != nil {
			return err
		}
		if getType(er.buf[er.off]) != cborTypeTag {
			break
		}
		er.rootActive = true
		if er.dm.tagsMd == TagsForbidden {
			return &TagsMdError{}
		}
		if _, _, _, err := er.parseHead(); err != nil {
			return err
		}
		numTags++
		if depth+numTags > er.dm.maxNestedLevels {
			return &MaxNestedLevelError{er.dm.maxNestedLevels}
		}
	}

	if err := er.ensure(1); err != nil {
		return err
	}
	er.rootActive = true

	t, ai, val, err := er.parseHead()
	if err != nil {
		return err
	}

	switch t {
	case cborTypeByteString, cborTypeTextString:
		if additionalInformation(ai).isIndefiniteLength() {
			if er.dm.indefLength == IndefLengthForbidden {
				return &IndefiniteLengthError{t}
			}
			for {
				if err := er.ensure(1); err != nil {
					return err
				}
				if isBreakFlag(er.buf[er.off]) {
					er.off++
					break
				}
				ct, cai, cval, cerr := er.parseHead()
				if cerr != nil {
					return cerr
				}
				if ct != t {
					return er.syntaxError("wrong element type " + ct.String() +
						" for indefinite-length " + t.String())
				}
				if additionalInformation(cai).isIndefiniteLength() {
					return er.syntaxError("indefinite-length " + ct.String() +
						" chunk is not definite-length")
				}
				if err := er.discardBytes(int64(cval)); err != nil {
					return err
				}
			}
		} else if err := er.discardBytes(int64(val)); err != nil {
			return err
		}

	case cborTypeArray, cborTypeMap:
		if additionalInformation(ai).isIndefiniteLength() {
			if er.dm.indefLength == IndefLengthForbidden {
				return &IndefiniteLengthError{t}
			}
			if depth+numTags+1 > er.dm.maxNestedLevels {
				return &MaxNestedLevelError{er.dm.maxNestedLevels}
			}
			childDepth := depth + numTags + 1
			count := 0
			for {
				if err := er.ensure(1); err != nil {
					return err
				}
				if isBreakFlag(er.buf[er.off]) {
					er.off++
					if t == cborTypeMap && count%2 == 1 {
						return er.syntaxError(`unexpected "break" code`)
					}
					if t == cborTypeArray && count > er.dm.maxArrayElements {
						return &MaxArrayElementsError{er.dm.maxArrayElements}
					}
					if t == cborTypeMap && count/2 > er.dm.maxMapPairs {
						return &MaxMapPairsError{er.dm.maxMapPairs}
					}
					break
				}
				if err := er.skipOne(childDepth); err != nil {
					return err
				}
				count++
			}
		} else {
			length := int64(val)
			if length < 0 {
				return er.syntaxError(t.String() + " length " + strconv.FormatUint(val, 10) +
					" is too large, it would cause integer overflow")
			}
			if t == cborTypeArray {
				if length > int64(er.dm.maxArrayElements) {
					return &MaxArrayElementsError{er.dm.maxArrayElements}
				}
			} else if length > int64(er.dm.maxMapPairs) {
				return &MaxMapPairsError{er.dm.maxMapPairs}
			}
			if depth+numTags+1 > er.dm.maxNestedLevels {
				return &MaxNestedLevelError{er.dm.maxNestedLevels}
			}
			childDepth := depth + numTags + 1
			count := length
			if t == cborTypeMap {
				count *= 2
			}
			for range count {
				if err := er.skipOne(childDepth); err != nil {
					return err
				}
			}
		}

	default:
		// Atomic data item (major type 0, 1, 7); content is the head argument.
	}

	return nil
}

// ensure makes sure at least n unconsumed bytes are buffered, reading from r
// as needed.  It returns nil when the bytes are available.  At a clean end of
// input with no open structure it returns io.EOF; if a data item is incomplete
// it returns *UnexpectedEOFError.
func (er *EventReader) ensure(n int) error {
	for len(er.buf)-er.off < n {
		if err := er.fill(); err != nil {
			return er.translateReadError(err)
		}
	}
	return nil
}

// readBytes returns the next n payload bytes as a slice valid until the next
// Next/Skip call.
func (er *EventReader) readBytes(n int64) ([]byte, error) {
	if err := er.ensureBytes(n); err != nil {
		return nil, err
	}
	b := er.buf[er.off : er.off+int(n)]
	er.off += int(n)
	return b, nil
}

// ensureBytes buffers n payload bytes without consuming them.
func (er *EventReader) ensureBytes(n int64) error {
	for int64(len(er.buf)-er.off) < n {
		if err := er.fill(); err != nil {
			return er.translateReadError(err)
		}
	}
	return nil
}

// discardBytes consumes and drops n payload bytes.
func (er *EventReader) discardBytes(n int64) error {
	for n > 0 {
		avail := int64(len(er.buf) - er.off)
		if avail > 0 {
			take := n
			if take > avail {
				take = avail
			}
			er.off += int(take)
			n -= take
			if n == 0 {
				return nil
			}
		}
		if err := er.fill(); err != nil {
			return er.translateReadError(err)
		}
	}
	return nil
}

// fill reads more data from r, compacting consumed buffer space first.
func (er *EventReader) fill() error {
	if er.off > 0 {
		er.bytesRead += er.off
		n := copy(er.buf, er.buf[er.off:])
		er.buf = er.buf[:n]
		er.off = 0
	}

	const minRead = 512
	if cap(er.buf)-len(er.buf) < minRead {
		newBuf := make([]byte, len(er.buf), cap(er.buf)*2+minRead)
		copy(newBuf, er.buf)
		er.buf = newBuf
	}

	n, err := er.r.Read(er.buf[len(er.buf):cap(er.buf)])
	er.buf = er.buf[:len(er.buf)+n]
	if n > 0 && err == io.EOF {
		// A Reader may return n > 0 bytes together with io.EOF; the bytes are
		// valid and EOF is observed on the following read.
		return nil
	}
	return err
}

// inputOffset is the absolute offset of the next unconsumed byte.
func (er *EventReader) inputOffset() int64 {
	return int64(er.bytesRead + er.off)
}

// translateReadError turns a raw read error into io.EOF at a clean top-level
// boundary or a truncation error that includes the offset and open structures.
func (er *EventReader) translateReadError(err error) error {
	if err == nil {
		return nil
	}
	if err == io.EOF && !er.rootActive && len(er.frames) == 0 {
		return io.EOF
	}
	if err != io.EOF {
		return err
	}
	return er.unexpectedEOF()
}

func (er *EventReader) unexpectedEOF() error {
	unclosed := make([]string, 0, len(er.frames))
	for _, f := range er.frames {
		unclosed = append(unclosed, describeFrame(f))
	}
	return &UnexpectedEOFError{
		Offset:   int64(er.bytesRead + len(er.buf)),
		Unclosed: unclosed,
	}
}

func describeFrame(f eventFrame) string {
	switch f.typ {
	case cborTypeByteString, cborTypeTextString:
		return f.typ.String()
	case cborTypeArray:
		if f.indefinite {
			return "indefinite-length array"
		}
		return "array of length " + strconv.FormatInt(f.length, 10)
	case cborTypeMap:
		if f.indefinite {
			return "indefinite-length map"
		}
		return "map of length " + strconv.FormatInt(f.length, 10)
	default:
		return f.typ.String()
	}
}

func (er *EventReader) syntaxError(msg string) error {
	return &SyntaxError{"cbor: " + msg}
}

func (er *EventReader) syntaxErrorAt(off int64, msg string) error {
	return &SyntaxError{"cbor: " + msg + " at offset " + strconv.FormatInt(off, 10)}
}

func isStringType(t cborType) bool {
	return t == cborTypeByteString || t == cborTypeTextString
}

// containerDepth counts open arrays and maps (the only containers that count
// toward MaxNestedLevels, per RFC 8949); indefinite-length strings are
// excluded.
func (er *EventReader) containerDepth() int {
	depth := 0
	for i := range er.frames {
		if er.frames[i].typ == cborTypeArray || er.frames[i].typ == cborTypeMap {
			depth++
		}
	}
	return depth
}
