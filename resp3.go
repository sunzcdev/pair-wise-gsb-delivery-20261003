package redcon

import (
	"bytes"
	"errors"
	"math"
	"math/big"
	"strconv"
	"unsafe"
)

// RESP3 protocol kinds, in addition to the RESP2 kinds (Integer, String,
// Bulk, Array, Error).
const (
	// Null is the RESP3 null value ("_\r\n").
	Null Type = '_'
	// Boolean is a RESP3 boolean value ("#t\r\n" or "#f\r\n").
	Boolean Type = '#'
	// Double is a RESP3 floating point value (",1.23\r\n").
	Double Type = ','
	// BigNumber is a RESP3 arbitrary-precision integer ("(42\r\n").
	BigNumber Type = '('
	// Verbatim is a RESP3 verbatim string with a 3 byte encoding prefix
	// ("=15\r\ntxt:hello\r\n").
	Verbatim Type = '='
	// Map is a RESP3 key/value map ("%2\r\n...").
	Map Type = '%'
	// Set is a RESP3 set of unique values ("~2\r\n...").
	Set Type = '~'
	// Push is a RESP3 out-of-band push frame (">2\r\n...").
	Push Type = '>'
	// Attribute is a RESP3 attribute frame ("|1\r\n...") that prefixes
	// another message and augments it with metadata.
	Attribute Type = '|'
	// BulkError is a RESP3 bulk error ("!5\r\nhello\r\n").
	BulkError Type = '!'
)

// Parse errors returned by ParseNextRESP and RESPParser.Next.
var (
	// ErrIncompleteRESP means that the input does not yet contain a full
	// message. Feeding additional bytes may allow parsing to succeed.
	ErrIncompleteRESP = errors.New("incomplete resp")
	// ErrInvalidRESP means that the input violates the protocol and cannot
	// be completed by adding more bytes.
	ErrInvalidRESP = errors.New("invalid resp")
)

// Limits enforced while decoding. They keep a peer from exhausting memory
// using declared lengths that are never delivered.
const (
	// maxRESPDepth is the maximum nesting depth of containers.
	maxRESPDepth = 64
	// maxRESPElements is the maximum number of entries declared by a single
	// array, map, set, push, or attribute header.
	maxRESPElements = 1<<30 - 1
	// maxRESPLength is the maximum number of payload bytes declared by a
	// bulk string, verbatim string, or bulk error.
	maxRESPLength = 512 * 1024 * 1024
)

type respErr struct {
	err    error
	detail string
}

func (e *respErr) Error() string {
	if e.detail == "" {
		return e.err.Error()
	}
	return e.err.Error() + ": " + e.detail
}

func (e *respErr) Unwrap() error { return e.err }

func invalidRESP(detail string) error {
	return &respErr{err: ErrInvalidRESP, detail: detail}
}

// readRESPLine returns the bytes following the kind byte up to (but not
// including) the terminating CRLF, and the index just past the CRLF.
func readRESPLine(b []byte) (line []byte, end int, err error) {
	i := bytes.IndexByte(b, '\n')
	if i < 0 {
		return nil, 0, ErrIncompleteRESP
	}
	if i == 0 || b[i-1] != '\r' {
		return nil, 0, invalidRESP("expected CRLF terminator")
	}
	return b[:i-1], i + 1, nil
}

// parseRESPCount parses a signed length/count payload and reports overflow.
func parseRESPCount(line []byte) (int64, error) {
	if len(line) == 0 {
		return 0, invalidRESP("empty length")
	}
	n, err := strconv.ParseInt(unsafe.String(unsafe.SliceData(line), len(line)), 10, 64)
	if err != nil {
		return 0, invalidRESP("invalid length")
	}
	return n, nil
}

// validSimpleInteger mirrors the historical ReadNextRESP integer grammar:
// digits with an optional leading minus sign, without range checking.
func validSimpleInteger(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	j := 0
	if b[0] == '-' {
		if len(b) == 1 {
			return false
		}
		j++
	}
	for ; j < len(b); j++ {
		if b[j] < '0' || b[j] > '9' {
			return false
		}
	}
	return true
}

