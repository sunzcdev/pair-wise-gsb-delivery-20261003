package redcon

import (
	"bytes"
	"fmt"
	"math/rand"
	"strconv"
	"testing"
	"time"
)

func isEmptyRESP(resp RESP) bool {
	return resp.Type == 0 && resp.Count == 0 &&
		resp.Data == nil && resp.Raw == nil
}

func expectBad(t *testing.T, payload string) {
	t.Helper()
	n, resp := ReadNextRESP([]byte(payload))
	if n > 0 || !isEmptyRESP(resp) {
		t.Fatalf("expected empty resp")
	}
}

func respVOut(a RESP) string {
	var data string
	var raw string
	if a.Data == nil {
		data = "nil"
	} else {
		data = strconv.Quote(string(a.Data))
	}
	if a.Raw == nil {
		raw = "nil"
	} else {
		raw = strconv.Quote(string(a.Raw))
	}
	return fmt.Sprintf("{Type: %d, Count: %d, Data: %s, Raw: %s}",
		a.Type, a.Count, data, raw,
	)
}

func respEquals(a, b RESP) bool {
	if a.Count != b.Count {
		return false
	}
	if a.Type != b.Type {
		return false
	}
	if (a.Data == nil && b.Data != nil) || (a.Data != nil && b.Data == nil) {
		return false
	}
	if string(a.Data) != string(b.Data) {
		return false
	}
	if (a.Raw == nil && b.Raw != nil) || (a.Raw != nil && b.Raw == nil) {
		return false
	}
	if string(a.Raw) != string(b.Raw) {
		return false
	}
	return true
}

func expectGood(t *testing.T, payload string, exp RESP) {
	t.Helper()
	n, resp := ReadNextRESP([]byte(payload))
	if n != len(payload) || isEmptyRESP(resp) {
		t.Fatalf("expected good resp")
	}
	if string(resp.Raw) != payload {
		t.Fatalf("expected '%s', got '%s'", payload, resp.Raw)
	}
	exp.Raw = []byte(payload)
	switch exp.Type {
	case Integer, String, Error:
		exp.Data = []byte(payload[1 : len(payload)-2])
	}
	if !respEquals(resp, exp) {
		t.Fatalf("expected %v, got %v", respVOut(exp), respVOut(resp))
	}
}

func TestRESP(t *testing.T) {
	expectBad(t, "")
	expectBad(t, "^hello\r\n")
	expectBad(t, "+hello\r")
	expectBad(t, "+hello\n")
	expectBad(t, ":\r\n")
	expectBad(t, ":-\r\n")
	expectBad(t, ":-abc\r\n")
	expectBad(t, ":abc\r\n")
	expectGood(t, ":-123\r\n", RESP{Type: Integer})
	expectGood(t, ":123\r\n", RESP{Type: Integer})
	expectBad(t, "+\r")
	expectBad(t, "+\n")
	expectGood(t, "+\r\n", RESP{Type: String})
	expectGood(t, "+hello world\r\n", RESP{Type: String})
	expectBad(t, "-\r")
	expectBad(t, "-\n")
	expectGood(t, "-\r\n", RESP{Type: Error})
	expectGood(t, "-hello world\r\n", RESP{Type: Error})
	expectBad(t, "$")
	expectBad(t, "$\r")
	expectBad(t, "$\r\n")
	expectGood(t, "$-1\r\n", RESP{Type: Bulk})
	expectGood(t, "$0\r\n\r\n", RESP{Type: Bulk, Data: []byte("")})
	expectBad(t, "$5\r\nhello\r")
	expectBad(t, "$5\r\nhello\n\n")
	expectGood(t, "$5\r\nhello\r\n", RESP{Type: Bulk, Data: []byte("hello")})
	expectBad(t, "*a\r\n")
	expectBad(t, "*3\r\n")
	expectBad(t, "*3\r\n:hello\r")
	expectGood(t, "*3\r\n:1\r\n:2\r\n:3\r\n",
		RESP{Type: Array, Count: 3, Data: []byte(":1\r\n:2\r\n:3\r\n")})

	var xx int
	_, r := ReadNextRESP([]byte("*4\r\n:1\r\n:2\r\n:3\r\n:4\r\n"))
	r.ForEach(func(resp RESP) bool {
		xx++
		x, _ := strconv.Atoi(string(resp.Data))
		if x != xx {
			t.Fatalf("expected %v, got %v", x, xx)
		}
		if xx == 3 {
			return false
		}
		return true
	})
	if xx != 3 {
		t.Fatalf("expected %v, got %v", 3, xx)
	}
}

