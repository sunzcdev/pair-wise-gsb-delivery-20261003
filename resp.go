package redcon

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Type of RESP
type Type byte

// Various RESP kinds
const (
	Integer = ':'
	String  = '+'
	Bulk    = '$'
	Array   = '*'
	Error   = '-'
	// RESP3 types
	Map       = '%'
	Set       = '~'
	Boolean   = '#'
	Double    = ','
	BigNumber = '('
	Verbatim  = '='
	Push      = '>'
	Attribute = '|'
)

// Limits used by the RESP reader to guard against abusive payloads.
const (
	// MaxRESPDepth is the maximum nesting depth of aggregate types.
	MaxRESPDepth = 128
	// MaxRESPElements is the maximum number of elements (or key/value
	// pairs for maps and attributes) allowed in a single aggregate.
	MaxRESPElements = 1 << 20
	// MaxRESPBulkLength is the maximum length of a bulk or verbatim
	// string payload.
	MaxRESPBulkLength = 1 << 29
)

// ErrIncomplete is returned by ReadNextRESPEx when the provided bytes end
// in the middle of a RESP value. More data is needed to complete the read.
// Any other error indicates that the input is invalid.
var ErrIncomplete = errors.New("redcon: incomplete resp")

type RESP struct {
	Type  Type
	Raw   []byte
	Data  []byte
	Count int
	// Attrs holds the RESP3 attributes that were attached to this value,
	// in the order they appeared on the wire. It is nil for values that
	// were not preceded by an attribute.
	Attrs []RESP
}

// ForEach iterates over each Array, Set, or Push element. For Map and
// Attribute values it iterates over each key and value in sequence.
func (r RESP) ForEach(iter func(resp RESP) bool) {
	data := r.Data
	count := r.Count
	if r.Type == Map || r.Type == Attribute {
		count *= 2
	}
	for i := 0; i < count; i++ {
		n, resp := ReadNextRESP(data)
		if !iter(resp) {
			return
		}
		data = data[n:]
	}
}

func (r RESP) Bytes() []byte {
	return r.Data
}

func (r RESP) String() string {
	return string(r.Data)
}

func (r RESP) Int() int64 {
	x, _ := strconv.ParseInt(r.String(), 10, 64)
	return x
}

func (r RESP) Float() float64 {
	x, _ := strconv.ParseFloat(r.String(), 10)
	return x
}

// Map returns a key/value map of an Array, Map, or Attribute.
// The receiver RESP must be an Array, Map, or Attribute with an equal
// number of values, where the value of the key is followed by the key.
// Example: key1,value1,key2,value2,key3,value3
func (r RESP) Map() map[string]RESP {
	if r.Type != Array && r.Type != Map && r.Type != Attribute {
		return nil
	}
	var n int
	var key string
	m := make(map[string]RESP)
	r.ForEach(func(resp RESP) bool {
		if n&1 == 0 {
			key = resp.String()
		} else {
			m[key] = resp
		}
		n++
		return true
	})
	return m
}

func (r RESP) MapGet(key string) RESP {
	if r.Type != Array && r.Type != Map && r.Type != Attribute {
		return RESP{}
	}
	var val RESP
	var n int
	var ok bool
	r.ForEach(func(resp RESP) bool {
		if n&1 == 0 {
			ok = resp.String() == key
		} else if ok {
			val = resp
			return false
		}
		n++
		return true
	})
	return val
}

func (r RESP) Exists() bool {
	return r.Type != 0
}

// ReadNextRESP returns the next resp in b and returns the number of bytes
// that took up the result. An empty RESP is returned when the data is
// incomplete or invalid. Use ReadNextRESPEx to distinguish between the two.
func ReadNextRESP(b []byte) (n int, resp RESP) {
	n, resp, err := ReadNextRESPEx(b)
	if err != nil {
		return 0, RESP{}
	}
	return n, resp
}

// ReadNextRESPEx reads the next RESP value from b, returning the number of
// bytes consumed along with the value. It returns ErrIncomplete when b does
// not yet contain a full value, allowing callers to keep buffering data that
// arrives in fragments. Any other error means the input is invalid.
func ReadNextRESPEx(b []byte) (n int, resp RESP, err error) {
	return readNextRESP(b, 0)
}

