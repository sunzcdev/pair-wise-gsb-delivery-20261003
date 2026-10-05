package mail_test

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
)

func TestAttachment_longUnicodeFilename(t *testing.T) {
	filename := strings.Repeat("第四季度销售报表🎉报价单", 4) + ` "100%".xlsx`
	const budget = 72

	var buf bytes.Buffer
	var top mail.Header
	w, err := mail.CreateWriter(&buf, top, message.WithParameterLineLen(budget))
	if err != nil {
		t.Fatalf("mail.CreateWriter: %v", err)
	}

	var att mail.AttachmentHeader
	att.SetFilename(filename, message.WithParameterLineLen(budget))
	pw, err := w.CreateAttachment(att, message.WithParameterLineLen(budget))
	if err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}
	if _, err := io.WriteString(pw, "payload"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("part Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("writer Close: %v", err)
	}

	// Every physical header line respects the configured budget.
	headerBlock := strings.SplitN(buf.String(), "\r\n\r\n", 2)[0]
	for _, line := range strings.Split(headerBlock, "\r\n") {
		if len(line) > budget {
			t.Errorf("header line exceeds budget %d (%d bytes): %q",
				budget, len(line), line)
		}
	}

	e, err := message.Read(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("message.Read: %v", err)
	}
	mr := e.MultipartReader()
	if mr == nil {
		t.Fatal("expected a multipart message")
	}
	part, err := mr.NextPart()
	if err != nil {
		t.Fatalf("NextPart: %v", err)
	}

	var got mail.AttachmentHeader
	got.Header = part.Header
	name, err := got.Filename()
	if err != nil {
		t.Fatalf("Filename: %v", err)
	}
	if name != filename {
		t.Errorf("filename round-trip mismatch:\n got %q\nwant %q", name, filename)
	}
}
