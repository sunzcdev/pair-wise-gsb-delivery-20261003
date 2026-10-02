package httphead

import (
	"bytes"
	"testing"
)

func TestScanOptionsDetailed(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		status ParseStatus
		kind   OptionErrorKind
		offset int
		option int
		param  int
	}{
		{
			name:   "complete",
			in:     `a;b=1,c;d=2`,
			status: ParseComplete,
		},
		{
			name:   "empty_input",
			in:     ``,
			status: ParseComplete,
		},
		{
			name:   "empty_elements",
			in:     `a,, , ,b`,
			status: ParseComplete,
		},
		{
			name:   "unterminated_string",
			in:     `a;b="unterminated`,
			status: ParseMalformed,
			kind:   OptionErrorUnterminatedString,
			offset: 4,
			option: 0,
			param:  1,
		},
		{
			// The offset must be the raw position of the opening quote even
			// when the quoted content contains escape sequences; it must not
			// be derived from the de-quoted value length.
			name:   "unterminated_string_with_escapes",
			in:     `a;x=1,b;q="w\"zzz`,
			status: ParseMalformed,
			kind:   OptionErrorUnterminatedString,
			offset: 10,
			option: 1,
			param:  1,
		},
		{
			name:   "unterminated_nested_comment",
			in:     `a;b=(nest(ed)`,
			status: ParseMalformed,
			kind:   OptionErrorUnterminatedComment,
			offset: 4,
			option: 0,
			param:  1,
		},
		{
			name:   "missing_value_eof",
			in:     `a;b=`,
			status: ParseMalformed,
			kind:   OptionErrorMissingValue,
			offset: 4,
			option: 0,
			param:  1,
		},
		{
			name:   "missing_value_eof_whitespace",
			in:     `a;b=   `,
			status: ParseMalformed,
			kind:   OptionErrorMissingValue,
			offset: 4,
			option: 0,
			param:  1,
		},
		{
			name:   "missing_value_eof_second_option",
			in:     `a;x=1,b;y=`,
			status: ParseMalformed,
			kind:   OptionErrorMissingValue,
			offset: 10,
			option: 1,
			param:  1,
		},
		{
			name:   "unterminated_string_in_key_position",
			in:     `a;x=1,b;"`,
			status: ParseMalformed,
			kind:   OptionErrorUnterminatedString,
			offset: 8,
			option: 1,
			param:  0,
		},
		{
			name:   "missing_value_before_comma",
			in:     `a;b=1,c;d=,`,
			status: ParseMalformed,
			kind:   OptionErrorMissingValue,
			offset: 10,
			option: 1,
			param:  1,
		},
		{
			name:   "bad_token_equal_after_comma",
			in:     `a,=b`,
			status: ParseMalformed,
			kind:   OptionErrorBadToken,
			offset: 2,
			option: 1,
			param:  0,
		},
		{
			name:   "bad_token_after_comma",
			in:     `a,==`,
			status: ParseMalformed,
			kind:   OptionErrorBadToken,
			offset: 2,
			option: 1,
			param:  0,
		},
		{
			name:   "bad_token_leading_separator",
			in:     `=a`,
			status: ParseMalformed,
			kind:   OptionErrorBadToken,
			offset: 0,
			option: 0,
			param:  0,
		},
		{
			name:   "bad_token_double_semicolon",
			in:     `a;;`,
			status: ParseMalformed,
			kind:   OptionErrorBadToken,
			offset: 2,
			option: 0,
			param:  0,
		},
		{
			name:   "closed_comment_is_bad_token",
			in:     `a;b=(n(est))`,
			status: ParseMalformed,
			kind:   OptionErrorBadToken,
			offset: 4,
			option: 0,
			param:  1,
		},
		{
			name:   "invalid_byte",
			in:     "a;b=\x7f",
			status: ParseMalformed,
			kind:   OptionErrorInvalidByte,
			offset: 4,
			option: 0,
			param:  1,
		},
		{
			name:   "invalid_byte_after_comma",
			in:     "a;x=1,\\",
			status: ParseMalformed,
			kind:   OptionErrorInvalidByte,
			offset: 6,
			option: 1,
			param:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, err := ScanOptionsDetailed([]byte(tc.in), func(int, []byte, []byte, []byte) Control {
				return ControlContinue
			})
			if status != tc.status {
				t.Fatalf("status = %v; want %v (err: %+v)", status, tc.status, err)
			}
			if status != ParseMalformed {
				return
			}
			if err.Kind != tc.kind {
				t.Errorf("Kind = %v; want %v", err.Kind, tc.kind)
			}
			if err.Offset != tc.offset {
				t.Errorf("Offset = %d; want %d", err.Offset, tc.offset)
			}
			if err.Option != tc.option {
				t.Errorf("Option = %d; want %d", err.Option, tc.option)
			}
			if err.Param != tc.param {
				t.Errorf("Param = %d; want %d", err.Param, tc.param)
			}
			if err.Error() == "" {
				t.Errorf("Error() returned empty string")
			}
			if tc.offset >= 0 && tc.offset > len(tc.in) {
				t.Errorf("offset %d out of range for input of length %d", tc.offset, len(tc.in))
			}
		})
	}
}

