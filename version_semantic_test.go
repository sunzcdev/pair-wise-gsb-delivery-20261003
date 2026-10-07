// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"errors"
	"strings"
	"testing"
)

// TestVersionLineEndingNormalization verifies the CR/CRLF (both versions)
// and NEL/LS (XML 1.1 only) line-ending rules, plus the requirement that
// character references are not normalized.
func TestVersionLineEndingNormalization(t *testing.T) {
	type want struct {
		text string
		attr string
	}
	cases := []struct {
		name  string
		input string
		v10   *want // nil => parse error
		v11   *want
	}{
		{
			name:  "CR and CRLF",
			input: "<a x='a\rb\r\nb'>x\ry\r\nz</a>",
			v10:   &want{"x\ny\nz", "a\nb\nb"},
			v11:   &want{"x\ny\nz", "a\nb\nb"},
		},
		{
			name:  "NEL and LS text",
			input: "<a>x\u0085y\u2028z</a>",
			v10:   &want{"x\u0085y\u2028z", ""},
			v11:   &want{"x\ny\nz", ""},
		},
		{
			name:  "NEL and LS attribute 1.1",
			input: "<a x='a\u0085b\u2028c'/>",
			v10:   &want{"", "a\u0085b\u2028c"},
			v11:   &want{"", "a\nb\nc"},
		},
		{
			name:  "char references preserved",
			input: "<a x='a&#x85;b&#x2028;c&#xD;d'>x&#x85;y&#xA;z</a>",
			v10:   &want{"x\u0085y\nz", "a\u0085b\u2028c\rd"},
			v11:   &want{"x\u0085y\nz", "a\u0085b\u2028c\rd"},
		},
		{
			name:  "CRLF in CDATA",
			input: "<a><![CDATA[a\rb\r\nc]]></a>",
			v10:   &want{"a\nb\nc", ""},
			v11:   &want{"a\nb\nc", ""},
		},
	}

	for _, c := range cases {
		for _, v := range []XMLVersion{Version10, Version11} {
			var expected *want
			if v == Version10 {
				expected = c.v10
			} else {
				expected = c.v11
			}
			t.Run(c.name+"/"+v.String(), func(t *testing.T) {
				doc := NewDocument()
				doc.ReadSettings.Version = v
				err := doc.ReadFromString(c.input)
				if expected == nil {
					if err == nil {
						t.Fatal("expected parse error, got nil")
					}
					return
				}
				if err != nil {
					t.Fatalf("parse error: %v", err)
				}
				root := doc.Root()
				if expected.text != "" && root.Text() != expected.text {
					t.Errorf("text = %q, want %q", root.Text(), expected.text)
				}
				if expected.attr != "" && root.SelectAttr("x").Value != expected.attr {
					t.Errorf("attr = %q, want %q", root.SelectAttr("x").Value, expected.attr)
				}
			})
		}
	}
}

// TestVersionRestrictedReferenceRoundTrip confirms the distinction between
// raw restricted characters and character references survives reading and
// writing in XML 1.1, and that XML 1.0 rejects the same references.
func TestVersionRestrictedReferenceRoundTrip(t *testing.T) {
	input := "<?xml version=\"1.1\"?><a x='v&#x1;w'>t&#x7F;u&#x84;z</a>"
	doc := parseVersioned(t, input, Version11)

	root := doc.Root()
	if got := root.Text(); got != "t\u007fu\u0084z" {
		t.Fatalf("text = %q", got)
	}
	if got := root.SelectAttr("x").Value; got != "v\u0001w" {
		t.Fatalf("attr = %q", got)
	}

	out, err := doc.WriteToString()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "&#x7F;") || !strings.Contains(out, "&#x84;") ||
		!strings.Contains(out, "&#x1;") {
		t.Fatalf("restricted characters were not re-referenced: %s", out)
	}

	// Parse the serialized output again; it must reproduce the same tree.
	round := parseVersioned(t, out, Version11)
	if round.Root().Text() != root.Text() {
		t.Fatalf("round trip text mismatch: %q vs %q", round.Root().Text(), root.Text())
	}

	// The same document selected as 1.0 must fail on each reference.
	bad := NewDocument()
	bad.ReadSettings.Version = Version10
	if err := bad.ReadFromString(strings.Replace(input, "1.1", "1.0", 1)); err == nil {
		t.Fatal("XML 1.0 accepted an XML 1.1-only character reference")
	}
}

