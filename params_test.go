package message

import (
	"bufio"
	"bytes"
	"io"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/emersion/go-message/textproto"
)

// A long filename mixing Chinese characters, emoji, quotes and percent
// signs.
const longMixedFilename = "2026年度报告 \"最终版\" 100%完成 🎉 中文长文件名混排😄 引号\"测试\" 百分号%符号 加长部分 v2.0 修正版.xlsx"

func isHexChar(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'F' || c >= 'a' && c <= 'f'
}

// checkFoldedLines verifies that every line is within budget and that no
// line ends in the middle of a %XX escape.
func checkFoldedLines(t *testing.T, raw string, budget int) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(raw, "\r\n"), "\r\n") {
		if len(line) > budget {
			t.Errorf("line exceeds budget (%d > %d): %q", len(line), budget, line)
		}
		end := strings.TrimRight(line, " ")
		end = strings.TrimSuffix(end, ";")
		if strings.HasSuffix(end, "%") {
			t.Errorf("line ends with a lone %%: %q", line)
		}
		if n := len(end); n >= 2 && end[n-2] == '%' && isHexChar(end[n-1]) {
			t.Errorf("line ends with a truncated %%XX escape: %q", line)
		}
	}
}

// contentDispositionLines extracts the (possibly folded) Content-Disposition
// header field lines from a raw message or header block.
func contentDispositionLines(raw string) []string {
	var block []string
	inBlock := false
	for _, line := range strings.Split(raw, "\r\n") {
		if strings.HasPrefix(line, "Content-Disposition:") {
			inBlock = true
		} else if inBlock && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inBlock = false
		}
		if inBlock {
			block = append(block, line)
		}
	}
	return block
}

func TestSetContentDisposition_RFC2231Continuation(t *testing.T) {
	const budget = 60

	var h Header
	h.ParamLineBudget = budget
	h.SetContentDisposition("attachment", map[string]string{"filename": longMixedFilename})

	var buf bytes.Buffer
	if err := textproto.WriteHeader(&buf, h.Header); err != nil {
		t.Fatal("Expected no error while writing header, got:", err)
	}
	raw := buf.String()

	// Every line must fit in the budget and escapes must not be split
	checkFoldedLines(t, raw, budget)

	// A plain filename fallback must be provided for receivers without
	// RFC 2231 support
	if !strings.Contains(raw, `filename="=?UTF-8?Q?`) {
		t.Errorf("Expected a plain filename fallback in:\n%s", raw)
	}

	// RFC 2231 continuation segments: the first one carries the charset
	// declaration, the following ones don't
	if !strings.Contains(raw, "filename*0*=utf-8''") {
		t.Errorf("Expected first continuation segment with charset declaration in:\n%s", raw)
	}
	if !strings.Contains(raw, "filename*1*=") {
		t.Errorf("Expected continuation segments in:\n%s", raw)
	}
	for _, line := range contentDispositionLines(raw) {
		if strings.Contains(line, "filename*1*=") && strings.Contains(line, "utf-8''") {
			t.Errorf("Charset declaration must only appear on the first segment: %q", line)
		}
	}

	// Round-trip: the receiver must get the complete filename
	rh, err := textproto.ReadHeader(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatal("Expected no error while reading header back, got:", err)
	}
	h2 := Header{Header: rh}
	disp, params, err := h2.ContentDisposition()
	if err != nil {
		t.Fatal("Expected no error while parsing Content-Disposition, got:", err)
	}
	if disp != "attachment" {
		t.Errorf("Expected disposition %q but got %q", "attachment", disp)
	}
	if params["filename"] != longMixedFilename {
		t.Errorf("Expected filename %q but got %q", longMixedFilename, params["filename"])
	}
}

func TestSetContentDisposition_defaultBudget(t *testing.T) {
	var h Header
	h.SetContentDisposition("attachment", map[string]string{"filename": longMixedFilename})

	var buf bytes.Buffer
	if err := textproto.WriteHeader(&buf, h.Header); err != nil {
		t.Fatal("Expected no error while writing header, got:", err)
	}
	checkFoldedLines(t, buf.String(), DefaultParamLineBudget)

	_, params, err := h.ContentDisposition()
	if err != nil {
		t.Fatal("Expected no error while parsing Content-Disposition, got:", err)
	}
	if params["filename"] != longMixedFilename {
		t.Errorf("Expected filename %q but got %q", longMixedFilename, params["filename"])
	}
}

