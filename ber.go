package ber

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxPacketLengthBytes specifies the maximum allowed packet size when calling ReadPacket or DecodePacket. Set to 0 for
// no limit.
var MaxPacketLengthBytes int64 = math.MaxInt32

// MaxNestingDepth specifies the maximum allowed nesting depth when calling ReadPacket, DecodePacket, or
// DecodePacketErr. Set to 0 for no limit.
var MaxNestingDepth int = 1000

type Packet struct {
	Identifier
	Value     interface{}
	ByteValue []byte
	// Data holds the raw content octets of a primitive packet. For a
	// constructed packet it is NOT populated after decoding or AppendChild --
	// the content is derived lazily from Children by Bytes(). Do not read Data
	// on a constructed packet; walk Children or call Bytes() instead.
	Data        *bytes.Buffer
	Children    []*Packet
	Description string
}

type Identifier struct {
	ClassType Class
	TagType   Type
	Tag       Tag
}

type Tag uint64

const (
	TagEOC              Tag = 0x00
	TagBoolean          Tag = 0x01
	TagInteger          Tag = 0x02
	TagBitString        Tag = 0x03
	TagOctetString      Tag = 0x04
	TagNULL             Tag = 0x05
	TagObjectIdentifier Tag = 0x06
	TagObjectDescriptor Tag = 0x07
	TagExternal         Tag = 0x08
	TagRealFloat        Tag = 0x09
	TagEnumerated       Tag = 0x0a
	TagEmbeddedPDV      Tag = 0x0b
	TagUTF8String       Tag = 0x0c
	TagRelativeOID      Tag = 0x0d
	TagSequence         Tag = 0x10
	TagSet              Tag = 0x11
	TagNumericString    Tag = 0x12
	TagPrintableString  Tag = 0x13
	TagT61String        Tag = 0x14
	TagVideotexString   Tag = 0x15
	TagIA5String        Tag = 0x16
	TagUTCTime          Tag = 0x17
	TagGeneralizedTime  Tag = 0x18
	TagGraphicString    Tag = 0x19
	TagVisibleString    Tag = 0x1a
	TagGeneralString    Tag = 0x1b
	TagUniversalString  Tag = 0x1c
	TagCharacterString  Tag = 0x1d
	TagBMPString        Tag = 0x1e
	TagBitmask          Tag = 0x1f // xxx11111b

	// HighTag indicates the start of a high-tag byte sequence
	HighTag Tag = 0x1f // xxx11111b
	// HighTagContinueBitmask indicates the high-tag byte sequence should continue
	HighTagContinueBitmask Tag = 0x80 // 10000000b
	// HighTagValueBitmask obtains the tag value from a high-tag byte sequence byte
	HighTagValueBitmask Tag = 0x7f // 01111111b
)

const (
	// LengthLongFormBitmask is the mask to apply to the length byte to see if a long-form byte sequence is used
	LengthLongFormBitmask = 0x80
	// LengthValueBitmask is the mask to apply to the length byte to get the number of bytes in the long-form byte sequence
	LengthValueBitmask = 0x7f

	// LengthIndefinite is returned from readLength to indicate an indefinite length
	LengthIndefinite = -1
)

var tagMap = map[Tag]string{
	TagEOC:              "EOC (End-of-Content)",
	TagBoolean:          "Boolean",
	TagInteger:          "Integer",
	TagBitString:        "Bit String",
	TagOctetString:      "Octet String",
	TagNULL:             "NULL",
	TagObjectIdentifier: "Object Identifier",
	TagObjectDescriptor: "Object Descriptor",
	TagExternal:         "External",
	TagRealFloat:        "Real (float)",
	TagEnumerated:       "Enumerated",
	TagEmbeddedPDV:      "Embedded PDV",
	TagUTF8String:       "UTF8 String",
	TagRelativeOID:      "Relative-OID",
	TagSequence:         "Sequence and Sequence of",
	TagSet:              "Set and Set OF",
	TagNumericString:    "Numeric String",
	TagPrintableString:  "Printable String",
	TagT61String:        "T61 String",
	TagVideotexString:   "Videotex String",
	TagIA5String:        "IA5 String",
	TagUTCTime:          "UTC Time",
	TagGeneralizedTime:  "Generalized Time",
	TagGraphicString:    "Graphic String",
	TagVisibleString:    "Visible String",
	TagGeneralString:    "General String",
	TagUniversalString:  "Universal String",
	TagCharacterString:  "Character String",
	TagBMPString:        "BMP String",
}

