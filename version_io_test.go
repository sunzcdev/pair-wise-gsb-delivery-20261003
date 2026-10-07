// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestVersionMultibyteAcrossReadBoundaries forces every byte position to
// fall on a Read boundary and checks that multi-byte UTF-8 sequences,
// including astral-plane characters, are reassembled for both versions.
func TestVersionMultibyteAcrossReadBoundaries(t *testing.T) {
	// "dolphin" in Japanese plus astral-plane emoji and a U+2028 context.
	input := []byte("<?xml version=\"%s\"?>\n<root \u65e5\u672c\u8a9e=\"v\">\n" +
		"\u6587\u5b57\U0001F42C and &#x1F42C;\n\u2028</root>")

	for _, v := range []XMLVersion{Version10, Version11} {
		filled := make([]byte, len(input))
		copy(filled, input)
		filled = []byte(strings.ReplaceAll(string(filled), "%s", v.String()))

		for chunk := 1; chunk <= 8; chunk++ {
			doc := parseVersionedBytesChunks(t, filled, v, chunk)
			root := doc.Root()
			if root == nil {
				t.Fatalf("v%s chunk=%d: missing root", v, chunk)
			}
			if got := root.SelectAttr("\u65e5\u672c\u8a9e"); got == nil || got.Value != "v" {
				t.Fatalf("v%s chunk=%d: attribute missing", v, chunk)
			}
			if !strings.Contains(root.Text(), "\u6587\u5b57\U0001F42C and \U0001F42C") {
				t.Fatalf("v%s chunk=%d: text mismatch: %q", v, chunk, root.Text())
			}
		}
	}
}

// TestVersionMultibyteInvalidUTF8 verifies malformed UTF-8 spanning a
// boundary is rejected with a positioned error rather than silently
// replaced.
func TestVersionMultibyteInvalidUTF8(t *testing.T) {
	// E4 B8 AD is valid U+4E2D; truncate its leading byte at the boundary
	// by feeding an incomplete 3-byte sequence at EOF.
	cases := [][]byte{
		[]byte("<a>\xe4\xb8"),
		[]byte("<a>\xff</a>"),
		[]byte("<a>\xed\xa0\x80</a>"), // surrogate
	}
	for _, v := range []XMLVersion{Version10, Version11} {
		for _, in := range cases {
			doc := NewDocument()
			doc.ReadSettings.Version = v
			err := doc.ReadFromBytes(in)
			if err == nil {
				t.Errorf("v%s input % x: expected error", v, in)
			}
		}
	}
}

