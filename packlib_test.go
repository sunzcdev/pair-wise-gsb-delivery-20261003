package lua

import (
	"testing"
)

func TestStrPackFixedVectors(t *testing.T) {
	L := NewState()
	defer L.Close()
	vectors := []struct {
		script string
		want   []byte
	}{
		// endianness
		{`return string.pack(">I2", 0x1234)`, []byte{0x12, 0x34}},
		{`return string.pack("<I2", 0x1234)`, []byte{0x34, 0x12}},
		{`return string.pack(">I4", 0xdeadbeef)`, []byte{0xde, 0xad, 0xbe, 0xef}},
		{`return string.pack("<I4", 0xdeadbeef)`, []byte{0xef, 0xbe, 0xad, 0xde}},
		// negative integers, two's complement
		{`return string.pack(">i2", -2)`, []byte{0xff, 0xfe}},
		{`return string.pack("<i4", -2)`, []byte{0xfe, 0xff, 0xff, 0xff}},
		{`return string.pack("i1", -1)`, []byte{0xff}},
		{`return string.pack(">i4", -2147483648)`, []byte{0x80, 0x00, 0x00, 0x00}},
		// 8-byte integers via decimal strings
		{`return string.pack(">i8", "-2")`, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe}},
		{`return string.pack("<I8", "18446744073709551615")`, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}},
		{`return string.pack(">i8", "0")`, []byte{0, 0, 0, 0, 0, 0, 0, 0}},
		// floats
		{`return string.pack(">f", 1.5)`, []byte{0x3f, 0xc0, 0x00, 0x00}},
		{`return string.pack("<d", 1.5)`, []byte{0, 0, 0, 0, 0, 0, 0xf8, 0x3f}},
		// fixed-length strings: zero padding and embedded NUL
		{`return string.pack("c5", "ab")`, []byte{'a', 'b', 0, 0, 0}},
		{`return string.pack("c3", "a\0b")`, []byte{'a', 0, 'b'}},
		{`return string.pack("c0", "")`, []byte{}},
		// zero-terminated strings
		{`return string.pack("z", "ab")`, []byte{'a', 'b', 0}},
		// length-prefixed strings
		{`return string.pack(">s1", "ab")`, []byte{2, 'a', 'b'}},
		{`return string.pack("<s2", "ab")`, []byte{2, 0, 'a', 'b'}},
		{`return string.pack(">s2", "a\0b")`, []byte{0, 3, 'a', 0, 'b'}},
		// explicit padding
		{`return string.pack("i1xi1", 1, 2)`, []byte{1, 0, 2}},
		// alignment via !n
		{`return string.pack("<!4 i1 i4", 1, 0x02030405)`, []byte{1, 0, 0, 0, 0x05, 0x04, 0x03, 0x02}},
		{`return string.pack(">!8 i1 d", 1, 1.5)`, []byte{1, 0, 0, 0, 0, 0, 0, 0, 0x3f, 0xf8, 0, 0, 0, 0, 0, 0}},
		// mixed endianness within one format
		{`return string.pack(">I2 <I2", 1, 1)`, []byte{0, 1, 1, 0}},
	}
	for _, vector := range vectors {
		if err := L.DoString(vector.script); err != nil {
			t.Errorf("%v script failed: %v", positionString(1), err)
			continue
		}
		got := L.CheckString(-1)
		L.Pop(1)
		if got != string(vector.want) {
			t.Errorf("%v %q: got %v, want %v", positionString(1), vector.script, []byte(got), vector.want)
		}
	}
}

func TestStrUnpackFixedVectors(t *testing.T) {
	L := NewState()
	defer L.Close()
	errorIfScriptFail(t, L, `
		-- endianness and next-position return
		local a, pos = string.unpack(">I2", string.char(0x12, 0x34, 0x99))
		assert(a == 0x1234 and pos == 3)
		a, pos = string.unpack("<I2", string.char(0x34, 0x12))
		assert(a == 0x1234 and pos == 3)
		-- negative integers
		a = string.unpack(">i2", string.char(0xff, 0xfe))
		assert(a == -2)
		a = string.unpack("i1", string.char(0xff))
		assert(a == -1)
		a = string.unpack("I1", string.char(0xff))
		assert(a == 255)
		-- 8-byte integers come back as decimal strings
		local b
		b, pos = string.unpack(">i8", string.char(0x80, 0, 0, 0, 0, 0, 0, 0))
		assert(b == "-9223372036854775808" and pos == 9)
		b = string.unpack("<I8", string.rep(string.char(0xff), 8))
		assert(b == "18446744073709551615")
		-- floats
		assert(string.unpack(">f", string.char(0x3f, 0xc0, 0, 0)) == 1.5)
		assert(string.unpack("<d", string.char(0, 0, 0, 0, 0, 0, 0xf8, 0x3f)) == 1.5)
		-- embedded NUL
		local s
		s, pos = string.unpack("c3", "a\0b")
		assert(s == "a\0b" and pos == 4)
		s = string.unpack(">s2", string.char(0, 3) .. "a\0b")
		assert(s == "a\0b")
		s = string.unpack("z", "ab\0cd")
		assert(s == "ab")
		-- alignment padding is skipped
		local x, y
		x, y, pos = string.unpack("<!4 i1 i4", string.char(1, 0, 0, 0, 0x05, 0x04, 0x03, 0x02))
		assert(x == 1 and y == 0x02030405 and pos == 9)
		-- negative start offsets
		a, pos = string.unpack("i1", "abc", -1)
		assert(a == 99 and pos == 4)
		s, pos = string.unpack("c2", "abc", -2)
		assert(s == "bc" and pos == 4)
		a, pos = string.unpack("i1", "abc", -3)
		assert(a == 97 and pos == 2)
		-- boundary: empty read at end of string
		pos = string.unpack("", "abc")
		assert(pos == 1)
		pos = string.unpack("", "abc", 4)
		assert(pos == 4)
	`)
}

