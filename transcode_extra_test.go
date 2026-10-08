package avro_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/twmb/avro"
)

// TestTranscode_UnionUnionBranchMapping rewrites writer union branches onto a
// reordered reader union: null moves index 0->1, an int promotes to long and
// moves 1->0.
func TestTranscode_UnionUnionBranchMapping(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`["null","int"]`)
	reader := avro.MustParse(`["long","null"]`)

	// null
	wire := wireInt(nil, 0)
	got := transcodeMust(t, writer, reader, wire)
	want := decodeEncodeReference(t, writer, reader, wire)
	if !bytes.Equal(got, want) {
		t.Fatalf("null branch: got %x want %x", got, want)
	}
	var v any
	if _, err := reader.Decode(got, &v); err != nil || v != nil {
		t.Fatalf("null branch decode: %v %#v", err, v)
	}

	// int branch (writer index 1) -> long branch (reader index 0)
	wire = append(wireInt(nil, 1), wireInt(nil, 123)...)
	got = transcodeMust(t, writer, reader, wire)
	want = decodeEncodeReference(t, writer, reader, wire)
	if !bytes.Equal(got, want) {
		t.Fatalf("int branch: got %x want %x", got, want)
	}
	v = nil
	if _, err := reader.Decode(got, &v); err != nil {
		t.Fatalf("long branch decode: %v", err)
	}
	if v.(int64) != 123 {
		t.Fatalf("branch value wrong: %#v", v)
	}
}

// TestTranscode_WriterUnionToNonUnion drops the writer's branch index.
func TestTranscode_WriterUnionToNonUnion(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`["int","long"]`)
	reader := avro.MustParse(`"long"`)
	wire := append(wireInt(nil, 1), wireLong(nil, 77)...) // branch 1: long
	got := transcodeMust(t, writer, reader, wire)
	want := decodeEncodeReference(t, writer, reader, wire)
	if !bytes.Equal(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

// TestTranscode_NonUnionToReaderUnion emits the selected reader branch index.
func TestTranscode_NonUnionToReaderUnion(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`"int"`)
	reader := avro.MustParse(`["null","long"]`)
	wire := wireInt(nil, 9)
	got := transcodeMust(t, writer, reader, wire)
	want := decodeEncodeReference(t, writer, reader, wire)
	if !bytes.Equal(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

// TestTranscode_UnionIncompatibleBranch fails compilation.
func TestTranscode_UnionIncompatibleBranch(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`["string"]`)
	reader := avro.MustParse(`["int"]`)
	if _, err := avro.Transcode(writer, reader); err == nil {
		t.Fatal("expected CompatibilityError")
	} else {
		var ce *avro.CompatibilityError
		if !errors.As(err, &ce) {
			t.Fatalf("want CompatibilityError got %T", err)
		}
	}
}

// TestTranscode_UnionDefaultFillsDefaultBranch: a reader-only union field
// takes the field default, encoding its selected branch.
func TestTranscode_UnionDefaultFillsDefaultBranch(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"a","type":"int"}]}`)
	reader := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"a","type":"int"},
		{"name":"u","type":["null","string"],"default":null}]}`)
	wire := mustEncode(t, writer, map[string]any{"a": int32(1)})
	got := transcodeMust(t, writer, reader, wire)
	want := decodeEncodeReference(t, writer, reader, wire)
	if !bytes.Equal(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

// TestTranscode_NestedRecordUnionEnum combines a reordered union of enum
// records with enum defaulting inside a field-reordered outer record.
func TestTranscode_NestedRecordUnionEnum(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`{
		"type":"record","name":"Outer","fields":[
			{"name":"kind","type":[
				{"type":"record","name":"A","fields":[{"name":"x","type":"int"}]},
				{"type":"record","name":"B","fields":[{"name":"e","type":
					{"type":"enum","name":"E","symbols":["ONE","TWO"]}}]}]},
			{"name":"n","type":"long"}]}`)
	reader := avro.MustParse(`{
		"type":"record","name":"Outer","fields":[
			{"name":"n","type":"long"},
			{"name":"kind","type":[
				{"type":"record","name":"B","fields":[{"name":"e","type":
					{"type":"enum","name":"E","symbols":["TWO","ONE","D"],"default":"D"}}]},
				{"type":"record","name":"A","fields":[{"name":"x","type":"long"}]}]}]}`)

	// A{x=400}: writer branch 0 -> reader branch 1, int->long.
	wireA := append(wireInt(nil, 0), wireInt(nil, 400)...) // kind branch 0, x
	wireA = append(wireA, wireLong(nil, 5)...)             // n
	got := transcodeMust(t, writer, reader, wireA)
	if want := decodeEncodeReference(t, writer, reader, wireA); !bytes.Equal(got, want) {
		t.Fatalf("A record union: got %x want %x", got, want)
	}

	// B{e=ONE}: writer branch 1 -> reader branch 0, enum ONE index 1->1
	// (reader order TWO=0,ONE=1).
	wireB := append(wireInt(nil, 1), wireInt(nil, 0)...) // kind branch 1, e=ONE
	wireB = append(wireB, wireLong(nil, 5)...)           // n
	got = transcodeMust(t, writer, reader, wireB)
	if want := decodeEncodeReference(t, writer, reader, wireB); !bytes.Equal(got, want) {
		t.Fatalf("B record union: got %x want %x", got, want)
	}
}

