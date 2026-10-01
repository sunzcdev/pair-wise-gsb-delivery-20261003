package redcon

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"
)

func mustParse3(t *testing.T, payload string) RESP {
	t.Helper()
	n, resp, err := ParseNextRESP([]byte(payload))
	if err != nil {
		t.Fatalf("expected good resp for %q, got %v", payload, err)
	}
	if n != len(payload) {
		t.Fatalf("expected to consume %d bytes, got %d for %q",
			len(payload), n, payload)
	}
	return resp
}

func expectIncomplete3(t *testing.T, payload string) {
	t.Helper()
	n, _, err := ParseNextRESP([]byte(payload))
	if !errors.Is(err, ErrIncompleteRESP) || n != 0 {
		t.Fatalf("expected ErrIncompleteRESP for %q, got n=%d err=%v",
			payload, n, err)
	}
}

func expectInvalid3(t *testing.T, payload string) {
	t.Helper()
	n, _, err := ParseNextRESP([]byte(payload))
	if !errors.Is(err, ErrInvalidRESP) || n != 0 {
		t.Fatalf("expected ErrInvalidRESP for %q, got n=%d err=%v",
			payload, n, err)
	}
}

func TestRESP3Scalars(t *testing.T) {
	// Boolean
	tr := mustParse3(t, "#t\r\n")
	if tr.Type != Boolean || !tr.Bool() || string(tr.Data) != "t" {
		t.Fatalf("bad true boolean: %v", tr)
	}
	fa := mustParse3(t, "#f\r\n")
	if fa.Type != Boolean || fa.Bool() {
		t.Fatalf("bad false boolean: %v", fa)
	}
	expectInvalid3(t, "#x\r\n")
	expectInvalid3(t, "#\r\n")
	expectIncomplete3(t, "#t\r")
	if string(AppendBoolean(nil, true)) != "#t\r\n" {
		t.Fatal("AppendBoolean(true) mismatch")
	}
	if string(AppendBoolean(nil, false)) != "#f\r\n" {
		t.Fatal("AppendBoolean(false) mismatch")
	}

	// Double
	for _, text := range []string{"0", "-0", "1", "1.5", "-3.14",
		"inf", "-inf", "nan", "+2.5"} {
		p := "," + text + "\r\n"
		r := mustParse3(t, p)
		if r.Type != Double || string(r.Data) != text {
			t.Fatalf("bad double for %q: %v", p, r)
		}
	}
	expectInvalid3(t, ",\r\n")
	expectInvalid3(t, ",abc\r\n")
	expectInvalid3(t, ",1.\r\n")
	expectIncomplete3(t, ",1.5\r")
	if string(AppendDouble(nil, 1.5)) != ",1.5\r\n" {
		t.Fatalf("AppendDouble mismatch: %q", AppendDouble(nil, 1.5))
	}
	if string(AppendDouble(nil, math.Inf(1))) != ",inf\r\n" {
		t.Fatal("AppendDouble +inf mismatch")
	}
	if string(AppendDouble(nil, math.Inf(-1))) != ",-inf\r\n" {
		t.Fatal("AppendDouble -inf mismatch")
	}
	if string(AppendDouble(nil, math.NaN())) != ",nan\r\n" {
		t.Fatal("AppendDouble nan mismatch")
	}
	if string(AppendDoubleText(nil, "12.34")) != ",12.34\r\n" {
		t.Fatal("AppendDoubleText mismatch")
	}

	// Big number
	big := mustParse3(t, "(3492890328409238509324850943850943825024385\r\n")
	if big.Type != BigNumber {
		t.Fatalf("expected BigNumber, got %v", big.Type)
	}
	x := big.BigInt()
	if x == nil || x.String() !=
		"3492890328409238509324850943850943825024385" {
		t.Fatalf("bad big int: %v", x)
	}
	if mustParse3(t, "(-42\r\n").BigInt().String() != "-42" {
		t.Fatal("negative big number mismatch")
	}
	expectInvalid3(t, "(\r\n")
	expectInvalid3(t, "(-)\r\n")
	expectInvalid3(t, "(12a3\r\n")
	expectIncomplete3(t, "(42\r")
	if string(AppendBigNumberString(nil, "123456789012345678901234567890")) !=
		"(123456789012345678901234567890\r\n" {
		t.Fatal("AppendBigNumberString mismatch")
	}

	// Verbatim string
	v := mustParse3(t, "=15\r\ntxt:Some string\r\n")
	if v.Type != Verbatim || v.VerbatimEncoding() != "txt" ||
		string(v.VerbatimValue()) != "Some string" {
		t.Fatalf("bad verbatim: enc=%q val=%q",
			v.VerbatimEncoding(), v.VerbatimValue())
	}
	mkd := mustParse3(t, "=11\r\nmkd:# hi!!!\r\n")
	if mkd.VerbatimEncoding() != "mkd" ||
		string(mkd.VerbatimValue()) != "# hi!!!" {
		t.Fatal("bad mkd verbatim")
	}
	expectInvalid3(t, "=3\r\nfoo\r\n")
	expectInvalid3(t, "=4\r\nabcd\r\n")
	expectIncomplete3(t, "=15\r\ntxt:Some")
	got := string(AppendVerbatimString(nil, "txt", "Some string"))
	if got != "=15\r\ntxt:Some string\r\n" {
		t.Fatalf("AppendVerbatimString mismatch: %q", got)
	}

	// Null and bulk error
	nr := mustParse3(t, "_\r\n")
	if nr.Type != Null || nr.Data != nil {
		t.Fatalf("bad null: %v", nr)
	}
	expectInvalid3(t, "_x\r\n")
	if string(AppendRESPNull(nil)) != "_\r\n" {
		t.Fatal("AppendRESPNull mismatch")
	}
	be := mustParse3(t, "!21\r\nSYNTAX invalid syntax\r\n")
	if be.Type != BulkError || string(be.Data) != "SYNTAX invalid syntax" {
		t.Fatalf("bad bulk error: %v", be)
	}
	if string(AppendBulkErrorString(nil, "boom")) != "!4\r\nboom\r\n" {
		t.Fatal("AppendBulkErrorString mismatch")
	}
}

