package fastjson

import (
	"errors"
	"strings"
	"testing"
)

func TestParserLimitsDefaultBehavior(t *testing.T) {
	t.Parallel()

	var p Parser
	if limits := p.Limits(); limits != (ParseLimits{}) {
		t.Fatalf("unexpected default limits: %+v", limits)
	}

	// The default depth limit must remain MaxDepth.
	s := strings.Repeat("[", MaxDepth) + strings.Repeat("]", MaxDepth)
	if _, err := p.Parse(s); err != nil {
		t.Fatalf("unexpected error for depth %d: %s", MaxDepth, err)
	}
	s = strings.Repeat("[", MaxDepth+1) + strings.Repeat("]", MaxDepth+1)
	_, err := p.Parse(s)
	if err == nil {
		t.Fatalf("expecting error for depth %d", MaxDepth+1)
	}
	if !strings.Contains(err.Error(), "too big depth for the nested JSON") {
		t.Fatalf("unexpected depth error: %s", err)
	}
	var le *LimitError
	if errors.As(err, &le) {
		t.Fatalf("default depth error must not be a LimitError: %s", err)
	}

	// Nodes and string bytes are unlimited by default.
	var sb strings.Builder
	sb.WriteString(`["`)
	sb.WriteString(strings.Repeat("a", 100000))
	sb.WriteString(`"`)
	for i := 0; i < 100000; i++ {
		sb.WriteString(`,1`)
	}
	sb.WriteString(`]`)
	if _, err := p.Parse(sb.String()); err != nil {
		t.Fatalf("unexpected error for unlimited parser: %s", err)
	}
}

func TestParserLimitDepth(t *testing.T) {
	t.Parallel()

	var p Parser
	p.SetLimits(ParseLimits{MaxDepth: 2})

	// Every value, including scalars, counts as a nesting level.
	if _, err := p.Parse(`[[],{}]`); err != nil {
		t.Fatalf("unexpected error at the depth limit: %s", err)
	}

	_, err := p.Parse(`[[1]]`)
	if err == nil {
		t.Fatalf("expecting depth limit error")
	}
	le := mustLimitError(t, err)
	if le.Kind != LimitKindDepth {
		t.Fatalf("unexpected limit kind: %s", le.Kind)
	}
	if le.Limit != 2 {
		t.Fatalf("unexpected limit: %d", le.Limit)
	}
	if le.Position != 2 {
		t.Fatalf("unexpected position: %d; expecting 2", le.Position)
	}
}

func TestParserLimitNodes(t *testing.T) {
	t.Parallel()

	var p Parser
	p.SetLimits(ParseLimits{MaxNodes: 3})

	// Exactly at the limit: array + two numbers.
	if _, err := p.Parse(`[1,2]`); err != nil {
		t.Fatalf("unexpected error at the nodes limit: %s", err)
	}

	// One node over the limit.
	_, err := p.Parse(`[1,2,3]`)
	if err == nil {
		t.Fatalf("expecting nodes limit error")
	}
	le := mustLimitError(t, err)
	if le.Kind != LimitKindNodes {
		t.Fatalf("unexpected limit kind: %s", le.Kind)
	}
	if le.Limit != 3 {
		t.Fatalf("unexpected limit: %d", le.Limit)
	}
	if le.Position != 5 {
		t.Fatalf("unexpected position: %d; expecting 5", le.Position)
	}

	// Object keys are not nodes.
	p.SetLimits(ParseLimits{MaxNodes: 2})
	if _, err := p.Parse(`{"a":1}`); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	// Empty containers count as single nodes.
	p.SetLimits(ParseLimits{MaxNodes: 1})
	if _, err := p.Parse(`{}`); err != nil {
		t.Fatalf("unexpected error for empty object: %s", err)
	}
	if _, err := p.Parse(`[]`); err != nil {
		t.Fatalf("unexpected error for empty array: %s", err)
	}
	if _, err := p.Parse(`[[]]`); err == nil {
		t.Fatalf("expecting nodes limit error for nested empty arrays")
	}

	// The position is reported in the original input, including
	// the leading whitespace.
	p.SetLimits(ParseLimits{MaxNodes: 3})
	_, err = p.Parse("  [1,2,3]")
	le = mustLimitError(t, err)
	if le.Position != 7 {
		t.Fatalf("unexpected position: %d; expecting 7", le.Position)
	}
}

func TestParserLimitStringBytes(t *testing.T) {
	t.Parallel()

	var p Parser
	p.SetLimits(ParseLimits{MaxStringBytes: 3})

	// Exactly at the limit.
	if _, err := p.Parse(`"abc"`); err != nil {
		t.Fatalf("unexpected error at the string bytes limit: %s", err)
	}

	// One byte over the limit.
	_, err := p.Parse(`"abcd"`)
	if err == nil {
		t.Fatalf("expecting string bytes limit error")
	}
	le := mustLimitError(t, err)
	if le.Kind != LimitKindStringBytes {
		t.Fatalf("unexpected limit kind: %s", le.Kind)
	}
	if le.Limit != 3 {
		t.Fatalf("unexpected limit: %d", le.Limit)
	}
	if le.Position != 0 {
		t.Fatalf("unexpected position: %d; expecting 0", le.Position)
	}

	// Object keys count towards the budget.
	p.SetLimits(ParseLimits{MaxStringBytes: 2})
	if _, err := p.Parse(`{"ab":1}`); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	_, err = p.Parse(`{"abc":1}`)
	le = mustLimitError(t, err)
	if le.Kind != LimitKindStringBytes {
		t.Fatalf("unexpected limit kind: %s", le.Kind)
	}
	if le.Position != 1 {
		t.Fatalf("unexpected position: %d; expecting 1", le.Position)
	}

	// Keys and values share the same budget.
	p.SetLimits(ParseLimits{MaxStringBytes: 4})
	if _, err := p.Parse(`{"ab":"cd"}`); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if _, err := p.Parse(`{"ab":"cde"}`); err == nil {
		t.Fatalf("expecting string bytes limit error")
	}
}

