package mbox

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"strings"
	"testing"
)

const mboxWithEmptyMessages = `From herp.derp@example.com Thu Jan  1 00:00:01 2015
From derp.herp@example.com Thu Jan  1 00:00:01 2015

From bernd.lauert@example.com Thu Jan  3 00:00:01 2015
From: bernd.lauert@example.com (Bernd Lauert)

Body.

From last@example.com Thu Jan  4 00:00:01 2015
`

func readSequential(t *testing.T, mbox string) [][]byte {
	t.Helper()

	var msgs [][]byte
	mr := NewReader(strings.NewReader(mbox))
	for {
		r, err := mr.NextMessage()
		if err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("Unexpected error after NextMessage(): %v", err)
		}

		b, err := ioutil.ReadAll(r)
		if err != nil {
			t.Fatalf("Unexpected error reading message: %v", err)
		}
		msgs = append(msgs, b)
	}
	return msgs
}

func testIndexConsistent(t *testing.T, mbox string) {
	t.Helper()

	idx, err := BuildIndex(strings.NewReader(mbox))
	if err != nil {
		t.Fatalf("BuildIndex() = %v", err)
	}

	// All offsets must refer to raw bytes of the original data.
	for i, e := range idx {
		if !strings.HasPrefix(mbox[e.Separator:], "From ") {
			t.Errorf("Entry %d: Separator = %d does not point at a separator line", i, e.Separator)
		}
		if !(e.Separator <= e.Body && e.Body <= e.End) {
			t.Errorf("Entry %d: offsets out of order: %+v", i, e)
		}
		if i+1 < len(idx) && e.End != idx[i+1].Separator {
			t.Errorf("Entry %d: End = %d, want %d (next Separator)", i, e.End, idx[i+1].Separator)
		}
		if i+1 == len(idx) && e.End != int64(len(mbox)) {
			t.Errorf("Last entry: End = %d, want %d (end of archive)", e.End, len(mbox))
		}
	}

	// Reading each message via the index must yield the same result as
	// reading the archive sequentially.
	want := readSequential(t, mbox)
	if len(idx) != len(want) {
		t.Fatalf("BuildIndex() found %d messages, sequential Reader found %d", len(idx), len(want))
	}

	ra := strings.NewReader(mbox)
	for i, e := range idx {
		got, err := ioutil.ReadAll(e.Reader(ra))
		if err != nil {
			t.Errorf("Unexpected error reading message %d: %v", i, err)
			continue
		}
		if !bytes.Equal(got, want[i]) {
			t.Errorf("Message %d: expected:\n%q\ngot:\n%q", i, want[i], got)
		}
	}
}

func TestIndexOneMessage(t *testing.T) {
	testIndexConsistent(t, mboxWithOneMessage)
}

func TestIndexThreeMessages(t *testing.T) {
	testIndexConsistent(t, mboxWithThreeMessages)
}

func TestIndexThreeMessagesCRLF(t *testing.T) {
	testIndexConsistent(t, toCRLF(mboxWithThreeMessages))
}

func TestIndexStartingLF(t *testing.T) {
	testIndexConsistent(t, mboxWithStartingLF)
}

func TestIndexMalformedButValid(t *testing.T) {
	testIndexConsistent(t, mboxWithThreeMessagesMalformedButValid)
}

func TestIndexEmptyMessages(t *testing.T) {
	testIndexConsistent(t, mboxWithEmptyMessages)
}

func TestIndexLastMessageWithoutTrailingNewline(t *testing.T) {
	testIndexConsistent(t, strings.TrimSuffix(mboxWithThreeMessages, "\n"))
}

func TestIndexLongLines(t *testing.T) {
	// Both the separator line and a body line exceed the bufio read buffer.
	longSep := strings.Repeat("x", 10000)
	longLine := strings.Repeat("This is a very long line. ", 5000)
	mbox := fmt.Sprintf("From herp.derp@example.com %s\nSubject: Test\n\n%s\n\nFrom derp.herp@example.com Thu Jan  1 00:00:01 2015\n\nBye.\n", longSep, longLine)
	testIndexConsistent(t, mbox)
}

func TestIndexInvalidFormat(t *testing.T) {
	if _, err := BuildIndex(strings.NewReader(mboxWithOneMessageMissingSeparator)); err != ErrInvalidFormat {
		t.Fatalf("BuildIndex() = %v, want %v", err, ErrInvalidFormat)
	}
}

func TestIndexNoMessages(t *testing.T) {
	for _, mbox := range []string{"", "\n\n\n"} {
		idx, err := BuildIndex(strings.NewReader(mbox))
		if err != nil {
			t.Fatalf("BuildIndex() = %v", err)
		}
		if len(idx) != 0 {
			t.Errorf("Expected no index entries, got %d", len(idx))
		}
	}
}

func TestIndexPartialReadThenNext(t *testing.T) {
	mbox := mboxWithThreeMessages
	want := readSequential(t, mbox)

	idx, err := BuildIndex(strings.NewReader(mbox))
	if err != nil {
		t.Fatalf("BuildIndex() = %v", err)
	}
	ra := strings.NewReader(mbox)

	// Read only part of the first message.
	buf := make([]byte, 16)
	n, err := idx[0].Reader(ra).Read(buf)
	if err != nil {
		t.Fatalf("Unexpected error partially reading message 0: %v", err)
	}
	if !bytes.Equal(buf[:n], want[0][:n]) {
		t.Errorf("Partial read: expected %q, got %q", want[0][:n], buf[:n])
	}

	// The remaining messages must still be readable, in any order.
	for _, i := range []int{2, 1} {
		got, err := ioutil.ReadAll(idx[i].Reader(ra))
		if err != nil {
			t.Fatalf("Unexpected error reading message %d: %v", i, err)
		}
		if !bytes.Equal(got, want[i]) {
			t.Errorf("Message %d: expected:\n%q\ngot:\n%q", i, want[i], got)
		}
	}
}