func TestRESP3Containers(t *testing.T) {
	// Map: "%2\r\n+first\r\n:1\r\n+second\r\n:2\r\n"
	var m []byte
	m = AppendMap(m, 2)
	m = AppendString(m, "first")
	m = AppendInt(m, 1)
	m = AppendString(m, "second")
	m = AppendInt(m, 2)
	r := mustParse3(t, string(m))
	if r.Type != Map || r.Count != 2 {
		t.Fatalf("bad map: type=%v count=%d", r.Type, r.Count)
	}
	mm := r.Map()
	if len(mm) != 2 || mm["first"].Int() != 1 || mm["second"].Int() != 2 {
		t.Fatalf("bad map contents: %v", mm)
	}
	if r.MapGet("second").Int() != 2 {
		t.Fatal("MapGet failed")
	}
	var keys, vals int
	r.ForEach(func(item RESP) bool {
		if keys == vals {
			keys++
		} else {
			vals++
		}
		return true
	})
	if keys != 2 || vals != 2 {
		t.Fatalf("map ForEach visited %d keys %d vals", keys, vals)
	}

	// Set: "~3\r\n+first\r\n+second\r\n+third\r\n"
	var s []byte
	s = AppendSet(s, 3)
	s = AppendString(s, "first")
	s = AppendString(s, "second")
	s = AppendString(s, "third")
	sr := mustParse3(t, string(s))
	if sr.Type != Set || sr.Count != 3 {
		t.Fatalf("bad set: %v", sr)
	}
	var n int
	sr.ForEach(func(item RESP) bool { n++; return true })
	if n != 3 {
		t.Fatalf("set ForEach visited %d", n)
	}

	// Push with mixed/nested content.
	var p []byte
	p = AppendPush(p, 3)
	p = AppendBulkString(p, "hello")
	p = AppendArray(p, 2)
	p = AppendInt(p, 1)
	p = AppendInt(p, 2)
	p = AppendDouble(p, 3.5)
	pr := mustParse3(t, string(p))
	if pr.Type != Push || pr.Count != 3 {
		t.Fatalf("bad push: %v", pr)
	}
	items := make([]RESP, 0, 3)
	pr.ForEach(func(item RESP) bool {
		items = append(items, item)
		return true
	})
	if items[0].Type != Bulk || string(items[0].Data) != "hello" {
		t.Fatalf("bad push item 0: %v", items[0])
	}
	if items[1].Type != Array || items[1].Count != 2 {
		t.Fatalf("bad push item 1: %v", items[1])
	}
	if items[2].Type != Double || items[2].Float() != 3.5 {
		t.Fatalf("bad push item 2: %v", items[2])
	}
	if string(pr.Raw) != string(p) {
		t.Fatal("push raw mismatch")
	}

	// Empty containers are legal.
	for _, hdr := range []string{"*0\r\n", "%0\r\n", "~0\r\n",
		">0\r\n"} {
		if _, err := (func() (int, error) {
			_, _, e := ParseNextRESP([]byte(hdr))
			return 0, e
		})(); err != nil {
			t.Fatalf("empty container %q should parse: %v", hdr, err)
		}
	}

	// Invalid container payloads.
	expectInvalid3(t, "~a\r\n")
	expectInvalid3(t, "~-1\r\n")
	expectInvalid3(t, ">-1\r\n")
	expectInvalid3(t, "%-1\r\n")
	expectIncomplete3(t, "%1\r\n+a\r\n")
	expectInvalid3(t, "%1\r\n+a\r\n^x\r\n")
	expectIncomplete3(t, "~2\r\n+a\r\n")
	expectInvalid3(t, "~2\r\n+a\r\n^bogus\r\n")
	expectIncomplete3(t, "~2\r\n+a\r\n")
}