// validDouble reports whether b is a RESP3 double payload: an optional sign
// followed by a decimal number or one of the special values inf/nan. Both
// "1" and "1.5" style forms are accepted, as Redis may emit either.
func validDouble(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	s := b
	if s[0] == '-' || s[0] == '+' {
		s = s[1:]
		if len(s) == 0 {
			return false
		}
	}
	if string(s) == "inf" {
		return true
	}
	if len(s) == 3 && (s[0] == 'n' || s[0] == 'N') &&
		(s[1] == 'a' || s[1] == 'A') &&
		(s[2] == 'n' || s[2] == 'N') {
		return true
	}
	dot := false
	digit := false
	lastDigit := false
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			digit = true
			lastDigit = true
		case c == '.' && !dot:
			dot = true
			lastDigit = false
		default:
			return false
		}
	}
	return digit && lastDigit
}

func validBigNumber(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	j := 0
	if b[0] == '-' || b[0] == '+' {
		if len(b) == 1 {
			return false
		}
		j++
	}
	for ; j < len(b); j++ {
		if b[j] < '0' || b[j] > '9' {
			return false
		}
	}
	return true
}

// ParseNextRESP parses a single RESP2 or RESP3 message from b.
//
// n is the number of bytes consumed. It returns ErrIncompleteRESP (possibly
// wrapped) when more bytes are needed and ErrInvalidRESP when the bytes can
// never form a valid message. Messages carrying RESP3 attributes expose
// them on RESP.Attributes in wire order; Raw spans the attribute frames as
// well as the augmented value.
func ParseNextRESP(b []byte) (n int, resp RESP, err error) {
	return parseNextRESP(b, 0)
}

// ReadNextRESP3 is an alias for ParseNextRESP.
func ReadNextRESP3(b []byte) (n int, resp RESP, err error) {
	return ParseNextRESP(b)
}

func parseNextRESP(b []byte, depth int) (int, RESP, error) {
	orig := b
	if len(b) == 0 {
		return 0, RESP{}, ErrIncompleteRESP
	}
	var attrs []RESP
	var attrTotal int
	for b[0] == byte(Attribute) {
		rn, attr, err := parseOne(b, depth+1)
		if err != nil {
			return 0, RESP{}, err
		}
		attrs = append(attrs, attr)
		attrTotal += rn
		b = b[rn:]
		depth++
		if len(b) == 0 {
			return 0, RESP{}, ErrIncompleteRESP
		}
	}
	rn, resp, err := parseOne(b, depth)
	if err != nil {
		return 0, RESP{}, err
	}
	if attrs != nil {
		resp.Attributes = attrs
		resp.Raw = orig[:attrTotal+rn]
	}
	return attrTotal + rn, resp, err
}

