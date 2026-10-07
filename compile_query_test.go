package msgpack_test

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"
)

func TestCompiledQueryFiltersProjectsSlicesAndReuses(t *testing.T) {
	rootData := buildCompiledQueryRecords(t, 100, 1024, false)
	rootQuery, err := msgpack.CompileQuery(msgpack.QuerySpec{
		Offset: 2,
		Limit:  10,
		Fields: []string{"id", "elapsed"},
		Predicate: msgpack.QueryPredicate{
			Path: "status",
			Eq:   "failed",
		},
	})
	require.NoError(t, err)

	rows, err := rootQuery.Exec(msgpack.NewDecoder(bytes.NewReader(rootData)))
	require.NoError(t, err)

	expected := []map[string]interface{}{
		{"id": int64(5), "elapsed": int64(50)},
		{"id": int64(7), "elapsed": int64(70)},
		{"id": int64(9), "elapsed": int64(90)},
		{"id": int64(11), "elapsed": int64(110)},
		{"id": int64(13), "elapsed": int64(130)},
		{"id": int64(15), "elapsed": int64(150)},
		{"id": int64(17), "elapsed": int64(170)},
		{"id": int64(19), "elapsed": int64(190)},
		{"id": int64(21), "elapsed": int64(210)},
		{"id": int64(23), "elapsed": int64(230)},
	}
	require.Equal(t, expected, rows)

	data := buildCompiledQueryRecords(t, 100, 1024, true)

	query, err := msgpack.CompileQuery(msgpack.QuerySpec{
		Path:   "jobs.current",
		Start:  10,
		End:    50,
		Offset: 2,
		Limit:  10,
		Fields: []string{"id", "elapsed"},
		Predicate: msgpack.QueryPredicate{
			Path: "status",
			Eq:   "failed",
		},
	})
	require.NoError(t, err)

	dec := msgpack.NewDecoder(bytes.NewReader(data))
	rows, err = query.Exec(dec)
	require.NoError(t, err)

	slicedExpected := []map[string]interface{}{
		{"id": int64(15), "elapsed": int64(150)},
		{"id": int64(17), "elapsed": int64(170)},
		{"id": int64(19), "elapsed": int64(190)},
		{"id": int64(21), "elapsed": int64(210)},
		{"id": int64(23), "elapsed": int64(230)},
		{"id": int64(25), "elapsed": int64(250)},
		{"id": int64(27), "elapsed": int64(270)},
		{"id": int64(29), "elapsed": int64(290)},
		{"id": int64(31), "elapsed": int64(310)},
		{"id": int64(33), "elapsed": int64(330)},
	}
	require.Equal(t, slicedExpected, rows)

	dec.Reset(bytes.NewReader(data))
	rows, err = dec.QueryCompiled(query)
	require.NoError(t, err)
	require.Equal(t, slicedExpected, rows)
}

func TestCompiledQueryPredicateAfterOutputsDoesNotBufferUnselectedTree(t *testing.T) {
	const records = 64
	const payloadSize = 1 << 20
	data := buildCompiledQueryRecords(t, records, payloadSize, false)

	query, err := msgpack.CompileQuery(msgpack.QuerySpec{
		Limit:  1,
		Fields: []string{"id", "elapsed"},
		Predicate: msgpack.QueryPredicate{
			Path: "status",
			Eq:   "failed",
		},
	})
	require.NoError(t, err)

	runtime.GC()
	var before, during, after runtime.MemStats
	runtime.ReadMemStats(&before)

	rows, err := query.Exec(msgpack.NewDecoder(bytes.NewReader(data)))

	runtime.ReadMemStats(&during)
	require.NoError(t, err)
	require.Equal(t, []map[string]interface{}{
		{"id": int64(1), "elapsed": int64(10)},
	}, rows)

	runtime.GC()
	runtime.ReadMemStats(&after)
	totalAllocated := during.TotalAlloc - before.TotalAlloc
	require.Less(t, totalAllocated, uint64(2<<20))
	require.LessOrEqual(t, after.HeapAlloc, before.HeapAlloc+2<<20)
}

