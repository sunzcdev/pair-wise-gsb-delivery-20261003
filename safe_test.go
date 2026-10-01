package message

import (
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"strconv"
	"strings"
	"testing"
)

// seqBoundaryGenerator returns boundary-1, boundary-2, ... in order.
func seqBoundaryGenerator() func() string {
	i := 0
	return func() string {
		i++
		return "boundary-" + strconv.Itoa(i)
	}
}

func readAllString(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := ioutil.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSafeWriter_boundaryCollision(t *testing.T) {
	gen := seqBoundaryGenerator()

	var buf bytes.Buffer
	var h Header
	h.Set("Content-Type", "multipart/mixed")
	w, err := CreateSafeWriter(&buf, h, &SafeWriterOptions{GenerateBoundary: gen})
	if err != nil {
		t.Fatal(err)
	}

	// Part 1: a nested multipart. Its first leaf contains a line that
	// collides with candidate "boundary-1". Its second leaf is base64: its
	// decoded content contains "--boundary-2", but the transport bytes don't,
	// so "boundary-2" is a valid boundary for the nested entity.
	var nh Header
	nh.Set("Content-Type", "multipart/alternative")
	nested, err := w.CreatePart(nh)
	if err != nil {
		t.Fatal(err)
	}

	leafText := "first line\r\n--boundary-1\r\nrest"
	var lh Header
	lh.Set("Content-Type", "text/plain")
	lw, err := nested.CreatePart(lh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(lw, leafText); err != nil {
		t.Fatal(err)
	}
	if err := lw.Close(); err != nil {
		t.Fatal(err)
	}

	leafBin := "line\r\n--boundary-2\r\n"
	var bh Header
	bh.Set("Content-Type", "application/octet-stream")
	bh.Set("Content-Transfer-Encoding", "base64")
	bw, err := nested.CreatePart(bh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(bw, leafBin); err != nil {
		t.Fatal(err)
	}
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := nested.Close(); err != nil {
		t.Fatal(err)
	}

	// Part 2: plain text containing lines colliding with "boundary-2" (now
	// used by the nested entity) and "boundary-3".
	partText := "hello\r\n--boundary-2\r\nand\r\n--boundary-3\r\nworld"
	var th Header
	th.Set("Content-Type", "text/plain")
	tw, err := w.CreatePart(th)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, partText); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	// The header (and thus anything at all) must not be written before Close.
	if buf.Len() != 0 {
		t.Fatalf("expected nothing to be written before Close, got %d bytes", buf.Len())
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Parse the output back with an independent parser and check that part
	// counts and contents match the input.
	e, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := e.Header.ContentType()
	if err != nil {
		t.Fatal(err)
	}
	if want := "boundary-4"; params["boundary"] != want {
		t.Errorf("outer boundary = %q, want %q", params["boundary"], want)
	}

	mr := e.MultipartReader()
	if mr == nil {
		t.Fatal("expected a multipart reader")
	}

	p1, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	_, nestedParams, err := p1.Header.ContentType()
	if err != nil {
		t.Fatal(err)
	}
	if want := "boundary-2"; nestedParams["boundary"] != want {
		t.Errorf("nested boundary = %q, want %q", nestedParams["boundary"], want)
	}

	nmr := p1.MultipartReader()
	if nmr == nil {
		t.Fatal("expected a nested multipart reader")
	}
	np1, err := nmr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if got := readAllString(t, np1.Body); got != leafText {
		t.Errorf("nested leaf 1 body = %q, want %q", got, leafText)
	}
	np2, err := nmr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if got := readAllString(t, np2.Body); got != leafBin {
		t.Errorf("nested leaf 2 body = %q, want %q", got, leafBin)
	}
	if _, err := nmr.NextPart(); err != io.EOF {
		t.Errorf("expected exactly 2 nested parts, got err = %v", err)
	}

	p2, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if got := readAllString(t, p2.Body); got != partText {
		t.Errorf("part 2 body = %q, want %q", got, partText)
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Errorf("expected exactly 2 parts, got err = %v", err)
	}
}

func TestSafeWriter_bufferExceeded(t *testing.T) {
	var buf bytes.Buffer
	var h Header
	h.Set("Content-Type", "multipart/mixed")
	w, err := CreateSafeWriter(&buf, h, &SafeWriterOptions{MaxBufferBytes: 64})
	if err != nil {
		t.Fatal(err)
	}

	var th Header
	th.Set("Content-Type", "text/plain")
	p, err := w.CreatePart(th)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(p, strings.Repeat("a", 256)); !errors.Is(err, ErrBufferExceeded) {
		t.Fatalf("expected ErrBufferExceeded from Write, got %v", err)
	}
	if err := w.Close(); !errors.Is(err, ErrBufferExceeded) {
		t.Fatalf("expected ErrBufferExceeded from Close, got %v", err)
	}

	// All-or-nothing: nothing may be written to the underlying writer.
	if buf.Len() != 0 {
		t.Errorf("expected nothing to be written, got %d bytes", buf.Len())
	}
}

func TestSafeWriter_spill(t *testing.T) {
	dir := t.TempDir()

	var buf bytes.Buffer
	var h Header
	h.Set("Content-Type", "multipart/mixed")
	w, err := CreateSafeWriter(&buf, h, &SafeWriterOptions{
		MaxBufferBytes: 64,
		TempDir:        dir,
	})
	if err != nil {
		t.Fatal(err)
	}

	bodies := []string{
		strings.Repeat("a", 1000),
		strings.Repeat("b", 1000),
	}
	for _, body := range bodies {
		var th Header
		th.Set("Content-Type", "text/plain")
		p, err := w.CreatePart(th)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(p, body); err != nil {
			t.Fatal(err)
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// The spilled output must still parse back to the exact same parts.
	e, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	mr := e.MultipartReader()
	if mr == nil {
		t.Fatal("expected a multipart reader")
	}
	for i, body := range bodies {
		p, err := mr.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		if got := readAllString(t, p.Body); got != body {
			t.Errorf("part %d body mismatch (len %d, want %d)", i, len(got), len(body))
		}
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Errorf("expected exactly %d parts, got err = %v", len(bodies), err)
	}

	// Temporary files must be cleaned up.
	entries, err := ioutil.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected temp dir to be cleaned up, found %d entries", len(entries))
	}
}

func TestSafeWriter_singlePart(t *testing.T) {
	var buf bytes.Buffer
	var h Header
	h.Set("Content-Type", "text/plain")
	w, err := CreateSafeWriter(&buf, h, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Non-multipart messages need no boundary: the header is written
	// immediately and the body is streamed.
	if buf.Len() == 0 {
		t.Fatal("expected header to be written immediately for non-multipart message")
	}
	if _, err := io.WriteString(w, "Who are you?"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	e, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAllString(t, e.Body); got != "Who are you?" {
		t.Errorf("body = %q, want %q", got, "Who are you?")
	}
}
