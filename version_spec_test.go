// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"strings"
	"testing"
)

// TestVersionXMLDeclRules verifies XML declaration pseudo-attribute
// ordering, encoding casing and standalone values.
func TestVersionXMLDeclRules(t *testing.T) {
	ok := []string{
		`<?xml version="1.1"?>`,
		`<?xml version='1.1'?>`,
		`<?xml version="1.1" encoding="utf-8"?>`,
		`<?xml version="1.1" encoding="UTF-8" standalone="yes"?>`,
		`<?xml version="1.1" standalone="no"?>`,
		`<?xml version = "1.1" ?>`,
	}
	bad := []string{
		`<?xml encoding="UTF-8"?>`,
		`<?xml version="1.1" standalone="yes" encoding="UTF-8"?>`,
		`<?xml version="1.1" bogus="x"?>`,
		`<?xml version="1.1" standalone="maybe"?>`,
		`<?XML version="1.1"?>`,
		`<?xml version='1.1"?>`,
	}
	for _, in := range ok {
		doc := NewDocument()
		doc.ReadSettings.Version = Version11
		if err := doc.ReadFromString(in + "<a/>"); err != nil {
			t.Errorf("decl %q rejected: %v", in, err)
		}
	}
	for _, in := range bad {
		doc := NewDocument()
		doc.ReadSettings.Version = Version11
		if err := doc.ReadFromString(in + "<a/>"); err == nil {
			t.Errorf("bad decl %q accepted", in)
		}
	}
}

// TestVersionAttrTabPreserved ensures literal tabs in attribute values are
// preserved (they are not line endings), then round-tripped as &#x9;.
func TestVersionAttrTabPreserved(t *testing.T) {
	for _, v := range []XMLVersion{Version10, Version11} {
		doc := parseVersioned(t, "<?xml version=\""+v.String()+"\"?><a x='a\tb'/>", v)
		if got := doc.Root().SelectAttr("x").Value; got != "a\tb" {
			t.Fatalf("v%s tab attr=%q", v, got)
		}
		out, err := doc.WriteToString()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "&#x9;") {
			t.Fatalf("v%s tab not referenced: %s", v, out)
		}
	}
}

// TestVersionNamespaces verifies namespace prefixes survive both read and
// write in both versions.
func TestVersionNamespaces(t *testing.T) {
	input := `<?xml version="%s"?><root xmlns:p="urn:x"><p:child p:attr="v"/></root>`
	for _, v := range []XMLVersion{Version10, Version11} {
		doc := parseVersioned(t, replaceVer(input, v), v)
		child := doc.FindElement("//p:child")
		if child == nil {
			t.Fatalf("v%s: child not found", v)
		}
		if child.Space != "p" || child.Tag != "child" {
			t.Fatalf("v%s: decomposed name = %q:%q", v, child.Space, child.Tag)
		}
		if uri := child.NamespaceURI(); uri != "urn:x" {
			t.Fatalf("v%s: uri=%q", v, uri)
		}
		out, err := doc.WriteToString()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "p:child") || !strings.Contains(out, "p:attr") {
			t.Fatalf("v%s: prefixes lost: %s", v, out)
		}
	}
}

// TestVersionPIQuestionGreater checks PI content and target handling.
func TestVersionPIQuestionGreater(t *testing.T) {
	doc := parseVersioned(t, `<?pi data with chars?><a/>`, Version11)
	var pi *ProcInst
	for _, c := range doc.Child {
		if p, ok := c.(*ProcInst); ok {
			pi = p
		}
	}
	if pi == nil || pi.Target != "pi" {
		t.Fatal("PI missing")
	}
}

// TestVersionWriteEscapesTextCR ensures CR in tree text becomes &#xD; on
// write for explicit versions.
func TestVersionWriteEscapesTextCR(t *testing.T) {
	for _, v := range []XMLVersion{Version10, Version11} {
		doc := NewDocument()
		doc.WriteSettings.Version = v
		root := doc.CreateElement("root")
		root.SetText("a\rb")
		out, err := doc.WriteToString()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "&#xD;") {
			t.Fatalf("v%s: CR not escaped: %q", v, out)
		}
	}
}

// TestVersionWriteCanonical checks canonical escaping combined with
// explicit versions.
func TestVersionWriteCanonical(t *testing.T) {
	doc := NewDocument()
	doc.WriteSettings.Version = Version11
	doc.WriteSettings.CanonicalText = true
	doc.WriteSettings.CanonicalAttrVal = true
	root := doc.CreateElement("root")
	root.CreateAttr("a", `x"y'z&<>`)
	root.SetText("a&b<c>d\"e'f\r")
	out, err := doc.WriteToString()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"&quot;", "&amp;", "&lt;", "&gt;", "&#xD;"} {
		if !strings.Contains(out, want) {
			t.Fatalf("canonical output missing %s: %s", want, out)
		}
	}
	if strings.Contains(out, "&apos;") {
		t.Fatalf("canonical attr should not escape apostrophe: %s", out)
	}
}

// TestVersionReadFromCountsBytes verifies byte counting works with the
// versioned reader.
func TestVersionReadFromCountsBytes(t *testing.T) {
	doc := NewDocument()
	doc.ReadSettings.Version = Version11
	input := "<?xml version=\"1.1\"?><a>\u0085</a>"
	n, err := doc.ReadFrom(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if int(n) != len(input) {
		t.Fatalf("read %d bytes, want %d", n, len(input))
	}
}

func replaceVer(s string, v XMLVersion) string {
	return strings.Replace(s, "%s", v.String(), 1)
}