func TestNextCommand(t *testing.T) {
	rand.Seed(time.Now().UnixNano())
	start := time.Now()
	for time.Since(start) < time.Second {
		// keep copy of pipeline args for final compare
		var plargs [][][]byte

		// create a pipeline of random number of commands with random data.
		N := rand.Int() % 10000
		var data []byte
		for i := 0; i < N; i++ {
			nargs := rand.Int() % 10
			data = AppendArray(data, nargs)
			var args [][]byte
			for j := 0; j < nargs; j++ {
				arg := make([]byte, rand.Int()%100)
				if _, err := rand.Read(arg); err != nil {
					t.Fatal(err)
				}
				data = AppendBulk(data, arg)
				args = append(args, arg)
			}
			plargs = append(plargs, args)
		}

		// break data into random number of chunks
		chunkn := rand.Int() % 100
		if chunkn == 0 {
			chunkn = 1
		}
		if len(data) < chunkn {
			continue
		}
		var chunks [][]byte
		var chunksz int
		for i := 0; i < len(data); i += chunksz {
			chunksz = rand.Int() % (len(data) / chunkn)
			var chunk []byte
			if i+chunksz < len(data) {
				chunk = data[i : i+chunksz]
			} else {
				chunk = data[i:]
			}
			chunks = append(chunks, chunk)
		}

		// process chunks
		var rbuf []byte
		var fargs [][][]byte
		for _, chunk := range chunks {
			var data []byte
			if len(rbuf) > 0 {
				data = append(rbuf, chunk...)
			} else {
				data = chunk
			}
			for {
				complete, args, _, leftover, err := ReadNextCommand(data, nil)
				data = leftover
				if err != nil {
					t.Fatal(err)
				}
				if !complete {
					break
				}
				fargs = append(fargs, args)
			}
			rbuf = append(rbuf[:0], data...)
		}
		// compare final args to original
		if len(plargs) != len(fargs) {
			t.Fatalf("not equal size: %v != %v", len(plargs), len(fargs))
		}
		for i := 0; i < len(plargs); i++ {
			if len(plargs[i]) != len(fargs[i]) {
				t.Fatalf("not equal size for item %v: %v != %v", i, len(plargs[i]), len(fargs[i]))
			}
			for j := 0; j < len(plargs[i]); j++ {
				if !bytes.Equal(plargs[i][j], plargs[i][j]) {
					t.Fatalf("not equal for item %v:%v: %v != %v", i, j, len(plargs[i][j]), len(fargs[i][j]))
				}
			}
		}
	}
}

func TestAppendBulkFloat(t *testing.T) {
	var b []byte
	b = AppendString(b, "HELLO")
	b = AppendBulkFloat(b, 9.123192839)
	b = AppendString(b, "HELLO")
	exp := "+HELLO\r\n$11\r\n9.123192839\r\n+HELLO\r\n"
	if string(b) != exp {
		t.Fatalf("expected '%s', got '%s'", exp, b)
	}
}

func TestAppendBulkInt(t *testing.T) {
	var b []byte
	b = AppendString(b, "HELLO")
	b = AppendBulkInt(b, -9182739137)
	b = AppendString(b, "HELLO")
	exp := "+HELLO\r\n$11\r\n-9182739137\r\n+HELLO\r\n"
	if string(b) != exp {
		t.Fatalf("expected '%s', got '%s'", exp, b)
	}
}

func TestAppendBulkUint(t *testing.T) {
	var b []byte
	b = AppendString(b, "HELLO")
	b = AppendBulkInt(b, 91827391370)
	b = AppendString(b, "HELLO")
	exp := "+HELLO\r\n$11\r\n91827391370\r\n+HELLO\r\n"
	if string(b) != exp {
		t.Fatalf("expected '%s', got '%s'", exp, b)
	}
}