func TestRESP3Attribute(t *testing.T) {
	// Attribute prefixing a simple value.
	// "|1\r\n+key\r\n:1234\r\n+value\r\n"
	var b []byte
	b = AppendAttribute(b, 1)
	b = AppendString(b, "key")
	b = AppendInt(b, 1234)
	b = AppendString(b, "value")
	r := mustParse3(t, string(b))
	if r.Type != String || string(r.Data) != "value" {
		t.Fatalf("attribute should yield the augmented value: %v", r)
	}
	if len(r.Attributes) != 1 {
		t.Fatalf("expected 1 attribute, got %d", len(r.Attributes))
	}
	a := r.Attributes[0]
	if a.Type != Attribute || a.Count != 1 ||
		string(a.Raw) != "|1\r\n+key\r\n:1234\r\n" {
		t.Fatalf("bad attribute: %v", a)
	}
	if a.MapGet("key").Int() != 1234 {
		t.Fatal("attribute map lookup failed")
	}
	if string(r.Raw) != string(b) {
		t.Fatalf("raw must include the attribute frame:\n%q\n%q", r.Raw, b)
	}

	// Attribute on every element of an array.
	var c []byte
	c = AppendArray(c, 2)
	c = AppendAttribute(c, 1)
	c = AppendBulkString(c, "ann")
	c = AppendBulkString(c, "1")
	c = AppendInt(c, 10)
	c = AppendAttribute(c, 1)
	c = AppendBulkString(c, "ttl")
	c = AppendInt(c, 5)
	c = AppendInt(c, 20)
	ar := mustParse3(t, string(c))
	if ar.Type != Array || ar.Count != 2 {
		t.Fatalf("bad attributed array: %v", ar)
	}
	var elems []RESP
	ar.ForEach(func(item RESP) bool {
		elems = append(elems, item)
		return true
	})
	if len(elems) != 2 || elems[0].Int() != 10 || elems[1].Int() != 20 {
		t.Fatalf("bad attributed elements: %v", elems)
	}
	if len(elems[0].Attributes) != 1 ||
		elems[0].Attributes[0].MapGet("ann").String() != "1" {
		t.Fatal("first element attribute lost")
	}
	if elems[1].Attributes[0].MapGet("ttl").Int() != 5 {
		t.Fatal("second element attribute lost")
	}
	if string(ar.Raw) != string(c) {
		t.Fatal("attributed array raw mismatch")
	}

	// Chained attributes, wire order preserved.
	var d []byte
	d = AppendAttribute(d, 1)
	d = AppendString(d, "outer")
	d = AppendInt(d, 1)
	d = AppendAttribute(d, 1)
	d = AppendString(d, "inner")
	d = AppendInt(d, 2)
	d = AppendInt(d, 99)
	dr := mustParse3(t, string(d))
	if dr.Int() != 99 || len(dr.Attributes) != 2 {
		t.Fatalf("bad chained attributes: %v", dr)
	}
	if dr.Attributes[0].MapGet("outer").Int() != 1 ||
		dr.Attributes[1].MapGet("inner").Int() != 2 {
		t.Fatalf("chained attribute order lost: %v", dr.Attributes)
	}

	// Bare attribute with no augmented value is incomplete.
	expectIncomplete3(t, "|1\r\n+k\r\n:1\r\n")
	expectInvalid3(t, "|1\r\n+k\r\n:x\r\n:1\r\n")
	expectInvalid3(t, "|-1\r\n+x\r\n")
}

