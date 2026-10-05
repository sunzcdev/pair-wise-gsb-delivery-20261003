package message

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/emersion/go-message/textproto"
)

func physicalLines(raw string) []string {
	s := strings.TrimSuffix(raw, "\r\n")
	return strings.Split(s, "\r\n")
}

func checkLineLengths(t *testing.T, raw string, maxLen int) {
	t.Helper()
	for i, line := range physicalLines(raw) {
		if len(line) > maxLen {
			t.Errorf("physical line %d is %d bytes long, exceeds budget %d: %q", i, len(line), maxLen, line)
		}
	}
}

func roundTrip(t *testing.T, filename, disp string, lineLen int) {
	t.Helper()

	var h Header
	h.SetContentDisposition(disp, map[string]string{"filename": filename}, WithParameterLineLen(lineLen))

	raw, err := h.Raw("Content-Disposition")
	if err != nil {
		t.Fatalf("Raw: %v", err)
	}
	checkLineLengths(t, string(raw), lineLen)

	gotDisp, params, err := h.ContentDisposition()
	if err != nil {
		t.Fatalf("ContentDisposition: %v", err)
	}
	if gotDisp != disp {
		t.Errorf("disposition: got %q, want %q", gotDisp, disp)
	}
	if params["filename"] != filename {
		t.Errorf("filename round-trip mismatch:\n got %q\nwant %q", params["filename"], filename)
	}

	// The plain ASCII fallback must remain readable on its own.
	plain := parsePlainFilename(string(raw))
	if plain == "" {
		t.Error("no plain filename fallback emitted")
	}
	if !isASCII(plain) {
		t.Errorf("plain fallback must be plain ASCII, got: %q", plain)
	}

	// A standard library parser reading the raw, folded field after unfolding
	// must still recover the full name.
	unfolded := unfold(string(raw))
	if !strings.Contains(unfolded, "filename*0*=") && !strings.Contains(unfolded, "filename*=") {
		t.Errorf("expected an RFC 2231 extended parameter in: %q", unfolded)
	}
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func parsePlainFilename(raw string) string {
	var plain string
	for _, param := range splitParams(stripKey(unfold(raw), "Content-Disposition")) {
		if name, value, ok := parseParam(param); ok && name == "filename" {
			plain = value
		}
	}
	return plain
}

func stripKey(field, key string) string {
	field = strings.TrimPrefix(field, key+":")
	return strings.TrimLeft(field, " \t")
}

func unfold(raw string) string {
	raw = strings.TrimSuffix(raw, "\r\n")
	lines := strings.Split(raw, "\r\n")
	var b strings.Builder
	for i, line := range lines {
		if i == 0 {
			b.WriteString(line)
			continue
		}
		// Continuation lines start with a folding space. The previous line
		// may already carry the separating ";".
		line = strings.TrimLeft(line, " \t")
		if strings.HasSuffix(b.String(), ";") {
			b.WriteString(" " + line)
		} else {
			b.WriteString("; " + line)
		}
	}
	return b.String()
}

func splitParams(s string) []string {
	var params []string
	start := strings.IndexByte(s, ';')
	if start < 0 {
		return nil
	}
	s = s[start+1:]
	curStart := 0
	inQuote := false
	for i := 0; i < len(s); i++ {
		if inQuote {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				continue
			}
			if s[i] == '"' {
				inQuote = false
			}
			continue
		}
		switch s[i] {
		case '"':
			inQuote = true
		case ';':
			params = append(params, strings.TrimSpace(s[curStart:i]))
			curStart = i + 1
		}
	}
	params = append(params, strings.TrimSpace(s[curStart:]))
	return params
}

func parseParam(param string) (name, value string, ok bool) {
	eq := strings.IndexByte(param, '=')
	if eq < 0 {
		return "", "", false
	}
	name = strings.ToLower(strings.TrimSpace(param[:eq]))
	value = strings.TrimSpace(param[eq+1:])
	value = strings.Trim(value, `"`)
	return name, value, true
}

