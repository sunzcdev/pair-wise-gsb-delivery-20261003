package lua

import (
	"math"
	"strconv"
	"strings"
)

// This file implements a deterministic subset of the Lua 5.3 binary
// packing facilities: string.pack, string.unpack and string.packsize.
//
// Supported format items:
//   "<"  little endian (default)
//   ">"  big endian
//   "!n" maximum alignment n (power of 2, up to 16; default 1)
//   "i[n]" / "I[n]" signed/unsigned integers of n = 1, 2, 4 or 8 bytes
//   "f"  float32, "d" float64
//   "c[n]" fixed-length byte string of exactly n bytes
//   "s[n]" length-prefixed string, length stored in n = 1, 2, 4 or 8 bytes
//   "z"  zero-terminated string
//   "x"  one padding byte
//
// Host-dependent native-width aliases (b, h, l, j, T, ...) are not
// implemented. Because LNumber is a float64, 8-byte integers use an
// explicit decimal string convention on both input and output; narrower
// integers require exactly representable integral values.

const packMaxAlign = 16

type packOp struct {
	kind  byte // 'i', 'I', 'f', 'd', 'c', 's', 'z' or 'x'
	size  int  // element size in bytes (length-prefix size for 's')
	align int
	big   bool
}

func packReadSize(L *LState, format string, pos int, def int) (int, int) {
	start := pos
	n := 0
	for pos < len(format) && format[pos] >= '0' && format[pos] <= '9' {
		n = n*10 + int(format[pos]-'0')
		if n > 1<<30 {
			L.RaiseError("invalid format (size too large)")
		}
		pos++
	}
	if pos == start {
		return def, pos
	}
	return n, pos
}

func parsePackFormat(L *LState, format string) []packOp {
	ops := make([]packOp, 0, 8)
	big := false
	maxAlign := 1
	pos := 0
	for pos < len(format) {
		c := format[pos]
		pos++
		switch c {
		case ' ':
		case '<':
			big = false
		case '>':
			big = true
		case '!':
			n, next := packReadSize(L, format, pos, 0)
			pos = next
			if n <= 0 || n > packMaxAlign || n&(n-1) != 0 {
				L.RaiseError("invalid format (alignment %d is not a power of 2)", n)
			}
			maxAlign = n
		case 'i', 'I':
			n, next := packReadSize(L, format, pos, 4)
			pos = next
			switch n {
			case 1, 2, 4, 8:
			default:
				L.RaiseError("invalid format (integer size %d is not 1, 2, 4 or 8)", n)
			}
			ops = append(ops, packOp{kind: c, size: n, align: intMin(n, maxAlign), big: big})
		case 'f':
			ops = append(ops, packOp{kind: c, size: 4, align: intMin(4, maxAlign), big: big})
		case 'd':
			ops = append(ops, packOp{kind: c, size: 8, align: intMin(8, maxAlign), big: big})
		case 'c':
			n, next := packReadSize(L, format, pos, -1)
			pos = next
			if n < 0 {
				L.RaiseError("invalid format (missing size for option 'c')")
			}
			ops = append(ops, packOp{kind: c, size: n, align: 1})
		case 's':
			n, next := packReadSize(L, format, pos, 8)
			pos = next
			switch n {
			case 1, 2, 4, 8:
			default:
				L.RaiseError("invalid format (string length size %d is not 1, 2, 4 or 8)", n)
			}
			ops = append(ops, packOp{kind: c, size: n, align: intMin(n, maxAlign), big: big})
		case 'z':
			ops = append(ops, packOp{kind: c, align: 1})
		case 'x':
			ops = append(ops, packOp{kind: c, size: 1, align: 1})
		default:
			L.RaiseError("invalid format option '%c'", c)
		}
	}
	return ops
}

func packPadding(off int, align int) int {
	return (-off) & (align - 1)
}

func packAppendUint(buf []byte, v uint64, size int, big bool) []byte {
	if big {
		for i := size - 1; i >= 0; i-- {
			buf = append(buf, byte(v>>(8*uint(i))))
		}
	} else {
		for i := 0; i < size; i++ {
			buf = append(buf, byte(v>>(8*uint(i))))
		}
	}
	return buf
}

func packAppendSmallInt(L *LState, buf []byte, op packOp, v float64, arg int) []byte {
	if v != math.Trunc(v) {
		L.ArgError(arg, "number has no integer representation")
	}
	bits := float64(8 * op.size)
	if op.kind == 'i' {
		if v < -math.Exp2(bits-1) || v > math.Exp2(bits-1)-1 {
			L.ArgError(arg, "integer overflow")
		}
		return packAppendUint(buf, uint64(int64(v)), op.size, op.big)
	}
	if v < 0 || v > math.Exp2(bits)-1 {
		L.ArgError(arg, "integer overflow")
	}
	return packAppendUint(buf, uint64(v), op.size, op.big)
}

func packAppendInt64String(L *LState, buf []byte, op packOp, arg int) []byte {
	s, ok := L.Get(arg).(LString)
	if !ok {
		L.ArgError(arg, "8-byte integers must be given as decimal strings")
	}
	if op.kind == 'i' {
		v, err := strconv.ParseInt(string(s), 10, 64)
		if err != nil {
			L.ArgError(arg, "invalid decimal string for 8-byte integer")
		}
		return packAppendUint(buf, uint64(v), 8, op.big)
	}
	v, err := strconv.ParseUint(string(s), 10, 64)
	if err != nil {
		L.ArgError(arg, "invalid decimal string for 8-byte unsigned integer")
	}
	return packAppendUint(buf, v, 8, op.big)
}

