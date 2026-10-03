package fastjson

import (
	"errors"
	"testing"
)

func parseWithPolicy(t *testing.T, s string, policy DuplicateKeyPolicy) *Value {
	t.Helper()
	p := &Parser{DuplicateKeys: policy}
	v, err := p.Parse(s)
	if err != nil {
		t.Fatalf("unexpected error for %q: %s", s, err)
	}
	return v
}

type pair struct {
	k string
	v string
}

func visitPairs(t *testing.T, o *Object) []pair {
	t.Helper()
	var pairs []pair
	o.Visit(func(k []byte, v *Value) {
		pairs = append(pairs, pair{string(k), v.String()})
	})
	return pairs
}

func TestDuplicateKeysKeepAllIsDefault(t *testing.T) {
	s := `{"name":"a","name":"b"}`

	// Zero-value Parser keeps every member: legacy behavior.
	p := &Parser{}
	v, err := p.Parse(s)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	o := v.GetObject()
	if o.Len() != 2 {
		t.Fatalf("Len: got %d, want 2", o.Len())
	}
	// Get returns the first member in legacy mode.
	if got := o.Get("name").String(); got != `"a"` {
		t.Fatalf("Get: got %s, want %q", got, `"a"`)
	}
	if got := v.String(); got != s {
		t.Fatalf("marshal: got %s, want %s", got, s)
	}
}

func TestDuplicateKeysReject(t *testing.T) {
	// "name" and "\u006eame" unescape to the same key.
	s := `{"name":"a","\u006eame":"b"}`

	p := &Parser{DuplicateKeys: DuplicateKeyReject}
	_, err := p.Parse(s)
	if err == nil {
		t.Fatalf("expecting duplicate key error")
	}
	var dke *DuplicateKeyError
	if !errors.As(err, &dke) {
		t.Fatalf("error is not *DuplicateKeyError: %T %s", err, err)
	}
	if dke.Key != "name" {
		t.Fatalf("Key: got %q, want %q", dke.Key, "name")
	}
	// First key opening quote at byte 1, second at byte 12.
	if dke.FirstOffset != 1 || dke.DuplicateOffset != 12 {
		t.Fatalf("offsets: got first=%d dup=%d, want 1 and 12",
			dke.FirstOffset, dke.DuplicateOffset)
	}
	if dke.DuplicateOffset <= dke.FirstOffset {
		t.Fatalf("duplicate offset must be after first offset")
	}
}

func TestDuplicateKeysRejectIndependentObjects(t *testing.T) {
	// Same key in two distinct objects must not conflict, even when nested.
	s := `{"name":{"name":"a"},"other":{"name":"b"},"arr":[{"name":"c"},{"name":"d"}]}`
	parseWithPolicy(t, s, DuplicateKeyReject)

	// Empty keys in distinct objects must not conflict either.
	parseWithPolicy(t, `{"":1,"a":{"":2},"b":{"":3}}`, DuplicateKeyReject)

	// But a duplicated empty key inside one object is rejected.
	p := &Parser{DuplicateKeys: DuplicateKeyReject}
	if _, err := p.Parse(`{"":1,"":2}`); err == nil {
		t.Fatalf("expecting duplicate empty key error")
	} else {
		var dke *DuplicateKeyError
		if !errors.As(err, &dke) || dke.Key != "" {
			t.Fatalf("unexpected error: %s", err)
		}
		if dke.FirstOffset != 1 || dke.DuplicateOffset != 6 {
			t.Fatalf("empty-key offsets: got %d and %d, want 1 and 6",
				dke.FirstOffset, dke.DuplicateOffset)
		}
	}
}

func TestDuplicateKeysDeepNestingNoLeak(t *testing.T) {
	// Build a deeply nested chain; every object reuses the same key.
	const depth = 100
	var input []byte
	for i := 0; i < depth; i++ {
		input = append(input, `{"k":`...)
	}
	input = append(input, `1`...)
	for i := 0; i < depth; i++ {
		input = append(input, '}')
	}
	v := parseWithPolicy(t, string(input), DuplicateKeyReject)

	cur := v
	for i := 0; i < depth; i++ {
		o, err := cur.Object()
		if err != nil {
			t.Fatalf("level %d: %s", i, err)
		}
		if o.Len() != 1 {
			t.Fatalf("level %d Len: got %d, want 1", i, o.Len())
		}
		cur = o.Get("k")
		if cur == nil {
			t.Fatalf("level %d: missing k", i)
		}
	}
	if cur.GetInt() != 1 {
		t.Fatalf("leaf: got %s, want 1", cur)
	}
}