type Class uint8

const (
	ClassUniversal   Class = 0   // 00xxxxxxb
	ClassApplication Class = 64  // 01xxxxxxb
	ClassContext     Class = 128 // 10xxxxxxb
	ClassPrivate     Class = 192 // 11xxxxxxb
	ClassBitmask     Class = 192 // 11xxxxxxb
)

var ClassMap = map[Class]string{
	ClassUniversal:   "Universal",
	ClassApplication: "Application",
	ClassContext:     "Context",
	ClassPrivate:     "Private",
}

type Type uint8

const (
	TypePrimitive   Type = 0  // xx0xxxxxb
	TypeConstructed Type = 32 // xx1xxxxxb
	TypeBitmask     Type = 32 // xx1xxxxxb
)

var TypeMap = map[Type]string{
	TypePrimitive:   "Primitive",
	TypeConstructed: "Constructed",
}

var Debug = false

func PrintBytes(out io.Writer, buf []byte, indent string) {
	dataLines := make([]string, (len(buf)/30)+1)
	numLines := make([]string, (len(buf)/30)+1)

	for i, b := range buf {
		dataLines[i/30] += fmt.Sprintf("%02x ", b)
		numLines[i/30] += fmt.Sprintf("%02d ", (i+1)%100)
	}

	for i := 0; i < len(dataLines); i++ {
		_, _ = out.Write([]byte(indent + dataLines[i] + "\n"))
		_, _ = out.Write([]byte(indent + numLines[i] + "\n\n"))
	}
}

func WritePacket(out io.Writer, p *Packet) {
	printPacket(out, p, 0, false)
}

func PrintPacket(p *Packet) {
	printPacket(os.Stdout, p, 0, false)
}

// Return a string describing packet content. This is not recursive,
// If the packet is a sequence, use `printPacket()`, or browse
// sequence yourself.
func DescribePacket(p *Packet) string {
	classStr := ClassMap[p.ClassType]

	tagTypeStr := TypeMap[p.TagType]

	tagStr := fmt.Sprintf("0x%02X", p.Tag)

	if p.ClassType == ClassUniversal {
		tagStr = tagMap[p.Tag]
	}

	value := fmt.Sprint(p.Value)
	description := ""

	if p.Description != "" {
		description = p.Description + ": "
	}

	return fmt.Sprintf("%s(%s, %s, %s) Len=%d %q", description, classStr, tagTypeStr, tagStr, p.contentLength(), value)
}

// contentLength reports the length in bytes of the packet's BER content -- the
// value that follows the length octets -- computed without materializing the
// encoded form. For a constructed packet this recurses through its children.
func (p *Packet) contentLength() int {
	if len(p.Children) == 0 {
		if p.Data == nil {
			return 0
		}
		return p.Data.Len()
	}

	n := 0
	for _, child := range p.Children {
		c := child.contentLength()
		n += len(encodeIdentifier(child.Identifier)) + len(encodeLength(c)) + c
	}
	return n
}

func printPacket(out io.Writer, p *Packet, indent int, printBytes bool) {
	indentStr := ""

	for len(indentStr) != indent {
		indentStr += " "
	}

	_, _ = fmt.Fprintf(out, "%s%s\n", indentStr, DescribePacket(p))

	if printBytes {
		PrintBytes(out, p.Bytes(), indentStr)
	}

	for _, child := range p.Children {
		printPacket(out, child, indent+1, printBytes)
	}
}

