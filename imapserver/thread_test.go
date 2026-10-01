package imapserver

import (
	"bufio"
	"bytes"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func TestWriteThreadData(t *testing.T) {
	threads := []imap.ThreadData{
		{Chain: []uint32{2}},
		{Chain: []uint32{3, 6}, SubThreads: []imap.ThreadData{
			{Chain: []uint32{4, 23}},
			{Chain: []uint32{44, 7, 96}},
		}},
	}

	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	enc := imapwire.NewEncoder(w, imapwire.ConnSideServer)
	enc.Atom("*").SP().Atom("THREAD")
	for i := range threads {
		enc.SP()
		writeThreadData(enc, &threads[i])
	}
	if err := enc.CRLF(); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	want := "* THREAD (2) (3 6 (4 23) (44 7 96))\r\n"
	if got := buf.String(); got != want {
		t.Errorf("writeThreadData() = %q, want %q", got, want)
	}
}
