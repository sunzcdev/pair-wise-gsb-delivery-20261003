package lua

import "testing"

// utf8Sample is "A" (1 byte) + "e-acute" (2 bytes) + "ideograph" (3 bytes)
// + "grinning face" (4 bytes): a sample spanning every UTF-8 length.
const utf8Sample = "Aé中😀"

func TestUtf8ModuleRegistered(t *testing.T) {
	L := NewState()
	defer L.Close()

	for _, name := range []string{"char", "codes", "codepoint", "len"} {
		if lv := L.GetField(L.GetGlobal("utf8"), name); lv.Type() != LTFunction {
			t.Errorf("utf8.%s should be a function, got %s", name, lv.Type())
		}
	}
	if cp := L.GetField(L.GetGlobal("utf8"), "charpattern"); cp.Type() != LTString {
		t.Errorf("utf8.charpattern should be a string, got %s", cp.Type())
	}

	// The byte-oriented string library must be untouched.
	errorIfScriptFail(t, L, `assert(string.len("`+utf8Sample+`") == 10)`)
}

func TestUtf8Len(t *testing.T) {
	L := NewState()
	defer L.Close()

	errorIfScriptFail(t, L, `
		local s = "`+utf8Sample+`"
		assert(utf8.len(("")) == 0)
		assert(utf8.len(("abc")) == 3)
		assert(utf8.len(s) == 4)

		-- explicit intervals, one-based byte positions
		assert(utf8.len(s, 1) == 4)
		assert(utf8.len(s, 2) == 3)
		assert(utf8.len(s, 2, 3) == 1)

		-- negative indices count back from the end
		assert(utf8.len(s, -9) == 3)
		assert(utf8.len(s, 1, -4) == 4)

		-- i == j + 1 yields an empty interval
		assert(utf8.len(("abc"), 4, 3) == 0)
		-- a position past the last byte cannot start a character
		assert(not pcall(utf8.len, ("abc"), 4, 4))

		-- a truncated sequence at the end is reported by its first byte
		local bad = "ab" .. string.char(0xC3)
		local ok, pos = utf8.len(bad)
		assert(ok == nil and pos == 3, "truncated sequence")

		-- lone continuation bytes report their own exact position
		local bad = "ab" .. string.char(0x80) .. "c"
		local ok, pos = utf8.len(bad)
		assert(ok == nil and pos == 3, "lone continuation")

		-- invalid lead byte
		local bad = "ab" .. string.char(0xFF)
		local ok, pos = utf8.len(bad)
		assert(ok == nil and pos == 3, "invalid lead")

		-- a well-formed lead followed by a bad continuation advances one byte
		local bad = "ab" .. string.char(0xC3, 0x63) .. "d"
		local ok, pos = utf8.len(bad)
		assert(ok == nil and pos == 3, "bad continuation")

		-- overlong two-byte encoding of '/'
		local bad = "ab" .. string.char(0xC0, 0xAF)
		local ok, pos = utf8.len(bad)
		assert(ok == nil and pos == 3, "overlong sequence")

		-- truncated sequence in the middle is found only when covered
		local bad = string.char(0xE4, 0xB8) .. "x"
		assert(utf8.len(bad, 3, 3) == 1)
		local ok, pos = utf8.len(bad, 1, 2)
		assert(ok == nil and pos == 1, "truncated mid sequence")

		-- UTF-8 encoding of a surrogate is invalid
		local bad = string.char(0xED, 0xA0, 0x80)
		local ok, pos = utf8.len(bad)
		assert(ok == nil and pos == 1, "surrogate")

		-- interval checks
		assert(not pcall(utf8.len, ("abc"), 0))
		assert(not pcall(utf8.len, ("abc"), 2, 5))
		assert(utf8.len(("abc"), 3, 2) == 0)
	`)
}