func TestFormat2231Pieces(t *testing.T) {
	const pieceBudget = 58

	pieces := format2231Pieces("filename", longMixedFilename, pieceBudget)
	if len(pieces) < 3 {
		t.Fatalf("Expected at least 3 continuation segments, got %d: %q", len(pieces), pieces)
	}

	var joined string
	for i, piece := range pieces {
		if len(piece) > pieceBudget {
			t.Errorf("Segment #%v exceeds budget (%d > %d): %q", i, len(piece), pieceBudget, piece)
		}

		prefix := "filename*" + strconv.Itoa(i) + "*="
		if !strings.HasPrefix(piece, prefix) {
			t.Errorf("Segment #%v doesn't start with %q: %q", i, prefix, piece)
		}
		seg := strings.TrimPrefix(piece, prefix)

		if i == 0 {
			if !strings.HasPrefix(seg, "utf-8''") {
				t.Errorf("First segment doesn't carry the charset declaration: %q", piece)
			}
			seg = strings.TrimPrefix(seg, "utf-8''")
		} else if strings.Contains(seg, "utf-8''") {
			t.Errorf("Segment #%v unexpectedly carries a charset declaration: %q", i, piece)
		}

		// Escape boundaries: no segment may end with a truncated %XX
		if strings.HasSuffix(seg, "%") || (len(seg) >= 2 && seg[len(seg)-2] == '%' && isHexChar(seg[len(seg)-1])) {
			t.Errorf("Segment #%v ends with a truncated %%XX escape: %q", i, piece)
		}

		joined += seg
	}

	decoded, err := url.PathUnescape(joined)
	if err != nil {
		t.Fatal("Expected no error while percent-decoding reassembled value, got:", err)
	}
	if decoded != longMixedFilename {
		t.Errorf("Expected reassembled value %q but got %q", longMixedFilename, decoded)
	}
}

func TestFormat2231Pieces_single(t *testing.T) {
	pieces := format2231Pieces("filename", "complémentarité.txt", DefaultParamLineBudget-2)
	if len(pieces) != 1 {
		t.Fatalf("Expected a single extended parameter piece, got %d: %q", len(pieces), pieces)
	}
	if !strings.HasPrefix(pieces[0], "filename*=utf-8''") {
		t.Errorf("Expected extended parameter with charset declaration, got %q", pieces[0])
	}
}

func TestContentDisposition_fallbackPreference(t *testing.T) {
	// When a plain filename fallback is provided alongside RFC 2231
	// continuation segments, the receiver must prefer the complete name.
	raw := "Content-Disposition: attachment; filename=\"short.txt\";\r\n" +
		" filename*0*=utf-8''%E4%B8%AD%E6%96%87;\r\n" +
		" filename*1*=%E6%96%87%E4%BB%B6.txt\r\n" +
		"\r\n"

	rh, err := textproto.ReadHeader(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatal("Expected no error while reading header, got:", err)
	}
	h := Header{Header: rh}
	_, params, err := h.ContentDisposition()
	if err != nil {
		t.Fatal("Expected no error while parsing Content-Disposition, got:", err)
	}
	if want := "中文文件.txt"; params["filename"] != want {
		t.Errorf("Expected filename %q but got %q", want, params["filename"])
	}
}

func TestWriter_ParamLineBudget(t *testing.T) {
	const budget = 50

	var h Header
	h.SetContentType("multipart/mixed", nil)

	var buf bytes.Buffer
	w, err := CreateWriter(&buf, h)
	if err != nil {
		t.Fatal("Expected no error while creating writer, got:", err)
	}
	w.ParamLineBudget = budget

	// The part header is formatted with the default budget, the writer
	// reformats it with its own budget
	var ph Header
	ph.SetContentDisposition("attachment", map[string]string{"filename": longMixedFilename})
	pw, err := w.CreatePart(ph)
	if err != nil {
		t.Fatal("Expected no error while creating part, got:", err)
	}
	if _, err := io.WriteString(pw, "attachment data"); err != nil {
		t.Fatal("Expected no error while writing part, got:", err)
	}
	pw.Close()
	w.Close()

	block := contentDispositionLines(buf.String())
	if len(block) < 2 {
		t.Fatalf("Expected a folded Content-Disposition field in:\n%s", buf.String())
	}
	for _, line := range block {
		if len(line) > budget {
			t.Errorf("Content-Disposition line exceeds budget (%d > %d): %q", len(line), budget, line)
		}
	}

	// Round-trip: the complete filename must survive
	e, err := Read(&buf)
	if err != nil {
		t.Fatal("Expected no error while reading message, got:", err)
	}
	mr := e.MultipartReader()
	if mr == nil {
		t.Fatal("Expected a multipart reader")
	}
	p, err := mr.NextPart()
	if err != nil {
		t.Fatal("Expected no error while reading part, got:", err)
	}
	_, params, err := p.Header.ContentDisposition()
	if err != nil {
		t.Fatal("Expected no error while parsing part Content-Disposition, got:", err)
	}
	if params["filename"] != longMixedFilename {
		t.Errorf("Expected filename %q but got %q", longMixedFilename, params["filename"])
	}
}
