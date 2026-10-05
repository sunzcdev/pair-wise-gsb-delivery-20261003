package mail_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/emersion/go-message/mail"
)

func TestAttachmentHeader_Filename(t *testing.T) {
	var h mail.AttachmentHeader
	h.Set("Content-Disposition", "attachment; filename=note.txt")

	if filename, err := h.Filename(); err != nil {
		t.Error("Expected no error while parsing filename, got:", err)
	} else if filename != "note.txt" {
		t.Errorf("Expected filename to be %q but got %q", "note.txt", filename)
	}
}

func TestAttachmentHeader_Filename_inContentType(t *testing.T) {
	// Note: putting the attachment's filename in Content-Type is discouraged.

	var h mail.AttachmentHeader
	h.Set("Content-Type", "text/plain; name=note.txt")

	if filename, err := h.Filename(); err != nil {
		t.Error("Expected no error while parsing filename, got:", err)
	} else if filename != "note.txt" {
		t.Errorf("Expected filename to be %q but got %q", "note.txt", filename)
	}
}

func TestAttachmentHeader_Filename_none(t *testing.T) {
	var h mail.AttachmentHeader
	if filename, err := h.Filename(); err != nil {
		t.Error("Expected no error while parsing filename, got:", err)
	} else if filename != "" {
		t.Errorf("Expected filename to be %q but got %q", "", filename)
	}
}

func TestAttachmentHeader_Filename_encoded(t *testing.T) {
	var h mail.AttachmentHeader
	h.Set("Content-Disposition", "attachment; filename=\"=?UTF-8?Q?Opis_przedmiotu_zam=c3=b3wienia_-_za=c5=82=c4=85cznik_nr_1?= =?UTF-8?Q?=2epdf?=\"")

	if filename, err := h.Filename(); err != nil {
		t.Error("Expected no error while parsing filename, got:", err)
	} else if filename != "Opis przedmiotu zamówienia - załącznik nr 1.pdf" {
		t.Errorf("Expected filename to be %q but got %q", "", filename)
	}
}

func TestAttachmentHeader_LongFilename(t *testing.T) {
	const budget = 64
	// A long filename mixing Chinese characters, emoji, quotes and
	// percent signs
	name := "2026年度报告 \"最终版\" 100%完成 🎉 中文长文件名混排😄 引号\"测试\" 百分号%符号 加长部分 v2.0 修正版.xlsx"

	var b bytes.Buffer
	var h mail.Header
	h.SetSubject("Long filename test")
	mw, err := mail.CreateWriter(&b, h)
	if err != nil {
		t.Fatal("Expected no error while creating mail writer, got:", err)
	}

	var ah mail.AttachmentHeader
	ah.ParamLineBudget = budget
	ah.Set("Content-Type", "application/octet-stream")
	ah.SetFilename(name)
	w, err := mw.CreateAttachment(ah)
	if err != nil {
		t.Fatal("Expected no error while creating attachment, got:", err)
	}
	io.WriteString(w, "attachment data")
	w.Close()
	mw.Close()

	// The Content-Disposition field must use RFC 2231 continuation
	// segments, with every line within the budget
	raw := b.String()
	if !strings.Contains(raw, "filename*0*=utf-8''") {
		t.Errorf("Expected RFC 2231 continuation segments in:\n%s", raw)
	}
	if !strings.Contains(raw, `filename="=?UTF-8?Q?`) {
		t.Errorf("Expected a plain filename fallback in:\n%s", raw)
	}
	inBlock := false
	for _, line := range strings.Split(raw, "\r\n") {
		if strings.HasPrefix(line, "Content-Disposition:") {
			inBlock = true
		} else if inBlock && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inBlock = false
		}
		if inBlock && len(line) > budget {
			t.Errorf("Content-Disposition line exceeds budget (%d > %d): %q", len(line), budget, line)
		}
	}

	// Round-trip: the receiver must get the complete filename
	mr, err := mail.CreateReader(&b)
	if err != nil {
		t.Fatal("Expected no error while creating mail reader, got:", err)
	}
	p, err := mr.NextPart()
	if err != nil {
		t.Fatal("Expected no error while reading attachment, got:", err)
	}
	rah, ok := p.Header.(*mail.AttachmentHeader)
	if !ok {
		t.Fatalf("Expected an attachment header, got %T", p.Header)
	}
	if filename, err := rah.Filename(); err != nil {
		t.Error("Expected no error while parsing filename, got:", err)
	} else if filename != name {
		t.Errorf("Expected filename to be %q but got %q", name, filename)
	}
}