// TestTranscode_ArrayPromotion transcodes each int element to long across
// multiple blocks, including negative-count framing.
func TestTranscode_ArrayPromotion(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`{"type":"array","items":"int"}`)
	reader := avro.MustParse(`{"type":"array","items":"long"}`)

	var wire []byte
	wire = append(wire, wireLong(nil, 2)...)
	wire = append(wire, wireInt(nil, 1)...)
	wire = append(wire, wireInt(nil, -2)...)
	wire = append(wire, wireLong(nil, 1)...)
	wire = append(wire, wireInt(nil, 3)...)
	wire = append(wire, 0)

	got := transcodeMust(t, writer, reader, wire)
	var agot []int64
	if _, err := reader.Decode(got, &agot); err != nil {
		t.Fatalf("decode transcoded array: %v", err)
	}
	if len(agot) != 3 || agot[0] != 1 || agot[1] != -2 || agot[2] != 3 {
		t.Fatalf("array values wrong: %#v", agot)
	}

	// Negative-count framing: 2 items, byte size following.
	var neg []byte
	neg = append(neg, wireLong(nil, -2)...)
	body := append(wireInt(nil, 1), wireInt(nil, 2)...)
	neg = append(neg, wireLong(nil, int64(len(body)))...)
	neg = append(neg, body...)
	neg = append(neg, 0)
	got = transcodeMust(t, writer, reader, neg)
	agot = nil
	if _, err := reader.Decode(got, &agot); err != nil {
		t.Fatalf("decode negative-frame array: %v", err)
	}
	if len(agot) != 2 || agot[0] != 1 || agot[1] != 2 {
		t.Fatalf("negative-frame values wrong: %#v", agot)
	}
}

// TestTranscode_MapPromotion transcodes map values int->long.
func TestTranscode_MapPromotion(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`{"type":"map","values":"int"}`)
	reader := avro.MustParse(`{"type":"map","values":"long"}`)

	var wire []byte
	wire = append(wire, wireLong(nil, 2)...)
	wire = append(wire, mapEntryWire("a", wireInt(nil, 1))...)
	wire = append(wire, mapEntryWire("bb", wireInt(nil, 2))...)
	wire = append(wire, 0)

	got := transcodeMust(t, writer, reader, wire)
	want := decodeEncodeReference(t, writer, reader, wire)
	if !bytes.Equal(got, want) {
		t.Fatalf("map got %x want %x", got, want)
	}
	var m map[string]int64
	if _, err := reader.Decode(got, &m); err != nil {
		t.Fatalf("decode map: %v", err)
	}
	if m["a"] != 1 || m["bb"] != 2 {
		t.Fatalf("map values wrong: %#v", m)
	}
}