func strPack(L *LState) int {
	ops := parsePackFormat(L, L.CheckString(1))
	buf := make([]byte, 0, 64)
	arg := 2
	for _, op := range ops {
		if pad := packPadding(len(buf), op.align); pad > 0 {
			buf = append(buf, make([]byte, pad)...)
		}
		switch op.kind {
		case 'i', 'I':
			if op.size == 8 {
				buf = packAppendInt64String(L, buf, op, arg)
			} else {
				buf = packAppendSmallInt(L, buf, op, float64(L.CheckNumber(arg)), arg)
			}
			arg++
		case 'f':
			buf = packAppendUint(buf, uint64(math.Float32bits(float32(L.CheckNumber(arg)))), 4, op.big)
			arg++
		case 'd':
			buf = packAppendUint(buf, math.Float64bits(float64(L.CheckNumber(arg))), 8, op.big)
			arg++
		case 'c':
			s := L.CheckString(arg)
			if len(s) > op.size {
				L.ArgError(arg, "string longer than given size")
			}
			arg++
			buf = append(buf, s...)
			if pad := op.size - len(s); pad > 0 {
				buf = append(buf, make([]byte, pad)...)
			}
		case 's':
			s := L.CheckString(arg)
			arg++
			if uint64(len(s)) > uint64(1)<<(8*uint(op.size))-1 {
				L.ArgError(arg, "string length does not fit in given size")
			}
			buf = packAppendUint(buf, uint64(len(s)), op.size, op.big)
			buf = append(buf, s...)
		case 'z':
			s := L.CheckString(arg)
			arg++
			if strings.IndexByte(s, 0) >= 0 {
				L.ArgError(arg, "strings 'z' cannot contain zeros")
			}
			buf = append(buf, s...)
			buf = append(buf, 0)
		case 'x':
			buf = append(buf, 0)
		}
	}
	L.Push(LString(string(buf)))
	return 1
}

func packCheckAvailable(L *LState, data string, off int, size int) {
	if size > len(data)-off {
		L.RaiseError("data string too short")
	}
}

func packReadUint(L *LState, data string, off int, size int, big bool) uint64 {
	packCheckAvailable(L, data, off, size)
	var v uint64
	if big {
		for i := 0; i < size; i++ {
			v = v<<8 | uint64(data[off+i])
		}
	} else {
		for i := size - 1; i >= 0; i-- {
			v = v<<8 | uint64(data[off+i])
		}
	}
	return v
}

func strUnpack(L *LState) int {
	ops := parsePackFormat(L, L.CheckString(1))
	data := L.CheckString(2)
	pos := L.OptInt(3, 1)
	if pos < 0 {
		pos = len(data) + pos + 1
		if pos < 0 {
			pos = 0
		}
	}
	if pos < 1 || pos > len(data)+1 {
		L.ArgError(3, "initial position out of string")
	}
	off := pos - 1
	nret := 0
	for _, op := range ops {
		off += packPadding(off, op.align)
		switch op.kind {
		case 'i', 'I':
			u := packReadUint(L, data, off, op.size, op.big)
			off += op.size
			if op.size == 8 {
				if op.kind == 'i' {
					L.Push(LString(strconv.FormatInt(int64(u), 10)))
				} else {
					L.Push(LString(strconv.FormatUint(u, 10)))
				}
			} else if op.kind == 'i' {
				shift := uint(64 - 8*op.size)
				L.Push(LNumber(float64(int64(u<<shift) >> shift)))
			} else {
				L.Push(LNumber(float64(u)))
			}
			nret++
		case 'f':
			u := packReadUint(L, data, off, 4, op.big)
			off += 4
			L.Push(LNumber(float64(math.Float32frombits(uint32(u)))))
			nret++
		case 'd':
			u := packReadUint(L, data, off, 8, op.big)
			off += 8
			L.Push(LNumber(math.Float64frombits(u)))
			nret++
		case 'c':
			packCheckAvailable(L, data, off, op.size)
			L.Push(LString(data[off : off+op.size]))
			off += op.size
			nret++
		case 's':
			length := packReadUint(L, data, off, op.size, op.big)
			off += op.size
			if length > uint64(len(data)-off) {
				L.RaiseError("data string too short")
			}
			L.Push(LString(data[off : off+int(length)]))
			off += int(length)
			nret++
		case 'z':
			idx := strings.IndexByte(data[off:], 0)
			if idx < 0 {
				L.RaiseError("unfinished string for format 'z'")
			}
			L.Push(LString(data[off : off+idx]))
			off += idx + 1
			nret++
		case 'x':
			packCheckAvailable(L, data, off, 1)
			off++
		}
	}
	L.Push(LNumber(off + 1))
	return nret + 1
}

func strPackSize(L *LState) int {
	ops := parsePackFormat(L, L.CheckString(1))
	size := 0
	for _, op := range ops {
		switch op.kind {
		case 's', 'z':
			L.RaiseError("variable-size format in packsize")
		}
		size += packPadding(size, op.align)
		size += op.size
	}
	L.Push(LNumber(size))
	return 1
}
