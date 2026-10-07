package msgpack_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/vmihailenco/msgpack/v5"
)

// encodeQueryTestInput encodes a map with a huge unselected "junk" subtree
// and a "records" array. Every record carries an unselected payload of
// payloadSize bytes. When statusFirst is false the predicate field is
// encoded after the output fields.
func encodeQueryTestInput(t *testing.T, numRecords, payloadSize int, statusFirst bool) []byte {
	t.Helper()

	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)

	require.NoError(t, enc.EncodeMapLen(2))

	require.NoError(t, enc.EncodeString("junk"))
	require.NoError(t, enc.EncodeString(strings.Repeat("x", payloadSize*10)))

	require.NoError(t, enc.EncodeString("records"))
	require.NoError(t, enc.EncodeArrayLen(numRecords))
	for i := 0; i < numRecords; i++ {
		status := "ok"
		if i%3 == 0 {
			status = "failed"
		}

		require.NoError(t, enc.EncodeMapLen(4))
		if statusFirst {
			require.NoError(t, enc.EncodeString("status"))
			require.NoError(t, enc.EncodeString(status))
		}
		require.NoError(t, enc.EncodeString("id"))
		require.NoError(t, enc.EncodeInt(int64(i)))
		require.NoError(t, enc.EncodeString("elapsed"))
		require.NoError(t, enc.EncodeInt(int64(1000+i)))
		require.NoError(t, enc.EncodeString("payload"))
		require.NoError(t, enc.EncodeString(strings.Repeat("p", payloadSize)))
		if !statusFirst {
			require.NoError(t, enc.EncodeString("status"))
			require.NoError(t, enc.EncodeString(status))
		}
	}

	return buf.Bytes()
}

func runCompiledQuery(t *testing.T, q *msgpack.CompiledQuery, data []byte) []interface{} {
	t.Helper()

	dec := msgpack.NewDecoder(bytes.NewReader(data))
	dec.UseLooseInterfaceDecoding(true)
	values, err := q.Query(dec)
	require.NoError(t, err)
	return values
}

func TestCompiledQueryFilter(t *testing.T) {
	for _, statusFirst := range []bool{false, true} {
		data := encodeQueryTestInput(t, 100, 1024, statusFirst)

		q, err := msgpack.CompileQuery("records[?status==failed][:10].{id,elapsed}")
		require.NoError(t, err)

		values := runCompiledQuery(t, q, data)
		require.Len(t, values, 20)
		for i := 0; i < 10; i++ {
			require.Equal(t, int64(i*3), values[2*i])
			require.Equal(t, uint64(1000+i*3), values[2*i+1])
		}
	}
}

func TestCompiledQueryFilterSingleOutput(t *testing.T) {
	data := encodeQueryTestInput(t, 100, 16, false)

	q, err := msgpack.CompileQuery("records[?status!=ok][2:4].id")
	require.NoError(t, err)

	// Matching records are 0, 3, 6, 9, ...; [2:4] keeps the 3rd and 4th.
	values := runCompiledQuery(t, q, data)
	require.Equal(t, []interface{}{int64(6), int64(9)}, values)
}

func TestCompiledQueryFilterWholeRecord(t *testing.T) {
	data := encodeQueryTestInput(t, 10, 16, false)

	q, err := msgpack.CompileQuery("records[?status==failed][0]")
	require.NoError(t, err)

	values := runCompiledQuery(t, q, data)
	require.Len(t, values, 1)
	rec, ok := values[0].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, int64(0), rec["id"])
	require.Equal(t, "failed", rec["status"])
}

func TestCompiledQueryFilterNumericPredicate(t *testing.T) {
	data := encodeQueryTestInput(t, 100, 16, false)

	q, err := msgpack.CompileQuery("records[?elapsed==1005].id")
	require.NoError(t, err)
	require.Equal(t, []interface{}{int64(5)}, runCompiledQuery(t, q, data))

	q, err = msgpack.CompileQuery("records[?id==7].elapsed")
	require.NoError(t, err)
	require.Equal(t, []interface{}{uint64(1007)}, runCompiledQuery(t, q, data))
}

func TestCompiledQuerySliceAndIndex(t *testing.T) {
	arr := make([]int64, 20)
	for i := range arr {
		arr[i] = int64(i * 10)
	}
	data, err := msgpack.Marshal(arr)
	require.NoError(t, err)

	for _, tt := range []struct {
		query string
		want  []interface{}
	}{
		{"[2:5]", []interface{}{int64(20), int64(30), int64(40)}},
		{"[:3]", []interface{}{int64(0), int64(10), int64(20)}},
		{"[18:]", []interface{}{int64(180), int64(190)}},
		{"[7]", []interface{}{int64(70)}},
		{"7", []interface{}{int64(70)}},
		{"[5:5]", nil},
		{"[15:30]", []interface{}{int64(150), int64(160), int64(170), int64(180), int64(190)}},
	} {
		q, err := msgpack.CompileQuery(tt.query)
		require.NoError(t, err, tt.query)
		require.Equal(t, tt.want, runCompiledQuery(t, q, data), tt.query)
	}

	q, err := msgpack.CompileQuery("*")
	require.NoError(t, err)
	require.Len(t, runCompiledQuery(t, q, data), 20)
}

