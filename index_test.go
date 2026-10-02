package mbox

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"strings"
	"testing"
)

// readSequential decodes all messages with the sequential Reader, the same
// reference the index readers are compared against.
func readSequential(t *testing.T, mbox string) [][]byte {
	t.Helper()

	r := NewReader(strings.NewReader(mbox))
	var messages [][]byte
	for {
		mr, err := r.NextMessage()
		if err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("sequential NextMessage(): %v", err)
		}

		b, err := ioutil.ReadAll(mr)
		if err != nil {
			t.Fatalf("sequential message read: %v", err)
		}
		messages = append(messages, b)
	}
	return messages
}

func assertIndexParity(t *testing.T, mbox string) *Index {
	t.Helper()

	want := readSequential(t, mbox)

	idx, err := BuildIndex(strings.NewReader(mbox))
	if err != nil {
		t.Fatalf("BuildIndex(): %v", err)
	}

	if idx.Len() != len(want) {
		t.Fatalf("indexed %d messages, sequential reader found %d", idx.Len(), len(want))
	}

	raw := []byte(mbox)
	var prevEnd int64
	for i, e := range idx.Entries {
		if !(e.Separator < e.Body && e.Body <= e.End) {
			t.Errorf("message %d: invalid offsets Separator=%d Body=%d End=%d",
				i, e.Separator, e.Body, e.End)
		}
		if i > 0 && e.Separator < prevEnd {
			t.Errorf("message %d: separator %d is before previous end %d",
				i, e.Separator, prevEnd)
		}
		if e.Separator+int64(len(header)) > int64(len(raw)) ||
			!bytes.HasPrefix(raw[e.Separator:], header) {
			t.Errorf("message %d: Separator %d does not point at a %q line",
				i, e.Separator, header)
		}
		if i == len(idx.Entries)-1 && e.End != int64(len(raw)) {
			t.Errorf("last message End = %d, want archive length %d",
				e.End, len(raw))
		}
		prevEnd = e.End

		got, err := ioutil.ReadAll(e.Reader(bytes.NewReader(raw)))
		if err != nil {
			t.Errorf("message %d: indexed read: %v", i, err)
			continue
		}
		if !bytes.Equal(got, want[i]) {
			t.Errorf("message %d: indexed reader = %q, want %q", i, got, want[i])
		}
	}

	return idx
}

func TestBuildIndexParity(t *testing.T) {
	tests := []struct {
		name string
		mbox string
	}{
		{"empty", ""},
		{"only blank lines", "\n\n\r\n"},
		{"one message", mboxWithOneMessage},
		{"three messages", mboxWithThreeMessages},
		{"leading blank line", mboxWithStartingLF},
		{"malformed but valid", mboxWithThreeMessagesMalformedButValid},
		{"last message no blank line", "From s D\nbody\nFrom s2 D2\nbody2"},
		{"last message no blank line CRLF", "From s D\r\nbody\r\nFrom s2 D2\r\nbody2"},
		{"only final newline", "From s D\n"},
		{"single empty message with blank", "From s D\n\n"},
		{"adjacent separators", "From a D\n\nFrom b D2\n\nFrom c D3\n\n"},
		{"adjacent separators without blank", "From a D\nFrom b D2\nFrom c D3\n"},
		{"empty first message", "\n\nFrom a D\n\nFrom b D2\nx\n"},
		{"extra blank lines between messages", "From a D\nb1\n\n\n\nFrom b D2\nb2\n"},
		{"escaped from line", "From a D\n>From quoted\nFrom \n"},
		{"CRLF", toCRLF(mboxWithThreeMessages)},
		{"CRLF empty messages", "From a D\r\n\r\nFrom b D2\r\n\r\n"},
		{"CRLF no blank between", "From a D\r\nb1\r\nFrom b D2\r\nb2\r\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertIndexParity(t, test.mbox)
		})
	}
}