func TestStrPackSize(t *testing.T) {
	L := NewState()
	defer L.Close()
	errorIfScriptFail(t, L, `
		assert(string.packsize("i1i2i4") == 7)
		assert(string.packsize("iI") == 8)
		assert(string.packsize("i8fd") == 20)
		assert(string.packsize("c5xx") == 7)
		assert(string.packsize(">!8 i1 d") == 16)
		assert(string.packsize("<!4 i1 i4") == 8)
		assert(string.packsize("") == 0)
	`)
	errorIfScriptNotFail(t, L, `string.packsize("s4")`, "variable-size format")
	errorIfScriptNotFail(t, L, `string.packsize("z")`, "variable-size format")
}

func TestStrPackRoundTrip(t *testing.T) {
	L := NewState()
	defer L.Close()
	errorIfScriptFail(t, L, `
		local fmt = "<!4 i1 i2 >I4 d f s1 z c5 x"
		local packed = string.pack(fmt, -5, -300, 4000000000, 1.5, 2.5, "hi", "there", "ab")
		local i1, i2, u4, d, f, s, z, c, pos = string.unpack(fmt, packed)
		assert(i1 == -5 and i2 == -300 and u4 == 4000000000)
		assert(d == 1.5 and f == 2.5)
		assert(s == "hi" and z == "there" and c == "ab\0\0\0")
		assert(pos == #packed + 1)

		local p8 = string.pack(">i8 <I8", "-9223372036854775808", "18446744073709551615")
		local a, b, n = string.unpack(">i8 <I8", p8)
		assert(a == "-9223372036854775808" and b == "18446744073709551615" and n == 17)

		local withnul = string.pack("s1 z c4", "a\0b", "cd", "e\0f")
		local r1, r2, r3, rpos = string.unpack("s1 z c4", withnul)
		assert(r1 == "a\0b" and r2 == "cd" and r3 == "e\0f\0" and rpos == #withnul + 1)
	`)
}

func TestStrPackErrors(t *testing.T) {
	L := NewState()
	defer L.Close()
	// format errors
	errorIfScriptNotFail(t, L, `string.pack("q", 1)`, "invalid format option")
	errorIfScriptNotFail(t, L, `string.pack("i3", 1)`, "integer size 3")
	errorIfScriptNotFail(t, L, `string.pack("i0", 1)`, "integer size 0")
	errorIfScriptNotFail(t, L, `string.pack("!3 i1", 1)`, "alignment 3")
	errorIfScriptNotFail(t, L, `string.pack("!0 i1", 1)`, "alignment 0")
	errorIfScriptNotFail(t, L, `string.pack("c", "a")`, "missing size")
	errorIfScriptNotFail(t, L, `string.unpack("i3", "abcd")`, "integer size 3")
	errorIfScriptNotFail(t, L, `string.packsize("s3")`, "length size 3")
	// value errors
	errorIfScriptNotFail(t, L, `string.pack("i1", 1.5)`, "no integer representation")
	errorIfScriptNotFail(t, L, `string.pack("i1", 200)`, "integer overflow")
	errorIfScriptNotFail(t, L, `string.pack("i1", -129)`, "integer overflow")
	errorIfScriptNotFail(t, L, `string.pack("I1", -1)`, "integer overflow")
	errorIfScriptNotFail(t, L, `string.pack("I4", 4294967296)`, "integer overflow")
	errorIfScriptNotFail(t, L, `string.pack("i8", 5)`, "decimal strings")
	errorIfScriptNotFail(t, L, `string.pack("i8", "notanumber")`, "invalid decimal string")
	errorIfScriptNotFail(t, L, `string.pack("I8", "-1")`, "invalid decimal string")
	errorIfScriptNotFail(t, L, `string.pack("i8", "9223372036854775808")`, "invalid decimal string")
	errorIfScriptNotFail(t, L, `string.pack("c2", "abc")`, "longer than given size")
	errorIfScriptNotFail(t, L, `string.pack("s1", string.rep("a", 256))`, "does not fit")
	errorIfScriptNotFail(t, L, `string.pack("z", "a\0b")`, "cannot contain zeros")
	// truncation and offsets must be Lua errors, not Go panics
	errorIfScriptNotFail(t, L, `string.unpack("i4", "ab")`, "data string too short")
	errorIfScriptNotFail(t, L, `string.unpack("c5", "ab")`, "data string too short")
	errorIfScriptNotFail(t, L, `string.unpack("s1", string.char(5) .. "ab")`, "data string too short")
	errorIfScriptNotFail(t, L, `string.unpack("s1", "")`, "data string too short")
	errorIfScriptNotFail(t, L, `string.unpack("z", "abc")`, "unfinished string")
	errorIfScriptNotFail(t, L, `string.unpack("x", "")`, "data string too short")
	errorIfScriptNotFail(t, L, `string.unpack("i1", "", 2)`, "initial position out of string")
	errorIfScriptNotFail(t, L, `string.unpack("i1", "abc", -5)`, "initial position out of string")
	errorIfScriptNotFail(t, L, `string.unpack("i1", "abc", 0)`, "initial position out of string")
}