// ReadPacket reads a single Packet from the reader.
func ReadPacket(reader io.Reader) (*Packet, error) {
	p, _, err := readPacket(reader, 0)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func DecodeString(data []byte) string {
	return string(data)
}

func ParseInt64(bytes []byte) (ret int64, err error) {
	if len(bytes) > 8 {
		// We'll overflow an int64 in this case.
		err = errors.New("integer too large")
		return
	}
	for bytesRead := 0; bytesRead < len(bytes); bytesRead++ {
		ret <<= 8
		ret |= int64(bytes[bytesRead])
	}

	// Shift up and down in order to sign extend the result.
	ret <<= 64 - uint8(len(bytes))*8
	ret >>= 64 - uint8(len(bytes))*8
	return
}

func encodeInteger(i int64) []byte {
	n := int64Length(i)
	out := make([]byte, n)

	var j int
	for ; n > 0; n-- {
		out[j] = byte(i >> uint((n-1)*8))
		j++
	}

	return out
}

func int64Length(i int64) (numBytes int) {
	numBytes = 1

	for i > 127 {
		numBytes++
		i >>= 8
	}

	for i < -128 {
		numBytes++
		i >>= 8
	}

	return
}

// DecodePacket decodes the given bytes into a single Packet
// If a decode error is encountered, nil is returned.
func DecodePacket(data []byte) *Packet {
	p, _, err := readPacket(bytes.NewBuffer(data), 0)
	if err != nil {
		return nil
	}

	return p
}

// DecodePacketErr decodes the given bytes into a single Packet
// If a decode error is encountered, nil is returned.
func DecodePacketErr(data []byte) (*Packet, error) {
	p, _, err := readPacket(bytes.NewBuffer(data), 0)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// readPacket reads a single Packet from the reader, returning the number of bytes read.
func readPacket(reader io.Reader, depth int) (*Packet, int, error) {
	if MaxNestingDepth > 0 && depth >= MaxNestingDepth {
		return nil, 0, fmt.Errorf("nesting depth %d exceeds maximum %d", depth, MaxNestingDepth)
	}

	identifier, length, read, err := readHeader(reader)
	if err != nil {
		return nil, read, err
	}

	if length != LengthIndefinite && MaxPacketLengthBytes > 0 && int64(length) > MaxPacketLengthBytes {
		return nil, read, fmt.Errorf("length %d greater than maximum %d", length, MaxPacketLengthBytes)
	}

	p := &Packet{
		Identifier: identifier,
	}

	p.Data = new(bytes.Buffer)
	p.Children = make([]*Packet, 0, 2)
	p.Value = nil

	if p.TagType == TypeConstructed {
		// TODO: if universal, ensure tag type is allowed to be constructed

		// Track how much content we've read
		contentRead := 0
		for {
			if length != LengthIndefinite {
				// End if we've read what we've been told to
				if contentRead == length {
					break
				}
				// Detect if a packet boundary didn't fall on the expected length
				if contentRead > length {
					return nil, read, fmt.Errorf("expected to read %d bytes, read %d", length, contentRead)
				}
			}

			// Read the next packet
			child, r, err := readPacket(reader, depth+1)
			if err != nil {
				return nil, read, unexpectedEOF(err)
			}
			contentRead += r
			read += r

			// Enforce the aggregate size limit for constructed packets. Indefinite length declares
			// no bound up front, so the content bytes are only known as they are read.
			if MaxPacketLengthBytes > 0 && int64(contentRead) > MaxPacketLengthBytes {
				return nil, read, fmt.Errorf("length %d greater than maximum %d", contentRead, MaxPacketLengthBytes)
			}

			// Test is this is the EOC marker for our packet
			if isEOCPacket(child) {
				if length == LengthIndefinite {
					break
				}
				return nil, read, errors.New("eoc child not allowed with definite length")
			}

			// Append and continue
			p.AppendChild(child)
		}
		return p, read, nil
	}

	if length == LengthIndefinite {
		return nil, read, errors.New("indefinite length used with primitive type")
	}

	var content []byte
	if length > 0 {
		// Read the content and limit it to the parsed length.
		// If the content is less than the length, we return an EOF error.
		content, err = io.ReadAll(io.LimitReader(reader, int64(length)))
		if err == nil && len(content) < int(length) {
			err = io.EOF
		}
		if err != nil {
			return nil, read, unexpectedEOF(err)
		}
		read += len(content)
	} else {
		// If length == 0, we set the ByteValue to an empty slice
		content = make([]byte, 0)
	}

	if p.ClassType == ClassUniversal {
		p.Data.Write(content)
		p.ByteValue = content

		switch p.Tag {
		case TagEOC:
		case TagBoolean:
			var val int64
			val, err = ParseInt64(content)

			p.Value = val != 0
		case TagInteger:
			p.Value, err = ParseInt64(content)
		case TagBitString:
		case TagOctetString:
			// the actual string encoding is not known here
			// (e.g. for LDAP content is already an UTF8-encoded
			// string). Return the data without further processing
			p.Value = DecodeString(content)
		case TagNULL:
		case TagObjectIdentifier:
			var arcs []*big.Int
			if arcs, err = parseOIDArcs(content); err == nil {
				p.Value = oidArcsToString(arcs)
			}
		case TagObjectDescriptor:
		case TagExternal:
		case TagRealFloat:
			p.Value, err = ParseReal(content)
		case TagEnumerated:
			p.Value, err = ParseInt64(content)
		case TagEmbeddedPDV:
		case TagUTF8String:
			val := DecodeString(content)
			if !utf8.Valid([]byte(val)) {
				err = errors.New("invalid UTF-8 string")
			} else {
				p.Value = val
			}
		case TagRelativeOID:
			var arcs []*big.Int
			if arcs, err = parseRelativeOIDArcs(content); err == nil {
				p.Value = oidArcsToString(arcs)
			}
		case TagSequence:
		case TagSet:
		case TagNumericString:
		case TagPrintableString:
			val := DecodeString(content)
			if err = isPrintableString(val); err == nil {
				p.Value = val
			}
		case TagT61String:
		case TagVideotexString:
		case TagIA5String:
			val := DecodeString(content)
			for i, c := range val {
				if c > 0x7F {
					err = fmt.Errorf("invalid character for IA5String at pos %d: %c", i, c)
					break
				}
			}
			if err == nil {
				p.Value = val
			}
		case TagUTCTime:
		case TagGeneralizedTime:
			p.Value, err = ParseGeneralizedTime(content)
		case TagGraphicString:
		case TagVisibleString:
		case TagGeneralString:
		case TagUniversalString:
		case TagCharacterString:
		case TagBMPString:
		}
	} else {
		p.Data.Write(content)
	}

	// A content-parse failure (invalid REAL, UTF-8, OID, over-long INTEGER,
	// etc.) yields no usable packet; return nil so callers never see a
	// partially-populated packet alongside an error.
	if err != nil {
		return nil, read, err
	}

	return p, read, nil
}

func isPrintableString(val string) error {
	for i, c := range val {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		default:
			switch c {
			case '\'', '(', ')', '+', ',', '-', '.', '=', '/', ':', '?', ' ':
			default:
				return fmt.Errorf("invalid character in position %d", i)
			}
		}
	}
	return nil
}

// Bytes returns the BER encoding of the packet and, for a constructed packet,
// its children. It is recomputed on each call (not cached), so avoid calling it
// repeatedly on large packets in hot paths.
func (p *Packet) Bytes() []byte {
	var out bytes.Buffer

	out.Write(encodeIdentifier(p.Identifier))

	if len(p.Children) == 0 {
		// Primitive packet (or an empty constructed one): the content is p.Data.
		out.Write(encodeLength(p.Data.Len()))
		out.Write(p.Data.Bytes())
		return out.Bytes()
	}

	// Constructed packet: serialize children on demand. We deliberately do not
	// cache this in p.Data (see AppendChild): doing so at every level made a
	// decoded tree cost O(depth x subtree) memory.
	var content bytes.Buffer
	for _, child := range p.Children {
		content.Write(child.Bytes())
	}
	out.Write(encodeLength(content.Len()))
	out.Write(content.Bytes())

	return out.Bytes()
}

func (p *Packet) AppendChild(child *Packet) {
	// Children are serialized lazily by Bytes(); we intentionally do not copy
	// the child's encoded form into p.Data. Eager buffering made every ancestor
	// retain a full copy of its subtree -- an O(depth x subtree) memory
	// amplification that a deeply-nested packet could exploit.
	p.Children = append(p.Children, child)
}

func Encode(classType Class, tagType Type, tag Tag, value interface{}, description string) *Packet {
	p := new(Packet)

	p.ClassType = classType
	p.TagType = tagType
	p.Tag = tag
	p.Data = new(bytes.Buffer)

	p.Children = make([]*Packet, 0, 2)

	p.Value = value
	p.Description = description

	if value != nil {
		v := reflect.ValueOf(value)

		switch classType {
		case ClassUniversal:
			switch tag {
			case TagOctetString:
				sv, ok := v.Interface().(string)

				if ok {
					p.Data.Write([]byte(sv))
				}
			case TagEnumerated:
				bv, ok := v.Interface().([]byte)
				if ok {
					p.Data.Write(bv)
				}
			case TagEmbeddedPDV:
				bv, ok := v.Interface().([]byte)
				if ok {
					p.Data.Write(bv)
				}
			}
		case ClassContext:
			switch tag {
			case TagEnumerated:
				bv, ok := v.Interface().([]byte)
				if ok {
					p.Data.Write(bv)
				}
			case TagEmbeddedPDV:
				bv, ok := v.Interface().([]byte)
				if ok {
					p.Data.Write(bv)
				}
			}
		}
	}
	return p
}

func NewSequence(description string) *Packet {
	return Encode(ClassUniversal, TypeConstructed, TagSequence, nil, description)
}

func NewBoolean(classType Class, tagType Type, tag Tag, value bool, description string) *Packet {
	intValue := int64(0)

	if value {
		intValue = 1
	}

	p := Encode(classType, tagType, tag, nil, description)

	p.Value = value
	p.Data.Write(encodeInteger(intValue))

	return p
}

// NewLDAPBoolean returns a RFC 4511-compliant Boolean packet.
func NewLDAPBoolean(classType Class, tagType Type, tag Tag, value bool, description string) *Packet {
	p := Encode(classType, tagType, tag, nil, description)

	p.Value = value
	if value {
		p.Data.Write([]byte{255})
	} else {
		p.Data.Write([]byte{0})
	}

	return p
}

// NewIntegerErr behaves like [NewInteger] but returns an error instead of
// panicking when value is not a supported signed or unsigned integer type.
func NewIntegerErr(classType Class, tagType Type, tag Tag, value interface{}, description string) (*Packet, error) {
	p := Encode(classType, tagType, tag, nil, description)

	p.Value = value
	switch v := value.(type) {
	case int:
		p.Data.Write(encodeInteger(int64(v)))
	case uint:
		p.Data.Write(encodeInteger(int64(v)))
	case int64:
		p.Data.Write(encodeInteger(v))
	case uint64:
		// TODO : check range or add encodeUInt...
		p.Data.Write(encodeInteger(int64(v)))
	case int32:
		p.Data.Write(encodeInteger(int64(v)))
	case uint32:
		p.Data.Write(encodeInteger(int64(v)))
	case int16:
		p.Data.Write(encodeInteger(int64(v)))
	case uint16:
		p.Data.Write(encodeInteger(int64(v)))
	case int8:
		p.Data.Write(encodeInteger(int64(v)))
	case uint8:
		p.Data.Write(encodeInteger(int64(v)))
	default:
		// TODO : add support for big.Int ?
		return nil, fmt.Errorf("invalid type %T, expected {u|}int{64|32|16|8}", v)
	}

	return p, nil
}

// NewInteger builds an INTEGER packet. It panics if value is not a signed or
// unsigned integer type; use [NewIntegerErr] to receive an error instead.
func NewInteger(classType Class, tagType Type, tag Tag, value interface{}, description string) *Packet {
	p, err := NewIntegerErr(classType, tagType, tag, value, description)
	if err != nil {
		panic(err.Error())
	}
	return p
}

func NewString(classType Class, tagType Type, tag Tag, value, description string) *Packet {
	p := Encode(classType, tagType, tag, nil, description)

	p.Value = value
	p.Data.Write([]byte(value))

	return p
}

func NewGeneralizedTime(classType Class, tagType Type, tag Tag, value time.Time, description string) *Packet {
	p := Encode(classType, tagType, tag, nil, description)
	var s string
	if value.Nanosecond() != 0 {
		s = value.Format(`20060102150405.000000000Z`)
	} else {
		s = value.Format(`20060102150405Z`)
	}
	p.Value = s
	p.Data.Write([]byte(s))
	return p
}

// NewRealErr behaves like [NewReal] but returns an error instead of panicking
// when value is not a float32 or float64.
func NewRealErr(classType Class, tagType Type, tag Tag, value interface{}, description string) (*Packet, error) {
	p := Encode(classType, tagType, tag, nil, description)

	switch v := value.(type) {
	case float64:
		p.Data.Write(encodeFloat(v))
	case float32:
		p.Data.Write(encodeFloat(float64(v)))
	default:
		return nil, fmt.Errorf("invalid type %T, expected float{64|32}", v)
	}
	return p, nil
}

// NewReal builds a REAL packet. It panics if value is not a float32 or float64;
// use [NewRealErr] to receive an error instead.
func NewReal(classType Class, tagType Type, tag Tag, value interface{}, description string) *Packet {
	p, err := NewRealErr(classType, tagType, tag, value, description)
	if err != nil {
		panic(err.Error())
	}
	return p
}

// NewOIDErr behaves like [NewOID] but returns an error instead of panicking on
// a non-string value or returning nil on an invalid OID string.
func NewOIDErr(classType Class, tagType Type, tag Tag, value interface{}, description string) (*Packet, error) {
	p := Encode(classType, tagType, tag, nil, description)

	v, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("invalid type %T, expected string", value)
	}
	encoded, err := encodeOID(v)
	if err != nil {
		return nil, err
	}
	p.Value = v
	p.Data.Write(encoded)
	return p, nil
}

