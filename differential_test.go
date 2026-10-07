// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"encoding/xml"
	"strings"
	"testing"
)

// TestStrictDifferential generates many well-formed XML 1.0 documents and
// checks that the strict parser and encoding/xml agree on acceptance.
func TestStrictDifferential(t *testing.T) {
	// A corpus of well-formed XML 1.0 documents covering structures and
	// legal characters that both parsers must accept.
	corpus := buildCorpus()

	for i, in := range corpus {
		// encoding/x
		var x interface{}
		xerr := xml.Unmarshal([]byte(in), &x)

		d := newVersionedDoc(Version10)
		serr := d.ReadFromString(in)

		if (xerr == nil) != (serr == nil) {
			t.Fatalf("case %d disagreement: encoding/xml=%v strict=%v\n%s",
				i, xerr, serr, in)
		}
	}
}

// TestStrictDifferentialInvalid verifies that clearly malformed documents
// are rejected by the strict parser.
func TestStrictDifferentialInvalid(t *testing.T) {
	cases := []string{
		"<r><r/>",
		"<r></s>",
		"<r a>",
		"<r a=1>",
		"<r a='x\"/>",
		"<r><!-- -- --></r>",
		"<r>&;</r>",
		"<r>&amp</r>",
		"<r>&#zz;</r>",
		"<r>&#x;</r>",
		"<r><![CDATA[",
		"<r><?pi",
		"<r><",
		"<>",
		"</r>",
		"<r a=\"1\" a=\"2\"/>",
		"<r a:b=\"1\" a:b=\"2\"/>",
	}
	for i, in := range cases {
		d := newVersionedDoc(Version10)
		err := d.ReadFromString(in)
		if err == nil {
			t.Errorf("case %d: expected rejection: %q", i, in)
		}
	}
}

func buildCorpus() []string {
	names := []string{"a", "b", "root", "ns:tag", "_x.y-z0", "å", "日"}
	texts := []string{
		"", " ", "hello", "a < b & c > d", "tab\there",
		"newline\r\nhere", "unicode 世界 😀", "brackets ] ]] ]]x",
		"&#65; &lt; &amp; &quot; &apos;", "\u00A0",
	}
	attrs := []string{
		``, ` a="1"`, ` b='x'`, ` c="a &amp; b < c"`,
		` d="line1
line2"`, ` ns:e='日本語'`, ` a1="1" a2="2"`,
	}

	var out []string
	decls := []string{
		"",
		`<?xml version="1.0"?>`,
		`<?xml version="1.0" encoding="UTF-8"?>`,
		"\uFEFF",
	}
	miscBefore := []string{"", " ", "\n  ", "<!--c--> ", "<?p data?>\n"}
	miscAfter := []string{"", "\n", "  <!--c-->"}

	for _, decl := range decls {
		for _, mb := range miscBefore {
			for _, name := range names {
				for _, attr := range attrs {
					for _, tx := range texts {
						out = append(out, decl+mb+
							"<"+name+attr+">"+tx+"</"+name+">")
					}
					// Self-closing.
					out = append(out, decl+mb+"<"+name+attr+"/>")
				}
			}
			for _, ma := range miscAfter {
				out = append(out, decl+mb+"<r/>"+ma)
			}
		}
	}

	// Nested and repeated structures.
	out = append(out,
		"<r><a><b><c/></b></a></r>",
		"<r>t1<a/>t2<a/>t3</r>",
		"<r><![CDATA[<raw>&stuff]]></r>",
		"<r><!-- a --><!-- b --></r>",
		"<r>before<a>x</a>after</r>",
		"<r></r><!---->",
		"<r a='\"' b=\"'\"/>",
	)

	// Drop empties and dedupe.
	seen := map[string]bool{}
	uniq := out[:0]
	for _, s := range out {
		if strings.TrimSpace(s) != "" && !seen[s] {
			seen[s] = true
			uniq = append(uniq, s)
		}
	}
	return uniq
}
