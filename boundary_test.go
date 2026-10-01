package message

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"strings"
	"testing"
)

// boundarySeq is a deterministic boundary generator for tests.
type boundarySeq struct {
	seq []string
	i   int
}

func (g *boundarySeq) next() string {
	if g.i < len(g.seq) {
		s := g.seq[g.i]
		g.i++
		return s
	}
	g.i++
	return fmt.Sprintf("extra-boundary-%d", g.i)
}

// stdlibLeafBodies parses a raw message with the standard library only (a
// parser independent from this package) and returns the bodies of all leaf
// parts, in order. Quoted-printable parts are transparently decoded by
// mime/multipart.
func stdlibLeafBodies(t *testing.T, raw []byte) []string {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("mail.ReadMessage: %v", err)
	}
	data, err := io.ReadAll(msg.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return stdlibWalk(t, msg.Header.Get("Content-Type"), data)
}

func stdlibWalk(t *testing.T, contentType string, body []byte) []string {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("mime.ParseMediaType(%q): %v", contentType, err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return []string{string(body)}
	}
	var out []string
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		out = append(out, stdlibWalk(t, part.Header.Get("Content-Type"), data)...)
	}
	return out
}

func stdlibBoundary(t *testing.T, raw []byte) string {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("mail.ReadMessage: %v", err)
	}
	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("mime.ParseMediaType: %v", err)
	}
	return params["boundary"]
}

func makeTextHeader(cte string) Header {
	var h Header
	h.Set("Content-Type", "text/plain")
	if cte != "" {
		h.Set("Content-Transfer-Encoding", cte)
	}
	return h
}