// Deprecated: NewOID panics on a non-string value and returns nil on an invalid
// OID string. Use [NewOIDErr], which reports both as errors.
func NewOID(classType Class, tagType Type, tag Tag, value interface{}, description string) *Packet {
	p, err := NewOIDErr(classType, tagType, tag, value, description)
	if err != nil {
		if _, ok := value.(string); !ok {
			panic(err.Error())
		}
		return nil
	}
	return p
}

// NewRelativeOIDErr behaves like [NewRelativeOID] but returns an error instead
// of panicking on a non-string value or returning nil on invalid input.
func NewRelativeOIDErr(classType Class, tagType Type, tag Tag, value interface{}, description string) (*Packet, error) {
	p := Encode(classType, tagType, tag, nil, description)

	v, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("invalid type %T, expected string", value)
	}
	encoded, err := encodeRelativeOID(v)
	if err != nil {
		return nil, err
	}
	p.Value = v
	p.Data.Write(encoded)
	return p, nil
}

// Deprecated: NewRelativeOID panics on a non-string value and returns nil on
// invalid input. Use [NewRelativeOIDErr], which reports both as errors.
func NewRelativeOID(classType Class, tagType Type, tag Tag, value interface{}, description string) *Packet {
	p, err := NewRelativeOIDErr(classType, tagType, tag, value, description)
	if err != nil {
		if _, ok := value.(string); !ok {
			panic(err.Error())
		}
		return nil
	}
	return p
}