func readNextRESP(b []byte, depth int) (n int, resp RESP, err error) {
	if len(b) == 0 {
		return 0, RESP{}, ErrIncomplete // no data to read
	}
	resp.Type = Type(b[0])
	switch resp.Type {
	case Integer, String, Bulk, Array, Error,
		Map, Set, Boolean, Double, BigNumber, Verbatim, Push, Attribute:
	default:
		return 0, RESP{}, &errProtocol{"invalid resp type"}
	}
	// read to end of line
	i := 1
	for ; ; i++ {
		if i == len(b) {
			return 0, RESP{}, ErrIncomplete // not enough data
		}
		if b[i] == '\n' {
			if b[i-1] != '\r' {
				return 0, RESP{}, &errProtocol{"missing CR character"}
			}
			i++
			break
		}
	}
	resp.Raw = b[0:i]
	resp.Data = b[1 : i-2]
	switch resp.Type {
	case Integer:
		// Integer
		if !validRESPInt(resp.Data) {
			return 0, RESP{}, &errProtocol{"invalid integer"}
		}
		return len(resp.Raw), resp, nil
	case String, Error:
		// String, Error
		return len(resp.Raw), resp, nil
	case Boolean:
		// Boolean (RESP3)
		if len(resp.Data) != 1 || (resp.Data[0] != 't' && resp.Data[0] != 'f') {
			return 0, RESP{}, &errProtocol{"invalid boolean"}
		}
		return len(resp.Raw), resp, nil
	case Double:
		// Double (RESP3)
		if len(resp.Data) == 0 {
			return 0, RESP{}, &errProtocol{"invalid double"}
		}
		if _, err := strconv.ParseFloat(string(resp.Data), 64); err != nil {
			return 0, RESP{}, &errProtocol{"invalid double"}
		}
		return len(resp.Raw), resp, nil
	case BigNumber:
		// Big Number (RESP3)
		data := resp.Data
		if len(data) > 0 && data[0] == '+' {
			data = data[1:]
		}
		if !validRESPInt(data) {
			return 0, RESP{}, &errProtocol{"invalid big number"}
		}
		return len(resp.Raw), resp, nil
	}
	if resp.Type == Bulk || resp.Type == Verbatim {
		// Bulk, Verbatim
		count, err := strconv.Atoi(string(resp.Data))
		if err != nil {
			return 0, RESP{}, &errProtocol{"invalid number of bytes"}
		}
		if resp.Type == Bulk && count < 0 {
			// null bulk string
			resp.Data = nil
			resp.Count = 0
			return len(resp.Raw), resp, nil
		}
		if count < 0 {
			return 0, RESP{}, &errProtocol{"invalid number of bytes"}
		}
		if count > MaxRESPBulkLength {
			return 0, RESP{}, &errProtocol{"bulk length exceeds limit"}
		}
		if len(b) < i+count+2 {
			return 0, RESP{}, ErrIncomplete // not enough data
		}
		if b[i+count] != '\r' || b[i+count+1] != '\n' {
			return 0, RESP{}, &errProtocol{"invalid end of line"}
		}
		resp.Data = b[i : i+count]
		resp.Raw = b[0 : i+count+2]
		resp.Count = 0
		if resp.Type == Verbatim {
			// the payload must start with a three byte format and a colon
			if count < 4 || resp.Data[3] != ':' {
				return 0, RESP{}, &errProtocol{"invalid verbatim string"}
			}
		}
		return len(resp.Raw), resp, nil
	}
	// Array, Map, Set, Push, Attribute
	count, err := strconv.Atoi(string(resp.Data))
	if err != nil {
		return 0, RESP{}, &errProtocol{"invalid number of elements"}
	}
	resp.Count = count
	if resp.Type == Array && count < 0 {
		// null array (RESP2)
		resp.Data = b[i:i]
		return len(resp.Raw), resp, nil
	}
	if count < 0 {
		return 0, RESP{}, &errProtocol{"invalid number of elements"}
	}
	elems := count
	if resp.Type == Map || resp.Type == Attribute {
		// maps and attributes carry key/value pairs
		if count > MaxRESPElements/2 {
			return 0, RESP{}, &errProtocol{"too many elements"}
		}
		elems = count * 2
	}
	if elems > MaxRESPElements {
		return 0, RESP{}, &errProtocol{"too many elements"}
	}
	if depth >= MaxRESPDepth {
		return 0, RESP{}, &errProtocol{"max resp depth exceeded"}
	}
	var tn int
	sdata := b[i:]
	for j := 0; j < elems; j++ {
		rn, _, err := readNextRESP(sdata, depth+1)
		if err != nil {
			return 0, RESP{}, err
		}
		tn += rn
		sdata = sdata[rn:]
	}
	resp.Data = b[i : i+tn]
	resp.Raw = b[0 : i+tn]
	if resp.Type == Attribute {
		// An attribute is metadata for the value that follows it. Parse
		// that value and attach the attribute to it so the relation is
		// not lost.
		rn, vresp, err := readNextRESP(sdata, depth+1)
		if err != nil {
			return 0, RESP{}, err
		}
		vresp.Attrs = append([]RESP{resp}, vresp.Attrs...)
		vresp.Raw = b[0 : i+tn+rn]
		return len(vresp.Raw), vresp, nil
	}
	return len(resp.Raw), resp, nil
}

