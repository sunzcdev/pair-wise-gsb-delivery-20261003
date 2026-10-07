// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"strings"
	"testing"
)

// TestStrictEdgeCases exercises parser boundary conditions.
func TestStrictEdgeCases(t *testing.T) {
	cases := []struct {
		version XMLVersion
		input   string
		valid   bool
	}{
		// Comments.
		{Version10, `<!--x--><r/>`, true},
		{Version10, `<!----><r/>`, true},
		{Version10, `<!--- --><r/>`, true},
		{Version10, `<!-- ---><r/>`, false},
		{Version10, `<!-- a -- b --><r/>`, false},
		// CDATA.
		{Version10, `<r><![CDATA[]]></r>`, true},
		{Version10, `<r><![CDATA[ ]]></r>`, true},
		{Version10, `<r><![CDATA[a]]x]]></r>`, true},
		{Version10, `<r><![CDATA[a]]b]]></r>`, true},
		{Version10, `<r><![CDATA[a]]>b]]></r>`, false},
		// PI.
		{Version10, `<?pi?><r/>`, true},
		{Version10, `<?pi data?>`, false},
		{Version10, `<r><?xml version="1.0"?></r>`, false},
		{Version10, `<?piwithcolon:x?><r/>`, true},
		{Version10, `<?Xml?><r/>`, false},
		{Version10, `<?XML?><r/>`, false},
		{Version11, `<?XML version="1.1"?>` + "<r/>", false},
		{Version11, `<?xml version="1.1"?>` + "<r/>", true},
		// Names.
		{Version10, `<a:b:c/>`, false},
		{Version10, `<:a/>`, false},
		{Version10, `<a:/>`, false},
		{Version10, `<_.-a.0/>`, true},
		// Quoting.
		{Version10, `<r a="x'y"/>`, true},
		{Version10, `<r a='x"y'/>`, true},
		{Version10, `<r a="unterminated/>`, false},
		{Version10, `<r a="x<y"/>`, false},
		// CharRefs.
		{Version10, `<r>&#9;&#10;&#13;&#32;</r>`, true},
		{Version10, `<r>&#x;</r>`, false},
		{Version10, `<r>&#;</r>`, false},
		{Version10, "<r>\u00A0\uFFFF</r>", false},
		{Version10, "<r>&#x110000;</r>", false},
		{Version10, "<r>&#99999999999;</r>", false},
		{Version10, `<r a="1"b="2"/>`, true},
		{Version10, `<r a=1/>`, false},
		{Version10, `<r a = "1"/>`, true},
		{Version10, "<r>\u007F</r>", false},
		{Version11, "<r>\u007F</r>", false}, // 0x7F restricted as raw
		// Whitespace structure.
		{Version10, "   \n <r/>  \n ", true},
		{Version10, "", false},
		{Version10, "   ", false},
		{Version10, "<r> <s/> </r>", true},
	}
	for i, c := range cases {
		d := newVersionedDoc(c.version)
		err := d.ReadFromString(c.input)
		if c.valid != (err == nil) {
			t.Fatalf("case %d input=%q: valid=%v err=%v", i, c.input, c.valid, err)
		}
		if err == nil {
			d.WriteSettings.Version = c.version
			out, werr := d.WriteToBytes()
			if werr != nil {
				t.Fatalf("case %d write: %v", i, werr)
			}
			d2 := newVersionedDoc(c.version)
			if rerr := d2.ReadFromBytes(out); rerr != nil {
				t.Fatalf("case %d reparse: %v\n%s", i, rerr, out)
			}
		}
	}
}

// TestStrictFuzzRandom feeds random splits of valid multi-byte documents to
// the reader to catch rune-boundary handling bugs.
func TestStrictFuzzRandom(t *testing.T) {
	doc := "<?xml version=\"1.1\"?>\r\n<root>" +
		"世界😀&#x1;&#x2028;<c a=\"日本語\"/>tail\r\n" +
		"</root>"
	data := []byte(doc)

	for seed := 0; seed < 200; seed++ {
		n := 1 + (seed*7)%13 // chunk sizes 1..13
		d := newVersionedDoc(Version11)
		r := &chunkReader{data: append([]byte(nil), data...), n: n}
		if _, err := d.ReadFrom(r); err != nil {
			t.Fatalf("seed=%d chunk=%d: %v", seed, n, err)
		}
	}
}

// TestStrictBytesRead verifies the returned byte count covers all input.
func TestStrictBytesRead(t *testing.T) {
	cases := []string{
		"<r/>",
		"\uFEFF<?xml version=\"1.1\"?><r/>",
		"<r>\r\n</r>",
		"<r>世界</r>",
	}
	for i, in := range cases {
		d := newVersionedDoc(Version11)
		n, err := d.ReadFrom(strings.NewReader(in))
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if n != int64(len(in)) {
			t.Fatalf("case %d: read %d bytes, want %d", i, n, len(in))
		}
	}
}
