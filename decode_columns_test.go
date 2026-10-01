package msgpack_test

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"
)

func buildColumnStream(t *testing.T, records ...map[string]interface{}) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	for _, rec := range records {
		require.NoError(t, enc.Encode(rec))
	}
	return buf.Bytes()
}

func TestDecodeColumnsBasic(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	records := []map[string]interface{}{
		{"id": int64(1), "name": "alice", "score": float64(9.5), "active": true, "payload": []byte("p1"), "ts": now, "extra": "skip-me"},
		{"id": int64(2), "name": "bob", "score": float64(7), "active": false, "payload": []byte("p2"), "ts": now, "extra": "skip-me2"},
		{"id": int64(3), "name": "carol", "score": float64(3.25), "active": true, "payload": []byte("p3"), "ts": now},
	}
	data := buildColumnStream(t, records...)

	var (
		ids     []int64
		names   []string
		scores  []float64
		actives []bool
		payload [][]byte
		tss     []time.Time
	)
	specs := []msgpack.ColSpec{
		{Path: "id", Kind: msgpack.ColInt, Dst: &ids},
		{Path: "name", Kind: msgpack.ColString, Dst: &names},
		{Path: "score", Kind: msgpack.ColFloat, Dst: &scores},
		{Path: "active", Kind: msgpack.ColBool, Dst: &actives},
		{Path: "payload", Kind: msgpack.ColBytes, Dst: &payload},
		{Path: "ts", Kind: msgpack.ColTime, Dst: &tss},
	}

	res, err := msgpack.UnmarshalColumns(data, specs, nil)
	require.NoError(t, err)
	require.Equal(t, len(records), res.Rows)

	// Reference: row-by-row decoding into maps.
	ref := msgpack.NewDecoder(bytes.NewReader(data))
	for i, rec := range records {
		m, err := ref.DecodeMap()
		require.NoError(t, err)

		require.Equal(t, rec["id"].(int64), ids[i])
		require.Equal(t, rec["name"].(string), names[i])
		require.Equal(t, m["score"].(float64), scores[i])
		require.Equal(t, rec["active"].(bool), actives[i])
		require.Equal(t, rec["payload"].([]byte), payload[i])
		require.True(t, tss[i].Equal(rec["ts"].(time.Time)))

		for _, col := range res.Columns {
			require.True(t, col.Present[i], "row %d col %q should be present", i, col.Path)
		}
	}

	// Dst slices and Values expose the same backing data.
	require.Equal(t, ids, res.Columns[0].Values.([]int64))
	require.Equal(t, names, res.Columns[1].Values.([]string))
	require.Equal(t, res.ResumeOffset, len(data))
}

func TestDecodeColumnsMissingVersusNull(t *testing.T) {
	records := []map[string]interface{}{
		{"id": int64(1), "note": nil}, // note explicitly null
		{"id": int64(2)},              // note missing
		{"id": int64(3), "note": "x"}, // note present value
	}
	data := buildColumnStream(t, records...)

	var ids []int64
	var notes []string
	specs := []msgpack.ColSpec{
		{Path: "id", Kind: msgpack.ColInt, Dst: &ids},
		{Path: "note", Kind: msgpack.ColString, Dst: &notes, TrackNull: true},
	}
	res, err := msgpack.UnmarshalColumns(data, specs, nil)
	require.NoError(t, err)

	noteCol := res.Columns[1]
	// explicit nil -> Present+Null; missing -> absent; value -> present.
	require.Equal(t, []bool{true, false, true}, noteCol.Present)
	require.Equal(t, []bool{true, false, false}, noteCol.Null)
	require.Equal(t, "", notes[0]) // explicit nil -> zero value
	require.Equal(t, "", notes[1]) // missing -> zero value
	require.Equal(t, "x", notes[2])

	// Null bitmap is only allocated when requested.
	idCol := res.Columns[0]
	require.Nil(t, idCol.Null)
	require.Equal(t, []bool{true, true, true}, idCol.Present)
}