func validRESPInt(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	var j int
	if data[0] == '-' {
		if len(data) == 1 {
			return false
		}
		j++
	}
	for ; j < len(data); j++ {
		if data[j] < '0' || data[j] > '9' {
			return false
		}
	}
	return true
}

// Kind is the kind of command
type Kind int

const (
	// Redis is returned for Redis protocol commands
	Redis Kind = iota
	// Tile38 is returnd for Tile38 native protocol commands
	Tile38
	// Telnet is returnd for plain telnet commands
	Telnet
)

var errInvalidMessage = &errProtocol{"invalid message"}

// ReadNextCommand reads the next command from the provided packet. It's
// possible that the packet contains multiple commands, or zero commands
// when the packet is incomplete.
// 'argsbuf' is an optional reusable buffer and it can be nil.
// 'complete' indicates that a command was read. false means no more commands.
// 'args' are the output arguments for the command.
// 'kind' is the type of command that was read.
// 'leftover' is any remaining unused bytes which belong to the next command.
// 'err' is returned when a protocol error was encountered.
func ReadNextCommand(packet []byte, argsbuf [][]byte) (
	complete bool, args [][]byte, kind Kind, leftover []byte, err error,
) {
	args = argsbuf[:0]
	if len(packet) > 0 {
		if packet[0] != '*' {
			if packet[0] == '$' {
				return readTile38Command(packet, args)
			}
			return readTelnetCommand(packet, args)
		}
		// standard redis command
		for s, i := 1, 1; i < len(packet); i++ {
			if packet[i] == '\n' {
				if packet[i-1] != '\r' {
					return false, args[:0], Redis, packet, errInvalidMultiBulkLength
				}
				count, ok := parseInt(packet[s : i-1])
				if !ok || count < 0 {
					return false, args[:0], Redis, packet, errInvalidMultiBulkLength
				}
				i++
				if count == 0 {
					return true, args[:0], Redis, packet[i:], nil
				}
			nextArg:
				for j := 0; j < count; j++ {
					if i == len(packet) {
						break
					}
					if packet[i] != '$' {
						return false, args[:0], Redis, packet,
							&errProtocol{"expected '$', got '" +
								string(packet[i]) + "'"}
					}
					for s := i + 1; i < len(packet); i++ {
						if packet[i] == '\n' {
							if packet[i-1] != '\r' {
								return false, args[:0], Redis, packet, errInvalidBulkLength
							}
							n, ok := parseInt(packet[s : i-1])
							if !ok || count <= 0 {
								return false, args[:0], Redis, packet, errInvalidBulkLength
							}
							i++
							if len(packet)-i >= n+2 {
								if packet[i+n] != '\r' || packet[i+n+1] != '\n' {
									return false, args[:0], Redis, packet, errInvalidBulkLength
								}
								args = append(args, packet[i:i+n])
								i += n + 2
								if j == count-1 {
									// done reading
									return true, args, Redis, packet[i:], nil
								}
								continue nextArg
							}
							break
						}
					}
					break
				}
				break
			}
		}
	}
	return false, args[:0], Redis, packet, nil
}