func mapEntryWire(key string, valWire []byte) []byte {
	b := wireLong(nil, int64(len(key)))
	b = append(b, key...)
	return append(b, valWire...)
}

// TestTranscode_ArrayOfReorderedRecords nests a reordered record inside an
// array and checks error path indexing points at the failing element.
func TestTranscode_ArrayOfReorderedRecords(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`{"type":"array","items":{"type":"record","name":"P",
		"fields":[{"name":"a","type":"int"},{"name":"b","type":"string"}]}}`)
	reader := avro.MustParse(`{"type":"array","items":{"type":"record","name":"P",
		"fields":[{"name":"b","type":"string"},{"name":"a","type":"long"}]}}`)
	vals := []any{
		map[string]any{"a": int32(1), "b": "x"},
		map[string]any{"a": int32(2), "b": "yy"},
		map[string]any{"a": int32(3), "b": "zzz"},
	}
	var wire []byte
	wire = append(wire, wireLong(nil, 3)...)
	for _, v := range vals {
		wire = append(wire, mustEncode(t, avro.MustParse(`{"type":"record","name":"P",
			"fields":[{"name":"a","type":"int"},{"name":"b","type":"string"}]}`), v)...)
	}
	wire = append(wire, 0)
	got := transcodeMust(t, writer, reader, wire)
	want := decodeEncodeReference(t, writer, reader, wire)
	if !bytes.Equal(got, want) {
		t.Fatalf("array-of-records got %x want %x", got, want)
	}
}

