package mbox

import (
	"bytes"
	"io"
	"io/ioutil"
	"strings"
	"testing"
	"time"
)

func TestWriter_mboxrdEscaping(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"zero layers", "From a\n", ">From a\n"},
		{"one layer", ">From a\n", ">>From a\n"},
		{"three layers", ">>>From a\n", ">>>>From a\n"},
		{"gt prefix no from", ">foo\n", ">foo\n"},
		{"from not at line start", "x From a\n", "x From a\n"},
		{"from without trailing space", "Fromx\n", "Fromx\n"},
		{"incomplete from at eof", "Fro", "Fro"},
		{"gt run without from", ">>>\n", ">>>\n"},
		{"crlf normalized", ">From a\r\nx\r\n", ">>From a\nx\n"},
		{"cr splits crlf", "a\r\nb", "a\nb"},
		{"bare cr is in-line", ">From\r a", ">From\r a"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			mw := NewWriterMode(&b, MBOXRD)
			w, err := mw.CreateMessage("-", testDate)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(w, tc.in); err != nil {
				t.Fatal(err)
			}
			if err := mw.Close(); err != nil {
				t.Fatal(err)
			}

			got := strings.SplitN(b.String(), "\n", 2)[1]
			got = strings.TrimSuffix(got, "\n\n")
			if got != tc.want {
				t.Errorf("want %q, got %q", tc.want, got)
			}
		})
	}
}

var testDate = mustTestDate()

func mustTestDate() time.Time {
	t, err := time.Parse(time.RFC1123Z, "Thu, 01 Jan 2015 00:00:01 +0000")
	if err != nil {
		panic(err)
	}
	return t
}

func writeMboxrd(t *testing.T, body string, chunk int) string {
	t.Helper()

	var b bytes.Buffer
	mw := NewWriterMode(&b, MBOXRD)
	w, err := mw.CreateMessage("-", testDate)
	if err != nil {
		t.Fatal(err)
	}

	if chunk <= 0 {
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	} else {
		buf := []byte(body)
		for len(buf) > 0 {
			n := chunk
			if n > len(buf) {
				n = len(buf)
			}
			written, err := w.Write(buf[:n])
			if err != nil {
				t.Fatal(err)
			}
			if written != n {
				t.Fatalf("Write() = %v, want %v", written, n)
			}
			buf = buf[n:]
		}
	}

	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Fragmented writes must produce byte-identical output to a single write,
// even for arbitrarily long lines without newlines and CRLF pairs split
// across chunks.
func TestWriter_mboxrdFragmentation(t *testing.T) {
	payloads := []string{
		"From a\n>From b\n>>From c\n",
		"From a\r\n>From b\r\n",
		"x\r",
		"\n",
		"F",
		"Fr",
		">From \r\n",
		strings.Repeat(">", 200000) + "From x\n",
		strings.Repeat(">", 200000) + "\n",
		strings.Repeat("a", 200000),
		"From " + strings.Repeat("z", 200000),
		">>>\r\nFrom \r\n",
	}

	chunks := []int{0, 1, 2, 3, 5, 7, 64, 4095, 4096, 4097, 65536}
	for i, p := range payloads {
		reference := writeMboxrd(t, p, 0)
		for _, c := range chunks {
			got := writeMboxrd(t, p, c)
			if got != reference {
				t.Fatalf("payload %d chunk %d:\nwant %q...\ngot  %q...", i, c,
					shorten(reference), shorten(got))
			}
		}
	}
}

func shorten(s string) string {
	if len(s) > 80 {
		return s[:40] + "..." + s[len(s)-30:]
	}
	return s
}

func readMboxrd(t *testing.T, mbox string) string {
	t.Helper()

	r := NewReaderMode(strings.NewReader(mbox), MBOXRD)
	mr, err := r.NextMessage()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ioutil.ReadAll(mr)
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

func TestReader_mboxrdUnescaping(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{"one layer", ">From a", "From a"},
		{"two layers", ">>From a", ">From a"},
		{"five layers", ">>>>>From a", ">>>>From a"},
		{"gt without from", ">>foo", ">>foo"},
		{"from mid-line stays", "x >From a", "x >From a"},
		{"from no space", ">Fromx", ">Fromx"},
		{"gt run alone", ">>>>", ">>>>"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mbox := "From sender Thu Jan  1 00:00:01 2015\r\n\r\n" + tc.line + "\r\n"
			want := "\r\n" + tc.want + "\r\n"
			if got := readMboxrd(t, mbox); got != want {
				t.Errorf("want %q, got %q", want, got)
			}
		})
	}
}