func readTile38Command(packet []byte, argsbuf [][]byte) (
	complete bool, args [][]byte, kind Kind, leftover []byte, err error,
) {
	for i := 1; i < len(packet); i++ {
		if packet[i] == ' ' {
			n, ok := parseInt(packet[1:i])
			if !ok || n < 0 {
				return false, args[:0], Tile38, packet, errInvalidMessage
			}
			i++
			if len(packet) >= i+n+2 {
				if packet[i+n] != '\r' || packet[i+n+1] != '\n' {
					return false, args[:0], Tile38, packet, errInvalidMessage
				}
				line := packet[i : i+n]
			reading:
				for len(line) != 0 {
					if line[0] == '{' {
						// The native protocol cannot understand json boundaries so it assumes that
						// a json element must be at the end of the line.
						args = append(args, line)
						break
					}
					if line[0] == '"' && line[len(line)-1] == '"' {
						if len(args) > 0 &&
							strings.ToLower(string(args[0])) == "set" &&
							strings.ToLower(string(args[len(args)-1])) == "string" {
							// Setting a string value that is contained inside double quotes.
							// This is only because of the boundary issues of the native protocol.
							args = append(args, line[1:len(line)-1])
							break
						}
					}
					i := 0
					for ; i < len(line); i++ {
						if line[i] == ' ' {
							value := line[:i]
							if len(value) > 0 {
								args = append(args, value)
							}
							line = line[i+1:]
							continue reading
						}
					}
					args = append(args, line)
					break
				}
				return true, args, Tile38, packet[i+n+2:], nil
			}
			break
		}
	}
	return false, args[:0], Tile38, packet, nil
}

func readTelnetCommand(packet []byte, argsbuf [][]byte) (
	complete bool, args [][]byte, kind Kind, leftover []byte, err error,
) {
	// just a plain text command
	for i := 0; i < len(packet); i++ {
		if packet[i] == '\n' {
			var line []byte
			if i > 0 && packet[i-1] == '\r' {
				line = packet[:i-1]
			} else {
				line = packet[:i]
			}
			var quote bool
			var quotech byte
			var escape bool
		outer:
			for {
				nline := make([]byte, 0, len(line))
				for i := 0; i < len(line); i++ {
					c := line[i]
					if !quote {
						if c == ' ' {
							if len(nline) > 0 {
								args = append(args, nline)
							}
							line = line[i+1:]
							continue outer
						}
						if c == '"' || c == '\'' {
							if i != 0 {
								return false, args[:0], Telnet, packet, errUnbalancedQuotes
							}
							quotech = c
							quote = true
							line = line[i+1:]
							continue outer
						}
					} else {
						if escape {
							escape = false
							switch c {
							case 'n':
								c = '\n'
							case 'r':
								c = '\r'
							case 't':
								c = '\t'
							}
						} else if c == quotech {
							quote = false
							quotech = 0
							args = append(args, nline)
							line = line[i+1:]
							if len(line) > 0 && line[0] != ' ' {
								return false, args[:0], Telnet, packet, errUnbalancedQuotes
							}
							continue outer
						} else if c == '\\' {
							escape = true
							continue
						}
					}
					nline = append(nline, c)
				}
				if quote {
					return false, args[:0], Telnet, packet, errUnbalancedQuotes
				}
				if len(line) > 0 {
					args = append(args, line)
				}
				break
			}
			return true, args, Telnet, packet[i+1:], nil
		}
	}
	return false, args[:0], Telnet, packet, nil
}

// appendPrefix will append a "$3\r\n" style redis prefix for a message.
func appendPrefix(b []byte, c byte, n int64) []byte {
	if n >= 0 && n <= 9 {
		return append(b, c, byte('0'+n), '\r', '\n')
	}
	b = append(b, c)
	b = strconv.AppendInt(b, n, 10)
	return append(b, '\r', '\n')
}

// AppendUint appends a Redis protocol uint64 to the input bytes.
func AppendUint(b []byte, n uint64) []byte {
	b = append(b, ':')
	b = strconv.AppendUint(b, n, 10)
	return append(b, '\r', '\n')
}

// AppendInt appends a Redis protocol int64 to the input bytes.
func AppendInt(b []byte, n int64) []byte {
	return appendPrefix(b, ':', n)
}

// AppendArray appends a Redis protocol array to the input bytes.
func AppendArray(b []byte, n int) []byte {
	return appendPrefix(b, '*', int64(n))
}

// AppendBulk appends a Redis protocol bulk byte slice to the input bytes.
func AppendBulk(b []byte, bulk []byte) []byte {
	b = appendPrefix(b, '$', int64(len(bulk)))
	b = append(b, bulk...)
	return append(b, '\r', '\n')
}

// AppendBulkString appends a Redis protocol bulk string to the input bytes.
func AppendBulkString(b []byte, bulk string) []byte {
	b = appendPrefix(b, '$', int64(len(bulk)))
	b = append(b, bulk...)
	return append(b, '\r', '\n')
}