func TestRFC2231_longMixedName(t *testing.T) {
	// Chinese, emoji, double quotes and a literal percent sign, repeated to
	// force continuation even under the default line budget.
	chunk := "中文文件名🎉\"报价\"100%最终版"
	filename := strings.Repeat(chunk, 6) + ".pdf"

	roundTrip(t, filename, "attachment", 40)
}

func TestRFC2231_lineBudgets(t *testing.T) {
	filename := "销售报表_2026_Q4_非常长的附件名称🎉🎉_quote\"_%25_final_draft.xlsx"

	for _, lineLen := range []int{76, 60, 45, 35} {
		t.Run("", func(t *testing.T) {
			var h Header
			h.SetContentDisposition("attachment", map[string]string{"filename": filename},
				WithParameterLineLen(lineLen))

			raw, err := h.Raw("Content-Disposition")
			if err != nil {
				t.Fatalf("Raw: %v", err)
			}
			checkLineLengths(t, string(raw), lineLen)

			_, params, err := h.ContentDisposition()
			if err != nil {
				t.Fatalf("ContentDisposition: %v", err)
			}
			if params["filename"] != filename {
				t.Errorf("round-trip mismatch:\n got %q\nwant %q", params["filename"], filename)
			}

			// Ensure the segments are numbered 0..N contiguously and that the
			// charset declaration appears exactly once, on segment 0.
			unfolded := unfold(string(raw))
			if !strings.Contains(unfolded, "filename*0*=utf-8''") {
				t.Fatalf("missing segment 0 with charset declaration: %q", unfolded)
			}
			if strings.Contains(unfolded, "filename*1*=utf-8''") ||
				strings.Contains(unfolded, "filename*0*=utf-8''utf-8''") {
				t.Errorf("charset declaration repeated on later segment: %q", unfolded)
			}
		})
	}
}

func TestRFC2231_shortNameIsUnsegmented(t *testing.T) {
	var h Header
	h.SetContentDisposition("attachment", map[string]string{"filename": "中文.txt"})

	raw, err := h.Raw("Content-Disposition")
	if err != nil {
		t.Fatalf("Raw: %v", err)
	}
	if strings.Contains(string(raw), "*0*=") {
		t.Errorf("short name should not be segmented: %q", string(raw))
	}
	if !strings.Contains(string(raw), "filename*=utf-8''") {
		t.Errorf("expected unsegmented extended parameter: %q", string(raw))
	}
	checkLineLengths(t, string(raw), DefaultParameterLineLen)
	if strings.Contains(string(raw), "*0*=") {
		t.Errorf("expected the unsegmented form, got continuations: %q", string(raw))
	}

	_, params, err := h.ContentDisposition()
	if err != nil {
		t.Fatalf("ContentDisposition: %v", err)
	}
	if params["filename"] != "中文.txt" {
		t.Errorf("round-trip mismatch: %q", params["filename"])
	}
}

func TestRFC2231_fallbackPrecedence(t *testing.T) {
	// Even with a plain filename present, the extended parameter must win.
	filename := "rapport détaillé.pdf"
	var h Header
	h.SetContentDisposition("attachment", map[string]string{"filename": filename})

	_, params, err := h.ContentDisposition()
	if err != nil {
		t.Fatalf("ContentDisposition: %v", err)
	}
	if params["filename"] != filename {
		t.Errorf("extended filename must take precedence: got %q, want %q",
			params["filename"], filename)
	}

	raw, _ := h.Raw("Content-Disposition")
	plain := parsePlainFilename(string(raw))
	if plain != "rapport d_taill_.pdf" {
		t.Errorf("unexpected plain fallback: %q", plain)
	}
}