func TestDuplicateKeysKeepFirst(t *testing.T) {
	// Escaped duplicate must be detected by unescaped name; order is the
	// position of the first occurrence.
	v := parseWithPolicy(t,
		`{"a":1,"\u006eame":"first","b":2,"name":"last"}`,
		DuplicateKeyKeepFirst)
	o := v.GetObject()

	if o.Len() != 3 {
		t.Fatalf("Len: got %d, want 3", o.Len())
	}
	pairs := visitPairs(t, o)
	want := []pair{
		{"a", "1"},
		{"name", `"first"`},
		{"b", "2"},
	}
	if len(pairs) != len(want) {
		t.Fatalf("Visit count: got %d, want %d", len(pairs), len(want))
	}
	for i := range want {
		if pairs[i] != want[i] {
			t.Fatalf("Visit[%d]: got %s=%s, want %s=%s",
				i, pairs[i].k, pairs[i].v, want[i].k, want[i].v)
		}
	}
	if got := o.Get("name").String(); got != `"first"` {
		t.Fatalf("Get: got %s, want %q", got, `"first"`)
	}
	if got := v.Get("name").String(); got != `"first"` {
		t.Fatalf("Value.Get: got %s, want %q", got, `"first"`)
	}
	// Visit unescapes keys in place, so marshal emits the unescaped name;
	// member count and order still agree across Get/Visit/Len/marshal.
	if got := v.String(); got != `{"a":1,"name":"first","b":2}` {
		t.Fatalf("marshal: got %s", got)
	}
}

func TestDuplicateKeysKeepLast(t *testing.T) {
	// The surviving member keeps the last occurrence's position.
	v := parseWithPolicy(t,
		`{"a":1,"\u006eame":"first","b":2,"name":"last","c":3}`,
		DuplicateKeyKeepLast)
	o := v.GetObject()

	if o.Len() != 4 {
		t.Fatalf("Len: got %d, want 4", o.Len())
	}
	pairs := visitPairs(t, o)
	want := []pair{
		{"a", "1"},
		{"b", "2"},
		{"name", `"last"`},
		{"c", "3"},
	}
	if len(pairs) != len(want) {
		t.Fatalf("Visit count: got %d, want %d", len(pairs), len(want))
	}
	for i := range want {
		if pairs[i] != want[i] {
			t.Fatalf("Visit[%d]: got %s=%s, want %s=%s",
				i, pairs[i].k, pairs[i].v, want[i].k, want[i].v)
		}
	}
	if got := o.Get("name").String(); got != `"last"` {
		t.Fatalf("Get: got %s, want %q", got, `"last"`)
	}
	if got := v.String(); got != `{"a":1,"b":2,"name":"last","c":3}` {
		t.Fatalf("marshal: got %s", got)
	}
}

func TestDuplicateKeysKeepEscapedRawKeyIntact(t *testing.T) {
	// Without calling Visit, the surviving key keeps its original escaped
	// spelling on the wire, proving duplicate detection unescaped a copy
	// and did not mutate the parsed backing buffer.
	v := parseWithPolicy(t,
		`{"a":1,"\u006eame":"first","name":"last"}`,
		DuplicateKeyKeepFirst)
	if got := v.String(); got != `{"a":1,"\u006eame":"first"}` {
		t.Fatalf("marshal before Visit: got %s", got)
	}
	if got := v.Get("name").String(); got != `"first"` {
		t.Fatalf("Get by unescaped name: got %s, want %q", got, `"first"`)
	}
}

