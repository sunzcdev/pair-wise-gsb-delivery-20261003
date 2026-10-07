// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newVersionedDoc creates a document configured to parse the given strict
// XML version.
func newVersionedDoc(v XMLVersion) *Document {
	d := NewDocument()
	d.ReadSettings.Version = v
	d.WriteSettings.Version = v
	return d
}

func mustRead(t *testing.T, v XMLVersion, input string) *Document {
	t.Helper()
	d := newVersionedDoc(v)
	if err := d.ReadFromString(input); err != nil {
		t.Fatalf("ReadFromString(%q): %v", input, err)
	}
	return d
}

// TestStrictVersionedCharRefs checks XML 1.0 vs 1.1 handling of character
// references to control characters that are legal only in XML 1.1.
func TestStrictVersionedCharRefs(t *testing.T) {
	input := "<root>&#x1;&#x7F;&#x85;&#x9F;</root>"

	d10 := newVersionedDoc(Version10)
	err := d10.ReadFromString(input)
	if err == nil {
		t.Fatal("XML 1.0 must reject &#x1;")
	}
	var se *SyntaxError
	if !errors.As(err, &se) {
		t.Fatalf("expected *SyntaxError, got %T: %v", err, err)
	}

	d11 := newVersionedDoc(Version11)
	if err := d11.ReadFromString(input); err != nil {
		t.Fatalf("XML 1.1 must accept restricted character references: %v", err)
	}
	got := d11.Root().Text()
	want := "\x01\x7F\u0085\u009F"
	if got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}

	// Round-trip preserves the restricted characters as references.
	out, err := d11.WriteToString()
	if err != nil {
		t.Fatal(err)
	}
	for _, wantRef := range []string{"&#x1;", "&#x7f;", "&#x85;", "&#x9f;"} {
		if !strings.Contains(strings.ToLower(out), wantRef) {
			t.Fatalf("output %q missing %s", out, wantRef)
		}
	}

	// The same references must parse again under 1.1.
	d11b := newVersionedDoc(Version11)
	if err := d11b.ReadFromString(out); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if d11b.Root().Text() != want {
		t.Fatalf("reparsed text = %q, want %q", d11b.Root().Text(), want)
	}
}

// TestStrictRawRestrictedChars verifies that XML 1.1 restricted characters
// appearing raw (not as references) are rejected, while they are valid
// references.
func TestStrictRawRestrictedChars(t *testing.T) {
	d := newVersionedDoc(Version11)
	err := d.ReadFromString("<r>" + "\x01" + "</r>")
	if err == nil {
		t.Fatal("raw restricted char must be rejected")
	}

	err = d.ReadFromString("<r a=\"" + "\x7F" + "\"/>")
	if err == nil {
		t.Fatal("raw restricted char in attribute must be rejected")
	}

	// NEL raw in text is legal in 1.1 but normalized to LF.
	d2 := newVersionedDoc(Version11)
	if err := d2.ReadFromString("<r>a" + "\u0085" + "b</r>"); err != nil {
		t.Fatal(err)
	}
	if got := d2.Root().Text(); got != "a\nb" {
		t.Fatalf("NEL normalization: %q", got)
	}

	// NEL raw is not a legal Char in XML 1.0 at all.
	d0 := newVersionedDoc(Version10)
	if err := d0.ReadFromString("<r>a" + "\u0085" + "b</r>"); err == nil {
		t.Fatal("NEL raw must be rejected in XML 1.0")
	}
}

