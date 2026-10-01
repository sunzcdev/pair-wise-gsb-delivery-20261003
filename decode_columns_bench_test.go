package msgpack_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

const columnBenchRows = 10000

// columnBenchRecord mirrors an analytics event with a handful of selected
// fields plus several heavy, unselected fields that the columnar path must
// skip without allocating.
func columnBenchRecord(i int) map[string]interface{} {
	return map[string]interface{}{
		"id":     int64(i),
		"user":   fmt.Sprintf("user-%06d", i),
		"score":  float64(i) * 1.25,
		"active": i%2 == 0,
		"attrs": map[string]interface{}{
			"country": "CN",
			"level":   int64(i % 100),
		},
		"tags": []interface{}{"a", "b", "c", "d", "e"},
		"blob": map[string]interface{}{
			"k1": "some-long-unselected-string-value",
			"k2": int64(123456789),
			"k3": []interface{}{int64(1), int64(2), int64(3)},
		},
	}
}

func columnBenchStream(b *testing.B) []byte {
	b.Helper()
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	for i := 0; i < columnBenchRows; i++ {
		if err := enc.Encode(columnBenchRecord(i)); err != nil {
			b.Fatal(err)
		}
	}
	return buf.Bytes()
}

func BenchmarkColumns(b *testing.B) {
	data := columnBenchStream(b)
	b.SetBytes(int64(len(data)))

	b.ReportAllocs()
	b.ResetTimer()

	for n := 0; n < b.N; n++ {
		var (
			ids     []int64
			users   []string
			scores  []float64
			actives []bool
			levels  []int64
		)
		cols := []msgpack.ColSpec{
			{Path: "id", Kind: msgpack.ColInt, Dst: &ids},
			{Path: "user", Kind: msgpack.ColString, Dst: &users},
			{Path: "score", Kind: msgpack.ColFloat, Dst: &scores},
			{Path: "active", Kind: msgpack.ColBool, Dst: &actives},
			{Path: "attrs.level", Kind: msgpack.ColInt, Dst: &levels, TrackNull: true},
		}
		res, err := msgpack.UnmarshalColumns(data, cols, &msgpack.ColumnOptions{
			RowsHint: columnBenchRows,
		})
		if err != nil {
			b.Fatal(err)
		}
		if res.Rows != columnBenchRows || len(ids) != columnBenchRows {
			b.Fatalf("rows=%d ids=%d", res.Rows, len(ids))
		}
	}
}

func BenchmarkRowsMapDecode(b *testing.B) {
	data := columnBenchStream(b)
	b.SetBytes(int64(len(data)))

	b.ReportAllocs()
	b.ResetTimer()

	for n := 0; n < b.N; n++ {
		dec := msgpack.NewDecoder(bytes.NewReader(data))
		var ids []int64
		var users []string
		var scores []float64
		var actives []bool
		var levels []int64

		for i := 0; i < columnBenchRows; i++ {
			m, err := dec.DecodeMap()
			if err != nil {
				b.Fatal(err)
			}
			ids = append(ids, m["id"].(int64))
			users = append(users, m["user"].(string))
			scores = append(scores, m["score"].(float64))
			actives = append(actives, m["active"].(bool))
			if attrs, ok := m["attrs"].(map[string]interface{}); ok {
				levels = append(levels, attrs["level"].(int64))
			}
		}
		if len(ids) != columnBenchRows {
			b.Fatalf("ids=%d", len(ids))
		}
	}
}

// BenchmarkColumnsNumeric selects only top-level scalar fields to show the
// intrinsic walk cost; the string column in BenchmarkColumns must allocate
// one string per value and dominates allocations there.
func BenchmarkColumnsNumeric(b *testing.B) {
	data := columnBenchStream(b)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()

	for n := 0; n < b.N; n++ {
		var ids []int64
		var scores []float64
		var actives []bool
		res, err := msgpack.UnmarshalColumns(data, []msgpack.ColSpec{
			{Path: "id", Kind: msgpack.ColInt, Dst: &ids},
			{Path: "score", Kind: msgpack.ColFloat, Dst: &scores},
			{Path: "active", Kind: msgpack.ColBool, Dst: &actives},
		}, &msgpack.ColumnOptions{RowsHint: columnBenchRows})
		if err != nil {
			b.Fatal(err)
		}
		if res.Rows != columnBenchRows {
			b.Fatal(res.Rows)
		}
	}
}
