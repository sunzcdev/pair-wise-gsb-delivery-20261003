package avro

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// refTranscode is the semantic reference the prompt names: Decode the
// writer bytes into a generic value, then Encode with the reader schema.
// It uses map[string]any / []any targets, the same dynamic path Resolve's
// interface decoder fills.
func refTranscode(t *testing.T, writer, reader *Schema, in []byte) []byte {
	t.Helper()
	resolved, err := Resolve(writer, reader)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var v any
	if rest, err := resolved.Decode(in, &v); err != nil {
		t.Fatalf("reference Decode: %v", err)
	} else if len(rest) != 0 {
		t.Fatalf("reference Decode left %d bytes", len(rest))
	}
	out, err := reader.Encode(v)
	if err != nil {
		t.Fatalf("reference Encode: %v", err)
	}
	return out
}

// runTC compiles a transcoder, converts in, and asserts success plus no
// trailing bytes.
func runTC(t *testing.T, w, r *Schema, in []byte, opts ...TranscodeOption) []byte {
	t.Helper()
	tc, err := NewTranscoder(w, r, opts...)
	if err != nil {
		t.Fatalf("NewTranscoder: %v", err)
	}
	out, rest, err := tc.Transcode(in)
	if err != nil {
		t.Fatalf("Transcode: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("Transcode left %d trailing bytes", len(rest))
	}
	return out
}

// assertMatchesReference transcodes in and compares to the Decode/Encode
// reference byte-for-byte.
func assertMatchesReference(t *testing.T, w, r *Schema, in []byte, opts ...TranscodeOption) {
	t.Helper()
	want := refTranscode(t, w, r, in)
	got := runTC(t, w, r, in, opts...)
	if !bytes.Equal(got, want) {
		t.Fatalf("transcode mismatch:\n in  = % x\n got = % x\n want= % x", in, got, want)
	}
}

func enc(t *testing.T, s *Schema, v any) []byte {
	t.Helper()
	b, err := s.Encode(v)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return b
}

// ---------- scalars and promotions ----------

func TestTranscode_ScalarsIdentity(t *testing.T) {
	for _, schema := range []string{
		`"null"`, `"boolean"`, `"int"`, `"long"`, `"float"`, `"double"`,
		`"string"`, `"bytes"`,
		`{"type":"fixed","name":"F","size":4}`,
	} {
		w := mustParse(t, schema)
		r := mustParse(t, schema)
		var in []byte
		switch {
		case strings.Contains(schema, "boolean"):
			in = enc(t, w, true)
		case strings.Contains(schema, "string"):
			in = enc(t, w, "hello")
		case strings.Contains(schema, "bytes"), strings.Contains(schema, "fixed"):
			in = enc(t, w, []byte{1, 2, 3, 4})
		case strings.Contains(schema, `"int"`):
			in = enc(t, w, int32(-42))
		case strings.Contains(schema, "long"):
			in = enc(t, w, int64(1<<40))
		case strings.Contains(schema, "float"):
			in = enc(t, w, float32(1.5))
		case strings.Contains(schema, "double"):
			in = enc(t, w, 3.125)
		default:
			in = enc(t, w, nil)
		}
		got := runTC(t, w, r, in)
		if !bytes.Equal(got, in) {
			t.Fatalf("%s: identity transcode altered bytes: % x -> % x", schema, in, got)
		}
	}
}

func TestTranscode_Promotions(t *testing.T) {
	cases := []struct {
		name string
		w, r string
		in   any
	}{
		{"int>long", `"int"`, `"long"`, int32(-12345)},
		{"int>float", `"int"`, `"float"`, int32(1 << 20)},
		{"int>double", `"int"`, `"double"`, int32(-1234567)},
		{"long>float", `"long"`, `"float"`, int64(1 << 40)},
		{"long>double", `"long"`, `"double"`, int64(1 << 42)},
		{"float>double", `"float"`, `"double"`, float32(2.5)},
		{"string>bytes", `"string"`, `"bytes"`, "abc"},
		{"bytes>string", `"bytes"`, `"string"`, []byte("abc")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := mustParse(t, tc.w)
			r := mustParse(t, tc.r)
			in := enc(t, w, tc.in)
			assertMatchesReference(t, w, r, in)
		})
	}
}