// parseOne parses exactly one message without consuming leading attribute
// frames. Attributes nested inside containers are handled by parseChildren.
func parseOne(b []byte, depth int) (n int, resp RESP, err error) {
	resp.Type = Type(b[0])
	line, lend, lerr := readRESPLine(b[1:])
	if lerr != nil {
		return 0, RESP{}, lerr
	}
	end := lend + 1 // account for the kind byte
	switch resp.Type {
	case Array, Map, Set, Push, Attribute:
		if depth >= maxRESPDepth {
			return 0, RESP{}, &respErr{
				err:    ErrInvalidRESP,
				detail: "nesting too deep",
			}
		}
	}
	switch resp.Type {
	case Integer:
		if !validSimpleInteger(line) {
			return 0, RESP{}, invalidRESP("invalid integer")
		}
		resp.Data = line
	case String, Error:
		resp.Data = line
	case Boolean:
		if len(line) != 1 || (line[0] != 't' && line[0] != 'f') {
			return 0, RESP{}, invalidRESP("invalid boolean")
		}
		resp.Data = line
	case Double:
		if !validDouble(line) {
			return 0, RESP{}, invalidRESP("invalid double")
		}
		resp.Data = line
	case BigNumber:
		if !validBigNumber(line) {
			return 0, RESP{}, invalidRESP("invalid big number")
		}
		resp.Data = line
	case Null:
		if len(line) != 0 {
			return 0, RESP{}, invalidRESP("invalid null")
		}
		resp.Data = nil
		resp.Raw = b[:end]
		return end, resp, nil
	case Bulk, Verbatim, BulkError:
		return parseBulkish(b, line, end, resp.Type)
	case Array:
		count, perr := parseRESPCount(line)
		if perr != nil {
			return 0, RESP{}, perr
		}
		if count < 0 {
			// RESP2 null array ("*-1\r\n") and friends. Preserve the
			// historical representation: no data, zero count.
			resp.Count = 0
			resp.Data = nil
			resp.Raw = b[:end]
			return end, resp, nil
		}
		if count > maxRESPElements {
			return 0, RESP{}, invalidRESP("element count too large")
		}
		resp.Count = int(count)
		tn, perr := parseChildren(b[end:], int(count), depth)
		if perr != nil {
			return 0, RESP{}, perr
		}
		resp.Data = b[end : end+tn]
		resp.Raw = b[:end+tn]
		return end + tn, resp, nil
	case Set, Push:
		count, perr := parseRESPCount(line)
		if perr != nil {
			return 0, RESP{}, perr
		}
		if count < 0 {
			return 0, RESP{}, invalidRESP("negative element count")
		}
		if count > maxRESPElements {
			return 0, RESP{}, invalidRESP("element count too large")
		}
		resp.Count = int(count)
		tn, perr := parseChildren(b[end:], int(count), depth)
		if perr != nil {
			return 0, RESP{}, perr
		}
		resp.Data = b[end : end+tn]
		resp.Raw = b[:end+tn]
		return end + tn, resp, nil
	case Map, Attribute:
		count, perr := parseRESPCount(line)
		if perr != nil {
			return 0, RESP{}, perr
		}
		if count < 0 {
			return 0, RESP{}, invalidRESP("negative pair count")
		}
		if count > maxRESPElements {
			return 0, RESP{}, invalidRESP("pair count too large")
		}
		resp.Count = int(count)
		tn, perr := parseChildren(b[end:], int(count)*2, depth)
		if perr != nil {
			return 0, RESP{}, perr
		}
		resp.Data = b[end : end+tn]
		resp.Raw = b[:end+tn]
		return end + tn, resp, nil
	default:
		return 0, RESP{}, invalidRESP("unknown type '" + string(b[0]) + "'")
	}
	resp.Raw = b[:end]
	return end, resp, nil
}

// parseBulkish parses Bulk, Verbatim, and BulkError frames.
func parseBulkish(b, line []byte, end int, typ Type) (int, RESP, error) {
	var resp RESP
	resp.Type = typ
	count, err := parseRESPCount(line)
	if err != nil {
		return 0, RESP{}, err
	}
	if count < 0 {
		if typ != Bulk {
			return 0, RESP{}, invalidRESP("negative length")
		}
		// RESP2 null bulk string.
		resp.Count = 0
		resp.Data = nil
		resp.Raw = b[:end]
		return end, resp, nil
	}
	if count > maxRESPLength {
		return 0, RESP{}, invalidRESP("length too large")
	}
	n := int(count)
	if len(b) < end+n+2 {
		return 0, RESP{}, ErrIncompleteRESP
	}
	if b[end+n] != '\r' || b[end+n+1] != '\n' {
		return 0, RESP{}, invalidRESP("expected CRLF after payload")
	}
	resp.Data = b[end : end+n]
	if typ == Verbatim {
		if n < 4 || b[end+3] != ':' {
			return 0, RESP{}, invalidRESP("invalid verbatim prefix")
		}
	}
	resp.Raw = b[:end+n+2]
	return end + n + 2, resp, nil
}