// TestTranscode_ZeroCopySameSchema verifies an identical schema forwards the
// input bytes untouched.
func TestTranscode_ZeroCopySameSchema(t *testing.T) {
	t.Parallel()
	s := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"s","type":"string"},
		{"name":"arr","type":{"type":"array","items":{"type":"fixed","name":"F","size":3}}}]}`)
	prefix := []byte{0xAA, 0xBB}
	wire := mustEncode(t, s, map[string]any{
		"s":   "hi",
		"arr": [][3]byte{{1, 2, 3}, {4, 5, 6}},
	})
	trailing := []byte{0xCC}
	tr, err := avro.Transcode(s, s)
	if err != nil {
		t.Fatal(err)
	}
	dst := append([]byte{}, prefix...)
	src := append(append([]byte{}, wire...), trailing...)
	out, rest, err := tr.Append(dst, src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, append(prefix, wire...)) {
		t.Fatalf("identity output differs:\ngot  %x\nwant %x", out, append(prefix, wire...))
	}
	if !bytes.Equal(rest, trailing) {
		t.Fatalf("trailing bytes not returned verbatim: %x", rest)
	}
}

// TestTranscode_ReorderBufferLimit reports a StageReorder error wrapping
// ErrReorderBufferExceeded once the held field crosses the cap.
func TestTranscode_ReorderBufferLimit(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"big","type":"string"},
		{"name":"small","type":"int"}]}`)
	reader := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"small","type":"int"},
		{"name":"big","type":"string"}]}`)

	big := strings.Repeat("z", 100)
	wire := mustEncode(t, writer, map[string]any{"big": big, "small": int32(1)})

	// Large enough succeeds and matches the reference.
	got := transcodeMust(t, writer, reader, wire, avro.WithReorderBuffer(1024))
	if want := decodeEncodeReference(t, writer, reader, wire); !bytes.Equal(got, want) {
		t.Fatal("buffered transcode mismatch")
	}

	// Smaller than the field fails and names the field.
	tr, err := avro.Transcode(writer, reader, avro.WithReorderBuffer(16))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	_, _, err = tr.Append(nil, wire)
	var te *avro.TranscodeError
	if !errors.As(err, &te) {
		t.Fatalf("want *TranscodeError, got %T %v", err, err)
	}
	if te.Stage != avro.StageReorder {
		t.Fatalf("stage = %q, want reorder", te.Stage)
	}
	if !errors.Is(err, avro.ErrReorderBufferExceeded) {
		t.Fatalf("error does not wrap ErrReorderBufferExceeded: %v", err)
	}
	if te.Path != "big" {
		t.Fatalf("path = %q, want big", te.Path)
	}

	// Zero disables buffering entirely.
	if _, err := avro.Transcode(writer, reader, avro.WithReorderBuffer(0)); err == nil {
		t.Fatal("zero limit with a reordered schema should fail at compile time")
	} else if !errors.Is(err, avro.ErrReorderBufferExceeded) {
		t.Fatalf("compile err = %v, want ErrReorderBufferExceeded", err)
	}
}

// TestTranscode_TruncatedReportsPathAndWritten checks the read stage, dotted
// path, and Written accounting on an append destination with a prefix.
func TestTranscode_TruncatedReportsPathAndWritten(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"a","type":"int"},
		{"name":"b","type":{"type":"record","name":"I","fields":[
			{"name":"x","type":"int"},
			{"name":"s","type":"string"}]}}]}`)
	reader := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"a","type":"long"},
		{"name":"b","type":{"type":"record","name":"I","fields":[
			{"name":"x","type":"long"},
			{"name":"s","type":"string"}]}}]}`)
	// a=7 (1 byte), b.x=1 (1 byte), then a truncated string: length 10 with
	// only 2 content bytes.
	wire := []byte{14, 2, 20, 'h', 'i'}

	tr, err := avro.Transcode(writer, reader)
	if err != nil {
		t.Fatal(err)
	}
	prefix := []byte{1, 2, 3}
	out, _, err := tr.Append(append([]byte{}, prefix...), wire)
	var te *avro.TranscodeError
	if !errors.As(err, &te) {
		t.Fatalf("want TranscodeError, got %T %v", err, err)
	}
	if te.Stage != avro.StageRead {
		t.Fatalf("stage = %q want read", te.Stage)
	}
	if te.Path != "b.s" {
		t.Fatalf("path = %q want b.s", te.Path)
	}
	var sb *avro.ShortBufferError
	if !errors.As(err, &sb) {
		t.Fatalf("want ShortBufferError underneath, got %v", err)
	}
	// Written counts only what this call appended: a as long (1) + b.x as
	// long (1) = 2, not the 3-byte prefix. On failure the returned slice is
	// the original dst, so out keeps just the prefix.
	if te.Written != 2 {
		t.Fatalf("written = %d, want 2", te.Written)
	}
	if !bytes.Equal(out[:len(prefix)], prefix) {
		t.Fatal("prefix dst was not preserved on failure")
	}
}

// TestTranscode_WriteStageAndWritten uses a failing io.Writer to exercise
// StageWrite and the no-rollback accounting.
func TestTranscode_WriteStageAndWritten(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"a","type":"int"},
		{"name":"b","type":"string"}]}`)
	reader := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"a","type":"long"},
		{"name":"b","type":"string"}]}`)
	wire := mustEncode(t, writer, map[string]any{"a": int32(5), "b": "payload"})

	fw := &failingWriter{failAfter: 1}
	tr, err := avro.Transcode(writer, reader)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tr.Write(fw, wire)
	var te *avro.TranscodeError
	if !errors.As(err, &te) {
		t.Fatalf("want TranscodeError, got %T %v", err, err)
	}
	if te.Stage != avro.StageWrite {
		t.Fatalf("stage = %q want write", te.Stage)
	}
	if te.Written != 1 {
		t.Fatalf("written = %d want 1", te.Written)
	}
}

type failingWriter struct{ failAfter, written int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.written >= w.failAfter {
		return 0, io.ErrShortWrite
	}
	n := min(w.failAfter-w.written, len(p))
	w.written += n
	return n, io.ErrShortWrite
}

// TestTranscode_ConcurrentReuse uses one compiled plan from many goroutines.
func TestTranscode_ConcurrentReuse(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"n","type":"int"},
		{"name":"s","type":"string"}]}`)
	reader := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"s","type":"string"},
		{"name":"n","type":"long"},
		{"name":"extra","type":"int","default":0}]}`)
	tr, err := avro.Transcode(writer, reader)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				v := map[string]any{"n": int32(g*1000 + i), "s": fmt.Sprintf("g%d-i%d", g, i)}
				wire := mustEncode(t, writer, v)
				want := decodeEncodeReference(t, writer, reader, wire)
				got, rest, gerr := tr.Append(nil, wire)
				if gerr != nil || len(rest) != 0 || !bytes.Equal(got, want) {
					t.Errorf("goroutine %d iter %d: err=%v rest=%d match=%v", g, i, gerr, len(rest), bytes.Equal(got, want))
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestTranscode_RecursiveSchema terminates compilation and correctly rewrites
// a self-referential record (linked list) with reordered fields and a
// promoted inner value.
func TestTranscode_RecursiveSchema(t *testing.T) {
	t.Parallel()
	writer := `{"type":"record","name":"Node","fields":[
		{"name":"value","type":"int"},
		{"name":"next","type":["null","Node"]}]}`
	reader := `{"type":"record","name":"Node","fields":[
		{"name":"next","type":["null","Node"]},
		{"name":"value","type":"long"}]}`
	w := avro.MustParse(writer)
	r := avro.MustParse(reader)

	// value=1, next union branch 1 -> Node{value=2, next null(0)}
	var wire []byte
	wire = append(wire, wireInt(nil, 1)...)
	wire = append(wire, wireInt(nil, 1)...)
	wire = append(wire, wireInt(nil, 2)...)
	wire = append(wire, wireInt(nil, 0)...)

	// Must compile without looping.
	tr, err := avro.Transcode(w, r)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, rest, err := tr.Append(nil, wire)
	if err != nil {
		t.Fatalf("transcode: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("rest %d", len(rest))
	}
	want := decodeEncodeReference(t, w, r, wire)
	if !bytes.Equal(got, want) {
		t.Fatalf("recursive got %x want %x", got, want)
	}
}

// TestTranscode_RecursiveSchemaTerminates guards the compile itself
// returning, independent of data.
func TestTranscode_RecursiveSchemaCompiles(t *testing.T) {
	t.Parallel()
	writer := `{"type":"record","name":"N","fields":[
		{"name":"a","type":"int"},
		{"name":"self","type":["null","N"]}]}`
	reader := `{"type":"record","name":"N","fields":[
		{"name":"self","type":["null","N"]},
		{"name":"a","type":"long"}]}`
	if _, err := avro.Transcode(avro.MustParse(writer), avro.MustParse(reader)); err != nil {
		t.Fatalf("recursive compile: %v", err)
	}
}

// TestTranscode_StreamableWithDefaultsAndDrops covers the non-buffered path
// when matched fields stay ordered but writer-only fields are dropped and
// reader-only defaults are inserted both before, between, and after matched
// fields.
func TestTranscode_StreamableWithDefaultsAndDrops(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"drop0","type":"boolean"},
		{"name":"a","type":"int"},
		{"name":"drop1","type":"string"},
		{"name":"b","type":"string"},
		{"name":"drop2","type":{"type":"array","items":"null"}}]}`)
	reader := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"before","type":"int","default":-1},
		{"name":"a","type":"long"},
		{"name":"middle","type":"string","default":"m"},
		{"name":"b","type":"string"},
		{"name":"after","type":"int","default":9}]}`)
	wire := mustEncode(t, writer, map[string]any{
		"drop0": true, "a": int32(4), "drop1": "x", "b": "bb",
		"drop2": []any{},
	})
	got := transcodeMust(t, writer, reader, wire)
	want := decodeEncodeReference(t, writer, reader, wire)
	if !bytes.Equal(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
	var m map[string]any
	if _, err := reader.Decode(got, &m); err != nil {
		t.Fatal(err)
	}
	if m["before"] != int32(-1) || m["a"] != int64(4) || m["middle"] != "m" ||
		m["b"] != "bb" || m["after"] != int32(9) {
		t.Fatalf("filled defaults wrong: %#v", m)
	}
}

// TestTranscode_FixedAndEmptyRecord pass through unchanged.
func TestTranscode_FixedAndEmptyRecord(t *testing.T) {
	t.Parallel()
	s := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"f","type":{"type":"fixed","name":"H","size":4}},
		{"name":"e","type":{"type":"record","name":"Empty","fields":[]}}]}`)
	wire := []byte{9, 9, 9, 9}
	got := transcodeMust(t, s, s, wire)
	if !bytes.Equal(got, wire) {
		t.Fatalf("fixed/empty got %x want %x", got, wire)
	}
}