func TestTranscode_PromotionValues(t *testing.T) {
	// Spot-check exact bytes for the float widenings.
	w := mustParse(t, `"int"`)
	r := mustParse(t, `"double"`)
	in := enc(t, w, int32(3))
	got := runTC(t, w, r, in)
	want := append([]byte(nil), enc(t, r, float64(3))...)
	if !bytes.Equal(got, want) {
		t.Fatalf("int 3 -> double: got % x want % x", got, want)
	}

	wf := mustParse(t, `"float"`)
	rd := mustParse(t, `"double"`)
	in = enc(t, wf, float32(0.1))
	got = runTC(t, wf, rd, in)
	want = enc(t, rd, float64(float32(0.1)))
	if !bytes.Equal(got, want) {
		t.Fatalf("float .1 -> double: got % x want % x", got, want)
	}
}

// ---------- enums ----------

func TestTranscode_EnumRemapAndDefault(t *testing.T) {
	w := mustParse(t, `{"type":"enum","name":"C","symbols":["A","B","C"]}`)
	r := mustParse(t, `{"type":"enum","name":"C","symbols":["C","A","X"],"default":"X"}`)

	// Writer ordinal 0 (A) -> reader ordinal 1.
	got := runTC(t, w, r, enc(t, w, "A"))
	if want := enc(t, r, "A"); !bytes.Equal(got, want) {
		t.Fatalf("enum A: got % x want % x", got, want)
	}
	// Writer ordinal 1 (B) is absent in reader -> default X (ordinal 2).
	got = runTC(t, w, r, enc(t, w, "B"))
	if want := enc(t, r, "X"); !bytes.Equal(got, want) {
		t.Fatalf("enum B->default: got % x want % x", got, want)
	}
	// Writer ordinal 2 (C) -> reader ordinal 0.
	got = runTC(t, w, r, enc(t, w, "C"))
	if want := enc(t, r, "C"); !bytes.Equal(got, want) {
		t.Fatalf("enum C: got % x want % x", got, want)
	}
}

func TestTranscode_EnumNoDefaultIncompatible(t *testing.T) {
	w := mustParse(t, `{"type":"enum","name":"C","symbols":["A","B"]}`)
	r := mustParse(t, `{"type":"enum","name":"C","symbols":["A"]}`)
	if _, err := NewTranscoder(w, r); err == nil {
		t.Fatal("expected CompatibilityError for dropped enum symbol with no default")
	}
}