func TestArrayMap(t *testing.T) {
	var dst []byte
	dst = AppendArray(dst, 4)
	dst = AppendBulkString(dst, "key1")
	dst = AppendBulkString(dst, "val1")
	dst = AppendBulkString(dst, "key2")
	dst = AppendBulkString(dst, "val2")
	n, resp := ReadNextRESP(dst)
	if n != len(dst) {
		t.Fatalf("expected '%d', got '%d'", len(dst), n)
	}
	m := resp.Map()
	if len(m) != 2 {
		t.Fatalf("expected '%d', got '%d'", 2, len(m))
	}
	if m["key1"].String() != "val1" {
		t.Fatalf("expected '%s', got '%s'", "val1", m["key1"].String())
	}
	if m["key2"].String() != "val2" {
		t.Fatalf("expected '%s', got '%s'", "val2", m["key2"].String())
	}
}

// expectIncomplete feeds payload to ReadNextRESPEx one byte at a time and
// expects ErrIncomplete until the final byte completes the value.
func expectIncomplete(t *testing.T, payload []byte) RESP {
	t.Helper()
	for i := 1; i < len(payload); i++ {
		n, resp, err := ReadNextRESPEx(payload[:i])
		if err != ErrIncomplete {
			t.Fatalf("expected ErrIncomplete at %d/%d bytes, got n=%d resp=%v err=%v",
				i, len(payload), n, respVOut(resp), err)
		}
		if n != 0 || !isEmptyRESP(resp) {
			t.Fatalf("expected empty resp at %d/%d bytes", i, len(payload))
		}
	}
	n, resp, err := ReadNextRESPEx(payload)
	if err != nil {
		t.Fatalf("expected complete resp, got err=%v", err)
	}
	if n != len(payload) {
		t.Fatalf("expected n=%d, got %d", len(payload), n)
	}
	if string(resp.Raw) != string(payload) {
		t.Fatalf("expected raw '%q', got '%q'", payload, resp.Raw)
	}
	return resp
}

func expectInvalid(t *testing.T, payload string) {
	t.Helper()
	_, resp, err := ReadNextRESPEx([]byte(payload))
	if err == nil || err == ErrIncomplete {
		t.Fatalf("expected invalid error for '%q', got %v", payload, err)
	}
	if !isEmptyRESP(resp) {
		t.Fatalf("expected empty resp for '%q'", payload)
	}
}

func TestRESP3RoundTrip(t *testing.T) {
	// boolean
	if resp := expectIncomplete(t, AppendBool(nil, true)); resp.Type != Boolean ||
		resp.String() != "t" {
		t.Fatalf("bad boolean: %v", respVOut(resp))
	}
	if resp := expectIncomplete(t, AppendBool(nil, false)); resp.Type != Boolean ||
		resp.String() != "f" {
		t.Fatalf("bad boolean: %v", respVOut(resp))
	}
	// double
	for _, f := range []float64{3.14, -1.5, 0, 1e20} {
		resp := expectIncomplete(t, AppendDouble(nil, f))
		if resp.Type != Double {
			t.Fatalf("bad double type: %v", respVOut(resp))
		}
		if resp.Float() != f {
			t.Fatalf("expected %v, got %v", f, resp.Float())
		}
	}
	for _, s := range []string{"inf", "-inf", "nan"} {
		resp := expectIncomplete(t, []byte(","+s+"\r\n"))
		if resp.Type != Double || resp.String() != s {
			t.Fatalf("bad double: %v", respVOut(resp))
		}
	}
	// big number
	big := "3492890328409238509324850943850943825024385"
	resp := expectIncomplete(t, AppendBigNumber(nil, big))
	if resp.Type != BigNumber || resp.String() != big {
		t.Fatalf("bad big number: %v", respVOut(resp))
	}
	resp = expectIncomplete(t, AppendBigNumber(nil, "-"+big))
	if resp.Type != BigNumber || resp.String() != "-"+big {
		t.Fatalf("bad big number: %v", respVOut(resp))
	}
	// verbatim string
	resp = expectIncomplete(t, AppendVerbatim(nil, "txt", []byte("hello world")))
	if resp.Type != Verbatim || resp.String() != "txt:hello world" {
		t.Fatalf("bad verbatim: %v", respVOut(resp))
	}
	// map
	var dst []byte
	dst = AppendMap(dst, 2)
	dst = AppendBulkString(dst, "key1")
	dst = AppendInt(dst, 1)
	dst = AppendBulkString(dst, "key2")
	dst = AppendBool(dst, true)
	resp = expectIncomplete(t, dst)
	if resp.Type != Map || resp.Count != 2 {
		t.Fatalf("bad map: %v", respVOut(resp))
	}
	m := resp.Map()
	if len(m) != 2 || m["key1"].Int() != 1 || m["key2"].String() != "t" {
		t.Fatalf("bad map contents: %v", m)
	}
	if resp.MapGet("key2").String() != "t" {
		t.Fatalf("bad map get")
	}
	// set
	dst = nil
	dst = AppendSet(dst, 3)
	dst = AppendString(dst, "a")
	dst = AppendString(dst, "b")
	dst = AppendString(dst, "c")
	resp = expectIncomplete(t, dst)
	if resp.Type != Set || resp.Count != 3 {
		t.Fatalf("bad set: %v", respVOut(resp))
	}
	var elems []string
	resp.ForEach(func(r RESP) bool {
		elems = append(elems, r.String())
		return true
	})
	if len(elems) != 3 || elems[0] != "a" || elems[1] != "b" || elems[2] != "c" {
		t.Fatalf("bad set elements: %v", elems)
	}
	// push
	dst = nil
	dst = AppendPush(dst, 2)
	dst = AppendBulkString(dst, "message")
	dst = AppendBulkString(dst, "hello")
	resp = expectIncomplete(t, dst)
	if resp.Type != Push || resp.Count != 2 {
		t.Fatalf("bad push: %v", respVOut(resp))
	}
	// nested resp3 inside resp2 array
	dst = nil
	dst = AppendArray(dst, 2)
	dst = AppendMap(dst, 1)
	dst = AppendBulkString(dst, "k")
	dst = AppendDouble(dst, 1.5)
	dst = AppendSet(dst, 1)
	dst = AppendBool(dst, false)
	resp = expectIncomplete(t, dst)
	if resp.Type != Array || resp.Count != 2 {
		t.Fatalf("bad array: %v", respVOut(resp))
	}
	var kinds []Type
	resp.ForEach(func(r RESP) bool {
		kinds = append(kinds, r.Type)
		return true
	})
	if len(kinds) != 2 || kinds[0] != Map || kinds[1] != Set {
		t.Fatalf("bad nested kinds: %v", kinds)
	}
}

