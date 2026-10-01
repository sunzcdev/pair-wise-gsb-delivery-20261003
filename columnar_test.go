package msgpack_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"
)

func columnarTestSpecs() []msgpack.ColumnSpec {
	return []msgpack.ColumnSpec{
		{Path: "id", Kind: msgpack.ColumnInt64},
		{Path: "name", Kind: msgpack.ColumnString},
		{Path: "score", Kind: msgpack.ColumnFloat64},
		{Path: "active", Kind: msgpack.ColumnBool},
		{Path: "blob", Kind: msgpack.ColumnBytes},
		{Path: "user.id", Kind: msgpack.ColumnInt64},
		{Path: "user.email", Kind: msgpack.ColumnString},
	}
}

func columnarTestRecords() []interface{} {
	var records []interface{}
	for i := 0; i < 200; i++ {
		user := map[string]interface{}{
			"id":    int64(i * 10),
			"email": fmt.Sprintf("user-%d@example.com", i),
		}
		rec := map[string]interface{}{
			"id":     int64(i),
			"name":   fmt.Sprintf("name-%d", i),
			"score":  float64(i) * 1.5,
			"active": i%2 == 0,
			"blob":   []byte(fmt.Sprintf("blob-%d", i)),
			"user":   user,
			"extra":  strings.Repeat("x", 64), // unselected field
		}
		switch {
		case i%7 == 0:
			delete(rec, "name") // missing field
		case i%5 == 0:
			rec["score"] = nil // explicit nil
		}
		if i%11 == 0 {
			rec["user"] = nil // nil nested map
		}
		if i%13 == 0 {
			delete(user, "id") // missing nested field
		}
		if i%9 == 0 {
			user["email"] = nil // explicit nil nested field
		}
		records = append(records, rec)
	}
	records = append(records, nil) // a nil record
	return records
}

func encodeRecords(t require.TestingT, records []interface{}) []byte {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	for _, rec := range records {
		require.NoError(t, enc.Encode(rec))
	}
	return buf.Bytes()
}

// decodeRows decodes the stream record by record into maps; it is the
// reference implementation the columnar output is checked against.
func decodeRows(t require.TestingT, data []byte) []map[string]interface{} {
	dec := msgpack.NewDecoder(bytes.NewReader(data))
	var rows []map[string]interface{}
	for {
		var m map[string]interface{}
		err := dec.Decode(&m)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		rows = append(rows, m)
	}
	return rows
}

func lookupPath(m map[string]interface{}, path string) (interface{}, bool) {
	var cur interface{} = m
	for _, seg := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]interface{})
		if !ok || mm == nil {
			return nil, false
		}
		cur, ok = mm[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func asInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int8:
		return int64(n)
	case int16:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case uint8:
		return int64(n)
	case uint16:
		return int64(n)
	case uint32:
		return int64(n)
	case uint64:
		return int64(n)
	}
	panic(fmt.Sprintf("asInt64: unexpected type %T", v))
}

func asFloat64(v interface{}) float64 {
	switch n := v.(type) {
	case float32:
		return float64(n)
	case float64:
		return n
	}
	panic(fmt.Sprintf("asFloat64: unexpected type %T", v))
}

