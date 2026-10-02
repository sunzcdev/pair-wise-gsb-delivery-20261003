package fastjson

import (
	"strconv"
	"strings"
	"testing"
)

func TestParseStrictInvalidUTF8(t *testing.T) {
	tests := []struct {
		s      string
		offset int
	}{
		// Invalid UTF-8 in a string value.
		{"\"\xff\"", 1},
		{"{\"key\":\"\xff\"}", 8},
		// Invalid UTF-8 in an object key.
		{"{\"\xff\":1}", 2},
		{"{\"\xff\":\"\xff\"}", 2},
		// The offset must be counted in original input bytes,
		// not in decoded chars: "€" occupies 3 bytes.
		{"{\"k\":\"€\xff\"}", 9},
		// An escaped backslash must not desync the scanner.
		{"{\"k\":\"\\\\\xff\"}", 8},
		{"\"\\\\\\\\\xff\"", 5},
		// Truncated multi-byte chars.
		{"\"a\xe4\xb8\"", 2},
		{"\"\xe4\"", 1},
		{"\"ok\xe4\xb8\"", 3},
		{"[\"\xf0\x9f\x98\"]", 2},
		// Overlong encoding and encoded surrogate.
		{"\"\xc0\xaf\"", 1},
		{"\"\xed\xa0\x80\"", 1},
	}
	for _, tt := range tests {
		if _, err := ParseStrict(tt.s); err == nil {
			t.Errorf("ParseStrict(%q): expecting error", tt.s)
		} else if !strings.Contains(err.Error(), "invalid UTF-8") {
			t.Errorf("ParseStrict(%q): unexpected error: %s", tt.s, err)
		} else if !strings.Contains(err.Error(), offsetSuffix(tt.offset)) {
			t.Errorf("ParseStrict(%q): error %q must contain byte offset %d", tt.s, err, tt.offset)
		}
		if err := ValidateStrict(tt.s); err == nil {
			t.Errorf("ValidateStrict(%q): expecting error", tt.s)
		} else if !strings.Contains(err.Error(), offsetSuffix(tt.offset)) {
			t.Errorf("ValidateStrict(%q): error %q must contain byte offset %d", tt.s, err, tt.offset)
		}
		// Non-strict mode must keep accepting these inputs.
		if _, err := Parse(tt.s); err != nil {
			t.Errorf("Parse(%q): unexpected error in non-strict mode: %s", tt.s, err)
		}
		if err := Validate(tt.s); err != nil {
			t.Errorf("Validate(%q): unexpected error in non-strict mode: %s", tt.s, err)
		}
	}
}

func TestParseStrictSurrogates(t *testing.T) {
	tests := []struct {
		s      string
		offset int
	}{
		// Lone high surrogate.
		{`"\uD800"`, 1},
		{`"\uD800x"`, 1},
		{`{"k":"\uDBFF"}`, 6},
		// Lone low surrogate.
		{`"\uDC00"`, 1},
		{`{"\uDFFF":1}`, 2},
		// Mis-ordered surrogate pair.
		{`"\uDC00\uD800"`, 1},
		{`["a","\uDE00\uD83D"]`, 6},
		// High surrogate followed by a non-surrogate escape.
		{`"\uD800\u0041"`, 1},
		// High surrogate followed by another high surrogate.
		{`"\uD800\uD800"`, 1},
		// High surrogate followed by a non-\u escape.
		{`"\uD800\n"`, 1},
		// High surrogate at the end of the string.
		{`"\uD800`, 1},
	}
	for _, tt := range tests {
		if _, err := ParseStrict(tt.s); err == nil {
			t.Errorf("ParseStrict(%q): expecting error", tt.s)
		} else if !strings.Contains(err.Error(), "surrogate") {
			t.Errorf("ParseStrict(%q): unexpected error: %s", tt.s, err)
		} else if !strings.Contains(err.Error(), offsetSuffix(tt.offset)) {
			t.Errorf("ParseStrict(%q): error %q must contain byte offset %d", tt.s, err, tt.offset)
		}
		if err := ValidateStrict(tt.s); err == nil {
			t.Errorf("ValidateStrict(%q): expecting error", tt.s)
		}
		// Non-strict mode must keep accepting these inputs
		// (except the unterminated string, which is a structural error).
		if tt.s == `"\uD800` {
			continue
		}
		if _, err := Parse(tt.s); err != nil {
			t.Errorf("Parse(%q): unexpected error in non-strict mode: %s", tt.s, err)
		}
		if err := Validate(tt.s); err != nil {
			t.Errorf("Validate(%q): unexpected error in non-strict mode: %s", tt.s, err)
		}
	}
}

