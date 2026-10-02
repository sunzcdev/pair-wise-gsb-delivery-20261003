package fastjson

import (
	"strings"
	"testing"
)

func TestParseStrictInvalidUnicode(t *testing.T) {
	tests := []struct {
		s string
		// want is the expected part of the ParseStrict error message.
		want string
		// wantValidate is the expected part of the ValidateStrict error
		// message. If empty, it equals want. If "-", any error is accepted.
		wantValidate string
	}{
		// Invalid UTF-8 in object keys and values.
		{"{\"k\":\"\xff\"}", "invalid UTF-8 byte 0xFF at position 6", ""},
		{"{\"\xff\":1}", "invalid UTF-8 byte 0xFF at position 2", ""},
		{"[\"\xff\"]", "invalid UTF-8 byte 0xFF at position 2", ""},
		// The position must be calculated in raw input bytes,
		// not in decoded chars: é takes 2 bytes.
		{"{\"k\":\"é\xff\"}", "invalid UTF-8 byte 0xFF at position 8", ""},
		// Leading whitespace must be counted in the position.
		{"  {\"k\":\"\xff\"}", "invalid UTF-8 byte 0xFF at position 8", ""},
		// Truncated multi-byte chars.
		{"\"\xe4\xb8\"", "invalid UTF-8 byte 0xE4 at position 1", ""},
		{"\"\xc3\"", "invalid UTF-8 byte 0xC3 at position 1", ""},
		{"{\"k\":\"abc\xc3\"}", "invalid UTF-8 byte 0xC3 at position 9", ""},
		// UTF-8 encoded surrogates are invalid UTF-8.
		{"\"\xed\xa0\x80\"", "invalid UTF-8 byte 0xED at position 1", ""},
		// Lone surrogates in object keys and values.
		{`{"k":"\uD800"}`, `unpaired high surrogate \uD800 at position 6`, ""},
		{`{"\uD800":1}`, `unpaired high surrogate \uD800 at position 2`, ""},
		{`{"k":"\uDC00"}`, `unpaired low surrogate \uDC00 at position 6`, ""},
		// Surrogate pair in the wrong order.
		{`{"k":"\uDC00\uD800"}`, `unpaired low surrogate \uDC00 at position 6`, ""},
		// Two high surrogates in a row.
		{`{"k":"\uD800\uD800"}`, `unpaired high surrogate \uD800 at position 6`, ""},
		// A high surrogate followed by a non-surrogate escape.
		{`{"k":"\uD800A"}`, `unpaired high surrogate \uD800 at position 6`, ""},
		{`{"k":"\uD800\u0041"}`, `unpaired high surrogate \uD800 at position 6`, ""},
		// Malformed \u escape sequences. ValidateStrict rejects them
		// during the general escape sequences validation.
		{`{"k":"\u12"}`, `too short \u escape sequence at position 6`, "-"},
		{`{"k":"\uZZZZ"}`, `invalid escape sequence \uZZZZ at position 6`, "-"},
	}
	for _, tt := range tests {
		var p Parser
		if _, err := p.ParseStrict(tt.s); err == nil {
			t.Errorf("ParseStrict(%q): expecting error", tt.s)
		} else if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("ParseStrict(%q): error %q doesn't contain %q", tt.s, err, tt.want)
		}
		if _, err := ParseStrict(tt.s); err == nil {
			t.Errorf("ParseStrict(%q): expecting error", tt.s)
		}
		if _, err := ParseBytesStrict([]byte(tt.s)); err == nil {
			t.Errorf("ParseBytesStrict(%q): expecting error", tt.s)
		}
		wantValidate := tt.wantValidate
		if wantValidate == "" {
			wantValidate = tt.want
		}
		if err := ValidateStrict(tt.s); err == nil {
			t.Errorf("ValidateStrict(%q): expecting error", tt.s)
		} else if wantValidate != "-" && !strings.Contains(err.Error(), wantValidate) {
			t.Errorf("ValidateStrict(%q): error %q doesn't contain %q", tt.s, err, wantValidate)
		}
		if err := ValidateBytesStrict([]byte(tt.s)); err == nil {
			t.Errorf("ValidateBytesStrict(%q): expecting error", tt.s)
		}
	}
}