func TestColumnarMatchesRowByRow(t *testing.T) {
	specs := columnarTestSpecs()
	records := columnarTestRecords()
	data := encodeRecords(t, records)

	dec := msgpack.NewDecoder(bytes.NewReader(data))
	cd, err := msgpack.NewColumnarDecoder(dec, specs, msgpack.CollectErrors)
	require.NoError(t, err)
	require.NoError(t, cd.Decode())
	require.Empty(t, cd.BadRows())
	require.Equal(t, len(records), cd.Rows())

	rows := decodeRows(t, data)
	require.Len(t, rows, len(records))

	cols := cd.Columns()
	require.Len(t, cols, len(specs))

	for ci, spec := range specs {
		col := cols[ci]
		require.Equal(t, spec.Kind, col.Kind())
		require.Equal(t, len(records), col.Len(), spec.Path)

		for i := range records {
			where := fmt.Sprintf("%s row %d", spec.Path, i)
			ref, ok := lookupPath(rows[i], spec.Path)
			if !ok {
				require.False(t, col.Present(i), where)
				require.False(t, col.Valid(i), where)
				continue
			}
			require.True(t, col.Present(i), where)
			if ref == nil {
				require.True(t, col.Null(i), where)
				require.False(t, col.Valid(i), where)
				continue
			}
			require.False(t, col.Null(i), where)
			require.True(t, col.Valid(i), where)
			switch spec.Kind {
			case msgpack.ColumnInt64:
				require.Equal(t, asInt64(ref), col.Ints[i], where)
			case msgpack.ColumnFloat64:
				require.Equal(t, asFloat64(ref), col.Floats[i], where)
			case msgpack.ColumnBool:
				require.Equal(t, ref.(bool), col.Bools[i], where)
			case msgpack.ColumnString:
				require.Equal(t, ref.(string), col.Strings[i], where)
			case msgpack.ColumnBytes:
				require.Equal(t, ref.([]byte), col.Bytes[i], where)
			}
		}
	}
}

func TestColumnarMissingVsNull(t *testing.T) {
	records := []interface{}{
		map[string]interface{}{"v": int64(42)},
		map[string]interface{}{"v": nil},
		map[string]interface{}{"other": int64(1)},
		nil,
	}
	data := encodeRecords(t, records)

	dec := msgpack.NewDecoder(bytes.NewReader(data))
	cd, err := msgpack.NewColumnarDecoder(dec, []msgpack.ColumnSpec{
		{Path: "v", Kind: msgpack.ColumnInt64},
	}, msgpack.CollectErrors)
	require.NoError(t, err)
	require.NoError(t, cd.Decode())
	require.Equal(t, 4, cd.Rows())

	col := cd.Columns()[0]

	require.True(t, col.Valid(0))
	require.Equal(t, int64(42), col.Ints[0])

	require.True(t, col.Present(1), "explicit nil is present")
	require.True(t, col.Null(1))
	require.False(t, col.Valid(1))

	require.False(t, col.Present(2), "missing field is not present")
	require.False(t, col.Null(2))

	require.False(t, col.Present(3), "nil record has no fields")
	require.False(t, col.Null(3))
}

func badTypeRecords() []interface{} {
	return []interface{}{
		map[string]interface{}{"id": int64(0), "name": "a"},
		map[string]interface{}{"id": "not-a-number", "name": "b"}, // bad: id is a string
		map[string]interface{}{"id": int64(2), "name": "c"},
		map[string]interface{}{"id": map[string]interface{}{"x": 1}, "name": "d"}, // bad: id is a map
		map[string]interface{}{"id": int64(4), "name": "e"},
	}
}

func badTypeSpecs() []msgpack.ColumnSpec {
	return []msgpack.ColumnSpec{
		{Path: "id", Kind: msgpack.ColumnInt64},
		{Path: "name", Kind: msgpack.ColumnString},
	}
}

func TestColumnarCollectErrors(t *testing.T) {
	data := encodeRecords(t, badTypeRecords())

	dec := msgpack.NewDecoder(bytes.NewReader(data))
	cd, err := msgpack.NewColumnarDecoder(dec, badTypeSpecs(), msgpack.CollectErrors)
	require.NoError(t, err)
	require.NoError(t, cd.Decode())

	require.Equal(t, 5, cd.Rows(), "bad rows still occupy a row slot")

	bad := cd.BadRows()
	require.Len(t, bad, 2)
	require.Equal(t, 1, bad[0].Index)
	require.Equal(t, 3, bad[1].Index)

	ids, names := cd.Columns()[0], cd.Columns()[1]
	for _, i := range []int{1, 3} {
		require.False(t, ids.Present(i), "bad row %d is all-missing", i)
		require.False(t, names.Present(i), "bad row %d is all-missing", i)
	}

	// Columns stay aligned: rows after a bad row hold their own values.
	require.Equal(t, []int64{0, 0, 2, 0, 4}, ids.Ints)
	require.Equal(t, []string{"a", "", "c", "", "e"}, names.Strings)
}