// TestStrictLineEndings verifies CR, CR/LF, NEL and LS normalization.
func TestStrictLineEndings(t *testing.T) {
	cases := []struct {
		version XMLVersion
		input   string
		text    string
		attr    string
	}{
		{Version10, "<r>a\rb</r>", "a\nb", ""},
		{Version10, "<r>a\r\nb</r>", "a\nb", ""},
		{Version10, "<r a=\"x\ry\"/>", "", "x y"},
		{Version11, "<r>a\rb</r>", "a\nb", ""},
		{Version11, "<r>a\r\nb</r>", "a\nb", ""},
		{Version11, "<r>a\u0085b\u2028c</r>", "a\nb\nc", ""},
		{Version11, "<r a=\"x\u0085y\u2028z\"/>", "", "x y z"},
		// Character references bypass normalization.
		{Version10, "<r>&#xD;</r>", "\r", ""},
		{Version11, "<r>&#x85;&#x2028;</r>", "\u0085\u2028", ""},
		{Version11, "<r a=\"x&#xA;y\"/>", "", "x\ny"},
	}
	for i, c := range cases {
		d := newVersionedDoc(c.version)
		if err := d.ReadFromString(c.input); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if c.text != "" && d.Root().Text() != c.text {
			t.Fatalf("case %d text = %q, want %q", i, d.Root().Text(), c.text)
		}
		if c.attr != "" {
			if got := d.Root().SelectAttr("a").Value; got != c.attr {
				t.Fatalf("case %d attr = %q, want %q", i, got, c.attr)
			}
		}
	}
}

// TestStrictNELAttributes verifies NEL handling in attribute values.
func TestStrictNELAttributes(t *testing.T) {
	d10 := newVersionedDoc(Version10)
	if err := d10.ReadFromString("<r a=\"x\u0085y\"/>"); err == nil {
		t.Fatal("raw NEL must be rejected in XML 1.0 attribute")
	}

	d11 := newVersionedDoc(Version11)
	if err := d11.ReadFromString("<r a=\"x\u0085y\"/>"); err != nil {
		t.Fatal(err)
	}
	if got := d11.Root().SelectAttr("a").Value; got != "x y" {
		t.Fatalf("raw NEL = %q, want space", got)
	}

	d11r := newVersionedDoc(Version11)
	if err := d11r.ReadFromString("<r a=\"x&#x85;y\"/>"); err != nil {
		t.Fatal(err)
	}
	if got := d11r.Root().SelectAttr("a").Value; got != "x\u0085y" {
		t.Fatalf("referenced NEL = %q", got)
	}
}

// TestStrictDTDRejected verifies that DTDs and internal subsets are
// rejected explicitly in both strict modes.
func TestStrictDTDRejected(t *testing.T) {
	inputs := []struct {
		v     XMLVersion
		input string
	}{
		{Version10, `<!DOCTYPE root><root/>`},
		{Version11, `<!DOCTYPE root><root/>`},
		{Version10, `<?xml version="1.0"?><!DOCTYPE root SYSTEM "x.dtd"><root/>`},
		{Version11, `<?xml version="1.1"?>
<!DOCTYPE root [<!ELEMENT root EMPTY>]><root/>`},
		{Version10, `<root><!ELEMENT x EMPTY></root>`},
	}
	for i, c := range inputs {
		d := newVersionedDoc(c.v)
		err := d.ReadFromString(c.input)
		if err == nil {
			t.Fatalf("case %d: DTD must be rejected: %q", i, c.input)
		}
		if !errors.Is(err, ErrUnsupportedDTD) {
			t.Fatalf("case %d: expected ErrUnsupportedDTD, got %v", i, err)
		}
	}
}

// TestStrictDeclarationVersion verifies that the XML declaration agrees with
// the selected version and that encoding is limited to UTF-8.
func TestStrictDeclarationVersion(t *testing.T) {
	good := []struct {
		v     XMLVersion
		input string
	}{
		{Version10, `<?xml version="1.0"?>` + "<r/>"},
		{Version11, `<?xml version="1.1"?>` + "<r/>"},
		{Version10, `<?xml version='1.0' encoding='UTF-8' standalone='yes'?>` + "<r/>"},
		{Version10, "<r/>"},
		{Version11, "<r/>"},
	}
	for i, c := range good {
		d := newVersionedDoc(c.v)
		if err := d.ReadFromString(c.input); err != nil {
			t.Fatalf("good[%d]: %v", i, err)
		}
		out, err := d.WriteToString()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, `version="`+c.v.String()+`"`) {
			t.Fatalf("good[%d] declaration mismatch: %q", i, out)
		}
	}

	bad := []struct {
		v     XMLVersion
		input string
	}{
		{Version10, `<?xml version="1.1"?>` + "<r/>"},
		{Version11, `<?xml version="1.0"?>` + "<r/>"},
		{Version11, `<?xml version="1.1" encoding="UTF-16"?>` + "<r/>"},
		{Version11, `<?xml version="1.1"?>` + "<r/>"},
	}
	for i, c := range bad {
		if i == 3 {
			continue
		}
		d := newVersionedDoc(c.v)
		if err := d.ReadFromString(c.input); err == nil {
			t.Fatalf("bad[%d] expected error: %q", i, c.input)
		}
	}
}

