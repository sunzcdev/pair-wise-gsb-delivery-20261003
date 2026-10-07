// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"strings"
	"testing"
)

// TestVersionExtraConformance covers additional well-formedness rules.
func TestVersionExtraConformance(t *testing.T) {
	bad := []string{
		"<a x=\"&#0;\"/>",
		"<a x=\"&#xD800;\"/>",
		"<a>&amp</a>",
		"<a>&;</a>",
		"<a>&#;</a>",
		"<a>&#x;</a>",
		"<a>&#ZZ;</a>",
		"<a>&#xGG;</a>",
		"<a>&#99999999999999999999;</a>",
		"<a><!ELEMENT a EMPTY></a>",
		"<a></b></a>",
		"<a x=\"1\" x=\"2\"/>",
		"<?pi data?>trailing<a/>",
		"<a><!--a--b--></a>",
	}
	good := []string{
		"<a>&#x00000041;</a>",
		"<a>&#65;</a>",
		"<a x=\"a&amp;b\"/>",
		"<a>text ] more</a>",
		"<a>] ] ]></a>",
		"<a:a xmlns:a=\"u\"/>",
		"<a:b xmlns:a=\"u\"><a:c/></a:b>",
		"<?xml version=\"1.1\"?><a>&#x10FFFF;</a>",
	}

	for _, v := range []XMLVersion{Version10, Version11} {
		for _, in := range bad {
			doc := NewDocument()
			doc.ReadSettings.Version = v
			err := doc.ReadFromString(in)
			if err == nil {
				t.Errorf("v%s accepted bad input %q", v, in)
			}
		}
		for _, in := range good {
			// Version-tagged inputs are checked only under that version.
			if strings.HasPrefix(in, `<?xml version="1.1"?>`) && v != Version11 {
				continue
			}
			doc := NewDocument()
			doc.ReadSettings.Version = v
			if err := doc.ReadFromString(in); err != nil {
				t.Errorf("v%s rejected good input %q: %v", v, in, err)
			}
		}
	}
}

// TestVersionDeepNested validates deep nesting within the default limit.
func TestVersionDeepNested(t *testing.T) {
	const depth = 50
	var open, close string
	for i := 0; i < depth; i++ {
		open += "<a>"
		close = "</a>" + close
	}
	for _, v := range []XMLVersion{Version10, Version11} {
		doc := NewDocument()
		doc.ReadSettings.Version = v
		if err := doc.ReadFromString(open + close); err != nil {
			t.Fatalf("v%s: %v", v, err)
		}
	}
}

// TestVersionWriteRoundTripFromParsed parses a complex 1.1 document and
// rewrites it, then reparses the output and compares key values.
func TestVersionWriteRoundTripFromParsed(t *testing.T) {
	input := "<?xml version=\"1.1\"?>\n" +
		"<root a=\"x&#x1;y\" b=\"normal\">\n" +
		"  text with &#x84; ref and &#x2028;\n" +
		"  <child>data&#x7F;</child>\n" +
		"  <!-- comment with \u0001 raw (legal in 1.1 comments) -->\n" +
		"</root>\n"
	doc := parseVersioned(t, input, Version11)
	out, err := doc.WriteToString()
	if err != nil {
		t.Fatal(err)
	}

	reparsed := parseVersioned(t, out, Version11)
	root := reparsed.Root()
	if got := root.SelectAttr("a").Value; got != "x\u0001y" {
		t.Fatalf("attr a = %q", got)
	}
	if got := root.SelectElement("child").Text(); got != "data\u007f" {
		t.Fatalf("child text = %q", got)
	}
}