func TestRFC2231_writerOption(t *testing.T) {
	filename := strings.Repeat("超长邮件附件名称🎉", 8) + ".bin"

	var h Header
	h.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
	h.SetContentDisposition("attachment", map[string]string{"filename": filename},
		WithParameterLineLen(50))

	var b bytes.Buffer
	w, err := CreateWriter(&b, h)
	if err != nil {
		t.Fatalf("CreateWriter: %v", err)
	}
	w.Close()

	// Every emitted header line respects the budget.
	headerBlock := strings.SplitN(b.String(), "\r\n\r\n", 2)[0]
	for _, line := range strings.Split(headerBlock, "\r\n") {
		if len(line) > 50 {
			t.Errorf("header line exceeds budget: %q (%d bytes)", line, len(line))
		}
	}

	e, err := Read(&b)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	_, params, err := e.Header.ContentDisposition()
	if err != nil {
		t.Fatalf("ContentDisposition: %v", err)
	}
	if params["filename"] != filename {
		t.Errorf("round-trip mismatch:\n got %q\nwant %q", params["filename"], filename)
	}
}

func TestRFC2231_createPartOption(t *testing.T) {
	filename := strings.Repeat("数据导出文件📊", 5) + ".csv"

	var top Header
	top.SetContentType("multipart/mixed", nil)

	var b bytes.Buffer
	mw, err := CreateWriter(&b, top)
	if err != nil {
		t.Fatalf("CreateWriter: %v", err)
	}

	var part Header
	part.SetContentDisposition("attachment", map[string]string{"filename": filename},
		WithParameterLineLen(44))
	pw, err := mw.CreatePart(part)
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	pw.Close()
	mw.Close()

	e, err := Read(&b)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	mr := e.MultipartReader()
	if mr == nil {
		t.Fatal("MultipartReader returned nil")
	}
	p, err := mr.NextPart()
	if err != nil {
		t.Fatalf("NextPart: %v", err)
	}
	_, params, err := p.Header.ContentDisposition()
	if err != nil {
		t.Fatalf("ContentDisposition: %v", err)
	}
	if params["filename"] != filename {
		t.Errorf("round-trip mismatch:\n got %q\nwant %q", params["filename"], filename)
	}
}

func TestRFC2231_specialChars(t *testing.T) {
	// Percent sign and quotes must round-trip exactly, not be interpreted as
	// part of an escape sequence.
	filename := `100% "final" 📄 name.txt`

	var h Header
	h.SetContentDisposition("attachment", map[string]string{"filename": filename},
		WithParameterLineLen(48))

	raw, err := h.Raw("Content-Disposition")
	if err != nil {
		t.Fatalf("Raw: %v", err)
	}
	checkLineLengths(t, string(raw), 48)

	_, params, err := h.ContentDisposition()
	if err != nil {
		t.Fatalf("ContentDisposition: %v", err)
	}
	if params["filename"] != filename {
		t.Errorf("round-trip mismatch:\n got %q\nwant %q", params["filename"], filename)
	}
}

func TestRFC2231_wireRoundTrip(t *testing.T) {
	filename := strings.Repeat("中文文件名🎉\"100%最终版", 5) + ".zip"
	const budget = 42

	var h Header
	h.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
	h.SetContentDisposition("attachment", map[string]string{"filename": filename},
		WithParameterLineLen(budget))

	var b bytes.Buffer
	w, err := CreateWriter(&b, h)
	if err != nil {
		t.Fatalf("CreateWriter: %v", err)
	}
	w.Close()

	// Parse the raw wire bytes through the real header reader.
	parsed, err := textproto.ReadHeader(bufio.NewReader(&b))
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	ph := Header{parsed}
	_, params, err := ph.ContentDisposition()
	if err != nil {
		t.Fatalf("ContentDisposition: %v", err)
	}
	if params["filename"] != filename {
		t.Errorf("wire round-trip mismatch:\n got %q\nwant %q", params["filename"], filename)
	}

	// Extended name must win over the plain fallback, even though both are
	// present on the wire.
	disp := ph.Get("Content-Disposition")
	if !strings.Contains(disp, "filename=") || !strings.Contains(disp, "filename*0*=utf-8''") {
		t.Errorf("expected both a plain fallback and an extended parameter: %q", disp)
	}
}