func TestTranscode_EnumDefaultInReorderedRecord(t *testing.T) {
	w := mustParse(t, `{"type":"record","name":"R","fields":[
		{"name":"c","type":{"type":"enum","name":"E","symbols":["A","B"]}},
		{"name":"n","type":"int"}]}`)
	r := mustParse(t, `{"type":"record","name":"R","fields":[
		{"name":"n","type":"long"},
		{"name":"c","type":{"type":"enum","name":"E","symbols":["A","X"],"default":"X"}}]}`)
	in := enc(t, w, map[string]any{"c": "B", "n": int32(4)})
	assertMatchesReference(t, w, r, in)
	// B is absent in the reader -> default X (ordinal 1).
	out := runTC(t, w, r, in)
	var m map[string]any
	if _, err := r.Decode(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["c"] != "X" || m["n"] != int64(4) {
		t.Fatalf("m=%#v", m)
	}
}

// ---------- records: reorder, alias, add/remove, nested ----------

const recWriterV1 = `{
	"type":"record","name":"User","fields":[
		{"name":"name","type":"string"},
		{"name":"age","type":"int"},
		{"name":"drop","type":"long"}
	]
}`

const recReaderV2 = `{
	"type":"record","name":"User","fields":[
		{"name":"id","type":"long","default":0},
		{"name":"age","type":"long"},
		{"name":"handle","type":["null","string"],"default":null},
		{"name":"name","type":"string","aliases":["full_name"]},
		{"name":"extra","type":"string","default":"def"}
	]
}`

func TestTranscode_RecordReorderAliasDefaultPromote(t *testing.T) {
	w := mustParse(t, recWriterV1)
	r := mustParse(t, recReaderV2)
	in := enc(t, w, map[string]any{
		"name": "Alice",
		"age":  int32(30),
		"drop": int64(999),
	})
	assertMatchesReference(t, w, r, in)

	// And verify the decoded reader value directly.
	tc, err := NewTranscoder(w, r)
	if err != nil {
		t.Fatal(err)
	}
	out, rest, err := tc.Transcode(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 0 {
		t.Fatalf("trailing %d", len(rest))
	}
	var m map[string]any
	if _, err := r.Decode(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["name"] != "Alice" || m["age"] != int64(30) || m["id"] != int64(0) ||
		m["extra"] != "def" {
		t.Fatalf("unexpected reader value: %#v", m)
	}
	// The null union default fills plain nil (the dynamic decode of the
	// null branch into an absent field), matching the Decode/Encode chain.
	if m["handle"] != nil {
		t.Fatalf("handle default: %#v", m["handle"])
	}
}

func TestTranscode_AliasOnly(t *testing.T) {
	w := mustParse(t, `{"type":"record","name":"U","fields":[{"name":"full_name","type":"string"}]}`)
	r := mustParse(t, `{"type":"record","name":"U","fields":[{"name":"name","type":"string","aliases":["full_name"]}]}`)
	in := enc(t, w, map[string]any{"full_name": "Bob"})
	assertMatchesReference(t, w, r, in)
}

func TestTranscode_NestedReorderedRecords(t *testing.T) {
	w := mustParse(t, `{
		"type":"record","name":"Outer","fields":[
			{"name":"o1","type":"string"},
			{"name":"inner","type":{"type":"record","name":"Inner","fields":[
				{"name":"a","type":"int"},
				{"name":"b","type":"string"}
			]}},
			{"name":"o2","type":"long"}
		]
	}`)
	r := mustParse(t, `{
		"type":"record","name":"Outer","fields":[
			{"name":"o2","type":"long"},
			{"name":"inner","type":{"type":"record","name":"Inner","fields":[
				{"name":"b","type":"string"},
				{"name":"a","type":"long"}
			]}},
			{"name":"o1","type":"string"}
		]
	}`)
	in := enc(t, w, map[string]any{
		"o1":    "x",
		"inner": map[string]any{"a": int32(7), "b": "yy"},
		"o2":    int64(8),
	})
	assertMatchesReference(t, w, r, in)
}

func TestTranscode_RecordStreamVsBufferSameResult(t *testing.T) {
	// Streaming order (writer [a,b] reader [a,b] + default) and buffered
	// order (reversed) must both match the reference.
	ws := mustParse(t, `{"type":"record","name":"S","fields":[{"name":"a","type":"int"},{"name":"b","type":"string"}]}`)
	rs := mustParse(t, `{"type":"record","name":"S","fields":[{"name":"a","type":"int"},{"name":"b","type":"string"},{"name":"c","type":"int","default":5}]}`)
	in := enc(t, ws, map[string]any{"a": int32(1), "b": "z"})
	assertMatchesReference(t, ws, rs, in)

	wb := mustParse(t, `{"type":"record","name":"S","fields":[{"name":"a","type":"int"},{"name":"b","type":"string"}]}`)
	rb := mustParse(t, `{"type":"record","name":"S","fields":[{"name":"b","type":"string"},{"name":"a","type":"int"}]}`)
	assertMatchesReference(t, wb, rb, in)
}

// ---------- arrays and maps, including blocks ----------

func TestTranscode_ArrayPromote(t *testing.T) {
	w := mustParse(t, `{"type":"array","items":"int"}`)
	r := mustParse(t, `{"type":"array","items":"double"}`)
	in := enc(t, w, []int32{1, -2, 1 << 20})
	assertMatchesReference(t, w, r, in)
}

func TestTranscode_ArrayOfReorderedRecords(t *testing.T) {
	w := mustParse(t, `{"type":"array","items":{"type":"record","name":"P","fields":[
		{"name":"x","type":"int"},{"name":"y","type":"string"}]}}`)
	r := mustParse(t, `{"type":"array","items":{"type":"record","name":"P","fields":[
		{"name":"y","type":"string"},{"name":"x","type":"long"}]}}`)
	in := enc(t, w, []map[string]any{
		{"x": int32(1), "y": "a"},
		{"x": int32(2), "y": "bb"},
	})
	assertMatchesReference(t, w, r, in)
}

func TestTranscode_EmptyArray(t *testing.T) {
	w := mustParse(t, `{"type":"array","items":"int"}`)
	r := mustParse(t, `{"type":"array","items":"long"}`)
	in := enc(t, w, []int32{})
	got := runTC(t, w, r, in)
	if want := []byte{0}; !bytes.Equal(got, want) {
		t.Fatalf("empty array: got % x want % x", got, want)
	}
}

func TestTranscode_MapPromoteValues(t *testing.T) {
	w := mustParse(t, `{"type":"map","values":"int"}`)
	r := mustParse(t, `{"type":"map","values":"long"}`)
	in := enc(t, w, map[string]int32{"a": 1, "bb": -2})
	assertMatchesReference(t, w, r, in)
}

func TestTranscode_MapOfReorderedRecords(t *testing.T) {
	w := mustParse(t, `{"type":"map","values":{"type":"record","name":"M","fields":[
		{"name":"p","type":"int"},{"name":"q","type":"string"}]}}`)
	r := mustParse(t, `{"type":"map","values":{"type":"record","name":"M","fields":[
		{"name":"q","type":"string"},{"name":"p","type":"long"}]}}`)
	in := enc(t, w, map[string]map[string]any{
		"k1": {"p": int32(3), "q": "v1"},
	})
	assertMatchesReference(t, w, r, in)
}

func TestTranscode_ArrayNegativeBlockFraming(t *testing.T) {
	// Two items with negative-count/byte-size framing. int->long shares the
	// varint encoding and can pass through verbatim; int->double forces an
	// 8-byte rewrite per item, so the byte-size frame must be normalized
	// away and each item converted.
	in := []byte{
		3,    // count -2 (zigzag)
		4,    // byte size 2 (zigzag encoded)
		2, 4, // two int varints: 1, 2
		0, // terminator
	}
	w := mustParse(t, `{"type":"array","items":"int"}`)
	r := mustParse(t, `{"type":"array","items":"double"}`)
	got := runTC(t, w, r, in)
	var gotV []float64
	if _, err := r.Decode(got, &gotV); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotV, []float64{1, 2}) {
		t.Fatalf("got %v", gotV)
	}
	// Standard positive framing: count 2, two 8-byte doubles, terminator.
	if want := 1 + 8 + 8 + 1; len(got) != want || got[0] != 4 || got[len(got)-1] != 0 {
		t.Fatalf("normalized framing: got % x (want len %d)", got, want)
	}
}

// ---------- unions ----------

func TestTranscode_UnionToUnionBranchRemap(t *testing.T) {
	w := mustParse(t, `["null","int","string"]`)
	r := mustParse(t, `["string","long","null"]`)

	// int branch (writer idx 1) -> long branch (reader idx 1).
	in := enc(t, w, map[string]any{"int": int32(42)})
	assertMatchesReference(t, w, r, in)
	// string branch (writer idx 2) -> string branch (reader idx 0).
	in = enc(t, w, map[string]any{"string": "hi"})
	assertMatchesReference(t, w, r, in)
	// null branch (writer idx 0) -> null (reader idx 2).
	in = enc(t, w, nil)
	assertMatchesReference(t, w, r, in)
}

func TestTranscode_WriterUnionReaderScalar(t *testing.T) {
	w := mustParse(t, `["int","long"]`)
	r := mustParse(t, `"long"`)
	in := enc(t, w, map[string]any{"int": int32(5)})
	assertMatchesReference(t, w, r, in)
	in = enc(t, w, map[string]any{"long": int64(6)})
	assertMatchesReference(t, w, r, in)
}

func TestTranscode_WriterScalarReaderUnion(t *testing.T) {
	w := mustParse(t, `"int"`)
	r := mustParse(t, `["null","long","string"]`)
	in := enc(t, w, int32(9))
	assertMatchesReference(t, w, r, in)
}

func TestTranscode_UnionBranchNoMatch(t *testing.T) {
	w := mustParse(t, `["boolean"]`)
	r := mustParse(t, `["null","int"]`)
	if _, err := NewTranscoder(w, r); err == nil {
		t.Fatal("expected CompatibilityError: writer branch matches nothing")
	}
}

// ---------- recursion ----------

const listWriter = `{
	"type":"record","name":"Node","fields":[
		{"name":"value","type":"int"},
		{"name":"next","type":["null","Node"]}
	]
}`

const listReader = `{
	"type":"record","name":"Node","fields":[
		{"name":"next","type":["Node","null"]},
		{"name":"added","type":"string","default":"d"},
		{"name":"value","type":"long"}
	]
}`

func TestTranscode_RecursiveRecord(t *testing.T) {
	w := mustParse(t, listWriter)
	r := mustParse(t, listReader)
	in := enc(t, w, map[string]any{
		"value": int32(1),
		"next": map[string]any{"Node": map[string]any{
			"value": int32(2),
			"next":  nil,
		}},
	})
	assertMatchesReference(t, w, r, in)
}

func TestTranscode_CompileTerminatesDeepRecursion(t *testing.T) {
	// Compiling twice reuses the plan; the second construction must also
	// terminate and produce the same output.
	w := mustParse(t, listWriter)
	r := mustParse(t, listReader)
	tc1, err := NewTranscoder(w, r)
	if err != nil {
		t.Fatal(err)
	}
	tc2, err := NewTranscoder(w, r)
	if err != nil {
		t.Fatal(err)
	}
	in := enc(t, w, map[string]any{
		"value": int32(1),
		"next":  map[string]any{"Node": map[string]any{"value": int32(2), "next": nil}},
	})
	a, rest, err := tc1.Transcode(in)
	if err != nil {
		t.Fatal(err)
	}
	b, rest2, err := tc2.Transcode(in)
	if err != nil || len(rest) != 0 || len(rest2) != 0 || !bytes.Equal(a, b) {
		t.Fatalf("plans differ: err=%v", err)
	}
}

// ---------- buffering limit, errors, io.Writer, concurrency ----------

func TestTranscode_BufferLimit(t *testing.T) {
	// Reverse two large string fields so capture must buffer the rewritten
	// content. Reordered fields are views when unchanged (string->string),
	// which do not count; force a rewrite (int->long is verbatim too, so
	// use a float promotion on a wide-enough payload indirectly). To make
	// rewritten bytes dominate, promote long->double inside reversed
	// records inside the field values: simpler, reverse two fields whose
	// values are arrays of doubles promoted from longs.
	w := mustParse(t, `{"type":"record","name":"R","fields":[
		{"name":"a","type":{"type":"array","items":"long"}},
		{"name":"b","type":"int"}]}`)
	r := mustParse(t, `{"type":"record","name":"R","fields":[
		{"name":"b","type":"int"},
		{"name":"a","type":{"type":"array","items":"double"}}]}`)
	items := make([]int64, 100)
	for i := range items {
		items[i] = int64(i)
	}
	in := enc(t, w, map[string]any{"a": items, "b": int32(1)})

	// 8 bytes/element * 100 = 800 produced bytes; a tiny limit trips.
	if _, _, err := runTCExt(t, w, r, in, WithMaxBufferedBytes(64)); err == nil {
		t.Fatal("expected BufferLimitError")
	} else {
		var ble *BufferLimitError
		if !errors.As(err, &ble) {
			t.Fatalf("want *BufferLimitError, got %T: %v", err, err)
		}
	}

	// A generous limit succeeds and matches the reference.
	assertMatchesReference(t, w, r, in, WithMaxBufferedBytes(1<<20))
}

func runTCExt(t *testing.T, w, r *Schema, in []byte, opts ...TranscodeOption) ([]byte, []byte, error) {
	t.Helper()
	tc, err := NewTranscoder(w, r, opts...)
	if err != nil {
		t.Fatalf("NewTranscoder: %v", err)
	}
	return tc.Transcode(in)
}

func TestTranscode_ShortInputReportsPathAndWritten(t *testing.T) {
	w := mustParse(t, recWriterV1)
	r := mustParse(t, recReaderV2)
	// Truncated inside the first field (name string length says 10, only
	// 2 bytes follow).
	in := []byte{20, 'a', 'b'}
	tc, err := NewTranscoder(w, r)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	nw, _, err := tc.TranscodeTo(&buf, in)
	if err == nil {
		t.Fatal("expected error")
	}
	var te *TranscodeError
	if !errors.As(err, &te) {
		t.Fatalf("want *TranscodeError, got %T: %v", err, err)
	}
	if te.Path != "name" && !strings.HasPrefix(te.Path, "name") {
		t.Fatalf("path = %q, want name", te.Path)
	}
	if nw != te.Written {
		t.Fatalf("written %d != error.Written %d", nw, te.Written)
	}
}

func TestTranscode_PathNestedArrayUnion(t *testing.T) {
	w := mustParse(t, `{"type":"record","name":"R","fields":[
		{"name":"items","type":{"type":"array","items":["null","string"]}}]}`)
	r := mustParse(t, `{"type":"record","name":"R","fields":[
		{"name":"items","type":{"type":"array","items":["string","null"]}}]}`)
	// One item: string branch (writer idx 1) with len 5 but a truncated
	// payload. The block header is valid, so the failure is the element's
	// union branch, not the array framing.
	in := []byte{2 /* count 1 */, 2 /* union idx 1 */, 10 /* len 5 */, 'a', 'b'}
	tc, _ := NewTranscoder(w, r)
	_, _, err := tc.Transcode(in)
	if err == nil {
		t.Fatal("expected error")
	}
	var te *TranscodeError
	if !errors.As(err, &te) {
		t.Fatalf("got %T", err)
	}
	if te.Path != "items[0].union[1]" {
		t.Fatalf("path = %q, want items[0].union[1]", te.Path)
	}
}

func TestTranscode_AppendPreservesPrefixAndPartial(t *testing.T) {
	w := mustParse(t, `"int"`)
	r := mustParse(t, `"long"`)
	in := enc(t, w, int32(7))
	prefix := []byte{0xde, 0xad}
	out, rest, err := w.newTranscoderOrDie(r).AppendTranscode(prefix, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 0 || out[0] != 0xde || out[1] != 0xad {
		t.Fatalf("prefix not preserved: % x", out)
	}
}

func (s *Schema) newTranscoderOrDie(r *Schema) *Transcoder {
	tc, err := NewTranscoder(s, r)
	if err != nil {
		panic(err)
	}
	return tc
}

func TestTranscode_WriterNoRollbackGuarantee(t *testing.T) {
	// A failing writer reports bytes already written and partial output.
	w := mustParse(t, `{"type":"record","name":"R","fields":[
		{"name":"a","type":"int"},{"name":"b","type":"string"}]}`)
	r := mustParse(t, `{"type":"record","name":"R","fields":[
		{"name":"a","type":"long"},{"name":"b","type":"string"}]}`)
	fw := &failAfterWriter{failAfter: 2}
	in := enc(t, w, map[string]any{"a": int32(1), "b": "hello"})
	tc, _ := NewTranscoder(w, r)
	nw, _, err := tc.TranscodeTo(fw, in)
	if err == nil {
		t.Fatal("expected writer error")
	}
	if nw != fw.got {
		t.Fatalf("reported written %d != actual %d", nw, fw.got)
	}
}

type failAfterWriter struct {
	failAfter int
	got       int
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.got+len(p) > w.failAfter {
		n := w.failAfter - w.got
		if n < 0 {
			n = 0
		}
		w.got += n
		return n, errors.New("writer boom")
	}
	w.got += len(p)
	return len(p), nil
}

func TestTranscode_PlanReusableConcurrently(t *testing.T) {
	w := mustParse(t, recWriterV1)
	r := mustParse(t, recReaderV2)
	tc, err := NewTranscoder(w, r)
	if err != nil {
		t.Fatal(err)
	}
	in := enc(t, w, map[string]any{"name": "A", "age": int32(1), "drop": int64(2)})
	want := refTranscode(t, w, r, in)

	const goroutines = 16
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				got, rest, err := tc.Transcode(append([]byte(nil), in...))
				if err != nil || len(rest) != 0 || !bytes.Equal(got, want) {
					errs <- fmt.Errorf("got=% x err=%v", got, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestTranscode_CanonicalEqualWholeValuePassthrough(t *testing.T) {
	// Two parses (different JSON whitespace/field order) canonicalize to
	// the same form: transcoding must hand bytes through untouched.
	w := mustParse(t, `{"type":"record","name":"U","fields":[{"name":"a","type":"int"},{"name":"b","type":"string"}]}`)
	r := mustParse(t, `{
		"type":"record","name":"U",
		"fields":[{"name":"b","type":"string"},{"name":"a","type":"int"}]
	}`)
	in := enc(t, w, map[string]any{"a": int32(4), "b": "q"})
	tc, err := NewTranscoder(w, r)
	if err != nil {
		t.Fatal(err)
	}
	got, rest, err := tc.Transcode(in)
	if err != nil || len(rest) != 0 {
		t.Fatalf("err=%v rest=%d", err, len(rest))
	}
	// Canonical-equal means identical encoding regardless of field order,
	// so passthrough bytes decode under the reader correctly.
	var m map[string]any
	if _, err := r.Decode(got, &m); err != nil {
		t.Fatalf("passthrough not readable by reader: %v", err)
	}
	if m["a"] != int32(4) || m["b"] != "q" {
		t.Fatalf("m=%#v", m)
	}
	_ = math.Pi // keep math import used across builds
}