// AppendString appends a Redis protocol string to the input bytes.
func AppendString(b []byte, s string) []byte {
	b = append(b, '+')
	b = append(b, stripNewlines(s)...)
	return append(b, '\r', '\n')
}

// AppendError appends a Redis protocol error to the input bytes.
func AppendError(b []byte, s string) []byte {
	b = append(b, '-')
	b = append(b, stripNewlines(s)...)
	return append(b, '\r', '\n')
}

// AppendOK appends a Redis protocol OK to the input bytes.
func AppendOK(b []byte) []byte {
	return append(b, '+', 'O', 'K', '\r', '\n')
}
func stripNewlines(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' || s[i] == '\n' {
			s = strings.Replace(s, "\r", " ", -1)
			s = strings.Replace(s, "\n", " ", -1)
			break
		}
	}
	return s
}

// AppendTile38 appends a Tile38 message to the input bytes.
func AppendTile38(b []byte, data []byte) []byte {
	b = append(b, '$')
	b = strconv.AppendInt(b, int64(len(data)), 10)
	b = append(b, ' ')
	b = append(b, data...)
	return append(b, '\r', '\n')
}

// AppendNull appends a Redis protocol null to the input bytes.
func AppendNull(b []byte) []byte {
	return append(b, '$', '-', '1', '\r', '\n')
}

// AppendMap appends a RESP3 map header for n key/value pairs to the input
// bytes. The pairs themselves must be appended afterwards.
func AppendMap(b []byte, n int) []byte {
	return appendPrefix(b, '%', int64(n))
}

// AppendSet appends a RESP3 set header for n elements to the input bytes.
// The elements themselves must be appended afterwards.
func AppendSet(b []byte, n int) []byte {
	return appendPrefix(b, '~', int64(n))
}

// AppendPush appends a RESP3 push header for n elements to the input bytes.
// The elements themselves must be appended afterwards.
func AppendPush(b []byte, n int) []byte {
	return appendPrefix(b, '>', int64(n))
}

// AppendAttribute appends a RESP3 attribute header for n key/value pairs to
// the input bytes. The pairs, followed by the value the attribute describes,
// must be appended afterwards.
func AppendAttribute(b []byte, n int) []byte {
	return appendPrefix(b, '|', int64(n))
}

// AppendBool appends a RESP3 boolean to the input bytes.
func AppendBool(b []byte, v bool) []byte {
	if v {
		return append(b, '#', 't', '\r', '\n')
	}
	return append(b, '#', 'f', '\r', '\n')
}

// AppendDouble appends a RESP3 double to the input bytes.
func AppendDouble(b []byte, f float64) []byte {
	b = append(b, ',')
	switch {
	case math.IsInf(f, 1):
		b = append(b, "inf"...)
	case math.IsInf(f, -1):
		b = append(b, "-inf"...)
	case math.IsNaN(f):
		b = append(b, "nan"...)
	default:
		b = strconv.AppendFloat(b, f, 'g', -1, 64)
	}
	return append(b, '\r', '\n')
}

// AppendBigNumber appends a RESP3 big number to the input bytes. The value
// must be a valid base-10 integer with an optional leading sign.
func AppendBigNumber(b []byte, s string) []byte {
	b = append(b, '(')
	b = append(b, s...)
	return append(b, '\r', '\n')
}

// AppendVerbatim appends a RESP3 verbatim string to the input bytes. The
// format must be exactly three bytes, such as "txt" or "mkd".
func AppendVerbatim(b []byte, format string, data []byte) []byte {
	b = appendPrefix(b, '=', int64(len(format)+1+len(data)))
	b = append(b, format...)
	b = append(b, ':')
	b = append(b, data...)
	return append(b, '\r', '\n')
}

// AppendBulkFloat appends a float64, as bulk bytes.
func AppendBulkFloat(dst []byte, f float64) []byte {
	return AppendBulk(dst, strconv.AppendFloat(nil, f, 'f', -1, 64))
}

// AppendBulkInt appends an int64, as bulk bytes.
func AppendBulkInt(dst []byte, x int64) []byte {
	return AppendBulk(dst, strconv.AppendInt(nil, x, 10))
}

// AppendBulkUint appends an uint64, as bulk bytes.
func AppendBulkUint(dst []byte, x uint64) []byte {
	return AppendBulk(dst, strconv.AppendUint(nil, x, 10))
}

