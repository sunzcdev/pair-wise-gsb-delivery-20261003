package message

import (
	"strings"
	"testing"
)

func TestDecodeHeaderRecovering(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		// offsets of the broken encoded-words in the input
		wantErrs []string
	}{
		{
			name: "plain",
			in:   "hello world",
			want: "hello world",
		},
		{
			name: "valid",
			in:   "=?UTF-8?Q?caf=C3=A9?=",
			want: "café",
		},
		{
			name: "adjacentWordsWhitespace",
			in:   "=?UTF-8?Q?foo?= \t =?UTF-8?Q?bar?=",
			want: "foobar",
		},
		{
			name: "mixedPlainText",
			in:   "Re: =?UTF-8?Q?caf=C3=A9?= and =?UTF-8?B?dGVzdA==?=!",
			want: "Re: café and test!",
		},
		{
			name:     "unknownCharset",
			in:       "=?x-unknown?Q?junk?=",
			want:     "=?x-unknown?Q?junk?=",
			wantErrs: []string{"=?x-unknown?Q?junk?="},
		},
		{
			name:     "unknownCharsetMixed",
			in:       "=?UTF-8?Q?Hello?= plain =?x-unknown?Q?junk?= =?UTF-8?Q?World?=",
			want:     "Hello plain =?x-unknown?Q?junk?= World",
			wantErrs: []string{"=?x-unknown?Q?junk?="},
		},
		{
			name:     "truncatedBase64",
			in:       "=?UTF-8?B?QQ=?= and =?UTF-8?B?dGVzdA==?=",
			want:     "=?UTF-8?B?QQ=?= and test",
			wantErrs: []string{"=?UTF-8?B?QQ=?="},
		},
		{
			name:     "invalidEncoding",
			in:       "=?UTF-8?X?junk?= tail",
			want:     "=?UTF-8?X?junk?= tail",
			wantErrs: []string{"=?UTF-8?X?junk?="},
		},
		{
			name:     "invalidQEncoding",
			in:       "=?UTF-8?Q?=ZZ?=",
			want:     "=?UTF-8?Q?=ZZ?=",
			wantErrs: []string{"=?UTF-8?Q?=ZZ?="},
		},
		{
			name:     "brokenWordKeepsWhitespace",
			in:       "=?x-unknown?Q?junk?= =?UTF-8?Q?ok?=",
			want:     "=?x-unknown?Q?junk?= ok",
			wantErrs: []string{"=?x-unknown?Q?junk?="},
		},
		{
			name:     "multipleBroken",
			in:       "=?bogus?Q?a?= mid =?UTF-8?B?QQ=?=",
			want:     "=?bogus?Q?a?= mid =?UTF-8?B?QQ=?=",
			wantErrs: []string{"=?bogus?Q?a?=", "=?UTF-8?B?QQ=?="},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, errs := DecodeHeaderRecovering(tc.in)
			if got != tc.want {
				t.Errorf("DecodeHeaderRecovering(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(errs) != len(tc.wantErrs) {
				t.Fatalf("DecodeHeaderRecovering(%q) returned %d errors, want %d: %v", tc.in, len(errs), len(tc.wantErrs), errs)
			}
			for i, word := range tc.wantErrs {
				e := errs[i]
				wantOffset := strings.Index(tc.in, word)
				if e.Offset != wantOffset {
					t.Errorf("error %d: Offset = %d, want %d (offset of %q in %q)", i, e.Offset, wantOffset, word, tc.in)
				}
				if e.Length != len(word) {
					t.Errorf("error %d: Length = %d, want %d", i, e.Length, len(word))
				}
				if tc.in[e.Offset:e.Offset+e.Length] != word {
					t.Errorf("error %d: input[Offset:Offset+Length] = %q, want %q", i, tc.in[e.Offset:e.Offset+e.Length], word)
				}
				if e.Err == nil {
					t.Errorf("error %d: Err is nil", i)
				}
			}
		})
	}
}

func TestDecodeHeaderRecovering_unknownCharsetError(t *testing.T) {
	_, errs := DecodeHeaderRecovering("=?x-unknown?Q?junk?=")
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}
	if !IsUnknownCharset(errs[0].Err) {
		t.Errorf("expected error to verify IsUnknownCharset, got: %v", errs[0].Err)
	}
}

func TestDecodeHeaderRecovering_noSilentDrops(t *testing.T) {
	// Every byte of the input must be accounted for: concatenating decoded
	// output with the raw broken words must cover all input fragments.
	in := "a =?UTF-8?Q?b?= =?bogus?Q?c?= d =?UTF-8?B?QQ=?= e"
	out, errs := DecodeHeaderRecovering(in)
	if len(errs) != 2 {
		t.Fatalf("expected 2 errors, got %d", len(errs))
	}
	for _, want := range []string{"a", "b", "=?bogus?Q?c?=", "d", "=?UTF-8?B?QQ=?=", "e"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q is missing fragment %q", out, want)
		}
	}
}

func TestDecodeHeader_strict(t *testing.T) {
	// Strict mode keeps erroring on unknown charsets and returns the
	// original header value.
	in := "=?x-unknown?Q?junk?= =?UTF-8?Q?ok?="
	got, err := decodeHeader(in)
	if err == nil {
		t.Fatalf("expected an error for unknown charset")
	}
	if !IsUnknownCharset(err) {
		t.Errorf("expected error to verify IsUnknownCharset, got: %v", err)
	}
	if got != in {
		t.Errorf("decodeHeader(%q) = %q on error, want the original value", in, got)
	}

	// Strict mode still decodes valid headers.
	got, err = decodeHeader("=?UTF-8?Q?caf=C3=A9?=")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "café" {
		t.Errorf("decodeHeader returned %q, want %q", got, "café")
	}
}

func TestHeaderTextRecovering(t *testing.T) {
	var h Header
	h.Set("Subject", "Re: =?bogus?Q?junk?= =?UTF-8?Q?caf=C3=A9?=")

	got, errs := h.TextRecovering("Subject")
	want := "Re: =?bogus?Q?junk?= café"
	if got != want {
		t.Errorf("TextRecovering() = %q, want %q", got, want)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}
	if errs[0].Offset != strings.Index("Re: =?bogus?Q?junk?= =?UTF-8?Q?caf=C3=A9?=", "=?bogus?") {
		t.Errorf("unexpected Offset: %d", errs[0].Offset)
	}
}