func TestParserLimitStringBytesEscaped(t *testing.T) {
	t.Parallel()

	var p Parser

	// Decoded (unescaped) bytes are counted, not raw bytes.
	p.SetLimits(ParseLimits{MaxStringBytes: 1})
	for _, s := range []string{`"\n"`, `""`, `"A"`, `"\\"`} {
		if _, err := p.Parse(s); err != nil {
			t.Fatalf("unexpected error for %s: %s", s, err)
		}
	}
	if _, err := p.Parse(`"ab"`); err == nil {
		t.Fatalf("expecting string bytes limit error")
	}

	// A surrogate pair decodes to a single 4-byte rune.
	p.SetLimits(ParseLimits{MaxStringBytes: 4})
	if _, err := p.Parse(`"\uD83D\uDE00"`); err != nil {
		t.Fatalf("unexpected error for surrogate pair: %s", err)
	}
	p.SetLimits(ParseLimits{MaxStringBytes: 3})
	if _, err := p.Parse(`"\uD83D\uDE00"`); err == nil {
		t.Fatalf("expecting string bytes limit error for surrogate pair")
	}

	// Escaped keys count decoded bytes too.
	p.SetLimits(ParseLimits{MaxStringBytes: 1})
	if _, err := p.Parse(`{"\n":1}`); err != nil {
		t.Fatalf("unexpected error for escaped key: %s", err)
	}
	if _, err := p.Parse(`{"\n\n":1}`); err == nil {
		t.Fatalf("expecting string bytes limit error for escaped key")
	}
}

func TestParserLimitsReuseAfterFailure(t *testing.T) {
	t.Parallel()

	var p Parser
	p.SetLimits(ParseLimits{MaxNodes: 2, MaxStringBytes: 3, MaxDepth: 2})

	// Fail on every limit kind in a row.
	for _, s := range []string{`[1,2,3]`, `"abcd"`, `[[[1]]]`} {
		if _, err := p.Parse(s); err == nil {
			t.Fatalf("expecting limit error for %s", s)
		}
	}

	// The parser must not carry the counters over to the next parse.
	v, err := p.Parse(`{"a":"bc"}`)
	if err != nil {
		t.Fatalf("unexpected error after limit failures: %s", err)
	}
	if string(v.GetStringBytes("a")) != "bc" {
		t.Fatalf("unexpected value: %s", v)
	}

	// The limits still apply after a successful parse.
	if _, err := p.Parse(`"abcd"`); err == nil {
		t.Fatalf("expecting string bytes limit error")
	}
	v, err = p.Parse(`"abc"`)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if string(v.GetStringBytes()) != "abc" {
		t.Fatalf("unexpected value: %s", v)
	}
}

func TestParserLimitsCombined(t *testing.T) {
	t.Parallel()

	var p Parser
	p.SetLimits(ParseLimits{MaxNodes: 4, MaxStringBytes: 6, MaxDepth: 3})

	// 4 nodes, 6 decoded string bytes, depth 2.
	if _, err := p.Parse(`{"ab":["cd",1]}`); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	// Each limit exceeded by one unit.
	if _, err := p.Parse(`{"ab":["cd",1,2]}`); err == nil {
		t.Fatalf("expecting nodes limit error")
	}
	if _, err := p.Parse(`{"ab":["cdefg",1]}`); err == nil {
		t.Fatalf("expecting string bytes limit error")
	}
	if _, err := p.Parse(`{"ab":[["cd",1]]}`); err == nil {
		t.Fatalf("expecting depth limit error")
	}
}

func TestDecodedStringLen(t *testing.T) {
	t.Parallel()

	ss := []string{
		"",
		"abc",
		`a\nb`,
		`\"\\\/\b\f\n\r\t`,
		`A`,
		`\uD83D\uDE00`,
		`\uD83D`,
		`\uD83Dx`,
		`\uD83D\uZZZZ`,
		`\uzzz`,
		`\u12`,
		`\q`,
		`foo\u0041bar\nbaz`,
		`trailing\`,
	}
	for _, s := range ss {
		// unescapeStringBestEffort unescapes the string in place,
		// so pass it a writable copy.
		want := len(unescapeStringBestEffort(string([]byte(s))))
		if got := decodedStringLen(s); got != want {
			t.Fatalf("unexpected decodedStringLen(%q): got %d; want %d", s, got, want)
		}
	}
}

func mustLimitError(t *testing.T, err error) *LimitError {
	t.Helper()
	if err == nil {
		t.Fatalf("expecting limit error")
	}
	var le *LimitError
	if !errors.As(err, &le) {
		t.Fatalf("expecting LimitError; got %T: %s", err, err)
	}
	return le
}