func TestDuplicateKeysKeepModesThreeOccurrences(t *testing.T) {
	s := `{"k":"x","k":"y","k":"z"}`

	first := parseWithPolicy(t, s, DuplicateKeyKeepFirst).GetObject()
	if first.Len() != 1 || first.Get("k").String() != `"x"` {
		t.Fatalf("KeepFirst: len=%d value=%s", first.Len(), first.Get("k"))
	}

	last := parseWithPolicy(t, s, DuplicateKeyKeepLast).GetObject()
	if last.Len() != 1 || last.Get("k").String() != `"z"` {
		t.Fatalf("KeepLast: len=%d value=%s", last.Len(), last.Get("k"))
	}
}

func TestDuplicateKeysKeepModesConsistencyNested(t *testing.T) {
	// Duplicate inside a nested object is handled independently of the
	// outer object, which also reuses the same key.
	s := `{"name":"outer1","child":{"name":"i1","name":"i2"},"name":"outer2"}`

	check := func(t *testing.T, policy DuplicateKeyPolicy,
		outerValue, innerValue, expected string) {
		v := parseWithPolicy(t, s, policy)
		o := v.GetObject()
		if o.Len() != 2 {
			t.Fatalf("outer Len: got %d, want 2", o.Len())
		}
		if got := o.Get("name").String(); got != outerValue {
			t.Fatalf("outer Get: got %s, want %s", got, outerValue)
		}
		inner := o.Get("child").GetObject()
		if inner.Len() != 1 {
			t.Fatalf("inner Len: got %d, want 1", inner.Len())
		}
		if got := inner.Get("name").String(); got != innerValue {
			t.Fatalf("inner Get: got %s, want %s", got, innerValue)
		}
		if got := v.String(); got != expected {
			t.Fatalf("marshal: got %s, want %s", got, expected)
		}
	}

	t.Run("keepfirst", func(t *testing.T) {
		check(t, DuplicateKeyKeepFirst, `"outer1"`, `"i1"`,
			`{"name":"outer1","child":{"name":"i1"}}`)
	})
	t.Run("keeplast", func(t *testing.T) {
		check(t, DuplicateKeyKeepLast, `"outer2"`, `"i2"`,
			`{"child":{"name":"i2"},"name":"outer2"}`)
	})
}

func TestDuplicateKeysParserReuse(t *testing.T) {
	p := &Parser{DuplicateKeys: DuplicateKeyReject}

	// A rejected parse must not poison the following one.
	if _, err := p.Parse(`{"a":1,"a":2}`); err == nil {
		t.Fatalf("expecting error on first parse")
	}
	v, err := p.Parse(`{"a":{"a":1},"b":{"a":2}}`)
	if err != nil {
		t.Fatalf("second parse failed: %s", err)
	}
	if v.GetObject().Len() != 2 {
		t.Fatalf("Len after reuse: got %d, want 2", v.GetObject().Len())
	}

	// Switching policy back to default restores legacy behavior.
	p.DuplicateKeys = DuplicateKeyKeepAll
	v, err = p.Parse(`{"a":1,"a":2}`)
	if err != nil {
		t.Fatalf("keep-all parse failed: %s", err)
	}
	if v.GetObject().Len() != 2 {
		t.Fatalf("keep-all Len: got %d, want 2", v.GetObject().Len())
	}
}

func TestDuplicateKeysScannerReject(t *testing.T) {
	var sc Scanner
	sc.DuplicateKeys = DuplicateKeyReject
	sc.Init(`{"name":1,"name":2} {"name":{"name":1}}`)

	if sc.Next() {
		t.Fatalf("first value must be rejected")
	}
	if err := sc.Error(); err == nil {
		t.Fatalf("expecting error")
	} else {
		var dke *DuplicateKeyError
		if !errors.As(err, &dke) {
			t.Fatalf("unexpected scanner error: %s", err)
		}
		if dke.FirstOffset != 1 || dke.DuplicateOffset != 10 {
			t.Fatalf("scanner offsets: got %d and %d, want 1 and 10",
				dke.FirstOffset, dke.DuplicateOffset)
		}
	}

	// A fresh scanner honors the policy for the valid nested document.
	var sc2 Scanner
	sc2.DuplicateKeys = DuplicateKeyReject
	sc2.Init(`{"name":{"name":1}}`)
	if !sc2.Next() {
		t.Fatalf("valid value must parse: %s", sc2.Error())
	}
}