func TestUtf8CodePoint(t *testing.T) {
	L := NewState()
	defer L.Close()

	errorIfScriptFail(t, L, `
		local s = "`+utf8Sample+`"
		local a, b, c, d = utf8.codepoint(s, 1, -1)
		assert(a == 65 and b == 233 and c == 20013 and d == 128512)

		assert(utf8.codepoint("abc", 1, 3) == 97)
		assert(select(3, utf8.codepoint("abc", 1, 3)) == 99)

		-- default i is 1 and j equals i
		assert(utf8.codepoint("abc") == 97)

		-- negative indices land on lead bytes
		assert(utf8.codepoint(s, -4) == 128512)
		assert(utf8.codepoint(s, -7) == 20013)
		local a, b = utf8.codepoint(s, -7, -4)
		assert(a == 20013 and b == 128512)

		-- i must point at a valid (non-continuation) byte
		assert(not pcall(utf8.codepoint, s, 3))

		-- end position cannot start a character past the string
		assert(not pcall(utf8.codepoint, ("abc"), 1, 4))
		assert(not pcall(utf8.codepoint, (""), 1, 0))

		-- invalid bytes raise with the exact byte position
		local bad = "ab" .. string.char(0x80) .. "c"
		local ok, err = pcall(utf8.codepoint, bad, 3)
		assert(not ok and string.find(tostring(err), "3", 1, true))

		local bad = string.char(0xF0, 0x9F)
		local ok, err = pcall(utf8.codepoint, bad, 1)
		assert(not ok and string.find(tostring(err), "1", 1, true))

		local bad = "x" .. string.char(0xC3)
		local ok, err = pcall(utf8.codepoint, bad, 2)
		assert(not ok and string.find(tostring(err), "2", 1, true))
	`)
}

func TestUtf8Char(t *testing.T) {
	L := NewState()
	defer L.Close()

	errorIfScriptFail(t, L, `
		assert(utf8.char() == "")
		assert(utf8.char(65, 233, 20013, 128512) == "`+utf8Sample+`")
		assert(utf8.char(0x7F) == string.char(0x7F))
		assert(utf8.char(0x80) == string.char(0xC2, 0x80))
		assert(utf8.char(0x7FF) == string.char(0xDF, 0xBF))
		assert(utf8.char(0x800) == string.char(0xE0, 0xA0, 0x80))
		assert(utf8.char(0xFFFF) == string.char(0xEF, 0xBF, 0xBF))
		assert(utf8.char(0x10000) == string.char(0xF0, 0x90, 0x80, 0x80))
		assert(utf8.char(0x10FFFF) == string.char(0xF4, 0x8F, 0xBF, 0xBF))
		assert(utf8.char(0) == string.char(0))

		-- values outside the Unicode range are rejected, not encoded as U+FFFD
		assert(not pcall(utf8.char, 0x110000))
		assert(not pcall(utf8.char, -1))
		assert(not pcall(utf8.char, 65, 0x110000))

		-- surrogates cannot be encoded as valid UTF-8
		assert(not pcall(utf8.char, 0xD800))
		assert(not pcall(utf8.char, 0xDFFF))

		-- non-number arguments are rejected
		assert(not pcall(utf8.char, "x"))
	`)

	// Round-trip encode/decode.
	errorIfScriptFail(t, L, `
		local s = utf8.char(65, 233, 20013, 128512)
		local want = {65, 233, 20013, 128512}
		local n = 0
		for p, c in utf8.codes(s) do
			n = n + 1
			assert(c == want[n])
		end
		assert(n == 4)
	`)
}

func TestUtf8Codes(t *testing.T) {
	L := NewState()
	defer L.Close()

	errorIfScriptFail(t, L, `
		local s = "`+utf8Sample+`"
		local positions, values = {}, {}
		for p, c in utf8.codes(s) do
			positions[#positions + 1] = p
			values[#values + 1] = c
		end
		assert(positions[1] == 1 and values[1] == 65)
		assert(positions[2] == 2 and values[2] == 233)
		assert(positions[3] == 4 and values[3] == 20013)
		assert(positions[4] == 7 and values[4] == 128512)

		-- empty string terminates immediately
		local seen = false
		for p, c in utf8.codes("") do seen = true end
		assert(seen == false)

		-- ASCII-only traversal
		local out = {}
		for p, c in utf8.codes("abc") do out[#out + 1] = c end
		assert(out[1] == 97 and out[2] == 98 and out[3] == 99)
	`)

	// Invalid input raises at the exact byte position and only when the
	// iterator actually reaches it (lazy decoding).
	errorIfScriptFail(t, L, `
		local f, st = utf8.codes("ab" .. string.char(0x80) .. "c")
		assert(select(2, f(st)) == 97)
		assert(select(2, f(st)) == 98)
		local ok, err = pcall(f, st)
		assert(not ok and string.find(tostring(err), "3", 1, true))

		local f, st = utf8.codes(string.char(0xE4, 0xB8, 0xAD))
		local p, c = f(st)
		assert(p == 1 and c == 0x4E2D)
		assert(f(st) == nil)

		local f, st = utf8.codes("x" .. string.char(0xFF))
		assert(select(1, f(st)) == 1)
		local ok, err = pcall(f, st)
		assert(not ok and string.find(tostring(err), "2", 1, true))

		local f, st = utf8.codes(string.char(0xF0, 0x9F, 0x98))
		local ok, err = pcall(f, st)
		assert(not ok and string.find(tostring(err), "1", 1, true))
	`)
}