// Limits guarding OID encode/decode against pathological inputs: a single arc
// may hold at most maxOIDArcBits bits, and an OID may hold at most maxOIDArcs
// arcs in total.
const (
	maxOIDArcBits = 512
	maxOIDArcs    = 256
)

var (
	bigTwo    = big.NewInt(2)
	bigForty  = big.NewInt(40)
	bigEighty = big.NewInt(80)
)

// parseDottedArcs splits a dotted-decimal OID string (e.g. "1.2.840") into its
// arcs. Arcs are arbitrary-precision integers, so arcs beyond the int64 range
// are accepted. label names the OID kind for error messages.
func parseDottedArcs(oidString, label string) ([]*big.Int, error) {
	parts := strings.Split(oidString, ".")
	arcs := make([]*big.Int, len(parts))
	for i, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("invalid %s: empty arc", label)
		}
		arc, ok := new(big.Int).SetString(part, 10)
		if !ok {
			return nil, fmt.Errorf("invalid %s part '%s'", label, part)
		}
		arcs[i] = arc
	}
	return arcs, nil
}

// checkOIDArcs validates the arcs of an OID or RELATIVE-OID: every arc must be
// non-negative and within the per-arc bit-length limit, and the total arc
// count must not exceed maxOIDArcs. label names the OID kind for error
// messages.
func checkOIDArcs(arcs []*big.Int, label string) error {
	if len(arcs) > maxOIDArcs {
		return fmt.Errorf("invalid %s: %d arcs exceeds the limit of %d", label, len(arcs), maxOIDArcs)
	}
	for i, arc := range arcs {
		if arc == nil || arc.Sign() < 0 {
			return fmt.Errorf("invalid %s: arc %d is negative", label, i)
		}
		if arc.BitLen() > maxOIDArcBits {
			return fmt.Errorf("invalid %s: arc %d exceeds %d bits", label, i, maxOIDArcBits)
		}
	}
	return nil
}