func TestCollisionFreeWriter_boundaryCollision(t *testing.T) {
	gen := &boundarySeq{seq: []string{"collision-boundary", "safe-boundary"}}

	var h Header
	h.Set("Content-Type", "multipart/mixed")

	var buf bytes.Buffer
	w, err := CreateCollisionFreeWriter(&buf, h, &BoundaryOptions{NextBoundary: gen.next})
	if err != nil {
		t.Fatalf("CreateCollisionFreeWriter: %v", err)
	}

	// The part body contains a delimiter line for the first candidate
	// boundary. The quoted-printable transfer encoding leaves it as-is on
	// the wire.
	body := "first line\r\n--collision-boundary\r\nlast line"
	pw, err := w.CreatePart(makeTextHeader("quoted-printable"))
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	if _, err := io.WriteString(pw, body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("Close part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if boundary := stdlibBoundary(t, buf.Bytes()); boundary != "safe-boundary" {
		t.Errorf("Expected boundary %q, got %q", "safe-boundary", boundary)
	}
	if gen.i != 2 {
		t.Errorf("Expected 2 boundary candidates to be consumed, got %d", gen.i)
	}

	parts := stdlibLeafBodies(t, buf.Bytes())
	if len(parts) != 1 {
		t.Fatalf("Expected 1 part, got %d", len(parts))
	}
	if parts[0] != body {
		t.Errorf("Expected part body %q, got %q", body, parts[0])
	}
}

func TestCollisionFreeWriter_nested(t *testing.T) {
	// The inner multipart writer is finalized first and consumes the first
	// candidate. The inner body contains a delimiter line for the second
	// candidate, so the outer writer must skip it.
	gen := &boundarySeq{seq: []string{"inner-boundary", "outer-collision", "outer-safe"}}

	var h Header
	h.Set("Content-Type", "multipart/mixed")

	var buf bytes.Buffer
	w, err := CreateCollisionFreeWriter(&buf, h, &BoundaryOptions{NextBoundary: gen.next})
	if err != nil {
		t.Fatalf("CreateCollisionFreeWriter: %v", err)
	}

	leafBody := "outer part"
	pw, err := w.CreatePart(makeTextHeader("8bit"))
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	if _, err := io.WriteString(pw, leafBody); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("Close part: %v", err)
	}

	var nestedHeader Header
	nestedHeader.Set("Content-Type", "multipart/alternative")
	nw, err := w.CreatePart(nestedHeader)
	if err != nil {
		t.Fatalf("CreatePart nested: %v", err)
	}
	nestedBody := "nested body\r\n--outer-collision\r\nend"
	npw, err := nw.CreatePart(makeTextHeader("8bit"))
	if err != nil {
		t.Fatalf("CreatePart inner: %v", err)
	}
	if _, err := io.WriteString(npw, nestedBody); err != nil {
		t.Fatalf("Write inner: %v", err)
	}
	if err := npw.Close(); err != nil {
		t.Fatalf("Close inner part: %v", err)
	}
	if err := nw.Close(); err != nil {
		t.Fatalf("Close nested: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if boundary := stdlibBoundary(t, buf.Bytes()); boundary != "outer-safe" {
		t.Errorf("Expected outer boundary %q, got %q", "outer-safe", boundary)
	}

	parts := stdlibLeafBodies(t, buf.Bytes())
	expected := []string{leafBody, nestedBody}
	if len(parts) != len(expected) {
		t.Fatalf("Expected %d parts, got %d: %q", len(expected), len(parts), parts)
	}
	for i := range expected {
		if parts[i] != expected[i] {
			t.Errorf("Part %d: expected body %q, got %q", i, expected[i], parts[i])
		}
	}
}

func TestCollisionFreeWriter_scansTransferBytes(t *testing.T) {
	// The decoded content contains a delimiter line for the first
	// candidate, but the base64 transfer encoding never emits "--" on the
	// wire. Detection happens on the transfer bytes, so no collision must
	// be reported and the first candidate is kept.
	gen := &boundarySeq{seq: []string{"candidate-a", "candidate-b"}}

	var h Header
	h.Set("Content-Type", "multipart/mixed")

	var buf bytes.Buffer
	w, err := CreateCollisionFreeWriter(&buf, h, &BoundaryOptions{NextBoundary: gen.next})
	if err != nil {
		t.Fatalf("CreateCollisionFreeWriter: %v", err)
	}

	body := "line one\r\n--candidate-a\r\nline two"
	pw, err := w.CreatePart(makeTextHeader("base64"))
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	if _, err := io.WriteString(pw, body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("Close part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if boundary := stdlibBoundary(t, buf.Bytes()); boundary != "candidate-a" {
		t.Errorf("Expected boundary %q, got %q", "candidate-a", boundary)
	}
	if gen.i != 1 {
		t.Errorf("Expected 1 boundary candidate to be consumed, got %d", gen.i)
	}

	parts := stdlibLeafBodies(t, buf.Bytes())
	if len(parts) != 1 {
		t.Fatalf("Expected 1 part, got %d", len(parts))
	}
	decoded, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if string(decoded) != body {
		t.Errorf("Expected decoded part body %q, got %q", body, decoded)
	}
}

func TestCollisionFreeWriter_bufferBudget(t *testing.T) {
	body := strings.Repeat("a", 1024)

	newWriter := func(buf *bytes.Buffer, opts *BoundaryOptions) *Writer {
		var h Header
		h.Set("Content-Type", "multipart/mixed")
		w, err := CreateCollisionFreeWriter(buf, h, opts)
		if err != nil {
			t.Fatalf("CreateCollisionFreeWriter: %v", err)
		}
		return w
	}

	t.Run("exceeded", func(t *testing.T) {
		var buf bytes.Buffer
		w := newWriter(&buf, &BoundaryOptions{MaxMemoryBytes: 16})
		pw, err := w.CreatePart(makeTextHeader("binary"))
		if err != nil {
			t.Fatalf("CreatePart: %v", err)
		}
		_, writeErr := io.WriteString(pw, body)
		if writeErr == nil {
			writeErr = pw.Close()
		}
		if writeErr == nil {
			writeErr = w.Close()
		}
		if !errors.Is(writeErr, ErrBoundaryBufferExceeded) {
			t.Fatalf("Expected ErrBoundaryBufferExceeded, got %v", writeErr)
		}
	})

	t.Run("spillToTempFile", func(t *testing.T) {
		dir := t.TempDir()
		var buf bytes.Buffer
		w := newWriter(&buf, &BoundaryOptions{
			MaxMemoryBytes:  16,
			TempDir:         dir,
			TempFilePattern: "go-message-test-*",
		})
		pw, err := w.CreatePart(makeTextHeader("binary"))
		if err != nil {
			t.Fatalf("CreatePart: %v", err)
		}
		if _, err := io.WriteString(pw, body); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := pw.Close(); err != nil {
			t.Fatalf("Close part: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		parts := stdlibLeafBodies(t, buf.Bytes())
		if len(parts) != 1 {
			t.Fatalf("Expected 1 part, got %d", len(parts))
		}
		if parts[0] != body {
			t.Errorf("Expected part body of %d bytes to round-trip", len(body))
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("Expected temporary file to be removed, %d entries left", len(entries))
		}
	})
}

func TestCollisionFreeWriter_notMultipart(t *testing.T) {
	var h Header
	h.Set("Content-Type", "text/plain")

	var buf bytes.Buffer
	if _, err := CreateCollisionFreeWriter(&buf, h, nil); err == nil {
		t.Fatal("Expected an error for a non-multipart Content-Type")
	}
}

func TestCollisionFreeWriter_rawWriteRejected(t *testing.T) {
	var h Header
	h.Set("Content-Type", "multipart/mixed")

	var buf bytes.Buffer
	w, err := CreateCollisionFreeWriter(&buf, h, nil)
	if err != nil {
		t.Fatalf("CreateCollisionFreeWriter: %v", err)
	}
	if _, err := w.Write([]byte("test")); err == nil {
		t.Fatal("Expected an error when writing a raw multipart body")
	}
}