func TestColumnarFailFast(t *testing.T) {
	data := encodeRecords(t, badTypeRecords())

	dec := msgpack.NewDecoder(bytes.NewReader(data))
	cd, err := msgpack.NewColumnarDecoder(dec, badTypeSpecs(), msgpack.FailFast)
	require.NoError(t, err)

	err = cd.Decode()
	var rowErr *msgpack.RowError
	require.True(t, errors.As(err, &rowErr))
	require.Equal(t, 1, rowErr.Index)
	require.Equal(t, 2, cd.Rows())

	// The stream is positioned at the next record boundary: decoding
	// resumes with the record after the bad one.
	err = cd.Decode()
	require.True(t, errors.As(err, &rowErr))
	require.Equal(t, 3, rowErr.Index)
	require.Equal(t, 4, cd.Rows())

	require.NoError(t, cd.Decode())
	require.Equal(t, 5, cd.Rows())

	require.Equal(t, []int64{0, 0, 2, 0, 4}, cd.Columns()[0].Ints)
	require.Equal(t, []string{"a", "", "c", "", "e"}, cd.Columns()[1].Strings)
	require.Len(t, cd.BadRows(), 2)
}

func TestColumnarCorruptStream(t *testing.T) {
	records := []interface{}{
		map[string]interface{}{"id": int64(0), "name": "a"},
		map[string]interface{}{"id": int64(1), "name": "b"},
		map[string]interface{}{"id": int64(2), "name": "c"},
	}
	data := encodeRecords(t, records)
	truncated := data[:len(data)-3]

	dec := msgpack.NewDecoder(bytes.NewReader(truncated))
	cd, err := msgpack.NewColumnarDecoder(dec, badTypeSpecs(), msgpack.FailFast)
	require.NoError(t, err)

	err = cd.Decode()
	require.Error(t, err)

	var rowErr *msgpack.RowError
	require.False(t, errors.As(err, &rowErr), "structural corruption is not a row error")

	// Rows decoded before the corruption are intact and aligned.
	require.Greater(t, cd.Rows(), 0)
	for _, col := range cd.Columns() {
		require.Equal(t, cd.Rows(), col.Len())
	}
}

func TestColumnarSpecValidation(t *testing.T) {
	dec := msgpack.NewDecoder(nil)

	_, err := msgpack.NewColumnarDecoder(dec, nil, msgpack.FailFast)
	require.Error(t, err)

	_, err = msgpack.NewColumnarDecoder(dec, []msgpack.ColumnSpec{
		{Path: "", Kind: msgpack.ColumnInt64},
	}, msgpack.FailFast)
	require.Error(t, err)

	_, err = msgpack.NewColumnarDecoder(dec, []msgpack.ColumnSpec{
		{Path: "a..b", Kind: msgpack.ColumnInt64},
	}, msgpack.FailFast)
	require.Error(t, err)

	_, err = msgpack.NewColumnarDecoder(dec, []msgpack.ColumnSpec{
		{Path: "a", Kind: msgpack.ColumnInt64},
		{Path: "a", Kind: msgpack.ColumnInt64},
	}, msgpack.FailFast)
	require.Error(t, err)

	_, err = msgpack.NewColumnarDecoder(dec, []msgpack.ColumnSpec{
		{Path: "a", Kind: msgpack.ColumnInt64},
		{Path: "a.b", Kind: msgpack.ColumnInt64},
	}, msgpack.FailFast)
	require.Error(t, err)

	_, err = msgpack.NewColumnarDecoder(dec, []msgpack.ColumnSpec{
		{Path: "a", Kind: msgpack.ColumnKind(99)},
	}, msgpack.FailFast)
	require.Error(t, err)
}