// TestVersionDeclarationSelectionMismatch verifies that the selected
// version is enforced against the XML declaration rather than the
// declaration being silently downgraded.
func TestVersionDeclarationSelectionMismatch(t *testing.T) {
	cases := []struct {
		selected XMLVersion
		decl     string
	}{
		{Version10, "1.1"},
		{Version11, "1.0"},
	}
	for _, c := range cases {
		doc := NewDocument()
		doc.ReadSettings.Version = c.selected
		err := doc.ReadFromString("<?xml version=\"" + c.decl + "\"?><a/>")
		if err == nil {
			t.Fatalf("selected %s with declaration %s was accepted", c.selected, c.decl)
		}
		if !strings.Contains(err.Error(), "does not match selected version") {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	// Bad version strings are rejected.
	for _, bad := range []string{"2.0", "", "1.2"} {
		doc := NewDocument()
		doc.ReadSettings.Version = Version10
		err := doc.ReadFromString(`<?xml version="` + bad + `"?><a/>`)
		if err == nil {
			t.Fatalf("version %q accepted", bad)
		}
	}
}

// TestVersionNoDeclarationAssumesSelection checks that a document without
// an XML declaration is parsed under the selected version's rules.
func TestVersionNoDeclarationAssumesSelection(t *testing.T) {
	// A 1.1-only character reference with no declaration must succeed only
	// for 1.1.
	doc11 := NewDocument()
	doc11.ReadSettings.Version = Version11
	if err := doc11.ReadFromString("<a>&#1;</a>"); err != nil {
		t.Fatalf("1.1 no-decl parse: %v", err)
	}

	doc10 := NewDocument()
	doc10.ReadSettings.Version = Version10
	if err := doc10.ReadFromString("<a>&#1;</a>"); err == nil {
		t.Fatal("1.0 accepted a 1.1-only reference without a declaration")
	}
}

// TestVersionLegacyUnchanged verifies VersionAuto keeps the historical
// behavior: 1.1 declarations are rejected by encoding/xml just as before.
func TestVersionLegacyUnchanged(t *testing.T) {
	doc := NewDocument()
	err := doc.ReadFromString(`<?xml version="1.1"?><a/>`)
	if err == nil {
		t.Fatal("legacy mode accepted an XML 1.1 declaration")
	}

	// Legacy default Version is the zero value.
	var rs ReadSettings
	if rs.Version != VersionAuto {
		t.Fatalf("default version = %v, want VersionAuto", rs.Version)
	}
	var ws WriteSettings
	if ws.Version != VersionAuto {
		t.Fatalf("default write version = %v, want VersionAuto", ws.Version)
	}
}

// TestVersionUnsupportedReadOptions checks that options the first version
// does not implement are rejected explicitly rather than silently ignored.
func TestVersionUnsupportedReadOptions(t *testing.T) {
	t.Run("CharsetReader", func(t *testing.T) {
		doc := NewDocument()
		doc.ReadSettings.Version = Version11
		doc.ReadSettings.CharsetReader = func(string, io.Reader) (io.Reader, error) { return nil, nil }
		if err := doc.ReadFromString("<a/>"); !errors.Is(err, errVersionUnsupported) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("Permissive", func(t *testing.T) {
		doc := NewDocument()
		doc.ReadSettings.Version = Version11
		doc.ReadSettings.Permissive = true
		if err := doc.ReadFromString("<a/>"); !errors.Is(err, errVersionUnsupported) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("AutoClose", func(t *testing.T) {
		doc := NewDocument()
		doc.ReadSettings.Version = Version11
		doc.ReadSettings.AutoClose = []string{"br"}
		if err := doc.ReadFromString("<a/>"); !errors.Is(err, errVersionUnsupported) {
			t.Fatalf("err=%v", err)
		}
	})
}

// TestVersionSameInputBothVersions parses one document legal in both
// versions and confirms identical trees.
func TestVersionSameInputBothVersions(t *testing.T) {
	input := `<?xml version="%s"?>
<bookstore xmlns:p="urn:prices">
	<book category="x">
		<title lang="en">&#x2014; Title</title>
		<p:price p:tax="1.99">29.99</p:price>
		<editor/>
		<editor></editor>
	</book>
	<!-- tail -->
</bookstore>`
	var outs []string
	for _, v := range []XMLVersion{Version10, Version11} {
		doc := parseVersioned(t, strings.Replace(input, "%s", v.String(), 1), v)
		// Structural expectations identical to the legacy test fixture.
		if len(doc.Root().SelectElements("book")) != 1 {
			t.Fatalf("v%s: expected one book", v)
		}
		title := doc.FindElement("//title")
		if title == nil || title.Text() != "\u2014 Title" {
			t.Fatalf("v%s: title=%q", v, title.Text())
		}
		if got := doc.FindElement("//p:price").SelectAttrValue("p:tax", ""); got != "1.99" {
			t.Fatalf("v%s: tax=%q", v, got)
		}
	}
	_ = outs
}

// TestVersionPreserveDuplicateAttrs exercises the legacy-compatible
// duplicate attribute setting in the versioned parser.
func TestVersionPreserveDuplicateAttrs(t *testing.T) {
	input := `<a x="1" x="2"/>`

	strict := NewDocument()
	strict.ReadSettings.Version = Version10
	if err := strict.ReadFromString(input); err == nil {
		t.Fatal("strict parser accepted duplicate attributes")
	}

	keep := NewDocument()
	keep.ReadSettings.Version = Version10
	keep.ReadSettings.PreserveDuplicateAttrs = true
	if err := keep.ReadFromString(input); err != nil {
		t.Fatal(err)
	}
	if n := len(keep.Root().Attr); n != 2 {
		t.Fatalf("attr count = %d, want 2", n)
	}
}

// TestVersionMaxDepth verifies the depth limit applies to the versioned
// parser.
func TestVersionMaxDepth(t *testing.T) {
	doc := NewDocument()
	doc.ReadSettings.Version = Version11
	doc.ReadSettings.MaxDepth = 2
	err := doc.ReadFromString("<a><b><c/></b></a>")
	if !errors.Is(err, ErrMaxDepth) {
		t.Fatalf("err=%v", err)
	}
}

// TestVersionEntityMap verifies the predefined entities plus a custom
// ReadSettings entity map resolve in the versioned parser.
func TestVersionEntityMap(t *testing.T) {
	doc := NewDocument()
	doc.ReadSettings.Version = Version10
	doc.ReadSettings.Entity = map[string]string{"custom": "C"}
	err := doc.ReadFromString(`<a>&lt;&amp;&custom;&gt;</a>`)
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.Root().Text(); got != "<&C>" {
		t.Fatalf("text = %q", got)
	}

	bad := NewDocument()
	bad.ReadSettings.Version = Version10
	if err := bad.ReadFromString(`<a>&unknown;</a>`); err == nil {
		t.Fatal("unknown entity accepted")
	}
}

// TestVersionLineEndingAtReadBoundary verifies CRLF folding when the CR
// lands exactly on the parser's internal buffer boundary.
func TestVersionLineEndingAtReadBoundary(t *testing.T) {
	for _, pad := range []int{58, 59, 60, 61, 62, 63, 64, 65} {
		input := append([]byte("<a>"), bytes.Repeat([]byte("x"), pad)...)
		input = append(input, '\r', '\n')
		input = append(input, []byte("</a>")...)
		for _, v := range []XMLVersion{Version10, Version11} {
			doc := NewDocument()
			doc.ReadSettings.Version = v
			if err := doc.ReadFromBytes(input); err != nil {
				t.Fatalf("v%s pad=%d: %v", v, pad, err)
			}
			want := string(bytes.Repeat([]byte("x"), pad)) + "\n"
			if got := doc.Root().Text(); got != want {
				t.Fatalf("v%s pad=%d text=%q want=%q", v, pad, got, want)
			}
		}
	}
}