// encodeOID takes a string representation of an OID and returns its DER-encoded byte slice along with any error.
func encodeOID(oidString string) ([]byte, error) {
	arcs, err := parseDottedArcs(oidString, "OID")
	if err != nil {
		return nil, err
	}
	return encodeOIDArcs(arcs)
}

// encodeOIDArcs encodes the given arcs as an OBJECT IDENTIFIER. The first two
// arcs are validated and combined into a single subidentifier (40*arc0 + arc1)
// using arbitrary-precision arithmetic, so a second arc beyond the int64 range
// is encoded losslessly.
func encodeOIDArcs(arcs []*big.Int) ([]byte, error) {
	if err := checkOIDArcs(arcs, "OID"); err != nil {
		return nil, err
	}
	if len(arcs) < 2 || arcs[0].Cmp(bigTwo) > 0 || (arcs[0].Cmp(bigTwo) < 0 && arcs[1].Cmp(bigForty) >= 0) {
		return nil, fmt.Errorf("invalid object identifier %v", arcs)
	}

	// The first two arcs are combined into a single value: 40*arc0 + arc1.
	first := new(big.Int).Mul(arcs[0], bigForty)
	first.Add(first, arcs[1])
	encoded := appendBase128Big(nil, first)
	for _, arc := range arcs[2:] {
		encoded = appendBase128Big(encoded, arc)
	}

	return encoded, nil
}