//------------------------------------------------------------------------------

const benchNumRecords = 1000

func benchmarkData() []byte {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	payload := bytes.Repeat([]byte("p"), 256)
	for i := 0; i < benchNumRecords; i++ {
		rec := map[string]interface{}{
			"id":      int64(i),
			"name":    fmt.Sprintf("name-%d", i),
			"score":   float64(i) * 1.5,
			"active":  i%2 == 0,
			"tags":    []string{"alpha", "beta", "gamma"},
			"payload": payload,
			"user": map[string]interface{}{
				"id":    int64(i * 10),
				"email": fmt.Sprintf("user-%d@example.com", i),
			},
			"extra1": strings.Repeat("y", 128),
			"extra2": int64(i * 100),
		}
		if err := enc.Encode(rec); err != nil {
			panic(err)
		}
	}
	return buf.Bytes()
}

func benchmarkSpecs() []msgpack.ColumnSpec {
	return []msgpack.ColumnSpec{
		{Path: "id", Kind: msgpack.ColumnInt64},
		{Path: "name", Kind: msgpack.ColumnString},
		{Path: "user.id", Kind: msgpack.ColumnInt64},
	}
}

func BenchmarkColumnarDecode(b *testing.B) {
	data := benchmarkData()

	dec := msgpack.NewDecoder(nil)
	cd, err := msgpack.NewColumnarDecoder(dec, benchmarkSpecs(), msgpack.CollectErrors)
	require.NoError(b, err)

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		dec.Reset(bytes.NewReader(data))
		cd.Reset()
		if err := cd.Decode(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRowByRowMapDecode decodes every record into a map and then
// transposes the selected fields into columns: the baseline the columnar
// decoder replaces.
func BenchmarkRowByRowMapDecode(b *testing.B) {
	data := benchmarkData()
	dec := msgpack.NewDecoder(nil)

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		dec.Reset(bytes.NewReader(data))

		ids := make([]int64, 0, benchNumRecords)
		names := make([]string, 0, benchNumRecords)
		uids := make([]int64, 0, benchNumRecords)

		for {
			var m map[string]interface{}
			err := dec.Decode(&m)
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			ids = append(ids, asInt64(m["id"]))
			names = append(names, m["name"].(string))
			uids = append(uids, asInt64(m["user"].(map[string]interface{})["id"]))
		}
	}
}

type benchStructRecord struct {
	ID      int64    `msgpack:"id"`
	Name    string   `msgpack:"name"`
	Score   float64  `msgpack:"score"`
	Active  bool     `msgpack:"active"`
	Tags    []string `msgpack:"tags"`
	Payload []byte   `msgpack:"payload"`
	User    struct {
		ID    int64  `msgpack:"id"`
		Email string `msgpack:"email"`
	} `msgpack:"user"`
	Extra1 string `msgpack:"extra1"`
	Extra2 int64  `msgpack:"extra2"`
}

// BenchmarkRowByRowStructDecode decodes every record into a struct, the
// fastest row-wise decoding path, and transposes the selected fields.
func BenchmarkRowByRowStructDecode(b *testing.B) {
	data := benchmarkData()
	dec := msgpack.NewDecoder(nil)

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		dec.Reset(bytes.NewReader(data))

		ids := make([]int64, 0, benchNumRecords)
		names := make([]string, 0, benchNumRecords)
		uids := make([]int64, 0, benchNumRecords)

		for {
			var rec benchStructRecord
			err := dec.Decode(&rec)
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			ids = append(ids, rec.ID)
			names = append(names, rec.Name)
			uids = append(uids, rec.User.ID)
		}
	}
}
