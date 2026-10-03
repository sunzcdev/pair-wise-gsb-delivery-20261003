package fastjson

import (
	"errors"
	"strings"
	"testing"
)

func visitAll(o *Object) []string {
	var keys []string
	o.Visit(func(key []byte, v *Value) {
		keys = append(keys, string(key))
	})
	return keys
}

func TestDuplicateKeyDefaultPreservesMembers(t *testing.T) {
	var p Parser

	v, err := p.Parse(`{"a":1,"b":2,"a":3}`)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	o, err := v.Object()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if o.Len() != 3 {
		t.Fatalf("unexpected Len: got %d; want 3", o.Len())
	}
	if keys := visitAll(o); strings.Join(keys, ",") != "a,b,a" {
		t.Fatalf("unexpected Visit order: got %v; want [a b a]", keys)
	}
	// Historical behavior: Get returns the first matching member.
	if got := o.Get("a").GetInt(); got != 1 {
		t.Fatalf("unexpected Get(a): got %d; want 1", got)
	}
	if got := string(o.MarshalTo(nil)); got != `{"a":1,"b":2,"a":3}` {
		t.Fatalf("unexpected marshal: got %s", got)
	}
}

func TestDuplicateKeyKeepFirst(t *testing.T) {
	var p Parser
	p.DuplicateKeys = DuplicateKeyKeepFirst

	v, err := p.Parse(`{"a":1,"b":2,"a":3}`)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	o, err := v.Object()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if o.Len() != 2 {
		t.Fatalf("unexpected Len: got %d; want 2", o.Len())
	}
	if keys := visitAll(o); strings.Join(keys, ",") != "a,b" {
		t.Fatalf("unexpected Visit order: got %v; want [a b]", keys)
	}
	if got := o.Get("a").GetInt(); got != 1 {
		t.Fatalf("unexpected Get(a): got %d; want 1", got)
	}
	if got := o.Get("b").GetInt(); got != 2 {
		t.Fatalf("unexpected Get(b): got %d; want 2", got)
	}
	if got := string(o.MarshalTo(nil)); got != `{"a":1,"b":2}` {
		t.Fatalf("unexpected marshal: got %s", got)
	}
}

func TestDuplicateKeyKeepLast(t *testing.T) {
	var p Parser
	p.DuplicateKeys = DuplicateKeyKeepLast

	v, err := p.Parse(`{"a":1,"b":2,"a":3}`)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	o, err := v.Object()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if o.Len() != 2 {
		t.Fatalf("unexpected Len: got %d; want 2", o.Len())
	}
	// The surviving "a" keeps the original position of the last member.
	if keys := visitAll(o); strings.Join(keys, ",") != "b,a" {
		t.Fatalf("unexpected Visit order: got %v; want [b a]", keys)
	}
	if got := o.Get("a").GetInt(); got != 3 {
		t.Fatalf("unexpected Get(a): got %d; want 3", got)
	}
	if got := o.Get("b").GetInt(); got != 2 {
		t.Fatalf("unexpected Get(b): got %d; want 2", got)
	}
	if got := string(o.MarshalTo(nil)); got != `{"b":2,"a":3}` {
		t.Fatalf("unexpected marshal: got %s", got)
	}
}

func TestDuplicateKeyRejectEscapedEquivalence(t *testing.T) {
	var p Parser
	p.DuplicateKeys = DuplicateKeyReject

	doc := `{"name":1,"\u006eame":2}`
	_, err := p.Parse(doc)
	if err == nil {
		t.Fatalf("expecting duplicate key error")
	}

	var dke *DuplicateKeyError
	if !errors.As(err, &dke) {
		t.Fatalf("error is not *DuplicateKeyError: %T %s", err, err)
	}
	if dke.Key != "name" {
		t.Fatalf("unexpected key: got %q; want %q", dke.Key, "name")
	}

	firstQuote := strings.Index(doc, `"name"`)
	dupQuote := strings.Index(doc, `"\u006eame"`)
	if dke.FirstOffset != firstQuote {
		t.Fatalf("unexpected FirstOffset: got %d; want %d", dke.FirstOffset, firstQuote)
	}
	if dke.DuplicateOffset != dupQuote {
		t.Fatalf("unexpected DuplicateOffset: got %d; want %d", dke.DuplicateOffset, dupQuote)
	}
	if doc[dke.FirstOffset] != '"' || doc[dke.DuplicateOffset] != '"' {
		t.Fatalf("offsets must point at opening quotes")
	}
	if dke.FirstLine != 1 || dke.FirstColumn != 2 {
		t.Fatalf("unexpected first position: line %d col %d; want 1,2", dke.FirstLine, dke.FirstColumn)
	}
	if dke.DuplicateLine != 1 || dke.DuplicateColumn != firstCol(doc, dupQuote) {
		t.Fatalf("unexpected duplicate position: line %d col %d; want 1,%d", dke.DuplicateLine, dke.DuplicateColumn, firstCol(doc, dupQuote))
	}
	if !strings.Contains(err.Error(), `duplicate key "name"`) {
		t.Fatalf("unexpected error message: %s", err)
	}
}