func TestRESP3Limits(t *testing.T) {
	// Exactly maxRESPDepth nested arrays parses (top level counts as 0).
	deep := strings.Repeat("*1\r\n", maxRESPDepth) + ":1\r\n"
	if _, _, err := ParseNextRESP([]byte(deep)); err != nil {
		t.Fatalf("depth-%d nesting should parse, got %v",
			maxRESPDepth, err)
	}

	// One more nested container is invalid.
	toodeep := strings.Repeat("*1\r\n", maxRESPDepth+1) + ":1\r\n"
	expectInvalid3(t, toodeep)

	// A truncated deep header is also a depth error, not incomplete.
	truncated := strings.Repeat("*1\r\n", maxRESPDepth+1)
	expectInvalid3(t, truncated)

	// Element counts above the cap are invalid.
	bigCount := "*" + uintToStr(maxRESPElements+1) + "\r\n"
	expectInvalid3(t, bigCount)
	expectInvalid3(t, "%"+uintToStr(maxRESPElements+1)+"\r\n")

	// Lengths that overflow int64 are invalid.
	expectInvalid3(t, "$999999999999999999999999\r\nx\r\n")

	// Large but bounded declared length without payload is incomplete,
	// not invalid.
	expectIncomplete3(t, "$"+uintToStr(maxRESPLength)+"\r\nshort")

	// Declared length above cap is invalid even with no payload.
	expectInvalid3(t, "$"+uintToStr(maxRESPLength+1)+"\r\nshort")

	// Attributes consume nesting budget.
	attrDeep := strings.Repeat("|1\r\n+a\r\n:1\r\n", maxRESPDepth+2) +
		":1\r\n"
	expectInvalid3(t, attrDeep)
}