func TestScanOptionsDetailedStopped(t *testing.T) {
	var seen [][]byte
	status, err := ScanOptionsDetailed([]byte(`a;b=1,c;d=2,e`), func(_ int, key, attr, val []byte) Control {
		seen = append(seen, key)
		if bytes.Equal(key, []byte(`c`)) {
			return ControlBreak
		}
		return ControlContinue
	})
	if status != ParseStopped {
		t.Fatalf("status = %v; want ParseStopped (err: %+v)", status, err)
	}
	if err != (OptionError{}) {
		t.Fatalf("err = %+v; want zero value", err)
	}
	if n := len(seen); n != 2 {
		t.Fatalf("callback received %d options before stop; want 2", n)
	}
}

func TestParseOptionsDetailed(t *testing.T) {
	// Successful input produces the same options as ParseOptions.
	in := []byte(`foo;a=1;b=2,bar;z,baz`)

	want, ok := ParseOptions(in, nil)
	if !ok {
		t.Fatalf("ParseOptions ok = false")
	}

	got, status, err := ParseOptionsDetailed(in, nil)
	if status != ParseComplete {
		t.Fatalf("status = %v; want ParseComplete (err: %+v)", status, err)
	}
	if !optionsEqual(got, want) {
		t.Fatalf("options = %v; want %v", got, want)
	}

	// Malformed input still returns the options parsed before the error.
	partial, st, derr := ParseOptionsDetailed([]byte(`a;x=1,b;y="bad`), nil)
	if st != ParseMalformed || derr.Kind != OptionErrorUnterminatedString {
		t.Fatalf("status=%v err=%+v; want malformed unterminated string", st, derr)
	}
	if len(partial) != 1 || !bytes.Equal(partial[0].Name, []byte(`a`)) {
		t.Fatalf("partial options = %v; want just [a]", partial)
	}
}

// TestDetailedAndBoolAgreement ensures the legacy bool API and the detailed
// API agree on all wellformed inputs in the existing test table.
func TestDetailedAndBoolAgreement(t *testing.T) {
	for _, tc := range parametersCases {
		if !tc.ok {
			continue
		}
		t.Run(tc.label, func(t *testing.T) {
			legacy := ScanOptions(tc.in, func(int, []byte, []byte, []byte) Control {
				return ControlContinue
			})
			status, err := ScanOptionsDetailed(tc.in, func(int, []byte, []byte, []byte) Control {
				return ControlContinue
			})
			if !legacy {
				t.Fatalf("bool API rejected wellformed input %q", tc.in)
			}
			if status != ParseComplete {
				t.Fatalf("detailed status = %v (err %+v); want ParseComplete for %q", status, err, tc.in)
			}
		})
	}
}

func TestLegacyMissingValueCompatibility(t *testing.T) {
	// Both APIs reject a value cut off by a comma.
	badComma := []byte(`a;b=,c`)
	if ok := ScanOptions(badComma, func(int, []byte, []byte, []byte) Control {
		return ControlContinue
	}); ok {
		t.Fatalf("legacy ScanOptions(%q) = true; want false", badComma)
	}
	if st, err := ScanOptionsDetailed(badComma, func(int, []byte, []byte, []byte) Control {
		return ControlContinue
	}); st != ParseMalformed || err.Kind != OptionErrorMissingValue {
		t.Fatalf("detailed status=%v err=%+v; want malformed missing value", st, err)
	}

	// The legacy bool API historically tolerates a dangling "=" at the very
	// end of input; the detailed API instead reports the missing value.
	dangling := []byte(`a;b=`)
	if ok := ScanOptions(dangling, func(int, []byte, []byte, []byte) Control {
		return ControlContinue
	}); !ok {
		t.Fatalf("legacy ScanOptions(%q) = false; want true for compatibility", dangling)
	}
	if st, err := ScanOptionsDetailed(dangling, func(int, []byte, []byte, []byte) Control {
		return ControlContinue
	}); st != ParseMalformed || err.Kind != OptionErrorMissingValue {
		t.Fatalf("detailed status=%v err=%+v; want malformed missing value", st, err)
	}
}

func BenchmarkScanOptionsDetailed(b *testing.B) {
	for _, bench := range parametersCases {
		b.Run(bench.label, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = ScanOptionsDetailed(bench.in, func(_ int, _, _, _ []byte) Control {
					return ControlContinue
				})
			}
		})
	}
}
