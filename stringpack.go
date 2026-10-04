package lua

import (
	"encoding/binary"
	"math"
	"strconv"
	"strings"
)

// This file implements an explicit subset of Lua 5.3's string.pack,
// string.unpack and string.packsize.
//
// Supported format options:
//   '<' little endian, '>' big endian, '!n' max alignment (power of two, 1..8)
//   'i[n]'/'I[n]' signed/unsigned integers, n in {1, 2, 4, 8}
//   'f' float32, 'd' float64
//   'c[n]' fixed-length string, 's[n]' length-prefixed string (n in {1,2,4,8}),
//   'z' zero-terminated string, 'x' one padding byte, ' ' ignored
//
// Host-native-width aliases (bare 'i', 'I', 's', 'n', 'j', 'l', 'T') are not
// implemented. Because LNumber is a float64, 8-byte integers use an explicit
// decimal string convention for both input and output; other widths require
// exactly representable integer values.

const (
	packMaxAlign = 8
	packMaxSize  = 1 << 30
)

type packEndianness int

const (
	packLittleEndian packEndianness = iota
	packBigEndian
)

type packHeader struct {
	endian   packEndianness
	maxAlign int
}

type packKind int

const (
	packKindNone packKind = iota // directive without payload
	packKindInt
	packKindUint
	packKindFloat
	packKindDouble
	packKindChar // c[n]
	packKindPstr // s[n]
	packKindZstr // z
	packKindPad  // x
)

type packOption struct {
	kind packKind
	size int
}

func newPackHeader() *packHeader {
	return &packHeader{endian: packLittleEndian, maxAlign: packMaxAlign}
}

func readPackSize(fmtstr string, i int) (int, int, bool) {
	start := i
	n := 0
	for i < len(fmtstr) && fmtstr[i] >= '0' && fmtstr[i] <= '9' {
		n = n*10 + int(fmtstr[i]-'0')
		if n > packMaxSize {
			n = packMaxSize
		}
		i++
	}
	return n, i, i > start
}

func checkPackIntSize(L *LState, c byte, n int) {
	switch n {
	case 1, 2, 4, 8:
	default:
		L.RaiseError("invalid size %d for format option '%c' (only 1, 2, 4 or 8)", n, c)
	}
}

func parsePackOption(L *LState, h *packHeader, fmtstr string, i int) (packOption, int) {
	c := fmtstr[i]
	switch c {
	case ' ':
		return packOption{kind: packKindNone}, 1
	case '<':
		h.endian = packLittleEndian
		return packOption{kind: packKindNone}, 1
	case '>':
		h.endian = packBigEndian
		return packOption{kind: packKindNone}, 1
	case '!':
		n, next, has := readPackSize(fmtstr, i+1)
		if !has {
			L.RaiseError("missing size for format option '!'")
		}
		if n < 1 || n > packMaxAlign || n&(n-1) != 0 {
			L.RaiseError("invalid alignment %d in format (must be a power of two between 1 and %d)", n, packMaxAlign)
		}
		h.maxAlign = n
		return packOption{kind: packKindNone}, next - i
	case 'i', 'I':
		n, next, has := readPackSize(fmtstr, i+1)
		if !has {
			L.RaiseError("missing size for format option '%c'", c)
		}
		checkPackIntSize(L, c, n)
		if c == 'i' {
			return packOption{kind: packKindInt, size: n}, next - i
		}
		return packOption{kind: packKindUint, size: n}, next - i
	case 'f':
		return packOption{kind: packKindFloat, size: 4}, 1
	case 'd':
		return packOption{kind: packKindDouble, size: 8}, 1
	case 'c':
		n, next, has := readPackSize(fmtstr, i+1)
		if !has {
			L.RaiseError("missing size for format option 'c'")
		}
		return packOption{kind: packKindChar, size: n}, next - i
	case 's':
		n, next, has := readPackSize(fmtstr, i+1)
		if !has {
			L.RaiseError("missing size for format option 's'")
		}
		checkPackIntSize(L, 's', n)
		return packOption{kind: packKindPstr, size: n}, next - i
	case 'z':
		return packOption{kind: packKindZstr}, 1
	case 'x':
		return packOption{kind: packKindPad, size: 1}, 1
	default:
		L.RaiseError("invalid format option '%c'", c)
	}
	return packOption{}, 0
}

func packAlign(h *packHeader, opt packOption, pos int) int {
	switch opt.kind {
	case packKindInt, packKindUint, packKindFloat, packKindDouble, packKindPstr:
	default:
		return 0
	}
	align := opt.size
	if align > h.maxAlign {
		align = h.maxAlign
	}
	return (align - (pos & (align - 1))) & (align - 1)
}

