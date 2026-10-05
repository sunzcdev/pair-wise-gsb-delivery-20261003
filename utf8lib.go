package lua

import (
	stdutf8 "unicode/utf8"
)

// OpenUtf8 opens the utf8 library.
//
// It is an independent library that operates on Unicode code points and
// leaves the byte-oriented string library untouched. Indices follow Lua
// conventions: they are one-based byte positions and negative values count
// back from the end of the string.
func OpenUtf8(L *LState) int {
	mod := L.RegisterModule(Utf8LibName, utf8Funcs).(*LTable)
	codes := L.NewClosure(utf8Codes, L.NewFunction(utf8CodesIter))
	mod.RawSetString("codes", codes)
	mod.RawSetString("charpattern", LString("[\x00-\x7F\xC2-\xF4][\x80-\xBF]*"))
	L.Push(mod)
	return 1
}

var utf8Funcs = map[string]LGFunction{
	"char":      utf8Char,
	"codepoint": utf8CodePoint,
	"len":       utf8Len,
}

// utf8Iterator holds the state of a single utf8.codes traversal. Iteration
// decodes one code point at a time and never materializes the string as a
// rune slice.
type utf8Iterator struct {
	str string
	pos int // next byte offset, zero based
}

// utf8BytePos converts a Lua one-based byte index (with negative indices
// counting from the end) into a zero-based byte offset.
func utf8BytePos(str string, i int) int {
	n := len(str)
	if i < 0 {
		i = n + i + 1
	}
	return i - 1
}

// decodeUtf8At decodes the code point beginning at str[p].
//
// Unlike utf8.DecodeRuneInString, a well-formed lead byte of a sequence
// truncated by the end of the string still reports its declared width, so
// callers can attribute an error to the exact lead byte position the way
// Lua does. An invalid or continuation byte advances by one.
func decodeUtf8At(str string, p int) (rune, int) {
	if str[p] < 0x80 {
		return rune(str[p]), 1
	}
	lead := str[p]
	if lead < 0xC2 || lead > 0xF4 {
		return stdutf8.RuneError, 1
	}
	declared := stdutf8.RuneLen(rune(lead))
	r, size := stdutf8.DecodeRuneInString(str[p:])
	if r == stdutf8.RuneError && size == 1 {
		if len(str)-p >= declared {
			return stdutf8.RuneError, 1
		}
		return stdutf8.RuneError, declared
	}
	return r, size
}

func utf8Len(L *LState) int {
	str := L.CheckString(1)
	n := len(str)
	start := utf8BytePos(str, L.OptInt(2, 1))
	end := utf8BytePos(str, L.OptInt(3, -1))
	if start < 0 || start > n || end < -1 || end >= n || (start == n && end != n-1) {
		L.ArgError(2, "interval out of range")
	}

	count := 0
	for p := start; p <= end; {
		r, size := decodeUtf8At(str, p)
		if r == stdutf8.RuneError {
			L.Push(LNil)
			L.Push(LNumber(p + 1))
			return 2
		}
		p += size
		count++
	}
	L.Push(LNumber(count))
	return 1
}

func utf8CodePoint(L *LState) int {
	str := L.CheckString(1)
	n := len(str)
	start := utf8BytePos(str, L.OptInt(2, 1))
	end := start
	if L.GetTop() >= 3 {
		end = utf8BytePos(str, L.CheckInt(3))
	}
	if start < 0 || end < start || end >= n {
		L.ArgError(2, "interval out of range")
	}

	pushed := 0
	p := start
	for {
		r, size := decodeUtf8At(str, p)
		if r == stdutf8.RuneError {
			L.RaiseError("invalid UTF-8 code at position %d", p+1)
		}
		L.Push(LNumber(r))
		pushed++
		p += size
		if p > end {
			break
		}
	}
	return pushed
}

func utf8Char(L *LState) int {
	top := L.GetTop()
	buf := make([]byte, 0, top*stdutf8.UTFMax)
	for i := 1; i <= top; i++ {
		r := rune(L.CheckInt(i))
		if !stdutf8.ValidRune(r) {
			L.ArgError(i, "value out of range")
		}
		buf = stdutf8.AppendRune(buf, r)
	}
	L.Push(LString(string(buf)))
	return 1
}

func utf8CodesIter(L *LState) int {
	data := L.CheckUserData(1).Value.(*utf8Iterator)
	if data.pos >= len(data.str) {
		return 0
	}
	p := data.pos
	r, size := decodeUtf8At(data.str, p)
	if r == stdutf8.RuneError {
		L.RaiseError("invalid UTF-8 code at position %d", p+1)
	}
	data.pos = p + size
	L.Push(LNumber(p + 1))
	L.Push(LNumber(r))
	return 2
}

func utf8Codes(L *LState) int {
	str := L.CheckString(1)
	L.Push(L.Get(UpvalueIndex(1)))
	ud := L.NewUserData()
	ud.Value = &utf8Iterator{str: str, pos: 0}
	L.Push(ud)
	return 2
}