func TestCompiledQueryMultiSelect(t *testing.T) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)

	require.NoError(t, enc.EncodeMapLen(3))
	require.NoError(t, enc.EncodeString("c"))
	require.NoError(t, enc.EncodeInt(3))
	require.NoError(t, enc.EncodeString("a"))
	require.NoError(t, enc.EncodeInt(1))
	require.NoError(t, enc.EncodeString("nested"))
	require.NoError(t, enc.EncodeMapLen(2))
	require.NoError(t, enc.EncodeString("x"))
	require.NoError(t, enc.EncodeInt(10))
	require.NoError(t, enc.EncodeString("y"))
	require.NoError(t, enc.EncodeInt(20))

	q, err := msgpack.CompileQuery("{a,c,nested.x}")
	require.NoError(t, err)

	// Values are returned in stream order.
	require.Equal(t,
		[]interface{}{int64(3), int64(1), int64(10)},
		runCompiledQuery(t, q, buf.Bytes()))
}

func TestCompiledQueryReuse(t *testing.T) {
	q, err := msgpack.CompileQuery("records[?status==failed][:2].id")
	require.NoError(t, err)

	for _, numRecords := range []int{10, 50} {
		data := encodeQueryTestInput(t, numRecords, 16, false)
		values := runCompiledQuery(t, q, data)
		require.Equal(t, []interface{}{int64(0), int64(3)}, values)
	}
}

func TestCompiledQueryTruncated(t *testing.T) {
	data := encodeQueryTestInput(t, 10, 64, false)

	q, err := msgpack.CompileQuery("records[?status==failed][:10].{id,elapsed}")
	require.NoError(t, err)

	sawRecordPath := false
	cuts := []int{len(data) - 1, len(data) - 10, len(data) / 2, len(data) * 3 / 4}
	for _, cut := range cuts {
		dec := msgpack.NewDecoder(bytes.NewReader(data[:cut]))
		_, err := q.Query(dec)
		require.Error(t, err, "cut=%d", cut)

		var qerr *msgpack.QueryError
		require.True(t, errors.As(err, &qerr), "cut=%d err=%v", cut, err)
		require.NotEmpty(t, qerr.Path)
		require.True(t,
			errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF),
			"cut=%d err=%v", cut, err)
		t.Logf("cut=%d path=%s", cut, qerr.Path)
		if strings.Contains(qerr.Path, "records[?status==failed][:10][") {
			sawRecordPath = true
		}
	}
	require.True(t, sawRecordPath, "expected an error path pointing at a record")
}

func TestCompiledQueryAllocs(t *testing.T) {
	// ~32MB of input, almost all of it in unselected subtrees.
	data := encodeQueryTestInput(t, 500, 64*1024, false)

	q, err := msgpack.CompileQuery("records[?status==failed][:10].{id,elapsed}")
	require.NoError(t, err)

	var values []interface{}
	allocs := testing.AllocsPerRun(5, func() {
		dec := msgpack.NewDecoder(bytes.NewReader(data))
		dec.UseLooseInterfaceDecoding(true)
		var err error
		values, err = q.Query(dec)
		if err != nil {
			t.Fatal(err)
		}
	})

	require.Len(t, values, 20)
	for i := 0; i < 10; i++ {
		require.Equal(t, int64(i*3), values[2*i])
		require.Equal(t, uint64(1000+i*3), values[2*i+1])
	}
	t.Logf("allocs per run: %v", allocs)
	require.Less(t, allocs, float64(200))
}

func TestCompileQueryErrors(t *testing.T) {
	for _, query := range []string{
		"records[?status].id",   // predicate without operator
		"records[?==failed].id", // empty predicate field
		"records[].id",          // empty brackets
		"records[abc].id",       // invalid array index
		"records[-1].id",        // negative array index
		"records[1:2:3].id",     // invalid slice
		"records..id",           // empty segment
		"records[?a==1",         // unbalanced brackets
		"records{a,b}.c",        // multi-select not in last position
		"{a,a}",                 // duplicate key
		"{0,1}",                 // branch does not start with a map key
		"a[?x==1][?y==2]",       // filter after filter
	} {
		_, err := msgpack.CompileQuery(query)
		require.Error(t, err, query)
	}
}

func BenchmarkCompiledQuery(b *testing.B) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	if err := enc.EncodeArrayLen(1000); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		if err := enc.EncodeMapLen(4); err != nil {
			b.Fatal(err)
		}
		if err := enc.EncodeString("id"); err != nil {
			b.Fatal(err)
		}
		if err := enc.EncodeInt(int64(i)); err != nil {
			b.Fatal(err)
		}
		if err := enc.EncodeString("elapsed"); err != nil {
			b.Fatal(err)
		}
		if err := enc.EncodeInt(int64(1000 + i)); err != nil {
			b.Fatal(err)
		}
		if err := enc.EncodeString("payload"); err != nil {
			b.Fatal(err)
		}
		if err := enc.EncodeString(strings.Repeat("p", 1024)); err != nil {
			b.Fatal(err)
		}
		if err := enc.EncodeString("status"); err != nil {
			b.Fatal(err)
		}
		if err := enc.EncodeString("failed"); err != nil {
			b.Fatal(err)
		}
	}
	data := buf.Bytes()

	q, err := msgpack.CompileQuery("[?status==failed][:10].{id,elapsed}")
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dec := msgpack.NewDecoder(bytes.NewReader(data))
		values, err := q.Query(dec)
		if err != nil {
			b.Fatal(err)
		}
		if len(values) != 20 {
			b.Fatalf("got %d values", len(values))
		}
	}
}