func TestDecodeColumnsNestedAndArrayPath(t *testing.T) {
	records := []map[string]interface{}{
		{"user": map[string]interface{}{"address": map[string]interface{}{"city": "Berlin"}},
			"items": []interface{}{map[string]interface{}{"id": int64(10)}, map[string]interface{}{"id": int64(11)}}},
		{"user": map[string]interface{}{"address": map[string]interface{}{"city": "Paris"}},
			"items": []interface{}{map[string]interface{}{"id": int64(20)}}},
		{"user": map[string]interface{}{"address": map[string]interface{}{}}, "items": nil},
	}
	data := buildColumnStream(t, records...)

	var cities []string
	var firstItemIDs []int64
	res, err := msgpack.UnmarshalColumns(data, []msgpack.ColSpec{
		{Path: "user.address.city", Kind: msgpack.ColString, Dst: &cities},
		{Path: "items.0.id", Kind: msgpack.ColInt, Dst: &firstItemIDs, TrackNull: true},
	}, nil)
	require.NoError(t, err)

	require.Equal(t, []string{"Berlin", "Paris", ""}, cities)
	require.Equal(t, []bool{true, true, false}, res.Columns[0].Present)

	// items.0.id present for first two, missing for the nil-items third row.
	require.Equal(t, []int64{10, 20, 0}, firstItemIDs)
	require.Equal(t, []bool{true, true, false}, res.Columns[1].Present)
	require.Equal(t, []bool{false, false, false}, res.Columns[1].Null)
}