func uintToStr(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [32]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// feedByteByByte feeds payload one byte at a time and asserts the message
// only completes on the final byte, while every prefix is reported as
// incomplete (never invalid).
func feedByteByByte(t *testing.T, payload []byte) RESP {
	t.Helper()
	p := NewRESPParser()
	for i := 0; i < len(payload)-1; i++ {
		p.Feed(payload[i : i+1])
		_, err := p.Next()
		if !errors.Is(err, ErrIncompleteRESP) {
			t.Fatalf("prefix %d of %q: expected incomplete, got %v",
				i, payload, err)
		}
	}
	p.Feed(payload[len(payload)-1:])
	resp, err := p.Next()
	if err != nil {
		t.Fatalf("final byte: expected complete, got %v", err)
	}
	if p.Buffered() != 0 {
		t.Fatalf("expected buffer drained, got %d bytes", p.Buffered())
	}
	return resp
}

func TestRESP3ByteByByte(t *testing.T) {
	payloads := [][]byte{
		[]byte("#t\r\n"),
		[]byte(",-3.14\r\n"),
		[]byte("(3492890328409238509324850943850943825024385\r\n"),
		[]byte("=15\r\ntxt:Some string\r\n"),
		[]byte("_\r\n"),
		[]byte("!4\r\nboom\r\n"),
	}

	var nested []byte
	nested = AppendMap(nested, 2)
	nested = AppendBulkString(nested, "a")
	nested = AppendSet(nested, 2)
	nested = AppendInt(nested, 1)
	nested = AppendBoolean(nested, true)
	nested = AppendBulkString(nested, "b")
	nested = AppendPush(nested, 1)
	nested = AppendDouble(nested, 2.5)
	payloads = append(payloads, nested)

	var attributed []byte
	attributed = AppendAttribute(attributed, 2)
	attributed = AppendString(attributed, "k1")
	attributed = AppendInt(attributed, 1)
	attributed = AppendString(attributed, "k2")
	attributed = AppendMap(attributed, 1)
	attributed = AppendString(attributed, "nested")
	attributed = AppendBoolean(attributed, false)
	attributed = AppendVerbatimString(attributed, "mkd", "# title")
	payloads = append(payloads, attributed)

	for _, payload := range payloads {
		one := feedByteByByte(t, payload)
		n, whole, err := ParseNextRESP(payload)
		if err != nil {
			t.Fatalf("one-shot parse of %q failed: %v", payload, err)
		}
		if n != len(payload) || !bytes.Equal(one.Raw, whole.Raw) ||
			one.Type != whole.Type || one.Count != whole.Count ||
			!bytes.Equal(one.Data, whole.Data) {
			t.Fatalf("streamed/oneshot mismatch for %q", payload)
		}
		if !bytes.Equal(one.Raw, payload) {
			t.Fatalf("raw roundtrip mismatch for %q", payload)
		}
	}
}

func TestRESP3RandomChunks(t *testing.T) {
	// Build a pipeline of distinct RESP3 messages.
	var stream []byte
	stream = AppendBoolean(stream, true)
	stream = AppendDouble(stream, 1.25)
	stream = AppendMap(stream, 1)
	stream = AppendString(stream, "k")
	stream = AppendBigNumberString(stream, "999999999999999999999999")
	stream = AppendSet(stream, 2)
	stream = AppendInt(stream, 7)
	stream = AppendRESPNull(stream)
	stream = AppendPush(stream, 1)
	stream = AppendString(stream, "update")

	// Feed using every possible fixed chunk size 1..len(stream).
	for chunk := 1; chunk <= len(stream); chunk++ {
		p := NewRESPParser()
		var got int
		for i := 0; i < len(stream); i += chunk {
			end := i + chunk
			if end > len(stream) {
				end = len(stream)
			}
			p.Feed(append([]byte(nil), stream[i:end]...))
			for {
				r, err := p.Next()
				if errors.Is(err, ErrIncompleteRESP) {
					break
				}
				if err != nil {
					t.Fatalf("chunk=%d: %v", chunk, err)
				}
				got += len(r.Raw)
			}
		}
		if got != len(stream) {
			t.Fatalf("chunk=%d consumed %d of %d bytes",
				chunk, got, len(stream))
		}
	}
}

func TestRESP3ParserInvalid(t *testing.T) {
	p := NewRESPParser()
	p.Feed([]byte(":abc\r\n"))
	if _, err := p.Next(); !errors.Is(err, ErrInvalidRESP) {
		t.Fatalf("expected invalid, got %v", err)
	}
	// Fatal until reset.
	p.Feed([]byte(":1\r\n"))
	if _, err := p.Next(); !errors.Is(err, ErrInvalidRESP) {
		t.Fatalf("parser should stay invalid, got %v", err)
	}
	p.Reset()
	p.Feed([]byte(":1\r\n"))
	r, err := p.Next()
	if err != nil || r.Int() != 1 {
		t.Fatalf("parser did not recover after reset: %v %v", r, err)
	}
}

func TestRESP3TypedRoundTrip(t *testing.T) {
	// Every typed encoder must parse back through the strict decoder;
	// the implementation cannot rely on WriteRaw accepting pre-encoded text.
	var b []byte
	b = AppendMap(b, 6)
	b = AppendBulkString(b, "bool")
	b = AppendBoolean(b, true)
	b = AppendBulkString(b, "dbl")
	b = AppendDouble(b, -0.5)
	b = AppendBulkString(b, "big")
	b = AppendBigNumberString(b, "123456789012345678901234567890")
	b = AppendBulkString(b, "coll")
	b = AppendSet(b, 2)
	b = AppendVerbatimString(b, "txt", "hello")
	b = AppendBulkErrorString(b, "ERR nope")
	b = AppendBulkString(b, "push")
	b = AppendPush(b, 2)
	b = AppendRESPNull(b)
	b = AppendDoubleText(b, "inf")
	b = AppendBulkString(b, "verb")
	b = AppendVerbatimString(b, "mkd", "# title")

	n, r, err := ParseNextRESP(b)
	if err != nil || n != len(b) {
		t.Fatalf("roundtrip parse failed: n=%d err=%v", n, err)
	}
	if r.Type != Map || r.Count != 6 {
		t.Fatalf("bad top map: %v", r)
	}
	if r.MapGet("bool").Bool() != true {
		t.Fatal("bool roundtrip failed")
	}
	if r.MapGet("dbl").Float() != -0.5 {
		t.Fatal("double roundtrip failed")
	}
	if x := r.MapGet("big").BigInt(); x == nil ||
		x.String() != "123456789012345678901234567890" {
		t.Fatal("big number roundtrip failed")
	}
	coll := r.MapGet("coll")
	if coll.Type != Set || coll.Count != 2 {
		t.Fatalf("set roundtrip failed: %v", coll)
	}
	push := r.MapGet("push")
	if push.Type != Push || push.Count != 2 {
		t.Fatalf("push roundtrip failed: %v", push)
	}
	verb := r.MapGet("verb")
	if verb.Type != Verbatim || verb.VerbatimEncoding() != "mkd" ||
		string(verb.VerbatimValue()) != "# title" {
		t.Fatalf("verbatim roundtrip failed: %v", verb)
	}
	if !bytes.Equal(r.Raw, b) {
		t.Fatalf("raw roundtrip mismatch:\n%q\n%q", r.Raw, b)
	}
}

func TestRESP2Compatibility(t *testing.T) {
	// The legacy entry point keeps its exact historical representation for
	// RESP2 inputs, including the ambiguous zero/empty failure result.
	type tc struct {
		payload string
		typ     Type
		count   int
		data    []byte
		null    bool
	}
	cases := []tc{
		{":123\r\n", Integer, 0, []byte("123"), false},
		{"+ok\r\n", String, 0, []byte("ok"), false},
		{"-err\r\n", Error, 0, []byte("err"), false},
		{"$0\r\n\r\n", Bulk, 0, []byte(""), false},
		{"$5\r\nhello\r\n", Bulk, 0, []byte("hello"), false},
		{"$5\r\nhello\r\n+extra\r\n", Bulk, 0, []byte("hello"), false},
		{"*3\r\n:1\r\n:2\r\n:3\r\n", Array, 3,
			[]byte(":1\r\n:2\r\n:3\r\n"), false},
		{"$-1\r\n", Bulk, 0, nil, true},
		{"*-1\r\n", Array, 0, nil, true},
	}
	for _, c := range cases {
		n, r := ReadNextRESP([]byte(c.payload))
		if n == 0 {
			t.Fatalf("legacy parse failed for %q", c.payload)
		}
		if r.Type != c.typ || r.Count != c.count ||
			(c.null && r.Data != nil) ||
			(!c.null && string(r.Data) != string(c.data)) {
			t.Fatalf("legacy mismatch for %q: %s", c.payload, respVOut(r))
		}
	}

	// Incomplete and invalid inputs remain indistinguishable through the
	// legacy API (both return zero/empty).
	for _, bad := range []string{"", "^x\r\n", ":abc\r\n", "+hi\r",
		"$5\r\nhel\r\n", "*3\r\n:1\r\n"} {
		n, r := ReadNextRESP([]byte(bad))
		if n != 0 || !isEmptyRESP(r) {
			t.Fatalf("expected empty legacy result for %q, got %d %s",
				bad, n, respVOut(r))
		}
	}

	// Legacy ForEach still walks array elements with early termination.
	_, r := ReadNextRESP([]byte("*4\r\n:1\r\n:2\r\n:3\r\n:4\r\n"))
	var seen int
	r.ForEach(func(item RESP) bool {
		seen++
		return seen < 2
	})
	if seen != 2 {
		t.Fatalf("legacy ForEach early stop failed: %d", seen)
	}
}

func TestRESP3Pipeline(t *testing.T) {
	var stream []byte
	stream = AppendBoolean(stream, true)
	stream = AppendInt(stream, 42)
	stream = AppendMap(stream, 1)
	stream = AppendString(stream, "k")
	stream = AppendRESPNull(stream)
	stream = AppendBigNumberString(stream, "999999999999999999999999")
	stream = AppendPush(stream, 1)
	stream = AppendDouble(stream, 2.5)
	stream = AppendRESPNull(stream)
	stream = AppendBulkError(stream, []byte("bad"))
	stream = AppendVerbatimString(stream, "txt", "hi")
	stream = AppendSet(stream, 0)
	stream = AppendAttribute(stream, 0)
	stream = AppendString(stream, "augmented")
	// trailing bytes belonging to a fourth, incomplete message
	stream = append(stream, ',')

	p := NewRESPParser()
	p.Feed(stream)
	r1, err := p.Next()
	if err != nil || r1.Type != Boolean || !r1.Bool() {
		t.Fatalf("msg1: %v %v", r1, err)
	}
	r2, err := p.Next()
	if err != nil || r2.Type != Integer || r2.Int() != 42 {
		t.Fatalf("msg2: %v %v", r2, err)
	}
	r3, err := p.Next()
	if err != nil || r3.Type != Map || r3.Count != 1 {
		t.Fatalf("msg3: %v %v", r3, err)
	}
	rx, err := p.Next()
	if err != nil || rx.Type != BigNumber ||
		rx.BigInt().String() != "999999999999999999999999" {
		t.Fatalf("msg4 big number: %v %v", rx, err)
	}
	rx, err = p.Next()
	if err != nil || rx.Type != Push || rx.Count != 1 {
		t.Fatalf("msg5 push: %v %v", rx, err)
	}
	rx, err = p.Next()
	if err != nil || rx.Type != Null {
		t.Fatalf("msg6 null: %v %v", rx, err)
	}
	rx, err = p.Next()
	if err != nil || rx.Type != BulkError {
		t.Fatalf("msg7 bulk error: %v %v", rx, err)
	}
	rx, err = p.Next()
	if err != nil || rx.Type != Verbatim {
		t.Fatalf("msg8 verbatim: %v %v", rx, err)
	}
	rx, err = p.Next()
	if err != nil || rx.Type != Set || rx.Count != 0 {
		t.Fatalf("msg9 set: %v %v", rx, err)
	}
	rx, err = p.Next()
	if err != nil || rx.Type != String || rx.String() != "augmented" ||
		len(rx.Attributes) != 1 {
		t.Fatalf("msg10 attributed: %v %v", rx, err)
	}
	if _, err := p.Next(); !errors.Is(err, ErrIncompleteRESP) {
		t.Fatalf("expected trailing incomplete, got %v", err)
	}
	if p.Buffered() != 1 {
		t.Fatalf("expected 1 trailing byte, got %d", p.Buffered())
	}
	// completing the trailing double yields the message
	p.Feed([]byte("1\r\n"))
	r4, err := p.Next()
	if err != nil || r4.Type != Double || r4.Float() != 1 {
		t.Fatalf("msg4: %v %v", r4, err)
	}
	if p.Buffered() != 0 {
		t.Fatalf("buffer should be empty, got %d", p.Buffered())
	}
}

func TestRESP3Writer(t *testing.T) {
	var w Writer
	w.WriteMap(1)
	w.WriteBulkString("flag")
	w.WriteBoolean(true)
	w.WriteBigNumberString("999999999999999999999999")
	w.WritePush(1)
	w.WriteDouble(2.5)
	w.WriteRESPNull()
	w.WriteBulkError([]byte("bad"))
	w.WriteVerbatim("txt", []byte("hi"))
	w.WriteSet(0)
	w.WriteAttribute(0)
	w.WriteString("augmented")

	payload := w.Buffer()
	p := NewRESPParser()
	p.Feed(payload)
	r, err := p.Next()
	if err != nil || r.Type != Map || r.Count != 1 {
		t.Fatalf("map: %v %v", r, err)
	}
	if r.MapGet("flag").Bool() != true {
		t.Fatal("writer boolean failed")
	}
	r, err = p.Next()
	if err != nil || r.Type != BigNumber ||
		r.BigInt().String() != "999999999999999999999999" {
		t.Fatalf("big number: %v %v", r, err)
	}
	r, err = p.Next()
	if err != nil || r.Type != Push || r.Count != 1 {
		t.Fatalf("push: %v %v", r, err)
	}
	for _, expect := range []Type{Null, BulkError, Verbatim, Set} {
		r, err = p.Next()
		if err != nil || r.Type != expect {
			t.Fatalf("expected %v, got %v err=%v", expect, r.Type, err)
		}
	}
	// Last message carries an empty attribute followed by a simple string.
	r, err = p.Next()
	if err != nil || r.Type != String || r.String() != "augmented" {
		t.Fatalf("attributed: %v %v", r, err)
	}
	if len(r.Attributes) != 1 || r.Attributes[0].Count != 0 {
		t.Fatalf("attribute lost: %v", r.Attributes)
	}
	if p.Buffered() != 0 {
		t.Fatalf("leftover bytes: %d", p.Buffered())
	}
}