func TestParseStrictValidUnicode(t *testing.T) {
	tests := []string{
		// An escaped backslash cannot start a \u escape sequence.
		`{"k":"\\uD800"}`,
		// Valid supplementary plane chars, raw and escaped.
		`{"k":"😀"}`,
		`{"k":"\uD83D\uDE00"}`,
		`{"\uD83D\uDE00":"😀"}`,
		// Valid BMP chars and escape sequences.
		`{"k":"é"}`,
		`{"k":"\n\t\"\\/"}`,
		// U+FFFD is a valid char when properly encoded.
		`{"k":"\uFFFD"}`,
		`{"k":"�"}`,
	}
	for _, tt := range tests {
		var p Parser
		if _, err := p.ParseStrict(tt); err != nil {
			t.Errorf("ParseStrict(%q): unexpected error: %s", tt, err)
		}
		if err := ValidateStrict(tt); err != nil {
			t.Errorf("ValidateStrict(%q): unexpected error: %s", tt, err)
		}
	}
}

func TestParseStrictGetMarshal(t *testing.T) {
	// Get and MarshalTo must not change the meaning of chars
	// after a successful strict parse.
	var p Parser
	v, err := p.ParseStrict(`{"emoji":"😀","raw":"😀","esc":"\\uD800"}`)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if s := string(v.GetStringBytes("emoji")); s != "😀" {
		t.Fatalf("unexpected emoji value: %q", s)
	}
	if s := string(v.GetStringBytes("raw")); s != "😀" {
		t.Fatalf("unexpected raw value: %q", s)
	}
	if s := string(v.GetStringBytes("esc")); s != `\uD800` {
		t.Fatalf("unexpected esc value: %q", s)
	}
	expected := `{"emoji":"😀","raw":"😀","esc":"\\uD800"}`
	if s := string(v.MarshalTo(nil)); s != expected {
		t.Fatalf("unexpected marshaled value: %q; expecting %q", s, expected)
	}

	// The same must hold for escaped object keys.
	v, err = p.ParseStrict(`{"\uD83D\uDE00":1}`)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if !v.Exists("😀") {
		t.Fatalf("cannot find the %q key", "😀")
	}
	if s := string(v.MarshalTo(nil)); s != `{"😀":1}` {
		t.Fatalf("unexpected marshaled value: %q", s)
	}
}

func TestParseNonStrictInvalidUnicode(t *testing.T) {
	// The non-strict mode must keep the best-effort behavior.
	tests := []string{
		"{\"k\":\"\xff\"}",
		"{\"\xff\":1}",
		`{"k":"\uD800"}`,
		`{"k":"\uDC00"}`,
		`{"k":"\uDC00\uD800"}`,
		`{"k":"\u12"}`,
		"\"\xc3\"",
	}
	for _, tt := range tests {
		var p Parser
		if _, err := p.Parse(tt); err != nil {
			t.Errorf("Parse(%q): unexpected error: %s", tt, err)
		}
	}

	// Validate already rejects malformed \u escape sequences in
	// non-strict mode, but it must accept everything else.
	validateTests := []string{
		"{\"k\":\"\xff\"}",
		"{\"\xff\":1}",
		`{"k":"\uD800"}`,
		`{"k":"\uDC00"}`,
		`{"k":"\uDC00\uD800"}`,
		"\"\xc3\"",
	}
	for _, tt := range validateTests {
		if err := Validate(tt); err != nil {
			t.Errorf("Validate(%q): unexpected error: %s", tt, err)
		}
	}
}