func (h *packHeader) appendUint(buf []byte, v uint64, size int) []byte {
	var b [8]byte
	if h.endian == packLittleEndian {
		binary.LittleEndian.PutUint64(b[:], v)
		return append(buf, b[:size]...)
	}
	binary.BigEndian.PutUint64(b[:], v)
	return append(buf, b[8-size:]...)
}

func (h *packHeader) readUint(data string, size int) uint64 {
	var b [8]byte
	if h.endian == packLittleEndian {
		copy(b[:size], data)
		return binary.LittleEndian.Uint64(b[:])
	}
	copy(b[8-size:], data)
	return binary.BigEndian.Uint64(b[:])
}

func checkPackNumber(L *LState, arg int) float64 {
	switch v := L.Get(arg).(type) {
	case LNumber:
		return float64(v)
	case LString:
		if num, err := parseNumber(string(v)); err == nil {
			return float64(num)
		}
	}
	L.TypeError(arg, LTNumber)
	return 0
}

func checkPackInt64(L *LState, arg int) int64 {
	switch v := L.Get(arg).(type) {
	case LString:
		if n, err := strconv.ParseInt(strings.TrimSpace(string(v)), 10, 64); err == nil {
			return n
		}
		L.ArgError(arg, "invalid decimal integer string for 8-byte integer")
	case LNumber:
		f := float64(v)
		if f != math.Trunc(f) {
			L.ArgError(arg, "number has no integer representation")
		}
		if f < -9223372036854775808.0 || f >= 9223372036854775808.0 {
			L.ArgError(arg, "integer overflow")
		}
		return int64(f)
	default:
		L.ArgError(arg, "integer expected (number or decimal string)")
	}
	return 0
}

func checkPackUint64(L *LState, arg int) uint64 {
	switch v := L.Get(arg).(type) {
	case LString:
		if n, err := strconv.ParseUint(strings.TrimSpace(string(v)), 10, 64); err == nil {
			return n
		}
		L.ArgError(arg, "invalid decimal integer string for 8-byte unsigned integer")
	case LNumber:
		f := float64(v)
		if f != math.Trunc(f) {
			L.ArgError(arg, "number has no integer representation")
		}
		if f < 0 || f >= 18446744073709551616.0 {
			L.ArgError(arg, "integer overflow")
		}
		return uint64(f)
	default:
		L.ArgError(arg, "unsigned integer expected (number or decimal string)")
	}
	return 0
}

func packAppendInt(L *LState, h *packHeader, buf []byte, size int, signed bool, arg int) []byte {
	if size == 8 {
		if signed {
			return h.appendUint(buf, uint64(checkPackInt64(L, arg)), 8)
		}
		return h.appendUint(buf, checkPackUint64(L, arg), 8)
	}
	f := checkPackNumber(L, arg)
	if f != math.Trunc(f) {
		L.ArgError(arg, "number has no integer representation")
	}
	bits := uint(8 * size)
	if signed {
		if f < -math.Exp2(float64(bits-1)) || f > math.Exp2(float64(bits-1))-1 {
			L.ArgError(arg, "integer overflow")
		}
		return h.appendUint(buf, uint64(int64(f)), size)
	}
	if f < 0 || f > math.Exp2(float64(bits))-1 {
		L.ArgError(arg, "integer overflow")
	}
	return h.appendUint(buf, uint64(f), size)
}

func strPack(L *LState) int {
	fmtstr := L.CheckString(1)
	h := newPackHeader()
	buf := make([]byte, 0, 64)
	arg := 2
	for i := 0; i < len(fmtstr); {
		opt, n := parsePackOption(L, h, fmtstr, i)
		i += n
		if opt.kind == packKindNone {
			continue
		}
		for j := packAlign(h, opt, len(buf)); j > 0; j-- {
			buf = append(buf, 0)
		}
		switch opt.kind {
		case packKindPad:
			buf = append(buf, 0)
		case packKindInt:
			buf = packAppendInt(L, h, buf, opt.size, true, arg)
			arg++
		case packKindUint:
			buf = packAppendInt(L, h, buf, opt.size, false, arg)
			arg++
		case packKindFloat:
			v := float32(L.CheckNumber(arg))
			arg++
			buf = h.appendUint(buf, uint64(math.Float32bits(v)), 4)
		case packKindDouble:
			v := float64(L.CheckNumber(arg))
			arg++
			buf = h.appendUint(buf, math.Float64bits(v), 8)
		case packKindChar:
			s := L.CheckString(arg)
			if len(s) != opt.size {
				L.ArgError(arg, "wrong length for fixed-size string")
			}
			arg++
			buf = append(buf, s...)
		case packKindPstr:
			s := L.CheckString(arg)
			if uint64(len(s)) > uint64(1)<<uint(8*opt.size)-1 {
				L.ArgError(arg, "string length does not fit in given size")
			}
			arg++
			buf = h.appendUint(buf, uint64(len(s)), opt.size)
			buf = append(buf, s...)
		case packKindZstr:
			s := L.CheckString(arg)
			if strings.IndexByte(s, 0) >= 0 {
				L.ArgError(arg, "string contains zeros")
			}
			arg++
			buf = append(buf, s...)
			buf = append(buf, 0)
		}
	}
	L.Push(LString(buf))
	return 1
}

