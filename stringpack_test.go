package lua

import (
	"fmt"
	"testing"
)

func packResult(t *testing.T, L *LState, script string) string {
	t.Helper()
	errorIfScriptFail(t, L, "packresult = "+script)
	return string(L.GetGlobal("packresult").(LString))
}

func TestStrPackByteVectors(t *testing.T) {
	L := NewState()
	defer L.Close()

	cases := []struct {
		script   string
		expected string
	}{
		// endianness
		{`string.pack("<i2", 258)`, "\x02\x01"},
		{`string.pack(">i2", 258)`, "\x01\x02"},
		{`string.pack("<i2>I2", 1, 2)`, "\x01\x00\x00\x02"},
		// negative integers, two's complement
		{`string.pack("<i1", -1)`, "\xff"},
		{`string.pack(">i2", -2)`, "\xff\xfe"},
		{`string.pack("<i4", -2)`, "\xfe\xff\xff\xff"},
		{`string.pack(">i4", -2)`, "\xff\xff\xff\xfe"},
		// unsigned
		{`string.pack(">I4", 3735928559)`, "\xde\xad\xbe\xef"}, // 0xDEADBEEF
		{`string.pack("<I2", 65535)`, "\xff\xff"},
		// 8-byte integers via decimal strings
		{`string.pack("<i8", "-2")`, "\xfe\xff\xff\xff\xff\xff\xff\xff"},
		{`string.pack(">i8", "-9223372036854775808")`, "\x80\x00\x00\x00\x00\x00\x00\x00"},
		{`string.pack(">I8", "18446744073709551615")`, "\xff\xff\xff\xff\xff\xff\xff\xff"},
		{`string.pack("<i8", "9007199254740993")`, "\x01\x00\x00\x00\x00\x00\x20\x00"},
		// floats
		{`string.pack("<f", 1.5)`, "\x00\x00\xc0\x3f"},
		{`string.pack(">f", 1.5)`, "\x3f\xc0\x00\x00"},
		{`string.pack(">d", 1.5)`, "\x3f\xf8\x00\x00\x00\x00\x00\x00"},
		{`string.pack("<d", -0.5)`, "\x00\x00\x00\x00\x00\x00\xe0\xbf"},
		// alignment and padding
		{`string.pack("!4<i1i4", 1, 2)`, "\x01\x00\x00\x00\x02\x00\x00\x00"},
		{`string.pack("!2>i1i2", 1, 515)`, "\x01\x00\x02\x03"},
		{`string.pack("!8<i1d", 1, 1.5)`, "\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\xf8\x3f"},
		{`string.pack("xi1", 7)`, "\x00\x07"},
		{`string.pack("i1xxi1", 1, 2)`, "\x01\x00\x00\x02"},
		// fixed-length string with embedded NUL
		{`string.pack("c5", "a\0b\0c")`, "a\x00b\x00c"},
		// length-prefixed and zero-terminated strings
		{`string.pack("s1", "hi")`, "\x02hi"},
		{`string.pack(">s2", "hi")`, "\x00\x02hi"},
		{`string.pack("z", "ab")`, "ab\x00"},
	}
	for _, c := range cases {
		errorIfNotEqual(t, packResult(t, L, c.script), c.expected)
	}
}

