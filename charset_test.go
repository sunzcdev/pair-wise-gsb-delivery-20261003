package message

import (
	"strings"
	"testing"
)

func TestDecodeHeaderRecover_unknownCharset(t *testing.T) {
	s := "=?x-unknown-charset?q?abc?="
	dec, diags := DecodeHeaderRecover(s)
	if dec != s {
		t.Errorf("DecodeHeaderRecover() = %q, want raw input %q", dec, s)
	}
	if len(diags) != 1 {
		t.Fatalf("DecodeHeaderRecover() returned %d diagnostics, want 1", len(diags))
	}
	d := diags[0]
	if d.Start != 0 || d.End != len(s) {
		t.Errorf("diagnostic position = %d-%d, want 0-%d", d.Start, d.End, len(s))
	}
	if !IsUnknownCharset(d.Err) {
		t.Errorf("diagnostic error = %v, want an unknown charset error", d.Err)
	}
}

func TestDecodeHeaderRecover_truncatedBase64(t *testing.T) {
	s := "=?utf-8?b?SGVsbG8gV29yb?="
	dec, diags := DecodeHeaderRecover(s)
	if dec != s {
		t.Errorf("DecodeHeaderRecover() = %q, want raw input %q", dec, s)
	}
	if len(diags) != 1 {
		t.Fatalf("DecodeHeaderRecover() returned %d diagnostics, want 1", len(diags))
	}
	if IsUnknownCharset(diags[0].Err) {
		t.Errorf("diagnostic error = %v, didn't want an unknown charset error", diags[0].Err)
	}
}

func TestDecodeHeaderRecover_adjacentWords(t *testing.T) {
	// Whitespace between adjacent encoded-words must be dropped.
	s := "=?utf-8?q?hello?= \t  =?utf-8?q?_world?="
	dec, diags := DecodeHeaderRecover(s)
	if dec != "hello world" {
		t.Errorf("DecodeHeaderRecover() = %q, want %q", dec, "hello world")
	}
	if len(diags) != 0 {
		t.Errorf("DecodeHeaderRecover() returned %d diagnostics, want 0", len(diags))
	}
}

func TestDecodeHeaderRecover_mixed(t *testing.T) {
	// Plain text, valid encoded-words and a broken encoded-word in the same
	// header field: valid fragments must be decoded, the broken one must be
	// kept verbatim.
	broken := "=?utf-8?b?broken?="
	s := "Re: =?utf-8?q?caf=C3=A9?= meeting " + broken + " notes"
	dec, diags := DecodeHeaderRecover(s)
	want := "Re: café meeting " + broken + " notes"
	if dec != want {
		t.Errorf("DecodeHeaderRecover() = %q, want %q", dec, want)
	}
	if len(diags) != 1 {
		t.Fatalf("DecodeHeaderRecover() returned %d diagnostics, want 1", len(diags))
	}
	d := diags[0]
	if got := s[d.Start:d.End]; got != broken {
		t.Errorf("s[%d:%d] = %q, want %q", d.Start, d.End, got, broken)
	}
}

func TestDecodeHeaderRecover_positions(t *testing.T) {
	// Diagnostic positions must be byte offsets in the original header field
	// value, not in the decoded string.
	s := "=?utf-8?q?=C3=A9?= =?utf-8?b?bad?="
	dec, diags := DecodeHeaderRecover(s)
	if want := "é=?utf-8?b?bad?="; dec != want {
		t.Errorf("DecodeHeaderRecover() = %q, want %q", dec, want)
	}
	if len(diags) != 1 {
		t.Fatalf("DecodeHeaderRecover() returned %d diagnostics, want 1", len(diags))
	}
	d := diags[0]
	if want := strings.Index(s, "=?utf-8?b?"); d.Start != want {
		t.Errorf("diagnostic start = %d, want %d", d.Start, want)
	}
	if d.End != len(s) {
		t.Errorf("diagnostic end = %d, want %d", d.End, len(s))
	}
}

func TestDecodeHeaderRecover_brokenBetweenWords(t *testing.T) {
	// A broken encoded-word kept verbatim is still surrounded by valid
	// fragments; whitespace between encoded-words is dropped.
	s := "=?utf-8?q?ok?= =?x-bad?q?broken?= =?utf-8?q?fine?="
	dec, diags := DecodeHeaderRecover(s)
	want := "ok=?x-bad?q?broken?=fine"
	if dec != want {
		t.Errorf("DecodeHeaderRecover() = %q, want %q", dec, want)
	}
	if len(diags) != 1 {
		t.Fatalf("DecodeHeaderRecover() returned %d diagnostics, want 1", len(diags))
	}
}

func TestDecodeHeaderRecover_notEncodedWord(t *testing.T) {
	// "=?" without a valid encoded-word structure is plain text.
	s := "a =? b =?x? c"
	dec, diags := DecodeHeaderRecover(s)
	if dec != s {
		t.Errorf("DecodeHeaderRecover() = %q, want raw input %q", dec, s)
	}
	if len(diags) != 0 {
		t.Errorf("DecodeHeaderRecover() returned %d diagnostics, want 0", len(diags))
	}
}

func TestDecodeHeader_strict(t *testing.T) {
	// Strict mode keeps reporting errors and doesn't drop the raw value.
	s := "Re: =?utf-8?q?caf=C3=A9?= =?x-unknown-charset?q?broken?="
	dec, err := decodeHeader(s)
	if err == nil {
		t.Fatalf("decodeHeader() returned no error for a broken encoded-word")
	}
	if !IsUnknownCharset(err) {
		t.Errorf("decodeHeader() error = %v, want an unknown charset error", err)
	}
	if dec != s {
		t.Errorf("decodeHeader() = %q, want raw input %q", dec, s)
	}
}

func TestHeader_TextRecover(t *testing.T) {
	var h Header
	h.Set("Subject", "=?utf-8?q?caf=C3=A9?= =?x-bad?q?broken?=")
	dec, diags := h.TextRecover("Subject")
	if want := "café=?x-bad?q?broken?="; dec != want {
		t.Errorf("TextRecover() = %q, want %q", dec, want)
	}
	if len(diags) != 1 {
		t.Fatalf("TextRecover() returned %d diagnostics, want 1", len(diags))
	}
}