// TestStrictErrorPositions verifies that syntax errors carry usable line,
// column and byte offsets.
func TestStrictErrorPositions(t *testing.T) {
	input := "<?xml version=\"1.0\"?>\n<root>\n  <child>&bad;\n  </child>\n</root>"
	d := newVersionedDoc(Version10)
	err := d.ReadFromString(input)
	if err == nil {
		t.Fatal("expected error")
	}
	var se *SyntaxError
	if !errors.As(err, &se) {
		t.Fatalf("expected *SyntaxError, got %T", err)
	}
	if se.Line != 3 || se.Byte < 30 {
		t.Fatalf("unexpected position %+v", se)
	}

	// Invalid UTF-8 is reported with a position.
	bad := append([]byte("<r>"), 0xFF, 0xFE)
	bad = append(bad, []byte("</r>")...)
	d2 := newVersionedDoc(Version10)
	if err := d2.ReadFromBytes(bad); err == nil {
		t.Fatal("expected invalid UTF-8 error")
	} else if !errors.As(err, &se) {
		t.Fatalf("expected *SyntaxError, got %T", err)
	}
}

// TestStrictWriteTreeErrors verifies that illegal tree values produce
// TreeErrors identifying the node.
func TestStrictWriteTreeErrors(t *testing.T) {
	cases := []struct {
		version XMLVersion
		build   func(d *Document)
		path    string
	}{
		{Version10, func(d *Document) {
			d.CreateElement("root").SetText("a\x01b")
		}, "/root/text()"},
		{Version11, func(d *Document) {
			d.CreateElement("root").SetText("a\x00b")
		}, "/root/text()"},
		{Version10, func(d *Document) {
			d.CreateElement("root").CreateAttr("a", "x\x01y")
		}, "/root/@a"},
		{Version10, func(d *Document) {
			d.CreateElement("1bad")
		}, ""},
		{Version11, func(d *Document) {
			e := d.CreateElement("root")
			e.CreateCData("a\x01b")
		}, "/root/cdata()"},
		{Version11, func(d *Document) {
			d.CreateElement("root").CreateComment("bad -- comment")
		}, "/root/comment()"},
	}
	for i, c := range cases {
		d := newVersionedDoc(c.version)
		c.build(d)
		err := d.WriteSettings.Version
		_ = err
		_, werr := d.WriteToBytes()
		if werr == nil {
			t.Fatalf("case %d: expected TreeError", i)
		}
		var te *TreeError
		if !errors.As(werr, &te) {
			t.Fatalf("case %d: expected *TreeError, got %T: %v", i, werr, werr)
		}
		if c.path != "" && te.Path != c.path {
			t.Fatalf("case %d: path = %q, want %q", i, te.Path, c.path)
		}
	}

	// A document containing a DOCTYPE directive is refused on write.
	d := newVersionedDoc(Version10)
	d.CreateDirective("DOCTYPE root")
	d.CreateElement("root")
	if _, err := d.WriteToBytes(); !errors.Is(err, ErrUnsupportedDTD) {
		t.Fatalf("expected ErrUnsupportedDTD, got %v", err)
	}
}

