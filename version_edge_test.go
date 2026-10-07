// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"errors"
	"strings"
	"testing"
)

// TestVersionBOM checks that only a single leading BOM is accepted and
// stripped; BOMs elsewhere are ordinary data in 1.1 only where legal.
func TestVersionBOM(t *testing.T) {
	for _, v := range []XMLVersion{Version10, Version11} {
		doc := parseVersioned(t, "\uFEFF<a/>", v)
		if doc.Root() == nil {
			t.Fatalf("v%s: BOM document missing root", v)
		}
		// The BOM must not be retained as character data.
		if len(doc.Child) != 1 {
			t.Fatalf("v%s: BOM retained as a token (%d children)", v, len(doc.Child))
		}
	}
}

// TestVersionNameChars validates the NameStartChar/NameChar productions
// shared by XML 1.0 (5th ed.) and 1.1 (2nd ed.).
func TestVersionNameChars(t *testing.T) {
	valid := []string{
		"a", "_", ":", "\u00F6", "\u4E2D", "\u2070", "\u3001", "\U00010000",
		"a0.b-\u00B7\u0300\u203F",
	}
	invalid := []string{
		"0abc", "-abc", ".abc", "a b", "a<", "\u0001",
	}
	invalid = append(invalid, "a"+string([]byte{0xED, 0xA0, 0x80})) // lone surrogate
	for _, v := range []XMLVersion{Version10, Version11} {
		for _, name := range valid {
			doc := NewDocument()
			doc.ReadSettings.Version = v
			input := "<" + name + "/>"
			if err := doc.ReadFromString(input); err != nil {
				t.Errorf("v%s: name %q rejected: %v", v, name, err)
			}
		}
		for _, name := range invalid {
			doc := NewDocument()
			doc.ReadSettings.Version = v
			input := "<" + name + "/>"
			if err := doc.ReadFromString(input); err == nil {
				t.Errorf("v%s: invalid name %q accepted", v, name)
			}
		}
	}
}

// TestVersionCDataPreserved verifies CDATA section tokens and whitespace
// flags under PreserveCData-equivalent behavior (always preserved by the
// versioned parser).
func TestVersionCDataPreserved(t *testing.T) {
	input := "<root>  <a><![CDATA[x<y&z]]></a> </root>"
	for _, v := range []XMLVersion{Version10, Version11} {
		doc := parseVersioned(t, input, v)
		a := doc.FindElement("//a")
		cd := a.Child[0].(*CharData)
		if !cd.IsCData() || cd.Data != "x<y&z" {
			t.Fatalf("v%s: cdata=%q cdataflag=%v", v, cd.Data, cd.IsCData())
		}

		out, err := doc.WriteToString()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "<![CDATA[x<y&z]]>") {
			t.Fatalf("v%s: CDATA not preserved on write: %s", v, out)
		}
	}
}

// TestVersionWhitespaceFlags confirms document and element indentation
// whitespace tokens carry the whitespace flag.
func TestVersionWhitespaceFlags(t *testing.T) {
	doc := parseVersioned(t, "<root>\n  <a/>\n</root>\n", Version10)
	for _, c := range doc.Child {
		if cd, ok := c.(*CharData); ok && !cd.IsWhitespace() {
			t.Fatalf("document whitespace token not flagged: %q", cd.Data)
		}
	}
	root := doc.Root()
	for _, c := range root.Child {
		if cd, ok := c.(*CharData); ok && !cd.IsWhitespace() {
			t.Fatalf("element whitespace token not flagged: %q", cd.Data)
		}
	}
}

// TestVersionSyntaxErrorsAreErrXML verifies errors.Is compatibility with
// the package's existing ErrXML sentinel.
func TestVersionSyntaxErrorsAreErrXML(t *testing.T) {
	doc := NewDocument()
	doc.ReadSettings.Version = Version11
	err := doc.ReadFromString("<a>")
	if !errors.Is(err, ErrXML) {
		t.Fatalf("err=%v is not ErrXML", err)
	}
}

// TestVersionReadFromFileAndBytes exercises both entry points.
func TestVersionReadFromBytes(t *testing.T) {
	for _, v := range []XMLVersion{Version10, Version11} {
		doc := NewDocument()
		doc.ReadSettings.Version = v
		input := []byte("<?xml version=\"" + v.String() + "\"?><a/>")
		if err := doc.ReadFromBytes(input); err != nil {
			t.Fatalf("v%s: %v", v, err)
		}
		if doc.Root() == nil {
			t.Fatalf("v%s: no root", v)
		}
	}
}