func TestParseStrictValid(t *testing.T) {
	tests := []string{
		`"foobar"`,
		`""`,
		`"\"\\\/\b\f\n\r\t"`,
		`"A"`,
		`"\u0041"`,
		// Escaped backslashes.
		`"\\"`,
		`"\\\\"`,
		`{"k":"\\\\"}`,
		`"a\\b"`,
		`"\\\""`,
		// BMP escapes.
		`"€"`,
		`"中文字符"`,
		// Valid supplementary plane chars, raw and escaped.
		`"😀"`,
		`"\uD83D\uDE00"`,
		`"𝄞"`,
		`{"😀":"😀"}`,
		`{"\uD83D\uDE00":"\uD83D\uDE00"}`,
		// Mixed content.
		`{"key":["😀","\\","tail€",123,true,null]}`,
		// U+FFFD is valid UTF-8.
		"\"\uFFFD\"",
	}
	for _, s := range tests {
		if _, err := ParseStrict(s); err != nil {
			t.Errorf("ParseStrict(%q): unexpected error: %s", s, err)
		}
		if err := ValidateStrict(s); err != nil {
			t.Errorf("ValidateStrict(%q): unexpected error: %s", s, err)
		}
	}
}

func TestParseStrictRoundTrip(t *testing.T) {
	// After a successful strict parse, Get and MarshalTo must not change
	// the meaning of the chars.
	s := "{\"emoji\":\"\\uD83D\\uDE00\",\"raw\":\"😀\",\"esc\":\"a\\\\b\\n\",\"\\uD83D\\uDE00\":1,\"key\":\"tail€\"}"
	v, err := ParseStrict(s)
	if err != nil {
		t.Fatalf("ParseStrict(%q): unexpected error: %s", s, err)
	}
	if got := string(v.GetStringBytes("emoji")); got != "😀" {
		t.Fatalf("unexpected emoji value: %q", got)
	}
	if got := string(v.GetStringBytes("raw")); got != "😀" {
		t.Fatalf("unexpected raw value: %q", got)
	}
	if got := string(v.GetStringBytes("esc")); got != "a\\b\n" {
		t.Fatalf("unexpected esc value: %q", got)
	}
	if got := string(v.GetStringBytes("key")); got != "tail€" {
		t.Fatalf("unexpected key value: %q", got)
	}
	if !v.Exists("😀") {
		t.Fatalf("cannot get value by a supplementary plane key")
	}

	// MarshalTo output must parse to an equal value in strict mode.
	vv, err := ParseStrict(b2s(v.MarshalTo(nil)))
	if err != nil {
		t.Fatalf("cannot re-parse marshaled value: %s", err)
	}
	if got := string(vv.GetStringBytes("emoji")); got != "😀" {
		t.Fatalf("unexpected emoji value after round-trip: %q", got)
	}
	if got := string(vv.GetStringBytes("raw")); got != "😀" {
		t.Fatalf("unexpected raw value after round-trip: %q", got)
	}
	if got := string(vv.GetStringBytes("esc")); got != "a\\b\n" {
		t.Fatalf("unexpected esc value after round-trip: %q", got)
	}
	if got := string(vv.GetStringBytes("key")); got != "tail€" {
		t.Fatalf("unexpected key value after round-trip: %q", got)
	}
	if !vv.Exists("😀") {
		t.Fatalf("cannot get value by a supplementary plane key after round-trip")
	}
}

func TestParseStrictBytes(t *testing.T) {
	if _, err := ParseBytesStrict([]byte(`{"a":"😀"}`)); err != nil {
		t.Fatalf("ParseBytesStrict: unexpected error: %s", err)
	}
	if _, err := ParseBytesStrict([]byte("{\"a\":\"\xff\"}")); err == nil {
		t.Fatalf("ParseBytesStrict: expecting error")
	}
	if err := ValidateBytesStrict([]byte(`{"a":"😀"}`)); err != nil {
		t.Fatalf("ValidateBytesStrict: unexpected error: %s", err)
	}
	if err := ValidateBytesStrict([]byte("{\"a\":\"\xff\"}")); err == nil {
		t.Fatalf("ValidateBytesStrict: expecting error")
	}

	var p Parser
	if _, err := p.ParseStrict(`{"a":"😀"}`); err != nil {
		t.Fatalf("Parser.ParseStrict: unexpected error: %s", err)
	}
	if _, err := p.ParseBytesStrict([]byte("{\"a\":\"\xed\xa0\x80\"}")); err == nil {
		t.Fatalf("Parser.ParseBytesStrict: expecting error")
	}
}

func TestParseStrictStructuralError(t *testing.T) {
	// Structural errors must still be reported when there is nothing
	// wrong with the strings themselves.
	for _, s := range []string{`{"a":`, `"\`, `{"a" 1}`, `[1,]`} {
		if _, err := ParseStrict(s); err == nil {
			t.Errorf("ParseStrict(%q): expecting error", s)
		}
		if err := ValidateStrict(s); err == nil {
			t.Errorf("ValidateStrict(%q): expecting error", s)
		}
	}
}

func offsetSuffix(offset int) string {
	return "at offset " + strconv.Itoa(offset)
}