// TestVersionWriteInvalidValuePosition checks that illegal tree values are
// reported with the owning element path and byte offset within the value.
func TestVersionWriteInvalidValuePosition(t *testing.T) {
	doc := NewDocument()
	doc.WriteSettings.Version = Version11
	root := doc.CreateElement("root")
	child := root.CreateElement("child")
	child.SetText("ab\x00cd")

	_, err := doc.WriteToString()
	var ive *InvalidValueError
	if !errors.As(err, &ive) {
		t.Fatalf("expected *InvalidValueError, got %T %v", err, err)
	}
	if ive.Path != "/root/child" {
		t.Errorf("path = %q, want /root/child", ive.Path)
	}
	if ive.ByteOffset != 2 {
		t.Errorf("offset = %d, want 2", ive.ByteOffset)
	}
	if ive.Version != Version11 {
		t.Errorf("version = %v, want 1.1", ive.Version)
	}
}

// TestVersionWriteInvalidAttrValue checks attribute value positions.
func TestVersionWriteInvalidAttrValue(t *testing.T) {
	doc := NewDocument()
	doc.WriteSettings.Version = Version10
	root := doc.CreateElement("root")
	root.CreateAttr("k", "x\u0001y")

	_, err := doc.WriteToString()
	var ive *InvalidValueError
	if !errors.As(err, &ive) {
		t.Fatalf("expected *InvalidValueError, got %v", err)
	}
	if ive.Kind != "attribute value" || ive.Attr != "k" {
		t.Errorf("kind=%q attr=%q", ive.Kind, ive.Attr)
	}
	if ive.Path != "/root" || ive.ByteOffset != 1 {
		t.Errorf("path=%q offset=%d", ive.Path, ive.ByteOffset)
	}
}

// TestVersionWriteInvalidElementName checks element and attribute name
// validation.
func TestVersionWriteInvalidElementName(t *testing.T) {
	doc := NewDocument()
	doc.WriteSettings.Version = Version10
	root := doc.CreateElement("root")
	root.CreateAttr("a\x01b", "v")
	_, err := doc.WriteToString()
	var ive *InvalidValueError
	if !errors.As(err, &ive) {
		t.Fatalf("expected *InvalidValueError, got %v", err)
	}
	if ive.Kind != "attribute name" {
		t.Errorf("kind = %q", ive.Kind)
	}
}

// TestVersionWriteNELAttributesEscaped verifies that NEL and LS inside
// attribute values are character references in both versions, since raw
// attribute line endings are normalized by parsers.
func TestVersionWriteNELAttributesEscaped(t *testing.T) {
	for _, v := range []XMLVersion{Version10, Version11} {
		doc := NewDocument()
		doc.WriteSettings.Version = v
		root := doc.CreateElement("root")
		root.CreateAttr("a", "x\u0085y\u2028z")
		root.SetText("t\u0085u")
		out, err := doc.WriteToString()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "&#x85;") || !strings.Contains(out, "&#x2028;") {
			t.Errorf("v%s: attribute NEL/LS not escaped: %s", v, out)
		}
	}
}

// TestVersionWriteXMLDeclConsistency ensures an existing declaration that
// disagrees with the selected write version is rejected rather than
// silently emitted.
func TestVersionWriteXMLDeclConsistency(t *testing.T) {
	doc := NewDocument()
	doc.CreateProcInst("xml", `version="1.0"`)
	doc.CreateElement("root")
	doc.WriteSettings.Version = Version11
	if _, err := doc.WriteToString(); err == nil {
		t.Fatal("expected mismatch error between declaration and write version")
	}
}

// TestVersionWriteRejectsDTD verifies DOCTYPE directives are rejected with
// ErrDTD on write in explicit versions.
func TestVersionWriteRejectsDTD(t *testing.T) {
	doc := NewDocument()
	doc.WriteSettings.Version = Version11
	doc.CreateElement("root")
	// Inject a DOCTYPE-like directive at document level.
	newDirective(`DOCTYPE root`, &doc.Element)
	_, err := doc.WriteToString()
	if !errors.Is(err, ErrDTD) {
		t.Fatalf("expected ErrDTD, got %v", err)
	}
}
