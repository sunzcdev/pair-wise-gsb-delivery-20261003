package lua

import (
	"testing"
)

// "aé界𐍈" = 97 (1B), 233 (2B), 30028 (3B), 66376 (4B)
// byte positions: 1, 2, 4, 7; total 10 bytes, 4 code points.
const utf8Sample = "aé界𐍈"

func TestUtf8Len(t *testing.T) {
	L := NewState()
	defer L.Close()
	OpenUtf8(L)
	L.Pop(1)
	L.SetGlobal("sample", LString(utf8Sample))

	errorIfScriptFail(t, L, `
		assert(utf8.len(sample) == 4)
		assert(utf8.len("") == 0)
		assert(utf8.len("hello") == 5)
		assert(utf8.len(sample, 2) == 3)
		assert(utf8.len(sample, 7) == 1)
		assert(utf8.len(sample, -4) == 1)
		assert(utf8.len(sample, -7) == 2)
		assert(utf8.len(sample, 2, 3) == 1)
		assert(utf8.len(sample, 2, 6) == 2)
		assert(utf8.len("abc", 2, 1) == 0)
	`)
}

func TestUtf8LenInvalid(t *testing.T) {
	L := NewState()
	defer L.Close()
	OpenUtf8(L)
	L.Pop(1)

	// isolated continuation byte at byte 2
	errorIfScriptFail(t, L, `
		local n, pos = utf8.len("a" .. string.char(0x80) .. "b")
		assert(n == nil and pos == 2)
	`)
	// truncated multi-byte sequence at byte 2
	errorIfScriptFail(t, L, `
		local n, pos = utf8.len("a" .. string.char(0xC3))
		assert(n == nil and pos == 2)
	`)
	// invalid byte after a multi-byte boundary
	errorIfScriptFail(t, L, `
		local n, pos = utf8.len("é" .. string.char(0xFF))
		assert(n == nil and pos == 3)
	`)
	// out-of-range arguments
	errorIfScriptNotFail(t, L, `utf8.len("abc", 5)`, "initial position out of string")
	errorIfScriptNotFail(t, L, `utf8.len("abc", 1, 4)`, "final position out of string")
}

func TestUtf8Codepoint(t *testing.T) {
	L := NewState()
	defer L.Close()
	OpenUtf8(L)
	L.Pop(1)
	L.SetGlobal("sample", LString(utf8Sample))

	errorIfScriptFail(t, L, `
		assert(utf8.codepoint(sample, 1) == 97)
		assert(utf8.codepoint(sample, 2) == 233)
		assert(utf8.codepoint(sample, 4) == 30028)
		assert(utf8.codepoint(sample, 7) == 66376)
		assert(utf8.codepoint(sample, -4) == 66376)
		assert(utf8.codepoint(sample) == 97)
		local a, b = utf8.codepoint(sample, 4, 7)
		assert(a == 30028 and b == 66376)
		assert(utf8.codepoint("abc", 2, 1) == nil)
	`)
	// invalid UTF-8 reports the exact byte position
	errorIfScriptNotFail(t, L,
		`utf8.codepoint("a" .. string.char(0x80), 2)`, "invalid UTF-8 code at byte 2")
	errorIfScriptNotFail(t, L,
		`utf8.codepoint(string.char(0xE4, 0xBD), 1)`, "invalid UTF-8 code at byte 1")
	// out-of-range arguments
	errorIfScriptNotFail(t, L, `utf8.codepoint("abc", 0)`, "out of range")
	errorIfScriptNotFail(t, L, `utf8.codepoint("abc", 1, 4)`, "out of range")
}

func TestUtf8Codes(t *testing.T) {
	L := NewState()
	defer L.Close()
	OpenUtf8(L)
	L.Pop(1)
	L.SetGlobal("sample", LString(utf8Sample))

	errorIfScriptFail(t, L, `
		local ps, cs = {}, {}
		for p, c in utf8.codes(sample) do
			ps[#ps + 1] = p
			cs[#cs + 1] = c
		end
		assert(#ps == 4)
		assert(ps[1] == 1 and ps[2] == 2 and ps[3] == 4 and ps[4] == 7)
		assert(cs[1] == 97 and cs[2] == 233 and cs[3] == 30028 and cs[4] == 66376)
		for p, c in utf8.codes("") do
			error("empty string must not iterate")
		end
	`)
}

func TestUtf8CodesInvalid(t *testing.T) {
	L := NewState()
	defer L.Close()
	OpenUtf8(L)
	L.Pop(1)

	// The iterator is lazy: valid prefixes are produced before the error.
	errorIfScriptFail(t, L, `
		local seen = 0
		local ok, err = pcall(function()
			for p, c in utf8.codes("ab" .. string.char(0xFF)) do
				seen = seen + 1
			end
		end)
		assert(not ok)
		assert(seen == 2)
		assert(string.find(err, "invalid UTF%-8 code at byte 3"))
	`)
	// isolated continuation byte
	errorIfScriptFail(t, L, `
		local ok, err = pcall(function()
			for p, c in utf8.codes(string.char(0x80)) do end
		end)
		assert(not ok)
		assert(string.find(err, "invalid UTF%-8 code at byte 1"))
	`)
	// truncated sequence
	errorIfScriptFail(t, L, `
		local ok, err = pcall(function()
			for p, c in utf8.codes("x" .. string.char(0xE4, 0xBD)) do end
		end)
		assert(not ok)
		assert(string.find(err, "invalid UTF%-8 code at byte 2"))
	`)
}

func TestUtf8Char(t *testing.T) {
	L := NewState()
	defer L.Close()
	OpenUtf8(L)
	L.Pop(1)
	L.SetGlobal("sample", LString(utf8Sample))

	errorIfScriptFail(t, L, `
		assert(utf8.char(97, 233, 30028, 66376) == sample)
		assert(utf8.char() == "")
		assert(utf8.char(0x10FFFF) == string.char(0xF4, 0x8F, 0xBF, 0xBF))
		assert(utf8.char(0) == string.char(0))
		-- surrogate halves are encoded, not replaced with U+FFFD
		assert(utf8.char(0xD800) == string.char(0xED, 0xA0, 0x80))
		-- round trip
		assert(utf8.char(utf8.codepoint(sample, 1, #sample)) == sample)
	`)
	// values beyond the Unicode range are rejected
	errorIfScriptNotFail(t, L, `utf8.char(0x110000)`, "value out of range")
	errorIfScriptNotFail(t, L, `utf8.char(-1)`, "value out of range")
}

func TestUtf8Charpattern(t *testing.T) {
	L := NewState()
	defer L.Close()
	OpenUtf8(L)
	L.Pop(1)

	errorIfScriptFail(t, L, `assert(type(utf8.charpattern) == "string")`)
}