// TestTranscode_AliasOnNamedType matches a record through a type alias while
// its fields are reordered.
func TestTranscode_AliasOnNamedType(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`{"type":"record","name":"Outer","fields":[
		{"name":"p","type":{"type":"record","name":"OldPoint","fields":[
			{"name":"x","type":"int"},{"name":"y","type":"int"}]}}]}`)
	reader := avro.MustParse(`{"type":"record","name":"Outer","fields":[
		{"name":"p","type":{"type":"record","name":"NewPoint","aliases":["OldPoint"],"fields":[
			{"name":"y","type":"int"},{"name":"x","type":"int"}]}}]}`)
	wire := mustEncode(t, writer, map[string]any{
		"p": map[string]any{"x": int32(1), "y": int32(2)},
	})
	got := transcodeMust(t, writer, reader, wire)
	want := decodeEncodeReference(t, writer, reader, wire)
	if !bytes.Equal(got, want) {
		t.Fatalf("alias record got %x want %x", got, want)
	}
}

// TestTranscode_MissingDefaultRejected at compile time.
func TestTranscode_MissingDefaultRejected(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`{"type":"record","name":"R","fields":[{"name":"a","type":"int"}]}`)
	reader := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"a","type":"int"},{"name":"b","type":"int"}]}`)
	var ce *avro.CompatibilityError
	if _, err := avro.Transcode(writer, reader); !errors.As(err, &ce) {
		t.Fatalf("want CompatibilityError, got %v", err)
	}
}

// TestTranscode_StreamableGapDefault streams when matched fields stay
// ascending but a default-only reader slot sits between them.
func TestTranscode_StreamableGapDefault(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"a","type":"int"},
		{"name":"c","type":"string"}]}`)
	reader := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"a","type":"long"},
		{"name":"b","type":"int","default":11},
		{"name":"c","type":"string"}]}`)
	wire := mustEncode(t, writer, map[string]any{"a": int32(3), "c": "z"})
	got := transcodeMust(t, writer, reader, wire, avro.WithReorderBuffer(0))
	want := decodeEncodeReference(t, writer, reader, wire)
	if !bytes.Equal(got, want) {
		t.Fatalf("gap-default got %x want %x", got, want)
	}
}

// TestTranscode_DescendingWithGap buffers a descending match and still
// produces the reference output.
func TestTranscode_DescendingWithGap(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"c","type":"string"},
		{"name":"a","type":"int"}]}`)
	reader := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"a","type":"long"},
		{"name":"b","type":"int","default":11},
		{"name":"c","type":"string"}]}`)
	wire := mustEncode(t, writer, map[string]any{"c": "z", "a": int32(3)})
	got := transcodeMust(t, writer, reader, wire)
	want := decodeEncodeReference(t, writer, reader, wire)
	if !bytes.Equal(got, want) {
		t.Fatalf("descending got %x want %x", got, want)
	}
	var m map[string]any
	if _, err := reader.Decode(got, &m); err != nil {
		t.Fatal(err)
	}
	if m["a"] != int64(3) || m["b"] != int32(11) || m["c"] != "z" {
		t.Fatalf("descending values wrong: %#v", m)
	}
}

// fuzzSchemas is the evolving pair for the parity fuzz: reordered fields,
// aliases, dropped and added fields, an enum remap with default, and
// promotions, while container/union shapes stay copy-safe so a decoded
// value comparison is a complete oracle.
var (
	fuzzWriter = avro.MustParse(`{
		"type":"record","name":"Fuzz",
		"fields":[
			{"name":"i","type":"int"},
			{"name":"arr","type":{"type":"array","items":"int"}},
			{"name":"sub","type":["null",{"type":"record","name":"Sub",
				"fields":[
					{"name":"s","type":"string"},
					{"name":"drop","type":"boolean"},
					{"name":"e","type":{"type":"enum","name":"Col","symbols":["X","Y","Z"]}}
				]}]},
			{"name":"m","type":{"type":"map","values":"int"}},
			{"name":"old_name","type":"long"}
		]}`)
	fuzzReader = avro.MustParse(`{
		"type":"record","name":"Fuzz",
		"fields":[
			{"name":"old_name","type":"double"},
			{"name":"i","type":"long"},
			{"name":"added","type":"string","default":"d"},
			{"name":"sub","type":["null",{"type":"record","name":"Sub",
				"fields":[
					{"name":"e","type":{"type":"enum","name":"Col",
						"symbols":["Z","X","NEW"],"default":"NEW"}},
					{"name":"s","type":"string"}
				]}]},
			{"name":"m","type":{"type":"map","values":"long"}},
			{"name":"arr","type":{"type":"array","items":"long"}},
			{"name":"renamed","type":"long","aliases":["old_name_no"], "default": 0}
		]}`)
)

func FuzzTranscodeParity(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte{28, 4, 6, 104, 105, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		tr, err := avro.Transcode(fuzzWriter, fuzzReader)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		out, rest, terr := tr.Append(nil, data)

		// Oracle: resolved Decode into a generic value, reader Encode.
		res, err := avro.Resolve(fuzzWriter, fuzzReader)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		var ov any
		oRest, derr := res.Decode(data, &ov)

		if terr != nil {
			if derr == nil {
				t.Fatalf("transcode failed (%v) but reference decode succeeded (%v consumed=%d)",
					terr, ov, len(data)-len(oRest))
			}
			// Both reject. The transcoder preserves the unconsumed prefix
			// where Decode reports nil on error; failure-state consumption
			// is intentionally not promised to match.
			return
		}
		if derr != nil {
			t.Fatalf("transcode succeeded but reference decode failed: %v", derr)
		}
		if len(rest) != len(oRest) {
			t.Fatalf("consume differs: transcode left %d, decode left %d", len(rest), len(oRest))
		}

		// The transcoded bytes must decode to the same generic value the
		// reader Encode of the reference value gives.
		var tv any
		if _, err := fuzzReader.Decode(out, &tv); err != nil {
			t.Fatalf("reader decode of transcoded bytes: %v", err)
		}
		refOut, err := fuzzReader.Encode(ov)
		if err != nil {
			t.Fatalf("reader encode of reference: %v", err)
		}
		var rv any
		if _, err := fuzzReader.Decode(refOut, &rv); err != nil {
			t.Fatalf("reader decode of reference bytes: %v", err)
		}
		if !reflect.DeepEqual(tv, rv) {
			t.Fatalf("value parity mismatch\ntranscode: %#v\nreference: %#v\ninput: %x", tv, rv, data)
		}
	})
}
