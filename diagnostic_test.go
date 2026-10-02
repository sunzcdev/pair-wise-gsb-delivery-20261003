package httphead

import (
	"fmt"
	"testing"
)

var scanOptionsDiagCases = []struct {
	label  string
	in     []byte
	status Status
	kind   ErrorKind
	offset int
	index  int
	param  int
}{
	{
		label:  "ok",
		in:     []byte(`foo;bar=1,baz`),
		status: StatusOK,
		index:  1,
		param:  -1,
	},
	{
		label:  "unclosed_quote",
		in:     []byte(`a;b="foo`),
		status: StatusError,
		kind:   ErrUnclosedQuote,
		offset: 4,
		index:  0,
		param:  0,
	},
	{
		label:  "unclosed_quote_raw_offset",
		in:     []byte(`a;b="\"",c;d="x`),
		status: StatusError,
		kind:   ErrUnclosedQuote,
		offset: 13,
		index:  1,
		param:  0,
	},
	{
		label:  "unclosed_comment",
		in:     []byte(`a;(hello`),
		status: StatusError,
		kind:   ErrUnclosedComment,
		offset: 2,
		index:  0,
		param:  -1,
	},
	{
		label:  "unclosed_nested_comment",
		in:     []byte(`a;(x(y`),
		status: StatusError,
		kind:   ErrUnclosedComment,
		offset: 2,
		index:  0,
		param:  -1,
	},
	{
		label:  "nested_comment_unexpected",
		in:     []byte(`a;(x(y)z)`),
		status: StatusError,
		kind:   ErrUnexpectedToken,
		offset: 2,
		index:  0,
		param:  -1,
	},
	{
		label:  "missing_value_eof",
		in:     []byte(`a;b=`),
		status: StatusError,
		kind:   ErrMissingValue,
		offset: 4,
		index:  0,
		param:  0,
	},
	{
		label:  "missing_value_comma",
		in:     []byte(`a;b=,c`),
		status: StatusError,
		kind:   ErrMissingValue,
		offset: 4,
		index:  0,
		param:  0,
	},
	{
		label:  "missing_value_raw_offset",
		in:     []byte(`a;b="\"",c;d=`),
		status: StatusError,
		kind:   ErrMissingValue,
		offset: 13,
		index:  1,
		param:  0,
	},
	{
		label:  "bad_token_after_comma",
		in:     []byte(`a,;b`),
		status: StatusError,
		kind:   ErrUnexpectedToken,
		offset: 2,
		index:  1,
		param:  -1,
	},
	{
		label:  "bad_token_after_comma_eq",
		in:     []byte(`a,=b`),
		status: StatusError,
		kind:   ErrUnexpectedToken,
		offset: 2,
		index:  1,
		param:  -1,
	},
	{
		label:  "empty",
		in:     []byte(``),
		status: StatusError,
		kind:   ErrUnexpectedEOF,
		offset: 0,
		index:  0,
		param:  -1,
	},
}

func TestScanOptionsDiag(t *testing.T) {
	for _, test := range scanOptionsDiagCases {
		t.Run(test.label, func(t *testing.T) {
			d := ScanOptionsDiag(test.in, func(int, []byte, []byte, []byte) Control {
				return ControlContinue
			})
			exp := Diagnostic{
				Status: test.status,
				Kind:   test.kind,
				Offset: test.offset,
				Index:  test.index,
				Param:  test.param,
			}
			if d.Status == StatusOK {
				// For successful scans only the status is fixed;
				// index/param describe the last scanned option.
				exp.Offset = d.Offset
			}
			if d != exp {
				t.Errorf("ScanOptionsDiag(%q) = %v; want %v", test.in, d, exp)
			}
		})
	}
}

func TestScanOptionsDiagBreak(t *testing.T) {
	var n int
	d := ScanOptionsDiag([]byte(`foo;bar=1,baz`), func(index int, key, param, value []byte) Control {
		n++
		return ControlBreak
	})
	if d.Status != StatusBreak {
		t.Errorf("unexpected status: %v; want %v", d.Status, StatusBreak)
	}
	if !d.OK() {
		t.Errorf("break must be reported as success")
	}
	if n != 1 {
		t.Errorf("callback called %d times; want 1", n)
	}
}