func TestBuildIndexRawOffsets(t *testing.T) {
	for _, nl := range []string{"\n", "\r\n"} {
		sep1 := "From sender1 D" + nl
		body1 := "Subject: 1" + nl + nl + "Body A" + nl
		blank := nl
		sep2 := "From sender2 D" + nl
		body2 := "Subject: 2" + nl + nl + "Body B" + nl

		mbox := sep1 + body1 + blank + sep2 + body2

		idx, err := BuildIndex(strings.NewReader(mbox))
		if err != nil {
			t.Fatalf("%q: BuildIndex(): %v", nl, err)
		}
		if idx.Len() != 2 {
			t.Fatalf("%q: Len() = %d, want 2", nl, idx.Len())
		}

		want := []IndexEntry{
			{
				Separator: 0,
				Body:      int64(len(sep1)),
				End:       int64(len(sep1 + body1 + blank)),
			},
			{
				Separator: int64(len(sep1 + body1 + blank)),
				Body:      int64(len(sep1 + body1 + blank + sep2)),
				End:       int64(len(mbox)),
			},
		}

		for i := range want {
			if got := idx.Entries[i]; got != want[i] {
				t.Errorf("%q: entry %d = %+v, want %+v", nl, i, got, want[i])
			}
		}
	}
}

// oneByteReader feeds the builder a single byte per Read call.
type oneByteReader struct {
	b []byte
	i int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.b[r.i]
	r.i++
	return 1, nil
}

func TestBuildIndexStreamSmallReads(t *testing.T) {
	mbox := mboxWithThreeMessages
	idx, err := BuildIndex(&oneByteReader{b: []byte(mbox)})
	if err != nil {
		t.Fatalf("BuildIndex(): %v", err)
	}
	if idx.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", idx.Len())
	}

	raw := []byte(mbox)
	want := readSequential(t, mbox)
	for i, e := range idx.Entries {
		got, err := ioutil.ReadAll(e.Reader(bytes.NewReader(raw)))
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if !bytes.Equal(got, want[i]) {
			t.Errorf("message %d mismatch:\n got %q\nwant %q", i, got, want[i])
		}
	}
}

func TestBuildIndexLongLine(t *testing.T) {
	// Longer than bufio.MaxScanTokenSize, and longer than bufio's default
	// 4 KiB read buffer to exercise the ErrBufferFull draining path.
	long := strings.Repeat("0123456789", 15000) // 150k bytes, no newline
	mbox := "From s D\n" + long + "\n"

	assertIndexParity(t, mbox)

	idx, err := BuildIndex(&oneByteReader{b: []byte(mbox)})
	if err != nil {
		t.Fatalf("BuildIndex(): %v", err)
	}
	if idx.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", idx.Len())
	}
	if want := int64(len(mbox)); idx.Entries[0].End != want {
		t.Errorf("End = %d, want %d", idx.Entries[0].End, want)
	}

	got, err := ioutil.ReadAll(idx.Entries[0].Reader(bytes.NewReader([]byte(mbox))))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != long+"\r\n" {
		t.Errorf("long line message = %d bytes, want %d", len(got), len(long)+2)
	}
}

func TestBuildIndexEmptyMessages(t *testing.T) {
	mbox := "From a D\n\nFrom b D2\nmiddle\nFrom c D3\n\n"
	idx := assertIndexParity(t, mbox)

	if idx.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", idx.Len())
	}

	// Empty messages have no body bytes before the separating blank line.
	if got := idx.Entries[0].End - idx.Entries[0].Body; got != 1 {
		t.Errorf("first empty message: raw range = %d bytes, want 1 (blank line)", got)
	}
	if got := idx.Entries[2].End - idx.Entries[2].Body; got != 1 {
		t.Errorf("last empty message: raw range = %d bytes, want 1 (blank line)", got)
	}
}