// A leading '>' run longer than the bufio read size must be processed
// across fragments without being buffered without bound.
func TestReader_mboxrdLongGTRun(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			"long run followed by from",
			strings.Repeat(">", 70000) + "From x\r\n",
			strings.Repeat(">", 69999) + "From x\r\n",
		},
		{
			"long run alone",
			strings.Repeat(">", 70000) + "\r\n",
			strings.Repeat(">", 70000) + "\r\n",
		},
		{
			"long run with partial from across fragments",
			strings.Repeat(">", 70000) + "Fromx\r\n",
			strings.Repeat(">", 70000) + "Fromx\r\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mbox := "From sender Thu Jan  1 00:00:01 2015\r\n" + tc.body
			if got := readMboxrd(t, mbox); got != tc.want {
				t.Errorf("len(want)=%d len(got)=%d", len(tc.want), len(got))
			}
		})
	}
}

// normalizeLineEndings turns all LF and CRLF into CRLF, matching the
// reader's output newline strategy.
func normalizeLineEndings(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// Round-trip bodies with zero and multiple quoting layers, using both LF
// and CRLF (split across write blocks). The original body must be
// recovered under the reader's CRLF newline strategy.
func trimTrailingBlankLines(s string) string {
	return strings.TrimRight(s, "\r\n")
}

func TestMboxrdRoundTrip(t *testing.T) {
	bodies := []string{
		"From: a\r\n\r\nhello\r\nFrom line\r\n>From quoted\r\n>>From deep\r\n",
		"From: a\n\nhello\nFrom line\n>From quoted\n>>From deep\n",
		">From a\r\n>>>From b\r\nmid From line\r\n",
		"From: a\r\n\r\n" + strings.Repeat(">", 70000) + "From x\r\n",
		"no escaping needed here at all\r\n",
		"From " + strings.Repeat("y", 70000) + "\r\n",
	}

	for i, body := range bodies {
		want := normalizeLineEndings(body)
		for _, chunk := range []int{1, 3, 4096} {
			mbox := writeMboxrd(t, body, chunk)
			got := readMboxrd(t, mbox)
			if trimTrailingBlankLines(got) != trimTrailingBlankLines(want) {
				t.Fatalf("body %d chunk %d mismatch (want %d bytes, got %d bytes)",
					i, chunk, len(want), len(got))
			}
		}
	}
}

// Multiple messages with CRLF split across write blocks round-trip.
func TestMboxrdRoundTripMultipleMessages(t *testing.T) {
	msgs := []string{
		"From: a\r\n\r\nFrom body\r\n>From quoted\r\n",
		"From: b\n\nline\r\n>>From deep\n",
	}

	var b bytes.Buffer
	mw := NewWriterMode(&b, MBOXRD)
	for _, m := range msgs {
		w, err := mw.CreateMessage("-", testDate)
		if err != nil {
			t.Fatal(err)
		}
		buf := []byte(m)
		for len(buf) > 0 {
			n := 3
			if n > len(buf) {
				n = len(buf)
			}
			if _, err := w.Write(buf[:n]); err != nil {
				t.Fatal(err)
			}
			buf = buf[n:]
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	r := NewReaderMode(&b, MBOXRD)
	for i, m := range msgs {
		mr, err := r.NextMessage()
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		got, err := ioutil.ReadAll(mr)
		if err != nil {
			t.Fatal(err)
		}
		want := normalizeLineEndings(m)
		if trimTrailingBlankLines(string(got)) != trimTrailingBlankLines(want) {
			t.Errorf("message %d mismatch", i)
		}
	}
	if _, err := r.NextMessage(); err != io.EOF {
		t.Fatalf("NextMessage() = %v, want EOF", err)
	}
}

// The default constructors keep the historical mboxo rules.
func TestMboxoDefaultUnchanged(t *testing.T) {
	mbox := "From sender Thu Jan  1 00:00:01 2015\r\n\r\n>From one\r\n>>From two\r\n"
	mboxrdWant := "\r\nFrom one\r\n>From two\r\n"
	mboxoWant := "\r\nFrom one\r\n>>From two\r\n"

	if got := readMboxrd(t, mbox); got != mboxrdWant {
		t.Errorf("MBOXRD: want %q, got %q", mboxrdWant, got)
	}

	r := NewReader(strings.NewReader(mbox))
	mr, err := r.NextMessage()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ioutil.ReadAll(mr)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != mboxoWant {
		t.Errorf("mboxo default changed: want %q, got %q", mboxoWant, got)
	}

	rm := NewReaderMode(strings.NewReader(mbox), MBOXO)
	mr2, err := rm.NextMessage()
	if err != nil {
		t.Fatal(err)
	}
	got2, err := ioutil.ReadAll(mr2)
	if err != nil {
		t.Fatal(err)
	}
	if string(got2) != mboxoWant {
		t.Errorf("explicit MBOXO: want %q, got %q", mboxoWant, got2)
	}
}