// parseChildren parses exactly childCount sequential values from b and
// returns the total number of bytes consumed. A value may be prefixed by
// one or more attribute frames; those frames are part of the child's Raw
// bytes and the child value exposes them in its Attributes field.
func parseChildren(b []byte, childCount, depth int) (int, error) {
	var tn int
	for j := 0; j < childCount; j++ {
		rn, _, err := parseNextRESP(b, depth+1)
		if err != nil {
			return 0, err
		}
		tn += rn
		b = b[rn:]
	}
	return tn, nil
}

// RESPParser incrementally parses RESP messages from bytes that may arrive
// in arbitrarily sized chunks, including one byte at a time.
//
// The zero value is ready to use. Byte slices returned in parsed messages
// alias the parser internal buffer and remain valid until the next call to
// Feed or Next; copy them when a longer lifetime is needed.
type RESPParser struct {
	buf   []byte
	start int
	fatal bool
}

// NewRESPParser returns a new incremental RESP parser.
func NewRESPParser() *RESPParser {
	return new(RESPParser)
}

// Feed appends p to the parser unparsed buffer. Feed is safe to call after
// Next returns ErrIncompleteRESP. After an ErrInvalidRESP result the parser
// must be Reset before further use.
func (p *RESPParser) Feed(pb []byte) {
	if p.fatal {
		return
	}
	// Copy the incoming bytes into a standalone slice first. pb may alias
	// the parser buffer (for example a sub-slice of previously returned
	// Raw bytes), which would corrupt an in-place compaction.
	in := make([]byte, len(pb))
	copy(in, pb)
	if p.start > 0 {
		n := copy(p.buf, p.buf[p.start:])
		p.buf = p.buf[:n]
		p.start = 0
	}
	p.buf = append(p.buf, in...)
}

// Next parses the next buffered message. It returns ErrIncompleteRESP when
// more bytes are required and ErrInvalidRESP when the buffered input can
// never form a valid message. On success the consumed bytes are discarded.
func (p *RESPParser) Next() (resp RESP, err error) {
	if p.fatal {
		return RESP{}, ErrInvalidRESP
	}
	n, resp, err := parseNextRESP(p.buf[p.start:], 0)
	if err != nil {
		if errors.Is(err, ErrInvalidRESP) {
			p.fatal = true
		}
		return RESP{}, err
	}
	p.start += n
	if p.start == len(p.buf) {
		p.buf = p.buf[:0]
		p.start = 0
	}
	return resp, nil
}

// Buffered returns the number of unparsed bytes currently buffered.
func (p *RESPParser) Buffered() int {
	return len(p.buf) - p.start
}

// Reset returns the parser to its empty initial state.
func (p *RESPParser) Reset() {
	p.buf = nil
	p.start = 0
	p.fatal = false
}

// Bool returns the value of a Boolean message. Non-boolean messages and
// malformed payloads report false.
func (r RESP) Bool() bool {
	return r.Type == Boolean && len(r.Data) == 1 && r.Data[0] == 't'
}

// BigInt returns the payload of a BigNumber message as a *big.Int. It
// returns nil when the payload is not a valid big number.
func (r RESP) BigInt() *big.Int {
	if r.Type != BigNumber {
		return nil
	}
	x := new(big.Int)
	if _, ok := x.SetString(string(r.Data), 10); !ok {
		return nil
	}
	return x
}

// VerbatimEncoding returns the 3 byte encoding prefix of a Verbatim
// message (for example "txt" or "mkd"). It returns "" for other kinds.
func (r RESP) VerbatimEncoding() string {
	if r.Type != Verbatim || len(r.Data) < 4 || r.Data[3] != ':' {
		return ""
	}
	return string(r.Data[:3])
}

