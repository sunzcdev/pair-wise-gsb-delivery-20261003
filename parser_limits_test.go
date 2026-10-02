package fastjson

import (
	"errors"
	"strings"
	"testing"
)

func checkLimitError(t *testing.T, err error, wantLimit string, wantOffset int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expecting non-nil error")
	}
	var le *LimitError
	if !errors.As(err, &le) {
		t.Fatalf("expecting LimitError; got %T: %s", err, err)
	}
	if le.Limit != wantLimit {
		t.Fatalf("unexpected limit kind; got %q; want %q; error: %s", le.Limit, wantLimit, err)
	}
	if le.Offset != wantOffset {
		t.Fatalf("unexpected offset; got %d; want %d; error: %s", le.Offset, wantOffset, err)
	}
	if !strings.Contains(err.Error(), wantLimit) {
		t.Fatalf("error message must contain the limit kind %q; got %q", wantLimit, err.Error())
	}
}

func TestParserDefaultLimitsUnchanged(t *testing.T) {
	var p Parser

	// The default depth limit must stay MaxDepth.
	deep := strings.Repeat("[", MaxDepth) + strings.Repeat("]", MaxDepth)
	if _, err := p.Parse(deep); err != nil {
		t.Fatalf("unexpected error for depth %d: %s", MaxDepth, err)
	}
	deeper := strings.Repeat("[", MaxDepth+1) + strings.Repeat("]", MaxDepth+1)
	_, err := p.Parse(deeper)
	checkLimitError(t, err, LimitDepth, MaxDepth)

	// Node and string counts are unlimited by default.
	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; i < 10000; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`"some reasonably long string value"`)
	}
	sb.WriteByte(']')
	if _, err := p.Parse(sb.String()); err != nil {
		t.Fatalf("unexpected error without limits: %s", err)
	}
}

func TestParserMaxDepth(t *testing.T) {
	var p Parser
	p.MaxDepth = 3

	// Exactly at the limit.
	if _, err := p.Parse(`[[[]]]`); err != nil {
		t.Fatalf("unexpected error at the exact depth limit: %s", err)
	}
	// One level over the limit.
	_, err := p.Parse(`[[[[]]]]`)
	checkLimitError(t, err, LimitDepth, 3)

	// The offset must account for leading whitespace.
	p.MaxDepth = 1
	_, err = p.Parse("  [[]]")
	checkLimitError(t, err, LimitDepth, 3)

	// Negative MaxDepth means unlimited depth.
	p.MaxDepth = -1
	deep := strings.Repeat("[", MaxDepth+100) + strings.Repeat("]", MaxDepth+100)
	if _, err := p.Parse(deep); err != nil {
		t.Fatalf("unexpected error with unlimited depth: %s", err)
	}
}

func TestParserMaxNodes(t *testing.T) {
	var p Parser

	// {"a":[1,2]} contains 4 nodes: object, array and two numbers.
	p.MaxNodes = 4
	v, err := p.Parse(`{"a":[1,2]}`)
	if err != nil {
		t.Fatalf("unexpected error at the exact nodes limit: %s", err)
	}
	if v.GetInt("a", "1") != 2 {
		t.Fatalf("unexpected value: %s", v)
	}

	// One node over the limit.
	p.MaxNodes = 3
	_, err = p.Parse(`{"a":[1,2]}`)
	checkLimitError(t, err, LimitNodes, 8)

	// Empty containers count as nodes.
	p.MaxNodes = 2
	if _, err := p.Parse(`[{}]`); err != nil {
		t.Fatalf("unexpected error for empty containers: %s", err)
	}
	p.MaxNodes = 1
	_, err = p.Parse(`[{}]`)
	checkLimitError(t, err, LimitNodes, 1)

	// The offset must point at the value exceeding the limit.
	p.MaxNodes = 2
	_, err = p.Parse(`[1,2]`)
	checkLimitError(t, err, LimitNodes, 3)
}