func TestScanOptionsDiagCompat(t *testing.T) {
	// The boolean API must keep its historical behavior on every input,
	// including the ones the diagnostic API is strict about.
	for _, test := range []struct {
		in []byte
		ok bool
	}{
		{[]byte(`a;b=`), true}, // missing value at EOF is tolerated by bool API
		{[]byte(`a;b=,c`), false},
		{[]byte(`a;b="foo`), false},
		{[]byte(`a;(x(y`), false},
		{[]byte(`a,;b`), false},
		{[]byte(`foo;bar=1,baz`), true},
		{[]byte(``), false},
	} {
		ok := ScanOptions(test.in, func(int, []byte, []byte, []byte) Control {
			return ControlContinue
		})
		if ok != test.ok {
			t.Errorf("ScanOptions(%q) = %v; want %v", test.in, ok, test.ok)
		}
	}

	// On wellformed input both APIs must agree and report the same tuples.
	for _, test := range parametersCases {
		var act []tuple
		d := ScanOptionsDiag(test.in, func(index int, key, param, value []byte) Control {
			act = append(act, tuple{index, key, param, value})
			return ControlContinue
		})
		if d.OK() != test.ok {
			t.Errorf("ScanOptionsDiag(%q).OK() = %v; want %v", test.in, d.OK(), test.ok)
		}
		if len(act) != len(test.exp) {
			t.Errorf("ScanOptionsDiag(%q) scanned %d tuples; want %d", test.in, len(act), len(test.exp))
		}
	}
}

func TestParseOptionsDiag(t *testing.T) {
	data := []byte(`foo;bar=1,baz`)
	exp, expOK := ParseOptions(data, nil)

	act, d := ParseOptionsDiag(data, nil)
	if d.Status != StatusOK {
		t.Fatalf("unexpected status: %v; want %v", d.Status, StatusOK)
	}
	if d.OK() != expOK {
		t.Errorf("ParseOptionsDiag().OK() = %v; want %v", d.OK(), expOK)
	}
	if !optionsEqual(act, exp) {
		t.Errorf("ParseOptionsDiag() = %v; want %v", act, exp)
	}

	if _, d := ParseOptionsDiag([]byte(`foo;bar="1`), nil); d.Status != StatusError ||
		d.Kind != ErrUnclosedQuote || d.Offset != 8 {
		t.Errorf("unexpected diagnostic: %v", d)
	}
}

func TestDiagnosticOffsetsAreRawBytes(t *testing.T) {
	// The unquoted value of the first parameter is shorter than its raw
	// representation; offsets must still point into the raw input.
	in := []byte(`a;b="\"",c;d="x`)
	d := ScanOptionsDiag(in, func(int, []byte, []byte, []byte) Control {
		return ControlContinue
	})
	if d.Status != StatusError || d.Kind != ErrUnclosedQuote {
		t.Fatalf("unexpected diagnostic: %v", d)
	}
	if d.Offset != 13 || in[d.Offset] != '"' {
		t.Errorf("offset = %d; want 13 (offset of the opening quote in raw input)", d.Offset)
	}
}

func TestScanOptionsDiagNoAllocs(t *testing.T) {
	data := []byte(`foo;a=1;b="2",bar,baz;x`)
	allocs := testing.AllocsPerRun(100, func() {
		ScanOptionsDiag(data, func(int, []byte, []byte, []byte) Control {
			return ControlContinue
		})
	})
	if allocs != 0 {
		t.Errorf("ScanOptionsDiag allocated %v times per run; want 0", allocs)
	}
}

func ExampleScanOptionsDiag() {
	d := ScanOptionsDiag([]byte(`foo;bar="1,baz`), func(int, []byte, []byte, []byte) Control {
		return ControlContinue
	})
	fmt.Println(d)
	// Output: unclosed quote at byte 8 (option 0, param 0)
}