func TestCompiledQueryHandlesNilRecords(t *testing.T) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	require.NoError(t, enc.EncodeArrayLen(3))
	require.NoError(t, enc.EncodeNil())
	require.NoError(t, enc.EncodeMapLen(1))
	require.NoError(t, enc.EncodeString("id"))
	require.NoError(t, enc.EncodeInt64(1))
	require.NoError(t, enc.EncodeNil())

	query, err := msgpack.CompileQuery(msgpack.QuerySpec{
		Fields: []string{"id"},
	})
	require.NoError(t, err)

	rows, err := query.Exec(msgpack.NewDecoder(&buf))
	require.NoError(t, err)
	require.Equal(t, []map[string]interface{}{{}, {"id": int64(1)}, {}}, rows)
}

func TestCompiledQuerySupportsMapKeysAndNestedArrayFields(t *testing.T) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	require.NoError(t, enc.EncodeMapLen(1))
	require.NoError(t, enc.EncodeString("items"))
	require.NoError(t, enc.EncodeArrayLen(2))
	require.NoError(t, enc.EncodeMapLen(2))
	require.NoError(t, enc.EncodeString("42"))
	require.NoError(t, enc.EncodeString("map-key"))
	require.NoError(t, enc.EncodeString("nested"))
	require.NoError(t, enc.EncodeArrayLen(2))
	require.NoError(t, enc.EncodeInt64(100))
	require.NoError(t, enc.EncodeString("selected"))
	require.NoError(t, enc.EncodeNil())

	query, err := msgpack.CompileQuery(msgpack.QuerySpec{
		Path:   "items",
		Limit:  1,
		Fields: []string{"42", "nested.1"},
	})
	require.NoError(t, err)

	rows, err := query.Exec(msgpack.NewDecoder(&buf))
	require.NoError(t, err)
	require.Equal(t, []map[string]interface{}{
		{"42": "map-key", "nested.1": "selected"},
	}, rows)
}

func TestCompiledQueryNavigatesArrayPathSegment(t *testing.T) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	require.NoError(t, enc.EncodeMapLen(1))
	require.NoError(t, enc.EncodeString("jobs"))
	require.NoError(t, enc.EncodeArrayLen(2))
	require.NoError(t, enc.EncodeNil())
	require.NoError(t, enc.EncodeArrayLen(1))
	require.NoError(t, enc.EncodeMapLen(1))
	require.NoError(t, enc.EncodeString("id"))
	require.NoError(t, enc.EncodeInt64(7))

	query, err := msgpack.CompileQuery(msgpack.QuerySpec{
		Path:   "jobs.1",
		Fields: []string{"id"},
	})
	require.NoError(t, err)

	rows, err := query.Exec(msgpack.NewDecoder(&buf))
	require.NoError(t, err)
	require.Equal(t, []map[string]interface{}{{"id": int64(7)}}, rows)
}

func TestCompiledQueryPredicateTypeMismatchSkipsRecord(t *testing.T) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	require.NoError(t, enc.EncodeArrayLen(2))
	require.NoError(t, enc.EncodeMapLen(2))
	require.NoError(t, enc.EncodeString("id"))
	require.NoError(t, enc.EncodeInt64(1))
	require.NoError(t, enc.EncodeString("status"))
	require.NoError(t, enc.EncodeInt64(7))
	require.NoError(t, enc.EncodeMapLen(2))
	require.NoError(t, enc.EncodeString("id"))
	require.NoError(t, enc.EncodeInt64(2))
	require.NoError(t, enc.EncodeString("status"))
	require.NoError(t, enc.EncodeString("failed"))

	query, err := msgpack.CompileQuery(msgpack.QuerySpec{
		Fields: []string{"id"},
		Predicate: msgpack.QueryPredicate{
			Path: "status",
			Eq:   "failed",
		},
	})
	require.NoError(t, err)

	rows, err := query.Exec(msgpack.NewDecoder(&buf))
	require.NoError(t, err)
	require.Equal(t, []map[string]interface{}{{"id": int64(2)}}, rows)
}

func TestCompiledQueryTruncatedOutputReportsOutputPath(t *testing.T) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	require.NoError(t, enc.EncodeArrayLen(1))
	require.NoError(t, enc.EncodeMapLen(1))
	require.NoError(t, enc.EncodeString("id"))
	truncated := buf.Bytes()

	query, err := msgpack.CompileQuery(msgpack.QuerySpec{Fields: []string{"id"}})
	require.NoError(t, err)

	_, err = query.Exec(msgpack.NewDecoder(bytes.NewReader(truncated)))
	require.Error(t, err)
	require.True(t, errors.Is(err, io.ErrUnexpectedEOF), err)
	var queryErr *msgpack.QueryError
	require.True(t, errors.As(err, &queryErr))
	require.Equal(t, "[0].id", queryErr.Path)
}