func TestDecodeColumnsSharedPrefixAndDuplicateLeaf(t *testing.T) {
	// Several columns share a nested prefix; one integer is also read as a
	// float through a second key. All values are decoded without shifting.
	records := []map[string]interface{}{
		{"meta": map[string]interface{}{"a": int64(7), "b": int64(8), "asFloat": float64(7)}},
		{"meta": map[string]interface{}{"a": int64(9), "b": int64(10), "asFloat": float64(9)}},
	}
	data := buildColumnStream(t, records...)

	var aInt, bInt []int64
	var aF []float64
	res, err := msgpack.UnmarshalColumns(data, []msgpack.ColSpec{
		{Path: "meta.a", Kind: msgpack.ColInt, Dst: &aInt},
		{Path: "meta.b", Kind: msgpack.ColInt, Dst: &bInt},
		{Path: "meta.asFloat", Kind: msgpack.ColFloat, Dst: &aF},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, []int64{7, 9}, aInt)
	require.Equal(t, []int64{8, 10}, bInt)
	require.Equal(t, []float64{7, 9}, aF)

	// Reference cross-check against row-by-row map decoding.
	ref := msgpack.NewDecoder(bytes.NewReader(data))
	for i := 0; i < 2; i++ {
		m, err := ref.DecodeMap()
		require.NoError(t, err)
		meta := m["meta"].(map[string]interface{})
		require.Equal(t, meta["a"].(int64), aInt[i])
		require.Equal(t, meta["b"].(int64), bInt[i])
		require.Equal(t, meta["asFloat"].(float64), aF[i])
	}
	require.Equal(t, 2, res.Rows)
}

func TestDecodeColumnsUnselectedSkipped(t *testing.T) {
	// Large unselected nested payload must be skipped without populating maps.
	records := []map[string]interface{}{
		{"id": int64(1), "blob": map[string]interface{}{"a": int64(1), "nested": []interface{}{int64(1), int64(2), int64(3)}}},
		{"id": int64(2), "blob": "string-value"},
		{"id": int64(3)},
	}
	data := buildColumnStream(t, records...)

	var ids []int64
	res, err := msgpack.UnmarshalColumns(data, []msgpack.ColSpec{
		{Path: "id", Kind: msgpack.ColInt, Dst: &ids},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2, 3}, ids)
	require.Equal(t, 3, res.Rows)
}

func TestDecodeColumnsUnselectedTimeAndExt(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	records := []map[string]interface{}{
		{"id": int64(1), "created": now, "nested": map[string]interface{}{"at": now}},
		{"id": int64(2), "created": now},
	}
	data := buildColumnStream(t, records...)

	var ids []int64
	var created []time.Time
	res, err := msgpack.UnmarshalColumns(data, []msgpack.ColSpec{
		{Path: "id", Kind: msgpack.ColInt, Dst: &ids},
		{Path: "created", Kind: msgpack.ColTime, Dst: &created},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2}, ids)
	require.True(t, created[0].Equal(now))
	require.True(t, created[1].Equal(now))
	require.Equal(t, len(data), res.ResumeOffset)
}

type slowReader struct {
	data []byte
	pos  int
}

// Read deliberately uses small chunks and implements only io.Reader so the
// decoder installs its own bufio wrapper.
func (r *slowReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := 3
	if n > len(p) {
		n = len(p)
	}
	if r.pos+n > len(r.data) {
		n = len(r.data) - r.pos
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

func TestDecodeColumnsBufferedReader(t *testing.T) {
	records := []map[string]interface{}{
		{"id": int64(1), "name": "alice", "blob": []byte("aaaaaaaaaaaaaaaa")},
		{"id": int64(2), "name": "bob", "blob": []byte("bbbbbbbbbbbbbbbb")},
		{"id": int64(3), "name": "carol"},
	}
	data := buildColumnStream(t, records...)

	var ids []int64
	var names []string
	dec := msgpack.NewDecoder(&slowReader{data: data})
	res, err := dec.DecodeColumns([]msgpack.ColSpec{
		{Path: "id", Kind: msgpack.ColInt, Dst: &ids},
		{Path: "name", Kind: msgpack.ColString, Dst: &names},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2, 3}, ids)
	require.Equal(t, []string{"alice", "bob", "carol"}, names)
	require.Equal(t, 3, res.Rows)
	require.Equal(t, len(data), res.ResumeOffset)
}

func TestDecodeColumnsBadRowKeepsAlignment(t *testing.T) {
	records := []map[string]interface{}{
		{"id": int64(1), "name": "a"},
		{"id": int64(2), "name": "b"},
		{"id": "not-an-int", "name": "c"}, // type mismatch for id column
		{"id": int64(4), "name": "d"},
	}
	data := buildColumnStream(t, records...)

	var ids []int64
	var names []string
	res, err := msgpack.UnmarshalColumns(data, []msgpack.ColSpec{
		{Path: "id", Kind: msgpack.ColInt, Dst: &ids},
		{Path: "name", Kind: msgpack.ColString, Dst: &names},
	}, &msgpack.ColumnOptions{ErrorPolicy: msgpack.SkipBadRows})
	require.NoError(t, err)

	// Every row occupies one slot in every column, nothing shifts.
	require.Equal(t, 4, res.Rows)
	require.Len(t, ids, 4)
	require.Len(t, names, 4)
	require.Equal(t, []int64{1, 2, 0, 4}, ids)
	require.Equal(t, []string{"a", "b", "c", "d"}, names)

	// Present bitmap shows the bad cell as absent, good name still present.
	require.Equal(t, []bool{true, true, false, true}, res.Columns[0].Present)
	require.Equal(t, []bool{true, true, true, true}, res.Columns[1].Present)

	require.Len(t, res.BadRows, 1)
	require.Equal(t, 2, res.BadRows[0].Index)
	require.Contains(t, res.BadRows[0].Err.Error(), `column "id"`)
}

func TestDecodeColumnsBadRowsNeverShift(t *testing.T) {
	// Several consecutive malformed cells/rows sandwiched between good rows;
	// every column must keep exactly one slot per record.
	records := []map[string]interface{}{
		{"id": int64(1), "name": "a", "ok": true},
		{"id": "x", "name": "b", "ok": false},    // id mismatch
		{"id": int64(3), "name": 42, "ok": true}, // name mismatch
		{"id": true, "name": "d", "ok": "yes"},   // both mismatch
		{"id": int64(5), "name": "e", "ok": false},
	}
	data := buildColumnStream(t, records...)

	var ids []int64
	var names []string
	var oks []bool
	res, err := msgpack.UnmarshalColumns(data, []msgpack.ColSpec{
		{Path: "id", Kind: msgpack.ColInt, Dst: &ids},
		{Path: "name", Kind: msgpack.ColString, Dst: &names},
		{Path: "ok", Kind: msgpack.ColBool, Dst: &oks},
	}, &msgpack.ColumnOptions{ErrorPolicy: msgpack.SkipBadRows})
	require.NoError(t, err)

	require.Equal(t, 5, res.Rows)
	require.Equal(t, []int64{1, 0, 3, 0, 5}, ids)
	require.Equal(t, []string{"a", "b", "", "d", "e"}, names)
	require.Equal(t, []bool{true, false, true, false, false}, oks)

	for ci := 0; ci < 3; ci++ {
		require.Len(t, res.Columns[ci].Present, 5)
	}
	// id present on rows 0,2,4 only.
	require.Equal(t, []bool{true, false, true, false, true}, res.Columns[0].Present)
	// name present on rows 0,1,3,4.
	require.Equal(t, []bool{true, true, false, true, true}, res.Columns[1].Present)
	// ok present on rows 0,1,2,4.
	require.Equal(t, []bool{true, true, true, false, true}, res.Columns[2].Present)

	require.Len(t, res.BadRows, 3)
	require.Equal(t, []int{1, 2, 3}, []int{
		res.BadRows[0].Index, res.BadRows[1].Index, res.BadRows[2].Index,
	})
}

func TestDecodeColumnsStopOnBadRow(t *testing.T) {
	records := []map[string]interface{}{
		{"id": int64(1)},
		{"id": "bad"},
		{"id": int64(3)},
	}
	data := buildColumnStream(t, records...)

	var ids []int64
	res, err := msgpack.UnmarshalColumns(data, []msgpack.ColSpec{
		{Path: "id", Kind: msgpack.ColInt, Dst: &ids},
	}, nil)

	bad, ok := err.(*msgpack.BadRow)
	require.True(t, ok, "expected *BadRow, got %T: %v", err, err)
	require.Equal(t, 1, bad.Index)
	require.Equal(t, 2, res.Rows)
	require.Equal(t, []int64{1, 0}, ids)

	// ResumeOffset points just past the bad record; the caller can feed the
	// remaining bytes to a fresh decoder to continue.
	require.Greater(t, res.ResumeOffset, 0)
	require.Less(t, res.ResumeOffset, len(data))

	var ids2 []int64
	res2, err := msgpack.UnmarshalColumns(data[res.ResumeOffset:], []msgpack.ColSpec{
		{Path: "id", Kind: msgpack.ColInt, Dst: &ids2},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, []int64{3}, ids2)
	require.Equal(t, 1, res2.Rows)
}

func TestDecodeColumnsFrameError(t *testing.T) {
	// Encode each record separately so its boundary is known, then corrupt
	// the second record by dropping a payload byte.
	first, err := msgpack.Marshal(map[string]interface{}{"id": int64(1)})
	require.NoError(t, err)
	second, err := msgpack.Marshal(map[string]interface{}{
		"id": int64(40000), "name": "corrupted",
	})
	require.NoError(t, err)

	truncated := append(append([]byte{}, first...), second[:len(second)-1]...)
	var ids2 []int64
	res, err := msgpack.UnmarshalColumns(truncated, []msgpack.ColSpec{
		{Path: "id", Kind: msgpack.ColInt, Dst: &ids2},
	}, &msgpack.ColumnOptions{ErrorPolicy: msgpack.SkipBadRows})

	frameErr, ok := err.(*msgpack.FrameError)
	require.True(t, ok, "expected *FrameError, got %T: %v", err, err)
	require.Equal(t, len(first), frameErr.Offset)
	require.Equal(t, frameErr.Offset, res.ResumeOffset)
	require.Equal(t, 1, res.Rows)
	require.Equal(t, []int64{1}, ids2[:res.Rows])
}

func TestDecodeColumnsMaxRowsResume(t *testing.T) {
	records := []map[string]interface{}{
		{"id": int64(1)}, {"id": int64(2)}, {"id": int64(3)}, {"id": int64(4)},
	}
	data := buildColumnStream(t, records...)

	var all []int64
	dec := msgpack.NewDecoder(bytes.NewReader(data))

	for {
		var ids []int64
		res, err := dec.DecodeColumns([]msgpack.ColSpec{
			{Path: "id", Kind: msgpack.ColInt, Dst: &ids},
		}, &msgpack.ColumnOptions{MaxRows: 2})
		// The final batch ends at EOF; that is still a successful result once
		// at least one record was framed.
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		if res.Rows == 0 {
			break
		}
		all = append(all, ids...)
		if !res.Truncated {
			break
		}
	}
	require.Equal(t, []int64{1, 2, 3, 4}, all)
}

func TestDecodeColumnsValidation(t *testing.T) {
	_, err := msgpack.UnmarshalColumns([]byte{0xc0}, nil, nil)
	require.Error(t, err)

	var bad []int32
	_, err = msgpack.UnmarshalColumns([]byte{0x80}, []msgpack.ColSpec{
		{Path: "id", Kind: msgpack.ColInt, Dst: &bad},
	}, nil)
	require.Error(t, err)

	var ok []int64
	_, err = msgpack.UnmarshalColumns([]byte{0x80}, []msgpack.ColSpec{
		{Path: "id", Kind: msgpack.ColInt, Dst: &ok},
		{Path: "id", Kind: msgpack.ColInt, Dst: new([]int64)},
	}, nil)
	require.Error(t, err)
}

func TestDecodeColumnsEmptyStream(t *testing.T) {
	var ids []int64
	res, err := msgpack.UnmarshalColumns(nil, []msgpack.ColSpec{
		{Path: "id", Kind: msgpack.ColInt, Dst: &ids},
	}, nil)
	require.True(t, errors.Is(err, io.EOF))
	require.Equal(t, 0, res.Rows)
}