// TestStrictWriteDeclaresVersion verifies declaration emission rules.
func TestStrictWriteDeclaresVersion(t *testing.T) {
	d := newVersionedDoc(Version11)
	d.CreateElement("root").SetText("hi")
	out, err := d.WriteToBytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, []byte(`<?xml version="1.1" encoding="UTF-8"?>`)) {
		t.Fatalf("missing 1.1 declaration: %q", out)
	}

	// Existing declaration must agree with the write version.
	d2 := newVersionedDoc(Version10)
	d2.CreateProcInst("xml", `version="1.1"`)
	d2.CreateElement("r")
	if _, err := d2.WriteToBytes(); err == nil {
		t.Fatal("expected version mismatch error")
	}

	// An existing, valid declaration is preserved (and not duplicated).
	d3 := newVersionedDoc(Version10)
	d3.CreateProcInst("xml", `version="1.0" standalone="yes"`)
	d3.CreateElement("r")
	out3, err := d3.WriteToBytes()
	if err != nil {
		t.Fatal(err)
	}
	want3 := `<?xml version="1.0" standalone="yes"?><r/>`
	if string(out3) != want3 {
		t.Fatalf("decl = %q, want %q", out3, want3)
	}

	// A declaration after the first node is invalid.
	d4 := newVersionedDoc(Version10)
	d4.CreateProcInst("app", "data")
	d4.CreateProcInst("xml", `version="1.0"`)
	d4.CreateElement("r")
	if _, err := d4.WriteToBytes(); err == nil {
		t.Fatal("expected error for misplaced XML declaration")
	}

	// A declaration after the root is also invalid.
	d5 := newVersionedDoc(Version10)
	d5.CreateElement("r")
	d5.CreateProcInst("xml", `version="1.0"`)
	if _, err := d5.WriteToBytes(); err == nil {
		t.Fatal("expected error for post-root XML declaration")
	}
}