func encodeRelativeOID(oidString string) ([]byte, error) {
	arcs, err := parseDottedArcs(oidString, "RELATIVE OID")
	if err != nil {
		return nil, err
	}
	return encodeRelativeOIDArcs(arcs)
}

// encodeRelativeOIDArcs encodes the given arcs as a RELATIVE-OID, using the
// same base-128 logic as OBJECT IDENTIFIER arcs.
func encodeRelativeOIDArcs(arcs []*big.Int) ([]byte, error) {
	if err := checkOIDArcs(arcs, "RELATIVE OID"); err != nil {
		return nil, err
	}
	if len(arcs) == 0 {
		return nil, errors.New("invalid RELATIVE OID: no arcs")
	}

	var encoded []byte
	for _, arc := range arcs {
		encoded = appendBase128Big(encoded, arc)
	}

	return encoded, nil
}

// appendBase128Big appends the base-128 (DER subidentifier) encoding of the
// non-negative integer n to dst. The encoding does not depend on the machine
// word size, so values beyond the int64 range are encoded losslessly.
func appendBase128Big(dst []byte, n *big.Int) []byte {
	bitLen := n.BitLen()
	if bitLen == 0 {
		return append(dst, 0)
	}

	l := (bitLen + 6) / 7
	buf := make([]byte, l)
	tmp := new(big.Int).Set(n)
	for i := l - 1; i >= 0; i-- {
		o := byte(tmp.Int64() & 0x7f)
		if i != l-1 {
			o |= 0x80
		}
		buf[i] = o
		tmp.Rsh(tmp, 7)
	}

	return append(dst, buf...)
}

func OIDToString(oi []int) string {
	var s strings.Builder
	s.Grow(32)

	buf := make([]byte, 0, 19)
	for i, v := range oi {
		if i > 0 {
			s.WriteByte('.')
		}
		s.Write(strconv.AppendInt(buf, int64(v), 10))
	}

	return s.String()
}

// oidArcsToString formats arbitrary-precision OID arcs in dotted-decimal form.
func oidArcsToString(arcs []*big.Int) string {
	var s strings.Builder
	s.Grow(32)

	for i, arc := range arcs {
		if i > 0 {
			s.WriteByte('.')
		}
		s.WriteString(arc.String())
	}

	return s.String()
}

// parseObjectIdentifier parses an OBJECT IDENTIFIER from the given bytes and
// returns its arcs as ints. It fails if any arc does not fit in an int; use
// parseOIDArcs for arbitrary-precision arcs. An object identifier is a
// sequence of variable length integers that are assigned in a hierarchy.
func parseObjectIdentifier(bytes []byte) ([]int, error) {
	arcs, err := parseOIDArcs(bytes)
	if err != nil {
		return nil, err
	}
	return oidArcsToInts(arcs)
}

func parseRelativeObjectIdentifier(bytes []byte) ([]int, error) {
	arcs, err := parseRelativeOIDArcs(bytes)
	if err != nil {
		return nil, err
	}
	return oidArcsToInts(arcs)
}