// VerbatimValue returns the payload of a Verbatim message without the
// 3 byte encoding prefix and separating colon.
func (r RESP) VerbatimValue() []byte {
	if r.Type != Verbatim || len(r.Data) < 4 || r.Data[3] != ':' {
		return nil
	}
	return r.Data[4:]
}

// AppendBoolean appends a RESP3 boolean to the input bytes.
func AppendBoolean(b []byte, v bool) []byte {
	if v {
		return append(b, '#', 't', '\r', '\n')
	}
	return append(b, '#', 'f', '\r', '\n')
}

// AppendDouble appends a RESP3 double to the input bytes. Non-finite values
// are encoded as "inf", "-inf", and "nan" exactly as Redis emits them.
func AppendDouble(b []byte, f float64) []byte {
	b = append(b, ',')
	switch {
	case math.IsNaN(f):
		b = append(b, 'n', 'a', 'n')
	case math.IsInf(f, 1):
		b = append(b, 'i', 'n', 'f')
	case math.IsInf(f, -1):
		b = append(b, '-', 'i', 'n', 'f')
	default:
		b = strconv.AppendFloat(b, f, 'g', -1, 64)
	}
	return append(b, '\r', '\n')
}

// AppendDoubleText appends a RESP3 double whose textual representation is
// provided verbatim. The text must be a valid payload such as "1.5",
// "inf", "-inf", or "nan".
func AppendDoubleText(b []byte, text string) []byte {
	b = append(b, ',')
	b = append(b, text...)
	return append(b, '\r', '\n')
}

// AppendBigNumber appends a RESP3 big number to the input bytes.
func AppendBigNumber(b []byte, num []byte) []byte {
	b = append(b, '(')
	b = append(b, num...)
	return append(b, '\r', '\n')
}

// AppendBigNumberString appends a RESP3 big number to the input bytes.
func AppendBigNumberString(b []byte, num string) []byte {
	return AppendBigNumber(b, []byte(num))
}

// AppendVerbatim appends a RESP3 verbatim string with the given 3 byte
// encoding (such as "txt" or "mkd") to the input bytes.
func AppendVerbatim(b []byte, encoding string, data []byte) []byte {
	if len(encoding) != 3 {
		encoding = "txt"
	}
	b = appendPrefix(b, '=', int64(len(data)+4))
	b = append(b, encoding...)
	b = append(b, ':')
	b = append(b, data...)
	return append(b, '\r', '\n')
}

// AppendVerbatimString appends a RESP3 verbatim string to the input bytes.
func AppendVerbatimString(b []byte, encoding, s string) []byte {
	return AppendVerbatim(b, encoding, []byte(s))
}

// AppendMap appends a RESP3 map header declaring n key/value pairs.
func AppendMap(b []byte, n int) []byte {
	return appendPrefix(b, '%', int64(n))
}

// AppendSet appends a RESP3 set header declaring n elements.
func AppendSet(b []byte, n int) []byte {
	return appendPrefix(b, '~', int64(n))
}

// AppendPush appends a RESP3 push frame header declaring n elements.
func AppendPush(b []byte, n int) []byte {
	return appendPrefix(b, '>', int64(n))
}

// AppendAttribute appends a RESP3 attribute frame header declaring n
// key/value pairs. The pairs and the augmented value must follow.
func AppendAttribute(b []byte, n int) []byte {
	return appendPrefix(b, '|', int64(n))
}

// AppendRESPNull appends the RESP3 null value ("_\r\n").
func AppendRESPNull(b []byte) []byte {
	return append(b, '_', '\r', '\n')
}

// AppendBulkError appends a RESP3 bulk error to the input bytes.
func AppendBulkError(b []byte, msg []byte) []byte {
	b = appendPrefix(b, '!', int64(len(msg)))
	b = append(b, msg...)
	return append(b, '\r', '\n')
}

// AppendBulkErrorString appends a RESP3 bulk error to the input bytes.
func AppendBulkErrorString(b []byte, msg string) []byte {
	return AppendBulkError(b, []byte(msg))
}