func prefixERRIfNeeded(msg string) string {
	msg = strings.TrimSpace(msg)
	firstWord := strings.Split(msg, " ")[0]
	addERR := len(firstWord) == 0
	for i := 0; i < len(firstWord); i++ {
		if firstWord[i] < 'A' || firstWord[i] > 'Z' {
			addERR = true
			break
		}
	}
	if addERR {
		msg = strings.TrimSpace("ERR " + msg)
	}
	return msg
}

// SimpleString is for representing a non-bulk representation of a string
// from an *Any call.
type SimpleString string

// SimpleInt is for representing a non-bulk representation of a int
// from an *Any call.
type SimpleInt int

// SimpleError is for representing an error without adding the "ERR" prefix
// from an *Any call.
type SimpleError error

// Marshaler is the interface implemented by types that
// can marshal themselves into a Redis response type from an *Any call.
// The return value is not check for validity.
type Marshaler interface {
	MarshalRESP() []byte
}

// AppendAny appends any type to valid Redis type.
//
//	nil             -> null
//	error           -> error (adds "ERR " when first word is not uppercase)
//	string          -> bulk-string
//	numbers         -> bulk-string
//	[]byte          -> bulk-string
//	bool            -> bulk-string ("0" or "1")
//	slice           -> array
//	map             -> array with key/value pairs
//	SimpleString    -> string
//	SimpleInt       -> integer
//	Marshaler       -> raw bytes
//	everything-else -> bulk-string representation using fmt.Sprint()
func AppendAny(b []byte, v interface{}) []byte {
	switch v := v.(type) {
	case SimpleString:
		b = AppendString(b, string(v))
	case SimpleInt:
		b = AppendInt(b, int64(v))
	case SimpleError:
		b = AppendError(b, v.Error())
	case nil:
		b = AppendNull(b)
	case error:
		b = AppendError(b, prefixERRIfNeeded(v.Error()))
	case string:
		b = AppendBulkString(b, v)
	case []byte:
		if v == nil {
			b = AppendNull(b)
		} else {
			b = AppendBulk(b, v)
		}
	case bool:
		if v {
			b = AppendBulkString(b, "1")
		} else {
			b = AppendBulkString(b, "0")
		}
	case int:
		b = AppendBulkInt(b, int64(v))
	case int8:
		b = AppendBulkInt(b, int64(v))
	case int16:
		b = AppendBulkInt(b, int64(v))
	case int32:
		b = AppendBulkInt(b, int64(v))
	case int64:
		b = AppendBulkInt(b, int64(v))
	case uint:
		b = AppendBulkUint(b, uint64(v))
	case uint8:
		b = AppendBulkUint(b, uint64(v))
	case uint16:
		b = AppendBulkUint(b, uint64(v))
	case uint32:
		b = AppendBulkUint(b, uint64(v))
	case uint64:
		b = AppendBulkUint(b, uint64(v))
	case float32:
		b = AppendBulkFloat(b, float64(v))
	case float64:
		b = AppendBulkFloat(b, float64(v))
	case Marshaler:
		b = append(b, v.MarshalRESP()...)
	default:
		vv := reflect.ValueOf(v)
		switch vv.Kind() {
		case reflect.Slice:
			n := vv.Len()
			b = AppendArray(b, n)
			for i := 0; i < n; i++ {
				b = AppendAny(b, vv.Index(i).Interface())
			}
		case reflect.Map:
			n := vv.Len()
			b = AppendArray(b, n*2)
			var i int
			var strKey bool
			var strsKeyItems []strKeyItem

			iter := vv.MapRange()
			for iter.Next() {
				key := iter.Key().Interface()
				if i == 0 {
					if _, ok := key.(string); ok {
						strKey = true
						strsKeyItems = make([]strKeyItem, n)
					}
				}
				if strKey {
					strsKeyItems[i] = strKeyItem{
						key.(string), iter.Value().Interface(),
					}
				} else {
					b = AppendAny(b, key)
					b = AppendAny(b, iter.Value().Interface())
				}
				i++
			}
			if strKey {
				sort.Slice(strsKeyItems, func(i, j int) bool {
					return strsKeyItems[i].key < strsKeyItems[j].key
				})
				for _, item := range strsKeyItems {
					b = AppendBulkString(b, item.key)
					b = AppendAny(b, item.value)
				}
			}
		default:
			b = AppendBulkString(b, fmt.Sprint(v))
		}
	}
	return b
}

type strKeyItem struct {
	key   string
	value interface{}
}