func TestStrUnpackByteVectors(t *testing.T) {
	L := NewState()
	defer L.Close()

	cases := []struct {
		data     string
		script   string
		expected []LValue
	}{
		{"\x02\x01", `string.unpack("<i2", data)`, []LValue{LNumber(258), LNumber(3)}},
		{"\xff\xfe", `string.unpack(">i2", data)`, []LValue{LNumber(-2), LNumber(3)}},
		{"\xff", `string.unpack("<i1", data)`, []LValue{LNumber(-1), LNumber(2)}},
		{"\xde\xad\xbe\xef", `string.unpack(">I4", data)`, []LValue{LNumber(3735928559), LNumber(5)}},
		{"\xfe\xff\xff\xff\xff\xff\xff\xff", `string.unpack("<i8", data)`, []LValue{LString("-2"), LNumber(9)}},
		{"\xff\xff\xff\xff\xff\xff\xff\xff", `string.unpack(">I8", data)`, []LValue{LString("18446744073709551615"), LNumber(9)}},
		{"\x00\x00\xc0\x3f", `string.unpack("<f", data)`, []LValue{LNumber(1.5), LNumber(5)}},
		{"\x3f\xf8\x00\x00\x00\x00\x00\x00", `string.unpack(">d", data)`, []LValue{LNumber(1.5), LNumber(9)}},
		{"a\x00b\x00c", `string.unpack("c5", data)`, []LValue{LString("a\x00b\x00c"), LNumber(6)}},
		{"ab\x00cd", `string.unpack("z", data)`, []LValue{LString("ab"), LNumber(4)}},
		{"\x02hi", `string.unpack("s1", data)`, []LValue{LString("hi"), LNumber(4)}},
		// negative start positions
		{"\x01\x02\x03", `string.unpack("i1", data, -2)`, []LValue{LNumber(2), LNumber(3)}},
		{"\x01\x02\x03", `string.unpack("i1", data, -1)`, []LValue{LNumber(3), LNumber(4)}},
		{"\x01\x02\x03", `string.unpack("i1", data, -3)`, []LValue{LNumber(1), LNumber(2)}},
		// alignment, including alignment relative to an absolute offset
		{"\x01\x00\x00\x00\x02\x00\x00\x00", `string.unpack("!4<i1i4", data)`, []LValue{LNumber(1), LNumber(2), LNumber(9)}},
		{"XX\x01\x00\x02\x00\x00\x00", `string.unpack("!4i1i4", data, 3)`, []LValue{LNumber(1), LNumber(2), LNumber(9)}},
	}
	for _, c := range cases {
		L.SetGlobal("data", LString(c.data))
		errorIfScriptFail(t, L, "packresult = {"+c.script+"}")
		tbl := L.GetGlobal("packresult").(*LTable)
		errorIfNotEqual(t, tbl.Len(), len(c.expected))
		for i, expected := range c.expected {
			errorIfNotEqual(t, fmt.Sprint(expected), fmt.Sprint(tbl.RawGetInt(i+1)))
		}
	}
}

func TestStrPackSize(t *testing.T) {
	L := NewState()
	defer L.Close()

	cases := []struct {
		fmt      string
		expected int
	}{
		{"i1", 1},
		{"i4", 4},
		{"<i2d", 16},
		{"!2i2d", 10},
		{"!4i1i4", 8},
		{"c5", 5},
		{"x", 1},
		{"f", 4},
		{"d", 8},
		{"i8", 8},
		{"  < i2 > i2 ", 4},
	}
	for _, c := range cases {
		errorIfScriptFail(t, L, fmt.Sprintf("packresult = string.packsize(%q)", c.fmt))
		errorIfNotEqual(t, L.GetGlobal("packresult"), LNumber(c.expected))
	}

	errorIfScriptNotFail(t, L, `string.packsize("z")`, "variable-size format")
	errorIfScriptNotFail(t, L, `string.packsize("s4")`, "variable-size format")
}