func TestRESP3Attribute(t *testing.T) {
	// attribute attached to a following value
	var dst []byte
	dst = AppendAttribute(dst, 1)
	dst = AppendBulkString(dst, "ttl")
	dst = AppendInt(dst, 3600)
	dst = AppendBulkString(dst, "real value")
	resp := expectIncomplete(t, dst)
	if resp.Type != Bulk || resp.String() != "real value" {
		t.Fatalf("bad attributed value: %v", respVOut(resp))
	}
	if len(resp.Attrs) != 1 {
		t.Fatalf("expected 1 attribute, got %d", len(resp.Attrs))
	}
	attr := resp.Attrs[0]
	if attr.Type != Attribute || attr.Count != 1 {
		t.Fatalf("bad attribute: %v", respVOut(attr))
	}
	if attr.MapGet("ttl").Int() != 3600 {
		t.Fatalf("bad attribute contents")
	}
	// two attributes keep wire order
	dst = nil
	dst = AppendAttribute(dst, 1)
	dst = AppendBulkString(dst, "first")
	dst = AppendInt(dst, 1)
	dst = AppendAttribute(dst, 1)
	dst = AppendBulkString(dst, "second")
	dst = AppendInt(dst, 2)
	dst = AppendString(dst, "OK")
	resp = expectIncomplete(t, dst)
	if resp.Type != String || len(resp.Attrs) != 2 {
		t.Fatalf("bad attributed value: %v", respVOut(resp))
	}
	if resp.Attrs[0].MapGet("first").Int() != 1 ||
		resp.Attrs[1].MapGet("second").Int() != 2 {
		t.Fatalf("attributes out of order")
	}
	// attribute inside a container
	dst = nil
	dst = AppendArray(dst, 2)
	dst = AppendAttribute(dst, 1)
	dst = AppendBulkString(dst, "meta")
	dst = AppendString(dst, "x")
	dst = AppendInt(dst, 42)
	dst = AppendInt(dst, 7)
	resp = expectIncomplete(t, dst)
	var first RESP
	resp.ForEach(func(r RESP) bool {
		first = r
		return false
	})
	if first.Type != Integer || first.Int() != 42 || len(first.Attrs) != 1 {
		t.Fatalf("bad attributed element: %v", respVOut(first))
	}
	// attribute without a following value is incomplete, not invalid
	if _, _, err := ReadNextRESPEx([]byte("|1\r\n$4\r\nmeta\r\n")); err != ErrIncomplete {
		t.Fatalf("expected ErrIncomplete, got %v", err)
	}
	if _, _, err := ReadNextRESPEx([]byte("|1\r\n$4\r\nmeta\r\n+ok\r\n")); err != ErrIncomplete {
		t.Fatalf("expected ErrIncomplete, got %v", err)
	}
	if _, _, err := ReadNextRESPEx([]byte("|1\r\n$4\r\nmeta\r\n+ok\r\n:1\r\n")); err != nil {
		t.Fatalf("expected valid read, got %v", err)
	}
}