func TestCompiledQueryDoesNotDecodeUnselectedMaps(t *testing.T) {
	data := buildCompiledQueryRecords(t, 12, 256, false)
	query, err := msgpack.CompileQuery(msgpack.QuerySpec{
		Limit:  1,
		Fields: []string{"id"},
		Predicate: msgpack.QueryPredicate{
			Path: "status",
			Eq:   "failed",
		},
	})
	require.NoError(t, err)

	dec := msgpack.NewDecoder(bytes.NewReader(data))
	dec.SetMapDecoder(func(decoder *msgpack.Decoder) (interface{}, error) {
		return nil, errors.New("unselected map was decoded")
	})

	rows, err := query.Exec(dec)
	require.NoError(t, err)
	require.Equal(t, []map[string]interface{}{{"id": int64(1)}}, rows)
}

func TestCompiledQueryTruncationReportsCurrentPath(t *testing.T) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	require.NoError(t, enc.EncodeArrayLen(1))
	require.NoError(t, enc.EncodeMapLen(4))
	require.NoError(t, enc.EncodeString("id"))
	require.NoError(t, enc.EncodeInt64(1))
	require.NoError(t, enc.EncodeString("elapsed"))
	require.NoError(t, enc.EncodeInt64(10))
	require.NoError(t, enc.EncodeString("status"))
	truncated := buf.Bytes()

	query, err := msgpack.CompileQuery(msgpack.QuerySpec{
		Fields: []string{"id", "elapsed"},
		Predicate: msgpack.QueryPredicate{
			Path: "status",
			Eq:   "failed",
		},
	})
	require.NoError(t, err)

	_, err = query.Exec(msgpack.NewDecoder(bytes.NewReader(truncated)))
	require.Error(t, err)
	require.True(t, errors.Is(err, io.ErrUnexpectedEOF), err)
	var queryErr *msgpack.QueryError
	require.True(t, errors.As(err, &queryErr))
	require.Equal(t, "[0].status", queryErr.Path)
}

func TestCompiledQueryTruncatedUnselectedValueReportsCurrentPath(t *testing.T) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	require.NoError(t, enc.EncodeMapLen(1))
	require.NoError(t, enc.EncodeString("jobs"))
	require.NoError(t, enc.EncodeArrayLen(1))
	require.NoError(t, enc.EncodeMapLen(2))
	require.NoError(t, enc.EncodeString("id"))
	require.NoError(t, enc.EncodeInt64(7))
	require.NoError(t, enc.EncodeString("blob"))
	truncated := buf.Bytes()

	query, err := msgpack.CompileQuery(msgpack.QuerySpec{
		Path:   "jobs",
		Fields: []string{"id"},
	})
	require.NoError(t, err)

	_, err = query.Exec(msgpack.NewDecoder(bytes.NewReader(truncated)))
	require.Error(t, err)
	require.True(t, errors.Is(err, io.ErrUnexpectedEOF), err)
	var queryErr *msgpack.QueryError
	require.True(t, errors.As(err, &queryErr))
	require.Equal(t, "jobs[0].blob", queryErr.Path)
}

func buildCompiledQueryRecords(t *testing.T, records, payloadSize int, wrapped bool) []byte {
	t.Helper()

	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	if wrapped {
		require.NoError(t, enc.EncodeMapLen(1))
		require.NoError(t, enc.EncodeString("jobs"))
		require.NoError(t, enc.EncodeMapLen(1))
		require.NoError(t, enc.EncodeString("current"))
	}
	require.NoError(t, enc.EncodeArrayLen(records))

	payload := bytes.Repeat([]byte{'x'}, payloadSize)
	for i := 0; i < records; i++ {
		status := "ok"
		if i%2 == 1 {
			status = "failed"
		}

		require.NoError(t, enc.EncodeMapLen(4))
		require.NoError(t, enc.EncodeString("id"))
		require.NoError(t, enc.EncodeInt64(int64(i)))
		require.NoError(t, enc.EncodeString("elapsed"))
		require.NoError(t, enc.EncodeInt64(int64(i*10)))
		require.NoError(t, enc.EncodeString("status"))
		require.NoError(t, enc.EncodeString(status))
		require.NoError(t, enc.EncodeString("payload"))
		require.NoError(t, enc.EncodeBytes(payload))
	}

	return buf.Bytes()
}