func TestStrPackErrors(t *testing.T) {
	L := NewState()
	defer L.Close()

	cases := []struct {
		script  string
		pattern string
	}{
		// format errors
		{`string.pack("i3", 1)`, "invalid size 3"},
		{`string.pack("i", 1)`, "missing size"},
		{`string.pack("I", 1)`, "missing size"},
		{`string.pack("s", "a")`, "missing size"},
		{`string.pack("c", "a")`, "missing size"},
		{`string.pack("q", 1)`, "invalid format option"},
		{`string.pack("n", 1)`, "invalid format option"},
		{`string.pack("!3i1", 1)`, "invalid alignment"},
		{`string.pack("!16i1", 1)`, "invalid alignment"},
		{`string.pack("!i1", 1)`, "missing size"},
		// value errors
		{`string.pack("i1", 1.5)`, "no integer representation"},
		{`string.pack("i1", 128)`, "integer overflow"},
		{`string.pack("i1", -129)`, "integer overflow"},
		{`string.pack("I1", -1)`, "integer overflow"},
		{`string.pack("I2", 65536)`, "integer overflow"},
		{`string.pack("i4", 2147483648)`, "integer overflow"},
		{`string.pack("i8", "12x")`, "invalid decimal integer string"},
		{`string.pack("i8", "9223372036854775808")`, "invalid decimal integer string"},
		{`string.pack("I8", "-1")`, "invalid decimal integer string"},
		{`string.pack("i8", {})`, "integer expected"},
		{`string.pack("c3", "ab")`, "wrong length"},
		{`string.pack("c3", "abcd")`, "wrong length"},
		{`string.pack("s1", string.rep("a", 256))`, "does not fit"},
		{`string.pack("z", "a\0b")`, "string contains zeros"},
		{`string.pack("i1")`, "value expected|number expected"},
		// unpack errors
		{`string.unpack("i4", "\1")`, "data string too short"},
		{`string.unpack("i1", "ab", 5)`, "initial position out of string"},
		{`string.unpack("i1", "ab", 0)`, "initial position out of string"},
		{`string.unpack("i1", "ab", -3)`, "initial position out of string"},
		{`string.unpack("z", "abc")`, "unfinished string"},
		{`string.unpack("s1", "\5ab")`, "data string too short"},
		{`string.unpack("c10", "ab")`, "data string too short"},
	}
	for _, c := range cases {
		errorIfScriptNotFail(t, L, c.script, c.pattern)
	}
}

func TestStrPackRoundTrip(t *testing.T) {
	L := NewState()
	defer L.Close()

	errorIfScriptFail(t, L, `
		local packed = string.pack("<i4 i2 I1 f d c5 s2 z", -123456, -300, 200, 1.5, -0.25, "ab\0cd", "hello", "tail")
		local i4, i2, i1, f, d, c5, s2, z, pos = string.unpack("<i4 i2 I1 f d c5 s2 z", packed)
		assert(i4 == -123456)
		assert(i2 == -300)
		assert(i1 == 200)
		assert(f == 1.5)
		assert(d == -0.25)
		assert(c5 == "ab\0cd")
		assert(s2 == "hello")
		assert(z == "tail")
		assert(pos == #packed + 1)

		-- 8-byte integers round-trip through decimal strings
		for _, v in ipairs{"0", "1", "-1", "255", "9223372036854775807", "-9223372036854775808"} do
			assert(string.unpack("<i8", string.pack("<i8", v)) == v)
			assert(string.unpack(">i8", string.pack(">i8", v)) == v)
		end
		for _, v in ipairs{"0", "1", "255", "18446744073709551615"} do
			assert(string.unpack("<I8", string.pack("<I8", v)) == v)
		end

		-- big-endian round trip across widths
		local s = string.pack(">i1i2i4I1I2I4", -1, -2, -3, 255, 65535, 4294967295)
		local a, b, c, d, e, f, p = string.unpack(">i1i2i4I1I2I4", s)
		assert(a == -1 and b == -2 and c == -3)
		assert(d == 255 and e == 65535 and f == 4294967295)
		assert(p == #s + 1)

		-- negative start position
		local v, np = string.unpack(">i2", string.char(0, 0, 0x12, 0x34), -2)
		assert(v == 0x1234 and np == 5)

		-- unpack from the middle of a buffer
		local s2 = string.pack("<i2i2", 11, 22)
		local x, np2 = string.unpack("<i2", s2, 3)
		assert(x == 22 and np2 == 5)

		-- empty format only reports the position
		assert(string.unpack("", "abc", 2) == 2)
	`)
}
