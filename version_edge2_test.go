// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"errors"
	"strings"
	"testing"
)

// TestVersionPIContents checks processing instruction content handling.
func TestVersionPIContents(t *testing.T) {
	doc := parseVersioned(t, "<a><?app hello world?></a>", Version11)
	var pi *ProcInst
	for _, c := range doc.Root().Child {
		if p, ok := c.(*ProcInst); ok {
			pi = p
		}
	}
	if pi == nil {
		t.Fatal("PI missing")
	}
	if pi.Inst != "hello world" {
		t.Fatalf("inst=%q", pi.Inst)
	}

	// A PI with no instruction text is legal.
	doc2 := parseVersioned(t, "<?empty?><a/>", Version10)
	found := false
	for _, c := range doc2.Child {
		if p, ok := c.(*ProcInst); ok && p.Target == "empty" && p.Inst == "" {
			found = true
		}
	}
	if !found {
		t.Fatal("empty PI not preserved")
	}
}

// TestVersionErrorPositions checks line/column reporting across NEL line
// endings in XML 1.1.
func TestVersionErrorPositions(t *testing.T) {
	input := "<?xml version=\"1.1\"?>\n<a>\n<b>\u0085<c>&#x0;</c></b></a>"
	doc := NewDocument()
	doc.ReadSettings.Version = Version11
	err := doc.ReadFromString(input)
	if err == nil {
		t.Fatal("expected error")
	}
	var se *syntaxError
	if !errors.As(err, &se) {
		t.Fatalf("err type %T", err)
	}
	if se.line != 4 {
		t.Fatalf("line=%d want 4", se.line)
	}
}

// TestVersionContentBeforeDecl rejects non-BOM content before the XML
// declaration.
func TestVersionContentBeforeDecl(t *testing.T) {
	bad := []string{
		" <?xml version=\"1.1\"?><a/>",
		"\n<?xml version=\"1.1\"?><a/>",
		"<!--c--><?xml version=\"1.1\"?><a/>",
	}
	for _, in := range bad {
		doc := NewDocument()
		doc.ReadSettings.Version = Version11
		if err := doc.ReadFromString(in); err == nil {
			t.Errorf("accepted content before declaration: %q", in)
		}
	}
}

// TestVersionWriteExplicitNoDeclaration ensures explicit write settings
// validate even a declaration-free tree.
func TestVersionWriteExplicitNoDeclaration(t *testing.T) {
	doc := NewDocument()
	doc.WriteSettings.Version = Version10
	doc.CreateElement("root").SetText("ok")
	out, err := doc.WriteToString()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "<?xml") {
		t.Fatalf("declaration was auto-added: %s", out)
	}
}

// TestVersionMixedTextAndReferences verifies text segments adjacent to
// references and elements produce a single merged CharData token.
func TestVersionMixedTextAndReferences(t *testing.T) {
	doc := parseVersioned(t, "<a>A &#66; C<b/>D &#67;</a>", Version10)
	root := doc.Root()
	// Expected children: CharData("A B C"), Element b, CharData("D C")
	if len(root.Child) != 3 {
		var kinds []string
		for _, c := range root.Child {
			kinds = append(kinds, typeName(c))
		}
		t.Fatalf("children=%v", kinds)
	}
	if root.Child[0].(*CharData).Data != "A B C" {
		t.Fatalf("first text=%q", root.Child[0].(*CharData).Data)
	}
	if root.Child[2].(*CharData).Data != "D C" {
		t.Fatalf("last text=%q", root.Child[2].(*CharData).Data)
	}
}

func typeName(t Token) string {
	switch t.(type) {
	case *CharData:
		return "CharData"
	case *Element:
		return "Element"
	case *Comment:
		return "Comment"
	case *ProcInst:
		return "ProcInst"
	case *Directive:
		return "Directive"
	}
	return "?"
}

// TestVersionCRReferencePreserved verifies numeric references to CR/LF are
// not collapsed with raw line-ending normalization.
func TestVersionCRReferencePreserved(t *testing.T) {
	for _, v := range []XMLVersion{Version10, Version11} {
		doc := parseVersioned(t, "<?xml version=\""+v.String()+"\"?><a>x&#xD;y&#xA;z</a>", v)
		if got := doc.Root().Text(); got != "x\ry\nz" {
			t.Fatalf("v%s text=%q", v, got)
		}
		out, err := doc.WriteToString()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "&#xD;") {
			t.Fatalf("v%s CR ref lost: %s", v, out)
		}
	}
}