// TestVersionProcInstTarget checks that the reserved "xml" target outside
// a leading declaration is rejected in both positions.
func TestVersionProcInstTarget(t *testing.T) {
	doc := NewDocument()
	doc.ReadSettings.Version = Version11
	err := doc.ReadFromString(`<a><?xml version="1.1"?></a>`)
	if err == nil {
		t.Fatal("xml PI inside element accepted")
	}

	doc2 := NewDocument()
	doc2.ReadSettings.Version = Version11
	err = doc2.ReadFromString(`<a/><?xml version="1.1"?>`)
	if err == nil {
		t.Fatal("xml PI after root accepted")
	}
}

// TestVersionCommentsAndPIsAroundRoot checks document-level Misc content in
// both versions.
func TestVersionCommentsAndPIsAroundRoot(t *testing.T) {
	input := `<?xml version="%s"?><?pi before?>
<!-- pre --><a/>
<!-- post --><?pi after?>`
	for _, v := range []XMLVersion{Version10, Version11} {
		doc := parseVersioned(t, strings.Replace(input, "%s", v.String(), 1), v)
		var comments, pis int
		for _, c := range doc.Child {
			switch c.(type) {
			case *Comment:
				comments++
			case *ProcInst:
				pis++
			}
		}
		if comments != 2 || pis != 3 {
			t.Fatalf("v%s: comments=%d pis=%d", v, comments, pis)
		}
	}
}

// TestVersionMalformedMarkup covers common well-formedness failures.
func TestVersionMalformedMarkup(t *testing.T) {
	bad := []string{
		"<a><b></a></b>",
		"<a>",
		"<a><",
		"<a>&</a>",
		"<a x='v>",
		"<a x=v>",
		"<a x='v' y='w'/> trailing",
		"",
		"<0a/>",
		"<a><!-- -- --></a>",
		"<a>x]]>y</a>",
		"<a><![CDATA[x]]>",
	}
	for _, v := range []XMLVersion{Version10, Version11} {
		for _, in := range bad {
			doc := NewDocument()
			doc.ReadSettings.Version = v
			if err := doc.ReadFromString(in); err == nil {
				t.Errorf("v%s accepted malformed input %q", v, in)
			}
		}
	}
}

// TestVersionCommentSingleHyphen verifies a single hyphen (not "--") is
// legal comment text, while "--" not followed by ">" is rejected.
func TestVersionCommentSingleHyphen(t *testing.T) {
	good := []string{"<a><!---x--></a>", "<a><!-- a - b --></a>", "<a><!--!--></a>", "<a><!----></a>"}
	bad := []string{"<a><!--a--b--></a>", "<a><!-- -- --></a>", "<a><!---></a>"}
	for _, v := range []XMLVersion{Version10, Version11} {
		for _, in := range good {
			doc := NewDocument()
			doc.ReadSettings.Version = v
			if err := doc.ReadFromString(in); err != nil {
				t.Errorf("v%s rejected %q: %v", v, in, err)
			}
		}
		for _, in := range bad {
			doc := NewDocument()
			doc.ReadSettings.Version = v
			if err := doc.ReadFromString(in); err == nil {
				t.Errorf("v%s accepted %q", v, in)
			}
		}
	}
}

// TestVersionCDataBrackets verifies handling of literal "]]" sequences that
// are not the closing "]]>".
func TestVersionCDataBrackets(t *testing.T) {
	good := map[string]string{
		"<a><![CDATA[]]x]]></a>":  "]]x",
		"<a><![CDATA[abc]]></a>":  "abc",
		"<a><![CDATA[a]]b]]></a>": "a]]b",
	}
	bad := []string{
		"<a><![CDATA[]]]]></a>",
		"<a><![CDATA[]]>x]]></a>",
	}
	for _, v := range []XMLVersion{Version10, Version11} {
		for in, want := range good {
			doc := parseVersioned(t, in, v)
			if got := doc.Root().Text(); got != want {
				t.Errorf("v%s %q text=%q want %q", v, in, got, want)
			}
		}
		for _, in := range bad {
			doc := NewDocument()
			doc.ReadSettings.Version = v
			if err := doc.ReadFromString(in); err == nil {
				t.Errorf("v%s accepted %q", v, in)
			}
		}
	}
}