func firstCol(doc string, offset int) int {
	if i := strings.LastIndexByte(doc[:offset], '\n'); i >= 0 {
		return offset - i
	}
	return offset + 1
}

func TestDuplicateKeyRejectLinesAndColumns(t *testing.T) {
	var p Parser
	p.DuplicateKeys = DuplicateKeyReject

	doc := "\n  {\n    \"name\": 1,\n    \"name\": 2\n  }"
	_, err := p.Parse(doc)
	if err == nil {
		t.Fatalf("expecting duplicate key error")
	}
	var dke *DuplicateKeyError
	if !errors.As(err, &dke) {
		t.Fatalf("error is not *DuplicateKeyError: %s", err)
	}
	if dke.FirstLine != 3 || dke.FirstColumn != 5 {
		t.Fatalf("unexpected first pos: line %d col %d", dke.FirstLine, dke.FirstColumn)
	}
	if dke.DuplicateLine != 4 || dke.DuplicateColumn != 5 {
		t.Fatalf("unexpected duplicate pos: line %d col %d", dke.DuplicateLine, dke.DuplicateColumn)
	}
}

func TestDuplicateKeyEmptyKey(t *testing.T) {
	doc := `{"":1,"":2}`

	t.Run("reject", func(t *testing.T) {
		var p Parser
		p.DuplicateKeys = DuplicateKeyReject
		_, err := p.Parse(doc)
		var dke *DuplicateKeyError
		if !errors.As(err, &dke) {
			t.Fatalf("expecting *DuplicateKeyError, got %v", err)
		}
		if dke.Key != "" || dke.FirstOffset != 1 || dke.DuplicateOffset != 6 {
			t.Fatalf("unexpected error fields: %+v", dke)
		}
	})

	t.Run("keep_first", func(t *testing.T) {
		var p Parser
		p.DuplicateKeys = DuplicateKeyKeepFirst
		v, err := p.Parse(doc)
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		o, err := v.Object()
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if o.Len() != 1 || o.Get("").GetInt() != 1 {
			t.Fatalf("unexpected object: %s", o.MarshalTo(nil))
		}
	})

	t.Run("keep_last", func(t *testing.T) {
		var p Parser
		p.DuplicateKeys = DuplicateKeyKeepLast
		v, err := p.Parse(doc)
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		o, err := v.Object()
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if o.Len() != 1 || o.Get("").GetInt() != 2 {
			t.Fatalf("unexpected object: %s", o.MarshalTo(nil))
		}
	})
}

func TestDuplicateKeyDoesNotLeakAcrossObjects(t *testing.T) {
	// The same key may appear once in each of several distinct objects,
	// including nested objects, siblings and array elements.
	doc := `{"outer":{"k":1},"sibling":{"k":2},"arr":[{"k":3},{"k":4}]}`

	for _, policy := range []DuplicateKeyPolicy{DuplicateKeyReject, DuplicateKeyKeepFirst, DuplicateKeyKeepLast} {
		var p Parser
		p.DuplicateKeys = policy
		v, err := p.Parse(doc)
		if err != nil {
			t.Fatalf("policy %s: unexpected error: %s", policy, err)
		}
		if v.Get("outer", "k").GetInt() != 1 {
			t.Fatalf("policy %s: outer.k mismatch", policy)
		}
		if v.Get("sibling", "k").GetInt() != 2 {
			t.Fatalf("policy %s: sibling.k mismatch", policy)
		}
		if v.Get("arr", "0", "k").GetInt() != 3 {
			t.Fatalf("policy %s: arr[0].k mismatch", policy)
		}
		if v.Get("arr", "1", "k").GetInt() != 4 {
			t.Fatalf("policy %s: arr[1].k mismatch", policy)
		}
	}
}

func TestDuplicateKeyNestedReject(t *testing.T) {
	var p Parser
	p.DuplicateKeys = DuplicateKeyReject

	doc := `{"a":{"b":{"c":1,"c":2}}}`
	_, err := p.Parse(doc)
	if err == nil {
		t.Fatalf("expecting error")
	}
	var dke *DuplicateKeyError
	if !errors.As(err, &dke) {
		t.Fatalf("error is not *DuplicateKeyError through wrapping: %T %s", err, err)
	}
	if dke.Key != "c" {
		t.Fatalf("unexpected key: %q", dke.Key)
	}
	first := strings.Index(doc, `"c":1`)
	dup := strings.Index(doc, `"c":2`)
	if dke.FirstOffset != first || dke.DuplicateOffset != dup {
		t.Fatalf("unexpected offsets: %+v", dke)
	}
	if dke.FirstLine != 1 || dke.DuplicateLine != 1 {
		t.Fatalf("unexpected lines: %+v", dke)
	}
}

