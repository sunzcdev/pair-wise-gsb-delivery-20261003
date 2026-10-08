package avro_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/twmb/avro"
)

// decodeEncodeReference is the semantic oracle the task pins: Decode the
// writer wire with the resolved schema into a generic value, then Encode with
// the plain reader schema. Transcoded bytes must equal that chain's output.
func decodeEncodeReference(t *testing.T, writer, reader *avro.Schema, wire []byte) []byte {
	t.Helper()
	res, err := avro.Resolve(writer, reader)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var v any
	if rest, err := res.Decode(wire, &v); err != nil {
		t.Fatalf("resolved Decode: %v", err)
	} else if len(rest) != 0 {
		t.Fatalf("resolved Decode left %d bytes", len(rest))
	}
	out, err := reader.Encode(v)
	if err != nil {
		t.Fatalf("reader Encode: %v", err)
	}
	return out
}

func transcodeMust(t *testing.T, writer, reader *avro.Schema, wire []byte, opts ...avro.TranscodeOpt) []byte {
	t.Helper()
	tr, err := avro.Transcode(writer, reader, opts...)
	if err != nil {
		t.Fatalf("Transcode compile: %v", err)
	}
	out, rest, err := tr.Append(nil, wire)
	if err != nil {
		t.Fatalf("Transcode: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("Transcode left %d bytes", len(rest))
	}
	return out
}

func mustEncode(t *testing.T, s *avro.Schema, v any) []byte {
	t.Helper()
	b, err := s.Encode(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b
}

// TestTranscode_ReorderedNestedRecords drives the headline case: nested
// records whose field order differs, plus aliases, added defaults, dropped
// fields, and a numeric promotion.
func TestTranscode_ReorderedNestedRecords(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`{
		"type": "record", "name": "User",
		"fields": [
			{"name": "age", "type": "int"},
			{"name": "name", "type": "string"},
			{"name": "drop_me", "type": "boolean"},
			{"name": "addr", "type": {
				"type": "record", "name": "Addr",
				"fields": [
					{"name": "city", "type": "string"},
					{"name": "zip", "type": "int"}
				]
			}}
		]
	}`)
	reader := avro.MustParse(`{
		"type": "record", "name": "User",
		"fields": [
			{"name": "name", "type": "string"},
			{"name": "handle", "type": "string", "aliases": ["name2"], "default": "anon"},
			{"name": "age", "type": "long"},
			{"name": "addr", "type": {
				"type": "record", "name": "Addr",
				"fields": [
					{"name": "zip", "type": "long"},
					{"name": "city", "type": "string"}
				]
			}},
			{"name": "email", "type": "string", "default": ""}
		]
	}`)

	wv := map[string]any{
		"age": int32(30), "name": "Alice", "drop_me": true,
		"addr": map[string]any{"city": "Paris", "zip": int32(75000)},
	}
	wire := mustEncode(t, writer, wv)
	want := decodeEncodeReference(t, writer, reader, wire)
	got := transcodeMust(t, writer, reader, wire)
	if !bytes.Equal(got, want) {
		t.Fatalf("transcoded bytes differ from Decode/Encode reference\ngot:  %x\nwant: %x", got, want)
	}

	// The transcoded output must itself decode under the reader schema.
	var rv map[string]any
	if _, err := reader.Decode(got, &rv); err != nil {
		t.Fatalf("reader Decode of transcoded bytes: %v", err)
	}
	if rv["name"] != "Alice" || rv["handle"] != "anon" || rv["email"] != "" {
		t.Fatalf("unexpected defaults/fields: %#v", rv)
	}
	if rv["age"] != int64(30) {
		t.Fatalf("age not promoted: %#v", rv["age"])
	}
	addr := rv["addr"].(map[string]any)
	if addr["city"] != "Paris" || addr["zip"] != int64(75000) {
		t.Fatalf("nested reorder wrong: %#v", addr)
	}
}

// TestTranscode_AliasRenames checks writer field matched by reader alias.
func TestTranscode_AliasRenames(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"old_name","type":"string"},
		{"name":"b","type":"int"}]}`)
	reader := avro.MustParse(`{"type":"record","name":"R","fields":[
		{"name":"b","type":"int"},
		{"name":"new_name","type":"string","aliases":["old_name"]}]}`)
	wire := mustEncode(t, writer, map[string]any{"old_name": "x", "b": int32(7)})
	want := decodeEncodeReference(t, writer, reader, wire)
	got := transcodeMust(t, writer, reader, wire)
	if !bytes.Equal(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

// TestTranscode_EnumRemapAndDefault covers symbol reordering, a dropped
// symbol taking the enum default, and identity pass-through.
func TestTranscode_EnumRemapAndDefault(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`{"type":"enum","name":"Color","symbols":["RED","GREEN","BLUE"]}`)
	reader := avro.MustParse(`{"type":"enum","name":"Color","symbols":["BLUE","RED","GREEN","GONE"],
		"default":"GONE"}`)

	cases := []int32{0, 1, 2} // RED, GREEN, BLUE
	wantIdx := []int32{1, 2, 0}
	for i, idx := range cases {
		wire := wireInt(nil, idx)
		got := transcodeMust(t, writer, reader, wire)
		want := wireInt(nil, wantIdx[i])
		if !bytes.Equal(got, want) {
			t.Fatalf("enum %d: got %x want %x", idx, got, want)
		}
		if !bytes.Equal(decodeEncodeReference(t, writer, reader, wire), want) {
			t.Fatalf("reference mismatch for enum %d", idx)
		}
	}

	// Same symbol order is a verbatim copy.
	same := avro.MustParse(`{"type":"enum","name":"Color","symbols":["RED","GREEN","BLUE"]}`)
	wire := wireInt(nil, int32(2))
	if got := transcodeMust(t, writer, same, wire); !bytes.Equal(got, wire) {
		t.Fatalf("identity enum did not pass through: %x", got)
	}
}

// TestTranscode_EnumNoDefaultIncompatible fails at compile time.
func TestTranscode_EnumNoDefaultIncompatible(t *testing.T) {
	t.Parallel()
	writer := avro.MustParse(`{"type":"enum","name":"C","symbols":["A","B"]}`)
	reader := avro.MustParse(`{"type":"enum","name":"C","symbols":["A"]}`)
	if _, err := avro.Transcode(writer, reader); err == nil {
		t.Fatal("expected CompatibilityError")
	} else {
		var ce *avro.CompatibilityError
		if !errors.As(err, &ce) {
			t.Fatalf("want CompatibilityError, got %T %v", err, err)
		}
	}
}

// TestTranscode_Promotions checks every numeric promotion and string/bytes.
func TestTranscode_Promotions(t *testing.T) {
	t.Parallel()
	type pr struct {
		ws, rs string
		wv     any
	}
	cases := []pr{
		{`"int"`, `"long"`, int32(-1)},
		{`"int"`, `"float"`, int32(1_000_001)},
		{`"int"`, `"double"`, int32(-42)},
		{`"long"`, `"float"`, int64(1 << 40)},
		{`"long"`, `"double"`, int64(1 << 40)},
		{`"float"`, `"double"`, float32(2.5)},
		{`"string"`, `"bytes"`, "hello"},
		{`"bytes"`, `"string"`, []byte("world")},
	}
	for _, c := range cases {
		w := avro.MustParse(c.ws)
		r := avro.MustParse(c.rs)
		wire := mustEncode(t, w, c.wv)
		got := transcodeMust(t, w, r, wire)
		want := decodeEncodeReference(t, w, r, wire)
		if !bytes.Equal(got, want) {
			t.Fatalf("%s>%s: got %x want %x", c.ws, c.rs, got, want)
		}
	}
}

// TestTranscode_NumericPrecision parity with float rounding.
func TestTranscode_NumericPrecision(t *testing.T) {
	t.Parallel()
	w := avro.MustParse(`"long"`)
	r := avro.MustParse(`"double"`)
	for _, n := range []int64{1 << 53, (1 << 53) + 1, -(1 << 53) - 1} {
		wire := wireLong(nil, n)
		got := transcodeMust(t, w, r, wire)
		want := decodeEncodeReference(t, w, r, wire)
		if !bytes.Equal(got, want) {
			t.Fatalf("long>double %d: got %x want %x", n, got, want)
		}
	}
	// float>double preserves the exact float32 value.
	wf := avro.MustParse(`"float"`)
	rf := avro.MustParse(`"double"`)
	wire := wireU32(nil, math.Float32bits(0.1))
	got := transcodeMust(t, wf, rf, wire)
	if math.Float64frombits(avroDecodeUint64(t, got)) != float64(float32(0.1)) {
		t.Fatalf("float>double did not widen exactly")
	}
}

func avroDecodeUint64(t *testing.T, b []byte) uint64 {
	t.Helper()
	if len(b) != 8 {
		t.Fatalf("want 8 bytes, got %d", len(b))
	}
	var u uint64
	for i := 0; i < 8; i++ {
		u |= uint64(b[i]) << (8 * i)
	}
	return u
}

func wireInt(dst []byte, n int32) []byte {
	u := uint32(n)<<1 ^ uint32(n>>31)
	return binary.AppendUvarint(dst, uint64(u))
}

func wireLong(dst []byte, n int64) []byte {
	u := uint64(n)<<1 ^ uint64(n>>63)
	return binary.AppendUvarint(dst, u)
}

func wireU32(dst []byte, u uint32) []byte {
	return binary.LittleEndian.AppendUint32(dst, u)
}
