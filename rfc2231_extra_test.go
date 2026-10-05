package message

import (
	"strings"
	"testing"
)

func TestRFC2231_segmentedAfterPlainParams(t *testing.T) {
	// Plain parameters precede a parameter that requires segmentation; the
	// separators between them must remain valid and the charset parameter must
	// survive untouched.
	var h Header
	h.SetContentType("text/plain", map[string]string{
		"charset": "utf-8",
		"name":    "很长很长的中文名称_2026_Q4_final_report_with_emoji🎉🎉🎉.txt",
	}, WithParameterLineLen(44))

	raw, err := h.Raw("Content-Type")
	if err != nil {
		t.Fatalf("Raw: %v", err)
	}
	checkLineLengths(t, string(raw), 44)

	if containsDoubleSemicolon(string(raw)) {
		t.Errorf("output contains a duplicated separator:\n%s", string(raw))
	}

	_, params, err := h.ContentType()
	if err != nil {
		t.Fatalf("ContentType: %v", err)
	}
	if params["charset"] != "utf-8" {
		t.Errorf("charset lost: %q", params["charset"])
	}
	if !isASCII(params["charset"]) {
		t.Errorf("charset should be ASCII: %q", params["charset"])
	}
}

func TestRFC2231_asciiSpecialChars(t *testing.T) {
	// Pure-ASCII values containing tspecials use quoted strings, not extended
	// encoding, and must round-trip exactly.
	for _, name := range []string{`q"uote.txt`, `back\slash.txt`, `semi;colon.txt`} {
		var h Header
		h.SetContentDisposition("attachment", map[string]string{"filename": name})
		_, params, err := h.ContentDisposition()
		if err != nil {
			t.Fatalf("ContentDisposition(%q): %v", name, err)
		}
		if params["filename"] != name {
			t.Errorf("round-trip mismatch for %q: got %q", name, params["filename"])
		}
		raw, _ := h.Raw("Content-Disposition")
		if containsDoubleSemicolon(string(raw)) {
			t.Errorf("duplicated separator for %q:\n%s", name, string(raw))
		}
	}
}

func TestRFC2231_segmentedIsLastParameter(t *testing.T) {
	// The segmented parameter is the only parameter: its final segment must
	// not carry a trailing ";".
	var h Header
	h.SetContentDisposition("attachment", map[string]string{
		"filename": "超长超长超长超长超长超长超长超长超长超长超长超长超长超长超长超长超长超长超长超长.dat",
	}, WithParameterLineLen(42))

	raw, err := h.Raw("Content-Disposition")
	if err != nil {
		t.Fatalf("Raw: %v", err)
	}
	checkLineLengths(t, string(raw), 42)

	lines := physicalLines(string(raw))
	last := lines[len(lines)-1]
	if last[len(last)-1] == ';' {
		t.Errorf("final segment must not end with ';': %q", last)
	}

	_, params, err := h.ContentDisposition()
	if err != nil {
		t.Fatalf("ContentDisposition: %v", err)
	}
	if params["filename"] == "" {
		t.Error("filename was lost in round-trip")
	}
}

func containsDoubleSemicolon(raw string) bool {
	// After unfolding, two parameters are separated by exactly one ";".
	u := unfold(raw)
	return strings.Contains(u, ";;")
}