func TestRESP3Invalid(t *testing.T) {
	expectInvalid(t, "#x\r\n")
	expectInvalid(t, "#tt\r\n")
	expectInvalid(t, "#\r\n")
	expectInvalid(t, ",\r\n")
	expectInvalid(t, ",abc\r\n")
	expectInvalid(t, "(\r\n")
	expectInvalid(t, "(-\r\n")
	expectInvalid(t, "(12a3\r\n")
	expectInvalid(t, "=3\r\nabc\r\n")
	expectInvalid(t, "=4\r\nabcd\r\n")
	expectInvalid(t, "=-1\r\n")
	expectInvalid(t, "%-1\r\n")
	expectInvalid(t, "%a\r\n")
	expectInvalid(t, "~-1\r\n")
	expectInvalid(t, ">-1\r\n")
	expectInvalid(t, "|-1\r\n")
	expectInvalid(t, "$99999999999999999999\r\n")
	expectInvalid(t, "*99999999999999999999\r\n")
	// still incomplete, not invalid
	for _, s := range []string{"#t", ",1.5\r", "(123", "=15\r\ntxt:hello", "%1\r\n+k\r\n"} {
		if _, _, err := ReadNextRESPEx([]byte(s)); err != ErrIncomplete {
			t.Fatalf("expected ErrIncomplete for '%q', got %v", s, err)
		}
	}
}

func TestRESPLimits(t *testing.T) {
	// nesting depth
	var deep []byte
	for i := 0; i < MaxRESPDepth; i++ {
		deep = AppendArray(deep, 1)
	}
	deep = AppendInt(deep, 1)
	if _, _, err := ReadNextRESPEx(deep); err != nil {
		t.Fatalf("expected depth %d to parse, got %v", MaxRESPDepth, err)
	}
	deeper := append(AppendArray(nil, 1), deep...)
	if _, _, err := ReadNextRESPEx(deeper); err == nil || err == ErrIncomplete {
		t.Fatalf("expected depth limit error, got %v", err)
	}
	// attribute chains count towards depth
	var chain []byte
	for i := 0; i < MaxRESPDepth+1; i++ {
		chain = AppendAttribute(chain, 0)
	}
	chain = AppendString(chain, "OK")
	if _, _, err := ReadNextRESPEx(chain); err == nil || err == ErrIncomplete {
		t.Fatalf("expected depth limit error, got %v", err)
	}
	// element count
	tooMany := appendPrefix(nil, '*', MaxRESPElements+1)
	if _, _, err := ReadNextRESPEx(tooMany); err == nil || err == ErrIncomplete {
		t.Fatalf("expected element limit error, got %v", err)
	}
	tooMany = appendPrefix(nil, '%', MaxRESPElements/2+1)
	if _, _, err := ReadNextRESPEx(tooMany); err == nil || err == ErrIncomplete {
		t.Fatalf("expected element limit error, got %v", err)
	}
	// bulk length
	tooBig := appendPrefix(nil, '$', MaxRESPBulkLength+1)
	if _, _, err := ReadNextRESPEx(tooBig); err == nil || err == ErrIncomplete {
		t.Fatalf("expected bulk limit error, got %v", err)
	}
	// a declared length within the limit is incomplete, not invalid
	if _, _, err := ReadNextRESPEx(appendPrefix(nil, '$', MaxRESPBulkLength)); err != ErrIncomplete {
		t.Fatalf("expected ErrIncomplete, got %v", err)
	}
}
