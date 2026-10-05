package lua

import (
	"unicode/utf8"
)

const maxUnicode = 0x10FFFF

func OpenUtf8(L *LState) int {
	mod := L.RegisterModule(Utf8LibName, utf8Funcs).(*LTable)
	mod.RawSetString("charpattern", LString("[\x00-\x7F\xC2-\xF4][\x80-\xBF]*"))
	L.Push(mod)
	return 1
}

var utf8Funcs = map[string]LGFunction{
	"char":      utf8Char,
	"codepoint": utf8Codepoint,
	"codes":     utf8Codes,
	"len":       utf8Len,
}

// utf8PosRelat converts a Lua 1-based byte position (possibly negative,
// meaning relative to the end of the string) into a 1-based position.
func utf8PosRelat(pos int, length int) int {
	if pos < 0 {
		pos += length + 1
		if pos < 0 {
			pos = 0
		}
	}
	return pos
}

// utf8Decode decodes the first code point in s. It reports ok=false when s
// starts with an invalid UTF-8 sequence (including overlong encodings,
// surrogates, truncated sequences and values above utf8.MaxRune) instead of
// silently returning utf8.RuneError.
func utf8Decode(s string) (r rune, size int, ok bool) {
	r, size = utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && size <= 1 {
		return 0, 0, false
	}
	return r, size, true
}

func utf8Char(L *LState) int {
	top := L.GetTop()
	buf := make([]byte, 0, top*utf8.UTFMax)
	tmp := make([]byte, utf8.UTFMax)
	for i := 1; i <= top; i++ {
		cp := L.CheckInt(i)
		if cp < 0 || cp > maxUnicode {
			L.ArgError(i, "value out of range")
		}
		r := rune(cp)
		var n int
		if utf8.ValidRune(r) {
			n = utf8.EncodeRune(tmp, r)
		} else {
			// r is a surrogate half; encode it as a 3-byte sequence
			// (like Lua does) rather than emitting a replacement char.
			tmp[0] = 0xE0 | byte(r>>12)
			tmp[1] = 0x80 | byte(r>>6)&0x3F
			tmp[2] = 0x80 | byte(r)&0x3F
			n = 3
		}
		buf = append(buf, tmp[:n]...)
	}
	L.Push(LString(buf))
	return 1
}

func utf8Codepoint(L *LState) int {
	str := L.CheckString(1)
	length := len(str)
	start := utf8PosRelat(L.OptInt(2, 1), length)
	end := utf8PosRelat(L.OptInt(3, start), length)
	if start < 1 {
		L.ArgError(2, "out of range")
	}
	if end > length {
		L.ArgError(3, "out of range")
	}
	if start > end {
		return 0
	}
	n := 0
	for pos := start - 1; pos < end; {
		r, size, ok := utf8Decode(str[pos:])
		if !ok {
			L.RaiseError("invalid UTF-8 code at byte %d", pos+1)
		}
		L.Push(LNumber(r))
		n++
		pos += size
	}
	return n
}

func utf8Len(L *LState) int {
	str := L.CheckString(1)
	length := len(str)
	start := utf8PosRelat(L.OptInt(2, 1), length)
	end := utf8PosRelat(L.OptInt(3, -1), length)
	if start < 1 || start-1 > length {
		L.ArgError(2, "initial position out of string")
	}
	if end > length {
		L.ArgError(3, "final position out of string")
	}
	count := 0
	for pos := start - 1; pos < end && pos < length; {
		_, size, ok := utf8Decode(str[pos:])
		if !ok {
			L.Push(LNil)
			L.Push(LNumber(pos + 1))
			return 2
		}
		pos += size
		count++
	}
	L.Push(LNumber(count))
	return 1
}

func utf8Codes(L *LState) int {
	L.CheckString(1)
	L.Push(L.NewFunction(utf8CodesIter))
	L.Push(L.Get(1))
	L.Push(LNumber(0))
	return 3
}

// utf8CodesIter lazily decodes one code point per call. The control value is
// the 1-based byte position of the previously returned code point (0 before
// the first call); invalid UTF-8 raises an error reporting the exact byte
// position.
func utf8CodesIter(L *LState) int {
	str := L.CheckString(1)
	control := L.CheckInt(2)
	next := 0
	if control > 0 {
		if control > len(str) {
			return 0
		}
		_, size, ok := utf8Decode(str[control-1:])
		if !ok {
			L.RaiseError("invalid UTF-8 code at byte %d", control)
		}
		next = control - 1 + size
	} else if control < 0 {
		L.ArgError(2, "initial position out of string")
	}
	if next >= len(str) {
		return 0
	}
	r, _, ok := utf8Decode(str[next:])
	if !ok {
		L.RaiseError("invalid UTF-8 code at byte %d", next+1)
	}
	L.Push(LNumber(next + 1))
	L.Push(LNumber(r))
	return 2
}