func TestParserMaxStringBytes(t *testing.T) {
	var p Parser

	// Object keys count towards the budget: "ab" key + "cd" value = 4 bytes.
	p.MaxStringBytes = 4
	if _, err := p.Parse(`{"ab":"cd"}`); err != nil {
		t.Fatalf("unexpected error at the exact string bytes limit: %s", err)
	}
	p.MaxStringBytes = 3
	_, err := p.Parse(`{"ab":"cd"}`)
	checkLimitError(t, err, LimitStringBytes, 6)

	// Escape sequences are counted after decoding.
	p.MaxStringBytes = 1
	for _, s := range []string{`"\n"`, `""`, "\"\x5cu0041\""} {
		if _, err := p.Parse(s); err != nil {
			t.Fatalf("unexpected error for %s: %s", s, err)
		}
	}

	// "AB" decodes to 2 bytes; its raw length is 12.
	p.MaxStringBytes = 2
	if _, err := p.Parse("\"\x5cu0041\x5cu0042\""); err != nil {
		t.Fatalf("unexpected error for decoded \\u sequences: %s", err)
	}
	p.MaxStringBytes = 1
	_, err = p.Parse("\"\x5cu0041\x5cu0042\"")
	checkLimitError(t, err, LimitStringBytes, 0)

	// A surrogate pair decodes to a single 4-byte rune.
	p.MaxStringBytes = 4
	if _, err := p.Parse(`"\uD83D\uDE00"`); err != nil {
		t.Fatalf("unexpected error for surrogate pair: %s", err)
	}
	p.MaxStringBytes = 3
	_, err = p.Parse(`"\uD83D\uDE00"`)
	checkLimitError(t, err, LimitStringBytes, 0)

	// Empty containers don't consume the string budget.
	p.MaxStringBytes = 1
	if _, err := p.Parse(`{"a":[{},[]]}`); err != nil {
		t.Fatalf("unexpected error for empty containers: %s", err)
	}

	// The budget accumulates across strings; the offset points
	// at the string exceeding the budget.
	p.MaxStringBytes = 3
	_, err = p.Parse(`["ab","cd"]`)
	checkLimitError(t, err, LimitStringBytes, 6)

	// The offset must account for leading whitespace.
	p.MaxStringBytes = 1
	_, err = p.Parse("\t " + `"ab"`)
	checkLimitError(t, err, LimitStringBytes, 2)
}

func TestParserLimitsReuseAfterFailure(t *testing.T) {
	var p Parser
	p.MaxDepth = 3
	p.MaxNodes = 3
	p.MaxStringBytes = 4

	// Fail on each of the limits, then successfully parse a valid input
	// with the same Parser.
	failing := []string{
		`[[[[]]]]`,     // depth
		`[1,2,3,4]`,    // nodes
		`["abcde"]`,    // string bytes
		`{"abcde": 1}`, // string bytes in key
	}
	for _, s := range failing {
		if _, err := p.Parse(s); err == nil {
			t.Fatalf("expecting error for %q", s)
		}
		v, err := p.Parse(`{"a":[1]}`)
		if err != nil {
			t.Fatalf("the parser must not carry over counts after a failure; got error for valid input: %s", err)
		}
		if v.GetInt("a", "0") != 1 {
			t.Fatalf("unexpected value after recovery: %s", v)
		}
	}

	// Counts must not accumulate across successful parses either.
	for i := 0; i < 3; i++ {
		if _, err := p.Parse(`{"ab":1}`); err != nil {
			t.Fatalf("unexpected error on repeated parse: %s", err)
		}
	}
}

func TestUnescapedLen(t *testing.T) {
	inputs := []string{
		``,
		`foobar`,
		`\n\t\r\b\f\/\\\"`,
		`A`,
		"\x5cu00e9",
		"\x5cuD83D\x5cuDE00",
		`\`,
		`\z`,
		`\u`,
		`\u12`,
		`\uZZZZ`,
		`\uD83D`,
		`\uD83D\u0041`,
		`\uD83Dxxxx`,
		`foo\nbar`,
		`\\`,
	}
	for _, s := range inputs {
		got := unescapedLen(s)
		// unescapeStringBestEffort unescapes the string in-place,
		// so pass it a writable copy.
		want := len(unescapeStringBestEffort(b2s([]byte(s))))
		if got != want {
			t.Fatalf("unexpected unescapedLen(%q); got %d; want %d", s, got, want)
		}
	}
}