func TestDuplicateKeyPolicyDoesNotLeakAcrossParses(t *testing.T) {
	var p Parser
	p.DuplicateKeys = DuplicateKeyReject

	if _, err := p.Parse(`{"x":1,"x":2}`); err == nil {
		t.Fatalf("expecting reject error")
	}

	// A valid document still parses with the same reject policy.
	v, err := p.Parse(`{"x":1,"y":{"x":2}}`)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if v.Get("x").GetInt() != 1 || v.Get("y", "x").GetInt() != 2 {
		t.Fatalf("unexpected values: %s", v.MarshalTo(nil))
	}

	// Resetting to the default policy preserves duplicates again.
	p.DuplicateKeys = DuplicateKeyDefault
	v, err = p.Parse(`{"x":1,"x":2}`)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if oo, _ := v.Object(); oo.Len() != 2 {
		t.Fatalf("default policy must preserve duplicates")
	}
}

func TestScannerDuplicateKeyReject(t *testing.T) {
	var sc Scanner
	sc.DuplicateKeys = DuplicateKeyReject

	sc.Init(`{"k":1} {"k":1,"k":2}`)
	if !sc.Next() {
		t.Fatalf("first value error: %s", sc.Error())
	}
	if sc.Next() {
		t.Fatalf("expecting duplicate key error on second value")
	}
	var dke *DuplicateKeyError
	if !errors.As(sc.Error(), &dke) {
		t.Fatalf("error is not *DuplicateKeyError: %v", sc.Error())
	}
	if dke.Key != "k" || dke.FirstLine != 1 {
		t.Fatalf("unexpected error: %+v", dke)
	}
}

func TestDuplicateKeyDeeplyNestedIsolation(t *testing.T) {
	// Build a document nested 100 levels deep, where each level contains
	// the same key exactly once. The policy must not leak between objects.
	var b strings.Builder
	const depth = 100
	for range depth {
		b.WriteString(`{"k":`)
	}
	b.WriteString(`1`)
	for range depth {
		b.WriteString(`}`)
	}
	doc := b.String()

	var p Parser
	p.DuplicateKeys = DuplicateKeyReject
	v, err := p.Parse(doc)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	cur := v
	for range depth {
		if cur == nil || cur.Type() != TypeObject {
			t.Fatalf("expected object while descending")
		}
		cur = cur.Get("k")
	}
	if cur == nil || cur.Type() != TypeNumber || cur.GetInt() != 1 {
		t.Fatalf("unexpected leaf value: %v", cur)
	}

	// A real duplicate at the deepest level must still be reported.
	bad := strings.Replace(doc, `"k":1`, `"k":1,"k":2`, 1)
	_, err = p.Parse(bad)
	var dke *DuplicateKeyError
	if !errors.As(err, &dke) {
		t.Fatalf("expecting *DuplicateKeyError for deep duplicate, got %v", err)
	}
	if dke.Key != "k" {
		t.Fatalf("unexpected key: %q", dke.Key)
	}
}

func TestDuplicateKeyRejectParseBytes(t *testing.T) {
	var p Parser
	p.DuplicateKeys = DuplicateKeyReject

	_, err := p.ParseBytes([]byte(`{"a":1,"a":2}`))
	var dke *DuplicateKeyError
	if !errors.As(err, &dke) {
		t.Fatalf("expecting *DuplicateKeyError, got %v", err)
	}
	if dke.Key != "a" || dke.FirstOffset != 1 || dke.DuplicateOffset != 7 {
		t.Fatalf("unexpected fields: %+v", dke)
	}
}

func TestDuplicateKeyThreeOccurrences(t *testing.T) {
	doc := `{"a":1,"a":2,"b":3,"a":4}`

	t.Run("keep_first", func(t *testing.T) {
		var p Parser
		p.DuplicateKeys = DuplicateKeyKeepFirst
		v, err := p.Parse(doc)
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		o, _ := v.Object()
		if o.Len() != 2 {
			t.Fatalf("unexpected Len: %d", o.Len())
		}
		if got := string(o.MarshalTo(nil)); got != `{"a":1,"b":3}` {
			t.Fatalf("unexpected marshal: %s", got)
		}
		if o.Get("a").GetInt() != 1 {
			t.Fatalf("unexpected a value")
		}
	})

	t.Run("keep_last", func(t *testing.T) {
		var p Parser
		p.DuplicateKeys = DuplicateKeyKeepLast
		v, err := p.Parse(doc)
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		o, _ := v.Object()
		if o.Len() != 2 {
			t.Fatalf("unexpected Len: %d", o.Len())
		}
		// "a" migrates to the position of its last occurrence, after "b".
		if got := string(o.MarshalTo(nil)); got != `{"b":3,"a":4}` {
			t.Fatalf("unexpected marshal: %s", got)
		}
		if keys := visitAll(o); strings.Join(keys, ",") != "b,a" {
			t.Fatalf("unexpected Visit order: %v", keys)
		}
		if o.Get("a").GetInt() != 4 {
			t.Fatalf("unexpected a value")
		}
	})
}