func checkUnpackAvail(L *LState, off, size, length int) {
	if size > length-off {
		L.RaiseError("data string too short")
	}
}

func unpackInitPos(L *LState, arg, length int) int {
	pos := L.OptInt(arg, 1)
	if pos < 0 {
		if -pos > length {
			pos = 0
		} else {
			pos = length + pos + 1
		}
	}
	if pos < 1 || pos > length+1 {
		L.ArgError(arg, "initial position out of string")
	}
	return pos
}

func pushUnpackedInt(L *LState, v uint64, size int, signed bool) {
	if signed {
		if size == 8 {
			L.Push(LString(strconv.FormatInt(int64(v), 10)))
			return
		}
		shift := uint(64 - 8*size)
		L.Push(LNumber(int64(v<<shift) >> shift))
		return
	}
	if size == 8 {
		L.Push(LString(strconv.FormatUint(v, 10)))
		return
	}
	L.Push(LNumber(v))
}

func strUnpack(L *LState) int {
	fmtstr := L.CheckString(1)
	data := L.CheckString(2)
	off := unpackInitPos(L, 3, len(data)) - 1
	h := newPackHeader()
	nres := 0
	for i := 0; i < len(fmtstr); {
		opt, n := parsePackOption(L, h, fmtstr, i)
		i += n
		if opt.kind == packKindNone {
			continue
		}
		off += packAlign(h, opt, off)
		switch opt.kind {
		case packKindPad:
			checkUnpackAvail(L, off, 1, len(data))
			off++
		case packKindInt, packKindUint:
			checkUnpackAvail(L, off, opt.size, len(data))
			v := h.readUint(data[off:off+opt.size], opt.size)
			off += opt.size
			pushUnpackedInt(L, v, opt.size, opt.kind == packKindInt)
			nres++
		case packKindFloat:
			checkUnpackAvail(L, off, 4, len(data))
			v := math.Float32frombits(uint32(h.readUint(data[off:off+4], 4)))
			off += 4
			L.Push(LNumber(v))
			nres++
		case packKindDouble:
			checkUnpackAvail(L, off, 8, len(data))
			v := math.Float64frombits(h.readUint(data[off:off+8], 8))
			off += 8
			L.Push(LNumber(v))
			nres++
		case packKindChar:
			checkUnpackAvail(L, off, opt.size, len(data))
			L.Push(LString(data[off : off+opt.size]))
			off += opt.size
			nres++
		case packKindPstr:
			checkUnpackAvail(L, off, opt.size, len(data))
			length := h.readUint(data[off:off+opt.size], opt.size)
			off += opt.size
			if length > uint64(len(data)-off) {
				L.RaiseError("data string too short")
			}
			L.Push(LString(data[off : off+int(length)]))
			off += int(length)
			nres++
		case packKindZstr:
			idx := strings.IndexByte(data[off:], 0)
			if idx < 0 {
				L.RaiseError("unfinished string for format 'z'")
			}
			L.Push(LString(data[off : off+idx]))
			off += idx + 1
			nres++
		}
	}
	L.Push(LNumber(off + 1))
	return nres + 1
}

func strPackSize(L *LState) int {
	fmtstr := L.CheckString(1)
	h := newPackHeader()
	total := 0
	for i := 0; i < len(fmtstr); {
		opt, n := parsePackOption(L, h, fmtstr, i)
		i += n
		if opt.kind == packKindNone {
			continue
		}
		if opt.kind == packKindZstr || opt.kind == packKindPstr {
			L.RaiseError("variable-size format in 'string.packsize'")
		}
		total += packAlign(h, opt, total) + opt.size
	}
	L.Push(LNumber(total))
	return 1
}
