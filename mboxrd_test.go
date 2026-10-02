package mbox

import (
	"bytes"
	"io"
	"io/ioutil"
	"strings"
	"testing"
	"time"
)

// writeInChunks writes p to w split into chunks of the given sizes (the last
// chunk may be shorter). It mirrors how callers may split "From ", the ">"
// chain or CRLF across Write calls.
func writeInChunks(t *testing.T, w io.Writer, p []byte, sizes ...int) {
	t.Helper()
	for i := 0; i < len(p); {
		n := sizes[i%len(sizes)]
		if i+n > len(p) {
			n = len(p) - i
		}
		written, err := w.Write(p[i : i+n])
		if err != nil {
			t.Fatalf("Write(%q) = %v", p[i:i+n], err)
		}
		if written != n {
			t.Fatalf("Write(%q) wrote %d bytes, want %d", p[i:i+n], written, n)
		}
		i += n
	}
}

func mboxrdWrite(t *testing.T, message []byte, sizes ...int) string {
	var b bytes.Buffer
	mw := NewWriterFormat(&b, Mboxrd)
	msg, err := mw.CreateMessage("sender@example.com",
		time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	writeInChunks(t, msg, message, sizes...)
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestMboxrdWriterQuoting(t *testing.T) {
	date := time.Unix(0, 0).UTC().Format(time.ANSIC)
	msg := strings.Join([]string{
		"From zero layers.",
		">From one layer.",
		">>>From three layers.",
		"middle From is untouched.",
		"plain > >From mix From x",
		"Fromx is not a separator.",
		"Fro neither.",
		">>but >From again.",
		"",
	}, "\n")

	want := "From sender@example.com " + date + "\n" + strings.Join([]string{
		">From zero layers.",
		">>From one layer.",
		">>>>From three layers.",
		"middle From is untouched.",
		"plain > >From mix From x",
		"Fromx is not a separator.",
		"Fro neither.",
		">>but >From again.",
		"",
	}, "\n")
	want += "\n\n"

	for _, sizes := range [][]int{
		{len(msg)}, // single write
		{1},        // byte by byte
		{3},
		{5},
		{2, 7},
		{64},
	} {
		if got := mboxrdWrite(t, []byte(msg), sizes...); got != want {
			t.Errorf("chunk sizes %v:\nwant %q\ngot  %q", sizes, want, got)
		}
	}
}

func TestMboxrdWriterCRLFSplit(t *testing.T) {
	date := time.Unix(0, 0).UTC().Format(time.ANSIC)
	msg := []byte("header\r\n\r\nFrom quoted\r\n>>From deep\r\nmid From x\r\n")
	want := "From sender@example.com " + date + "\n" +
		"header\n\n>From quoted\n>>>From deep\nmid From x\n\n\n"

	for _, sizes := range [][]int{{1}, {2}, {3}, {5}, {13}, {len(msg)}} {
		if got := mboxrdWrite(t, msg, sizes...); got != want {
			t.Errorf("chunk sizes %v:\nwant %q\ngot  %q", sizes, want, got)
		}
	}
}

func TestMboxrdWriterWriteCounts(t *testing.T) {
	// Regardless of internal quoting, Write must report all input bytes as
	// accepted even when "From " and the ">" run are split across calls.
	for _, format := range []Format{Mboxo, Mboxrd} {
		var b bytes.Buffer
		mw := NewWriterFormat(&b, format)
		msg, err := mw.CreateMessage("s", time.Unix(0, 0).UTC())
		if err != nil {
			t.Fatal(err)
		}
		chunks := []string{">>>", "Fr", "om ", "body\r", "\n", "From x"}
		var total int
		for _, ch := range chunks {
			n, err := msg.Write([]byte(ch))
			if err != nil {
				t.Fatalf("format %v: %v", format, err)
			}
			if n != len(ch) {
				t.Fatalf("format %v: Write(%q) = %d, want %d", format, ch, n, len(ch))
			}
			total += n
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMboxrdWriterUnterminatedLine(t *testing.T) {
	// A line without a trailing newline can never be a fully recognized
	// "From " line; fragments must be flushed verbatim on Close.
	for _, msg := range []string{
		"From",
		"Fro",
		"From ",
		">>>",
		">>>From",
		">>>From ",
		"abc",
	} {
		got := mboxrdWrite(t, []byte(msg), 1)
		if !strings.HasSuffix(got, msg+"\n\n") {
			t.Errorf("unterminated %q not preserved: %q", msg, got)
		}
	}
}

func TestMboxrdWriterCandidateCRLF(t *testing.T) {
	// CRLF must be normalized even when the CR is part of a candidate prefix
	// run, and a "From " starting right after a split CRLF must be quoted.
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"gt then CRLF", ">>>\r\n", ">>>\n"},
		{"from prefix then CRLF", "Fro\r\n", "Fro\n"},
		{"gt then From CRLF", ">>From x\r\n", ">>>From x\n"},
		{"CRLF split then From", "x\r\nFrom y\r\n", "x\n>From y\n"},
		{"lone CR then From", "x\rFrom y\n", "x\rFrom y\n"},
		{"long gt non from", strings.Repeat(">", 100000) + "x\n",
			strings.Repeat(">", 100000) + "x\n"},
	}

	date := time.Unix(0, 0).UTC().Format(time.ANSIC)
	prefix := "From sender@example.com " + date + "\n"
	for _, tc := range cases {
		for _, sizes := range [][]int{{1}, {2}, {4}, {len(tc.in)}} {
			got := mboxrdWrite(t, []byte(tc.in), sizes...)
			want := prefix + tc.want + "\n\n"
			if got != want {
				t.Errorf("%s sizes %v:\nwant %q\ngot  %q", tc.name, sizes, want, got)
			}
		}
	}
}

func TestMboxrdWriterBoundedBuffering(t *testing.T) {
	// An arbitrarily long line (and an arbitrarily long ">" run) must be
	// streamed: the writer may only ever hold a constant amount of state.
	var rec countingWriter
	mw := NewWriterFormat(&rec, Mboxrd)
	msg, err := mw.CreateMessage("sender@example.com", time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}

	const n = 1 << 20
	before := rec.total
	if _, err := msg.Write(bytes.Repeat([]byte{'>'}, n)); err != nil {
		t.Fatal(err)
	}
	if rec.total-before != n {
		t.Fatalf("held %d of %d leading \">\" bytes", n-(rec.total-before), n)
	}
	if m := msg.(*messageWriter); m.state != stateGt {
		t.Fatalf("state = %d, want stateGt", m.state)
	}

	// Completing the line with "From x\n" adds exactly one quoting layer.
	if _, err := io.WriteString(msg, "From x\n"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	prefix := strings.Repeat(">", n+1) + "From x\n\n\n"
	if !strings.HasSuffix(rec.b.String(), prefix) {
		t.Fatalf("long \">\" run not quoted with one extra layer")
	}

	// A long ordinary line is written through immediately.
	rec = countingWriter{}
	mw = NewWriterFormat(&rec, Mboxrd)
	msg, _ = mw.CreateMessage("sender@example.com", time.Unix(0, 0).UTC())
	line := bytes.Repeat([]byte{'x'}, n)
	before = rec.total
	if _, err := msg.Write(line); err != nil {
		t.Fatal(err)
	}
	if rec.total-before != n {
		t.Fatalf("buffered %d bytes of a long unterminated line", n-(rec.total-before))
	}
}

type countingWriter struct {
	b     bytes.Buffer
	total int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.total += len(p)
	return c.b.Write(p)
}

func TestMboxrdReaderUnescaping(t *testing.T) {
	mbox := "From sender@example.com Thu Jan  1 00:00:01 1970\r\n" +
		strings.Join([]string{
			">From zero layers.",
			">>From one layer.",
			">>>>From three layers.",
			"middle From is untouched.",
			">Fromx is untouched.",
			">>>> plain line untouched.",
			"",
			"",
			"",
		}, "\r\n")
	want := strings.Join([]string{
		"From zero layers.",
		">From one layer.",
		">>>From three layers.",
		"middle From is untouched.",
		">Fromx is untouched.",
		">>>> plain line untouched.",
		"",
		"",
		"",
	}, "\r\n")

	r := NewReaderFormat(strings.NewReader(mbox), Mboxrd)
	msg, err := r.NextMessage()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ioutil.ReadAll(msg)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("want %q\ngot  %q", want, got)
	}
}

func TestMboxoReaderKeepsMultiLayerEscaping(t *testing.T) {
	// The default mboxo rules only remove a single ">From " layer; deeper
	// layers are not interpreted.
	mbox := "From sender@example.com Thu Jan  1 00:00:01 1970\r\n" +
		">From one\r\n>>From two\r\n>>>From three\r\nx\r\n"
	want := "From one\r\n>>From two\r\n>>>From three\r\nx\r\n"

	r := NewReader(strings.NewReader(mbox))
	msg, err := r.NextMessage()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ioutil.ReadAll(msg)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("want %q\ngot  %q", want, got)
	}
}

func TestMboxrdReaderLongEscapedLine(t *testing.T) {
	// A quoted line longer than bufio's ReadLine chunks must only be
	// unescaped once and must remain part of the current message.
	long := strings.Repeat("x", 5000)
	mbox := "From s x\r\n>>From " + long + "\r\n"

	r := NewReaderFormat(strings.NewReader(mbox), Mboxrd)
	msg, err := r.NextMessage()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ioutil.ReadAll(msg)
	if err != nil {
		t.Fatal(err)
	}
	want := ">From " + long + "\r\n"
	if string(got) != want {
		t.Fatalf("long quoted line mismatch (len got=%d want=%d)", len(got), len(want))
	}
	if _, err := r.NextMessage(); err != io.EOF {
		t.Fatalf("NextMessage() = %v, want io.EOF", err)
	}
}

func TestMboxrdReaderMiddleFromUntouched(t *testing.T) {
	// A "From " that is not at the start of a line must be returned verbatim
	// and must never be treated as a separator.
	mbox := "From s x\r\nmiddle From line\r\n\tFrom tabbed\r\n \r\nFrom s2 y\r\nB\r\n"
	r := NewReaderFormat(strings.NewReader(mbox), Mboxrd)

	msg, err := r.NextMessage()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ioutil.ReadAll(msg)
	if err != nil {
		t.Fatal(err)
	}
	want := "middle From line\r\n\tFrom tabbed\r\n \r\n"
	if string(got) != want {
		t.Errorf("want %q\ngot  %q", want, got)
	}

	msg, err = r.NextMessage()
	if err != nil {
		t.Fatal(err)
	}
	got, _ = ioutil.ReadAll(msg)
	if string(got) != "B\r\n" {
		t.Errorf("second message = %q", got)
	}
}

// TestMboxrdRoundTrip writes messages with the mboxrd writer (splitting the
// input across chunk boundaries, including splits inside "From ", the ">" run
// and CRLF), then reads them back with the mboxrd reader and verifies that the
// original message text is preserved under the reader's CRLF newline policy.
func TestMboxrdRoundTrip(t *testing.T) {
	input := strings.Join([]string{
		"From: sender@example.com",
		"Subject: mboxrd round trip",
		"",
		"Zero layer: From with love.",
		"One layer: >From with love.",
		"Three layers: >>>From with love.",
		"Middle From stays.",
		"Two in one body: From a and >From b.",
		"Long line: " + strings.Repeat("abc", 5000),
		"",
	}, "\n")
	// Close() terminates a message with two extra LF, which the reader
	// surfaces as two trailing CRLF (matching the mboxo package behavior).
	want := strings.Replace(input, "\n", "\r\n", -1) + "\r\n\r\n"

	var chunkings = [][]int{
		{len(input)},
		{1},
		{2},
		{5},
		{4096},
		{3, 1, 7, 64},
	}

	for n, sizes := range chunkings {
		var buf bytes.Buffer
		mw := NewWriterFormat(&buf, Mboxrd)
		msg, err := mw.CreateMessage("sender@example.com", time.Unix(0, 0).UTC())
		if err != nil {
			t.Fatal(err)
		}
		writeInChunks(t, msg, []byte(input), sizes...)
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}

		r := NewReaderFormat(bytes.NewReader(buf.Bytes()), Mboxrd)
		rm, err := r.NextMessage()
		if err != nil {
			t.Fatalf("chunking %d: NextMessage() = %v\narchive:\n%q", n, err, buf.String())
		}
		got, err := ioutil.ReadAll(rm)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("chunking %d: round trip mismatch:\nwant %q\ngot  %q", n, want, got)
		}
		if _, err := r.NextMessage(); err != io.EOF {
			t.Fatalf("chunking %d: NextMessage() = %v, want io.EOF", n, err)
		}
	}
}

func TestMboxrdRoundTripMultipleMessages(t *testing.T) {
	messages := []string{
		"Subject: one\n\n>From a\n",
		"Subject: two\n\nFrom b\n>>From c\nmid From d\n",
		"Subject: three\n\nplain\n",
	}

	var buf bytes.Buffer
	mw := NewWriterFormat(&buf, Mboxrd)
	for _, m := range messages {
		msg, err := mw.CreateMessage("sender@example.com", time.Unix(0, 0).UTC())
		if err != nil {
			t.Fatal(err)
		}
		writeInChunks(t, msg, []byte(m), 1)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	r := NewReaderFormat(bytes.NewReader(buf.Bytes()), Mboxrd)
	for i, wantLF := range messages {
		msg, err := r.NextMessage()
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		got, err := ioutil.ReadAll(msg)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(wantLF, "\n", "\r\n", -1) + "\r\n\r\n"
		if string(got) != want {
			t.Errorf("message %d:\nwant %q\ngot  %q", i, want, got)
		}
	}
	if _, err := r.NextMessage(); err != io.EOF {
		t.Fatalf("NextMessage() = %v, want io.EOF", err)
	}
}