func TestIndexReaderPartialReadThenNext(t *testing.T) {
	mbox := mboxWithThreeMessages
	idx, err := BuildIndex(strings.NewReader(mbox))
	if err != nil {
		t.Fatalf("BuildIndex(): %v", err)
	}
	want := readSequential(t, mbox)
	raw := bytes.NewReader([]byte(mbox))

	// Partially read the first message, then jump straight to the second one.
	prefix := make([]byte, 16)
	if n, err := io.ReadFull(idx.Entries[0].Reader(raw), prefix); err != nil || n != len(prefix) {
		t.Fatalf("partial read: n=%d err=%v", n, err)
	}
	if !bytes.HasPrefix(want[0], prefix) {
		t.Errorf("partial prefix %q does not start message %q", prefix, want[0])
	}

	// The skipped message does not have to be drained.
	got, err := ioutil.ReadAll(idx.Entries[1].Reader(raw))
	if err != nil {
		t.Fatalf("second message read: %v", err)
	}
	if !bytes.Equal(got, want[1]) {
		t.Errorf("second message = %q, want %q", got, want[1])
	}

	// Read a single byte of the last message, then re-read it fully through a
	// fresh, range-limited reader.
	one := make([]byte, 1)
	if n, err := idx.Entries[2].Reader(raw).Read(one); err != nil || n != 1 {
		t.Fatalf("single byte read: n=%d err=%v", n, err)
	}
	if one[0] != want[2][0] {
		t.Errorf("first byte = %q, want %q", one[0], want[2][0])
	}

	got, err = ioutil.ReadAll(idx.Entries[2].Reader(raw))
	if err != nil {
		t.Fatalf("last message re-read: %v", err)
	}
	if !bytes.Equal(got, want[2]) {
		t.Errorf("last message = %q, want %q", got, want[2])
	}

	// Readers created from the same archive independently must not interfere
	// with each other.
	r1 := idx.Entries[0].Reader(raw)
	r2 := idx.Entries[2].Reader(raw)
	b2, err := ioutil.ReadAll(r2)
	if err != nil {
		t.Fatalf("interleaved read r2: %v", err)
	}
	b1, err := ioutil.ReadAll(r1)
	if err != nil {
		t.Fatalf("interleaved read r1: %v", err)
	}
	if !bytes.Equal(b1, want[0]) || !bytes.Equal(b2, want[2]) {
		t.Errorf("interleaved readers diverged:\nr1 = %q\nr2 = %q", b1, b2)
	}
}

func TestIndexReaderTinyBuffers(t *testing.T) {
	mbox := toCRLF(mboxWithThreeMessages)
	idx := assertIndexParity(t, mbox)
	want := readSequential(t, mbox)
	raw := bytes.NewReader([]byte(mbox))

	for i, e := range idx.Entries {
		buf := make([]byte, 1)
		var got []byte
		mr := e.Reader(raw)
		for {
			n, err := mr.Read(buf)
			got = append(got, buf[:n]...)
			if err == io.EOF {
				break
			} else if err != nil {
				t.Fatalf("message %d: %v", i, err)
			}
		}
		if !bytes.Equal(got, want[i]) {
			t.Errorf("message %d: byte-at-a-time read = %q, want %q", i, got, want[i])
		}
	}
}

func TestBuildIndexInvalid(t *testing.T) {
	tests := []string{
		mboxWithOneMessageMissingSeparator,
		"junk before the first separator\nFrom s D\n",
		"From s D", // unterminated separator at end of file
		"\n\njunk\nFrom s D\n",
	}

	for _, mbox := range tests {
		if _, err := BuildIndex(strings.NewReader(mbox)); err == nil {
			t.Errorf("BuildIndex(%q) = nil error, want non-nil", mbox)
		}
	}
}

func ExampleBuildIndex() {
	mbox := strings.NewReader(`From a@example.com Thu Jan  1 00:00:01 2015
From: a@example.com
Subject: First

Hello.

From b@example.com Thu Jan  2 00:00:01 2015
From: b@example.com
Subject: Last

Goodbye.
`)

	idx, err := BuildIndex(mbox)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	fmt.Println("messages:", idx.Len())

	// Only read the last message, directly from its raw byte range.
	last := idx.Entries[idx.Len()-1]
	b, err := ioutil.ReadAll(last.Reader(mbox))
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	fmt.Print(string(b))
	// Output:
	// messages: 2
	// From: b@example.com
	// Subject: Last
	//
	// Goodbye.
}