// chunkReader returns at most n bytes per Read call, so multi-byte UTF-8
// sequences and CR/LF pairs frequently straddle Read boundaries.
type chunkReader struct {
	data []byte
	n    int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := r.n
	if n > len(p) {
		n = len(p)
	}
	if n > len(r.data) {
		n = len(r.data)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

// TestStrictMultiByteAcrossReads feeds documents through readers with
// varying chunk sizes so 3-byte and 4-byte UTF-8 sequences and CR/LF pairs
// are split across Read boundaries.
func TestStrictMultiByteAcrossReads(t *testing.T) {
	input := "<?xml version=\"1.1\"?>\r\n" +
		"<root a=\"世界\">&#x1; Hello \u4E16\u754C \U0001F600 a\u0085b\u2028c</root>"

	for _, n := range []int{1, 2, 3, 4, 5, 7, 16, 64, 4096} {
		d := newVersionedDoc(Version11)
		r := &chunkReader{data: []byte(input), n: n}
		if _, err := d.ReadFrom(r); err != nil {
			t.Fatalf("chunk=%d: %v", n, err)
		}
		root := d.Root()
		if got := root.SelectAttr("a").Value; got != "世界" {
			t.Fatalf("chunk=%d attr=%q", n, got)
		}
		txt := root.Text()
		if !strings.Contains(txt, "Hello 世界 😀 a\nb\nc") {
			t.Fatalf("chunk=%d text=%q", n, txt)
		}
		if !strings.HasPrefix(txt, "\x01 ") {
			t.Fatalf("chunk=%d restricted char lost: %q", n, txt)
		}

		// Re-parse the output using equally small reads.
		out, err := d.WriteToBytes()
		if err != nil {
			t.Fatal(err)
		}
		d2 := newVersionedDoc(Version11)
		if _, err := d2.ReadFrom(&chunkReader{data: out, n: n}); err != nil {
			t.Fatalf("chunk=%d reparse: %v\n%s", n, err, out)
		}
	}
}

// TestStrictW3CSamples runs the bundled W3C-style conformance samples.
func TestStrictW3CSamples(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "w3c"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no W3C samples found")
	}

	for _, e := range entries {
		name := e.Name()
		path := filepath.Join("testdata", "w3c", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		expectValid := strings.HasPrefix(name, "valid")
		version := Version10
		if strings.Contains(name, "1.1") {
			version = Version11
		}

		d := newVersionedDoc(version)
		err = d.ReadFromBytes(data)
		if expectValid {
			if err != nil {
				t.Errorf("%s: expected valid: %v", name, err)
				continue
			}
			out, err := d.WriteToBytes()
			if err != nil {
				t.Errorf("%s: write: %v", name, err)
				continue
			}
			// The serialized document must reparse.
			d2 := newVersionedDoc(version)
			if err := d2.ReadFromBytes(out); err != nil {
				t.Errorf("%s: reparse: %v\n%s", name, err, out)
			}
		} else {
			if err == nil {
				t.Errorf("%s: expected parse error", name)
				continue
			}
			if strings.Contains(name, "dtd") && !errors.Is(err, ErrUnsupportedDTD) {
				t.Errorf("%s: expected ErrUnsupportedDTD, got %v", name, err)
			}
		}
	}
}

// TestStrictSameInputBothVersions parses identical inputs with both versions
// and confirms the documented differences.
func TestStrictSameInputBothVersions(t *testing.T) {
	cases := []struct {
		input   string
		valid10 bool
		valid11 bool
		text11  string
		attr11  string
	}{
		{"<r/>", true, true, "", ""},
		{"<r>&#x1;</r>", false, true, "\x01", ""},
		{"<r>&#x7f;</r>", false, true, "\x7F", ""},
		{"<r>" + "\u0085" + "</r>", false, true, "\n", ""},
		{"<r a=\"x&#xd;y\"/>", true, true, "", "x\ry"},
		{"<r>&#0;</r>", false, false, "", ""},
		{"<r>&#xd800;</r>", false, false, "", ""},
		{"<r>&#x110000;</r>", false, false, "", ""},
		{"<r>&#x10ffff;</r>", true, true, "\U0010FFFF", ""},
		{"<r a='1'>text</r>", true, true, "text", "1"},
	}
	for i, c := range cases {
		check := func(v XMLVersion, valid bool) {
			d := newVersionedDoc(v)
			err := d.ReadFromString(c.input)
			if valid != (err == nil) {
				t.Fatalf("case %d v=%v: valid=%v err=%v", i, v, valid, err)
			}
			if valid && v == Version11 {
				if c.text11 != "" && d.Root().Text() != c.text11 {
					t.Fatalf("case %d text=%q want %q", i, d.Root().Text(), c.text11)
				}
				if c.attr11 != "" && d.Root().SelectAttr("a").Value != c.attr11 {
					t.Fatalf("case %d attr=%q want %q", i, d.Root().SelectAttr("a").Value, c.attr11)
				}
			}
		}
		check(Version10, c.valid10)
		check(Version11, c.valid11)
	}
}

// TestStrictDocumentTokens verifies the tree token structure: the XML
// declaration becomes an "xml" ProcInst and whitespace runs are coalesced,
// matching the legacy decoder's token layout.
func TestStrictDocumentTokens(t *testing.T) {
	d := newVersionedDoc(Version10)
	input := "<?xml version=\"1.0\"?>\n  <r/>\n"
	if err := d.ReadFromString(input); err != nil {
		t.Fatal(err)
	}
	if len(d.Child) != 4 {
		t.Fatalf("children = %d, want 4: %#v", len(d.Child), d.Child)
	}
	pi, ok := d.Child[0].(*ProcInst)
	if !ok || pi.Target != "xml" || pi.Inst != `version="1.0"` {
		t.Fatalf("child 0 = %#v", d.Child[0])
	}
	ws, ok := d.Child[1].(*CharData)
	if !ok || ws.Data != "\n  " || !ws.IsWhitespace() {
		t.Fatalf("child 1 = %#v", d.Child[1])
	}

	out, err := d.WriteToString()
	if err != nil {
		t.Fatal(err)
	}
	if out != input {
		t.Fatalf("roundtrip = %q, want %q", out, input)
	}
}

// TestStrictLegacyCompatibility verifies that leaving Version unset retains
// the legacy encoding/xml behavior, including acceptance of DTDs and
// absence of an emitted XML declaration.
func TestStrictLegacyCompatibility(t *testing.T) {
	// Legacy mode tolerates a DTD.
	d := NewDocument()
	input := `<!DOCTYPE root><root a="1">text</root>`
	if err := d.ReadFromString(input); err != nil {
		t.Fatalf("legacy read: %v", err)
	}
	out, err := d.WriteToString()
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(out, "<?xml") {
		t.Fatalf("legacy writer must not emit a declaration: %q", out)
	}
	if d.Root().SelectAttr("a").Value != "1" {
		t.Fatal("legacy tree mismatch")
	}

	// The bundled legacy test corpus continues to parse identically.
	d2 := NewDocument()
	if err := d2.ReadFromString(bookstoreXML); err != nil {
		t.Fatal(err)
	}
	if d2.Root().Tag != "bookstore" {
		t.Fatalf("legacy bookstore root=%q", d2.Root().Tag)
	}
}

// TestStrictMaxDepth verifies the depth limit in strict modes.
func TestStrictMaxDepth(t *testing.T) {
	deep := strings.Repeat("<a>", defaultMaxDepth) + strings.Repeat("</a>", defaultMaxDepth)
	d := newVersionedDoc(Version10)
	if err := d.ReadFromString(deep); err != nil {
		t.Fatalf("depth=%d should be allowed: %v", defaultMaxDepth, err)
	}

	too := strings.Repeat("<a>", defaultMaxDepth+1) + strings.Repeat("</a>", defaultMaxDepth+1)
	d2 := NewDocument()
	d2.ReadSettings.Version = Version11
	d2.ReadSettings.MaxDepth = 10
	if err := d2.ReadFromString(strings.Repeat("<a>", 11) + strings.Repeat("</a>", 11)); !errors.Is(err, ErrMaxDepth) {
		t.Fatalf("expected ErrMaxDepth, got %v", err)
	}
	d3 := newVersionedDoc(Version11)
	if err := d3.ReadFromString(too); !errors.Is(err, ErrMaxDepth) {
		t.Fatalf("expected ErrMaxDepth, got %v", err)
	}
}

// TestStrictBOM verifies that a leading UTF-8 BOM is consumed and only at
// the start.
func TestStrictBOM(t *testing.T) {
	input := "\uFEFF<?xml version=\"1.0\"?><r>\uFEFF</r>"
	d := newVersionedDoc(Version10)
	if err := d.ReadFromString(input); err != nil {
		// U+FEFF in content is a legal Char (zero-width no-break space).
		t.Fatal(err)
	}
	if got := d.Root().Text(); got != "\uFEFF" {
		t.Fatalf("content BOM = %q", got)
	}

	// Leading BOM before an otherwise undeclared document.
	d2 := newVersionedDoc(Version10)
	if err := d2.ReadFromString("\uFEFF<r/>"); err != nil {
		t.Fatal(err)
	}
}

// TestStrictEscapeRoundTrip verifies serializer escaping rules.
func TestStrictEscapeRoundTrip(t *testing.T) {
	d := newVersionedDoc(Version11)
	root := d.CreateElement("r")
	root.CreateAttr("a", "x\r\ny\tz")
	root.SetText("a<b>&c\"d'e")
	out, err := d.WriteToString()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"&lt;", "&amp;", "&quot;", "&apos;", "&#xD;", "&#xA;"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q missing %s", out, want)
		}
	}
	d2 := newVersionedDoc(Version11)
	if err := d2.ReadFromString(out); err != nil {
		t.Fatal(err)
	}
	if got := d2.Root().SelectAttr("a").Value; got != "x\r\ny\tz" {
		t.Fatalf("attr roundtrip = %q", got)
	}
	if got := d2.Root().Text(); got != "a<b>&c\"d'e" {
		t.Fatalf("text roundtrip = %q", got)
	}
}

// TestStrictSingleQuoteAttrs verifies single-quoted attribute output.
func TestStrictSingleQuoteAttrs(t *testing.T) {
	d := newVersionedDoc(Version10)
	d.WriteSettings.AttrSingleQuote = true
	d.CreateElement("r").CreateAttr("a", `it's "x"`)
	out, err := d.WriteToString()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `a='it&apos;s "x"'`) {
		t.Fatalf("output = %q", out)
	}
	d2 := newVersionedDoc(Version10)
	if err := d2.ReadFromString(out); err != nil {
		t.Fatal(err)
	}
	if got := d2.Root().SelectAttr("a").Value; got != `it's "x"` {
		t.Fatalf("roundtrip = %q", got)
	}
}