// oidArcsToInts converts arbitrary-precision arcs to ints, failing if any arc
// does not fit in an int on this platform.
func oidArcsToInts(arcs []*big.Int) ([]int, error) {
	out := make([]int, len(arcs))
	for i, arc := range arcs {
		if !arc.IsInt64() {
			return nil, errors.New("base 128 integer too large")
		}
		v := arc.Int64()
		if int64(int(v)) != v {
			return nil, errors.New("base 128 integer too large")
		}
		out[i] = int(v)
	}
	return out, nil
}

// parseOIDArcs parses an OBJECT IDENTIFIER from the given bytes and returns
// its arcs as arbitrary-precision integers.
func parseOIDArcs(bytes []byte) ([]*big.Int, error) {
	if len(bytes) == 0 {
		return nil, errors.New("zero length OBJECT IDENTIFIER")
	}

	// In the worst case, we get two elements from the first subidentifier
	// (which is encoded differently) and then every subidentifier is a single
	// byte long.
	capHint := len(bytes) + 1
	if capHint > maxOIDArcs {
		capHint = maxOIDArcs
	}
	arcs := make([]*big.Int, 0, capHint)

	// The first subidentifier is 40*arc0 + arc1:
	// According to this packing, arc0 can take the values 0, 1 and 2 only.
	// When arc0 = 0 or arc0 = 1, then arc1 is <= 39. When arc0 = 2,
	// then there are no restrictions on arc1.
	// The first subidentifier merges two arcs (40*arc0 + arc1), so it may
	// exceed the per-arc bit-length limit by the 7 bits needed for the merge.
	v, offset, err := parseBase128Big(bytes, 0, maxOIDArcBits+7)
	if err != nil {
		return nil, err
	}
	if v.Cmp(bigEighty) < 0 {
		q, r := new(big.Int).QuoRem(v, bigForty, new(big.Int))
		arcs = append(arcs, q, r)
	} else {
		arcs = append(arcs, big.NewInt(2), new(big.Int).Sub(v, bigEighty))
	}

	return parseBase128Arcs(bytes, offset, arcs)
}

// parseRelativeOIDArcs parses a RELATIVE-OID from the given bytes and returns
// its arcs as arbitrary-precision integers.
func parseRelativeOIDArcs(bytes []byte) ([]*big.Int, error) {
	if len(bytes) == 0 {
		return nil, errors.New("zero length RELATIVE OBJECT IDENTIFIER")
	}

	return parseBase128Arcs(bytes, 0, nil)
}

// parseBase128Arcs decodes successive base-128 subidentifiers from
// bytes[offset:], appending them to arcs and returning the result.
func parseBase128Arcs(bytes []byte, offset int, arcs []*big.Int) ([]*big.Int, error) {
	for offset < len(bytes) {
		if len(arcs) >= maxOIDArcs {
			return nil, fmt.Errorf("object identifier has more than %d arcs", maxOIDArcs)
		}
		arc, next, err := parseBase128Big(bytes, offset, maxOIDArcBits)
		if err != nil {
			return nil, err
		}
		arcs = append(arcs, arc)
		offset = next
	}
	return arcs, nil
}

// parseBase128Big parses a base-128 encoded integer of arbitrary precision
// from the given offset in the given byte slice. It returns the value and the
// new offset. The encoding must be minimal (the leading octet must not be
// 0x80) and terminated (the final octet must have its high bit clear), and the
// value must not exceed maxBits bits.
func parseBase128Big(bytes []byte, offset, maxBits int) (*big.Int, int, error) {
	ret := new(big.Int)
	for shifted := 0; offset < len(bytes); shifted++ {
		b := bytes[offset]
		// integers should be minimally encoded, so the leading octet should
		// never be 0x80
		if shifted == 0 && b == 0x80 {
			return nil, 0, errors.New("integer is not minimally encoded")
		}
		ret.Lsh(ret, 7)
		ret.Or(ret, big.NewInt(int64(b&0x7f)))
		offset++
		if ret.BitLen() > maxBits {
			return nil, 0, errors.New("base 128 integer too large")
		}
		if b&0x80 == 0 {
			return ret, offset, nil
		}
	}
	return nil, 0, errors.New("truncated base 128 integer")
}
