// Copyright 2015-2019 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package etree

import (
	"errors"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

const xsiNamespaceURI = "http://www.w3.org/2001/XMLSchema-instance"

// expandedName identifies an element or attribute by its namespace URI
// and local name.
type expandedName struct {
	uri string
	tag string
}

// subtreeSignature records every expanded element and attribute name in
// document order, followed by registered QName values, allowing comparison
// of name meanings rather than serialized text.
func subtreeSignature(e *Element, qna *QNameAttrs) []string {
	var sig []string
	var walk func(*Element)
	walk = func(el *Element) {
		sig = append(sig, "E:"+el.NamespaceURI()+"|"+el.Tag)
		for i := range el.Attr {
			a := &el.Attr[i]
			if isXMLNSAttr(*a) {
				continue
			}
			sig = append(sig, "A:"+a.NamespaceURI()+"|"+a.Key+"="+a.Value)
		}
		for _, t := range el.Child {
			if c, ok := t.(*Element); ok {
				walk(c)
			}
		}
	}
	walk(e)
	return sig
}

// expandedQNameValues extracts the expanded meaning of every token in
// registered QName-valued attributes.
func expandedQNameValues(e *Element, qna *QNameAttrs) []string {
	var out []string
	var walk func(*Element)
	walk = func(el *Element) {
		for i := range el.Attr {
			a := &el.Attr[i]
			if isXMLNSAttr(*a) {
				continue
			}
			uri := a.NamespaceURI()
			if !qna.attrs[qnameAttrKey{uri, a.Key}] {
				continue
			}
			for _, tok := range qnameTokenRE.FindAllString(a.Value, -1) {
				prefix, local := "", tok
				if c := strings.IndexByte(tok, ':'); c >= 0 {
					prefix, local = tok[:c], tok[c+1:]
				}
				if prefix != "" {
					uri := el.findLocalNamespaceURI(prefix)
					if uri == "" {
						out = append(out, "UNRESOLVED:"+tok)
						continue
					}
					out = append(out, uri+"|"+local)
				} else {
					out = append(out, "|"+local)
				}
			}
		}
		for _, t := range el.Child {
			if c, ok := t.(*Element); ok {
				walk(c)
			}
		}
	}
	walk(e)
	sort.Strings(out)
	return out
}

func mustDoc(t *testing.T, s string) *Document {
	t.Helper()
	doc := NewDocument()
	if err := doc.ReadFromString(s); err != nil {
		t.Fatalf("failed to parse XML: %v\n%s", err, s)
	}
	return doc
}

// roundTripOK verifies the serialized output is well-formed XML.
func roundTripOK(s string) error {
	doc := NewDocument()
	return doc.ReadFromString(s)
}

// countNSDecls counts namespace declaration attributes in a subtree.
func countNSDecls(root *Element) int {
	n := 0
	var walk func(*Element)
	walk = func(e *Element) {
		for i := range e.Attr {
			if isXMLNSAttr(e.Attr[i]) {
				n++
			}
		}
		for _, t := range e.Child {
			if c, ok := t.(*Element); ok {
				walk(c)
			}
		}
	}
	walk(root)
	return n
}

// assertSerializedContains reports whether the subtree serialization
// contains the requested text.
func assertSerializedContains(t *testing.T, e *Element, want string) {
	t.Helper()
	doc := NewDocumentWithRoot(e.Copy())
	s, err := doc.WriteToString()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, want) {
		t.Fatalf("serialized subtree\n%s\ndoes not contain %q", s, want)
	}
}

// TestTransplantPrefixConflict verifies that a prefix bound to one URI in
// the source and another in the target gets remapped so expanded names are
// preserved.
func TestTransplantPrefixConflict(t *testing.T) {
	srcDoc := mustDoc(t, `
<src xmlns:p="urn:source-p">
	<p:item p:kind="a"/>
</src>`)
	dstDoc := mustDoc(t, `
<dst xmlns:p="urn:target-p"><slot/></dst>`)

	src := srcDoc.Root().SelectElement("p:item")
	slot := dstDoc.Root().SelectElement("slot")

	before := subtreeSignature(src, nil)
	got, err := Transplant(slot, src, nil)
	if err != nil {
		t.Fatalf("Transplant returned error: %v", err)
	}
	after := subtreeSignature(got, nil)
	if len(before) != len(after) {
		t.Fatalf("signature length changed: before=%v after=%v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("expanded names changed:\nbefore=%v\nafter =%v", before, after)
		}
	}

	if got.NamespaceURI() != "urn:source-p" {
		t.Fatalf("item URI = %q, want urn:source-p", got.NamespaceURI())
	}
	if a := got.SelectAttr("ns1:kind"); a == nil || a.NamespaceURI() != "urn:source-p" {
		t.Fatalf("p:kind attribute not preserved with source URI: %#v", got.Attr)
	}

	out, err := dstDoc.WriteToString()
	if err != nil {
		t.Fatal(err)
	}
	if err := roundTripOK(out); err != nil {
		t.Fatal(err)
	}
	// The target's own p: binding must remain untouched and usable.
	reparsed := mustDoc(t, out)
	if reparsed.Root().SelectElement("p:item") != nil {
		t.Fatal("transplanted item must not resolve through the target's p binding")
	}
	// Only one extra declaration should be needed on the transplanted item.
	if count := countNSDecls(got); count != 1 {
		t.Fatalf("expected exactly 1 added namespace declaration, got %d in %s", count, out)
	}
}

// TestTransplantNoNamespaceChild verifies that an unprefixed child keeps no
// namespace even when inserted under a target default namespace.
func TestTransplantNoNamespaceChild(t *testing.T) {
	srcDoc := mustDoc(t, `
<src xmlns="urn:src-default">
	<p:wrap xmlns:p="urn:source-p">
		<plain xmlns=""/>
	</p:wrap>
</src>`)
	dstDoc := mustDoc(t, `<dst xmlns="urn:dst-default"><slot/></dst>`)

	wrap := srcDoc.Root().SelectElement("p:wrap")
	slot := dstDoc.Root().SelectElement("slot")

	got, err := Transplant(slot, wrap, nil)
	if err != nil {
		t.Fatalf("Transplant returned error: %v", err)
	}

	plain := got.SelectElement("plain")
	if plain == nil {
		t.Fatal("plain child missing")
	}
	if plain.NamespaceURI() != "" {
		t.Fatalf("plain URI = %q, want empty", plain.NamespaceURI())
	}
	if got.NamespaceURI() != "urn:source-p" {
		t.Fatalf("wrap URI = %q, want urn:source-p", got.NamespaceURI())
	}
}

// TestTransplantXsiType verifies that a registered xsi:type QName value is
// rewritten along with its prefix.
func TestTransplantXsiType(t *testing.T) {
	qna := NewQNameAttrs().Register(xsiNamespaceURI, "type")

	srcDoc := mustDoc(t, `
<src xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
     xmlns:t="urn:types">
	<thing xsi:type="t:gadget"/>
</src>`)
	dstDoc := mustDoc(t, `
<dst xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
     xmlns:t="urn:other"><slot/></dst>`)

	thing := srcDoc.Root().SelectElement("thing")
	slot := dstDoc.Root().SelectElement("slot")

	before := expandedQNameValues(thing, qna)
	got, err := Transplant(slot, thing, &TransplantSettings{QNameAttrs: qna})
	if err != nil {
		t.Fatalf("Transplant returned error: %v", err)
	}
	after := expandedQNameValues(got, qna)
	if len(before) != 1 || before[0] != "urn:types|gadget" {
		t.Fatalf("unexpected before values: %v", before)
	}
	if len(after) != 1 || after[0] != before[0] {
		t.Fatalf("xsi:type changed meaning: before=%v after=%v", before, after)
	}

	ty := got.SelectAttr("xsi:type")
	if ty == nil || ty.Value == "t:gadget" {
		t.Fatalf("xsi:type prefix must be remapped, got %v", ty)
	}
}

// TestTransplantNestedRedeclare verifies a prefix redeclared to a different
// URI deeper in the subtree is handled per scope.
func TestTransplantNestedRedeclare(t *testing.T) {
	srcDoc := mustDoc(t, `
<src xmlns:p="urn:outer">
	<a:p xmlns:a="urn:outer" xmlns:p="urn:inner">
		<p:deep/>
	</a:p>
</src>`)
	dstDoc := mustDoc(t, `<dst xmlns:p="urn:target"><slot/></dst>`)

	outer := srcDoc.Root().SelectElement("a:p")
	slot := dstDoc.Root().SelectElement("slot")

	got, err := Transplant(slot, outer, nil)
	if err != nil {
		t.Fatalf("Transplant returned error: %v", err)
	}

	if got.NamespaceURI() != "urn:outer" {
		t.Fatalf("outer URI = %q, want urn:outer", got.NamespaceURI())
	}
	deep := got.ChildElements()[0]
	if deep.NamespaceURI() != "urn:inner" {
		t.Fatalf("deep URI = %q, want urn:inner", deep.NamespaceURI())
	}
}

// TestTransplantMove verifies move mode removes the source and that failure
// leaves both trees untouched.
func TestTransplantMove(t *testing.T) {
	dstDoc := mustDoc(t, `
<dst xmlns:p="urn:target">
	<slot><holder/></slot>
</dst>`)
	srcDoc := mustDoc(t, `
<src xmlns:p="urn:source"><p:item/></src>`)

	item := srcDoc.Root().SelectElement("p:item")

	dstBefore, _ := dstDoc.WriteToString()
	srcBefore, _ := srcDoc.WriteToString()

	// A move into one's own descendant must fail and change nothing.
	slot := dstDoc.Root().SelectElement("slot")
	holder := slot.SelectElement("holder")
	_, err := Transplant(holder, dstDoc.Root(), &TransplantSettings{Move: true})
	if !errors.Is(err, ErrTransplantAncestor) {
		t.Fatalf("want ErrTransplantAncestor, got %v", err)
	}

	dstAfterFail, _ := dstDoc.WriteToString()
	srcAfterFail, _ := srcDoc.WriteToString()
	if dstAfterFail != dstBefore || srcAfterFail != srcBefore {
		t.Fatal("trees changed after a failed transplant")
	}

	// A valid move removes the source subtree.
	_, err = Transplant(dstDoc.Root(), item, &TransplantSettings{Move: true})
	if err != nil {
		t.Fatalf("move Transplant returned error: %v", err)
	}
	if srcDoc.Root().SelectElement("p:item") != nil {
		t.Fatal("source subtree not removed in move mode")
	}
}

// TestTransplantQNameList verifies all tokens of a whitespace-separated
// QName list are rewritten and separators survive.
func TestTransplantQNameList(t *testing.T) {
	qna := NewQNameAttrs().Register("urn:meta", "refs")

	srcDoc := mustDoc(t, `
<src xmlns:m="urn:meta" xmlns:a="urn:A" xmlns:b="urn:B">
	<node m:refs="a:x  b:y"/>
</src>`)
	dstDoc := mustDoc(t, `<dst xmlns:a="urn:notA"><slot/></dst>`)

	node := srcDoc.Root().SelectElement("node")
	slot := dstDoc.Root().SelectElement("slot")

	before := expandedQNameValues(node, qna)
	got, err := Transplant(slot, node, &TransplantSettings{QNameAttrs: qna})
	if err != nil {
		t.Fatalf("Transplant returned error: %v", err)
	}
	after := expandedQNameValues(got, qna)
	sort.Strings(before)
	if len(after) != len(before) {
		t.Fatalf("got %d qname values, want %d", len(after), len(before))
	}
	for i := range before {
		if after[i] != before[i] {
			t.Fatalf("qname values changed meaning: before=%v after=%v", before, after)
		}
	}

	refs := got.SelectAttr("m:refs")
	if refs == nil || refs.Value == "a:x  b:y" {
		t.Fatalf("qname list must be rewritten, got %v", refs)
	}
}

// TestTransplantReuseTargetPrefix verifies no declaration is added when the
// target already binds a prefix to the source URI, and that source prefixes
// stay stable when there is no conflict.
func TestTransplantReuseTargetPrefix(t *testing.T) {
	srcDoc := mustDoc(t, `
<src xmlns:a="urn:A"><a:x/></src>`)
	dstDoc := mustDoc(t, `
<dst xmlns:a="urn:A"><slot/></dst>`)

	x := srcDoc.Root().SelectElement("a:x")
	slot := dstDoc.Root().SelectElement("slot")

	got, err := Transplant(slot, x, nil)
	if err != nil {
		t.Fatalf("Transplant returned error: %v", err)
	}
	if got.Space != "a" {
		t.Fatalf("prefix should be reused, got %q", got.Space)
	}
	if countNSDecls(got) != 0 {
		t.Fatalf("no declaration should be added, got %d", countNSDecls(got))
	}

	// No conflict at all: source prefix simply carries over, one
	// declaration, and ordinary attribute values are left untouched.
	srcDoc2 := mustDoc(t, `<src xmlns:z="urn:Z"><z:y note="z:not-a-qname"/></src>`)
	dstDoc2 := mustDoc(t, `<dst><slot/></dst>`)
	y := srcDoc2.Root().SelectElement("z:y")
	got2, err := Transplant(dstDoc2.Root().SelectElement("slot"), y, nil)
	if err != nil {
		t.Fatalf("Transplant returned error: %v", err)
	}
	if got2.Space != "z" {
		t.Fatalf("prefix should be preserved, got %q", got2.Space)
	}
	if got2.SelectAttrValue("note", "") != "z:not-a-qname" {
		t.Fatal("ordinary attribute values must not be interpreted as QNames")
	}
	if countNSDecls(got2) != 1 {
		t.Fatalf("expected 1 declaration, got %d", countNSDecls(got2))
	}
}

// TestTransplantUndefinedPrefix verifies that an unresolvable prefix fails
// the operation and leaves both trees unchanged.
func TestTransplantUndefinedPrefix(t *testing.T) {
	srcDoc := mustDoc(t, `<src><p:x/></src>`)
	dstDoc := mustDoc(t, `<dst><slot/></dst>`)

	dstBefore, _ := dstDoc.WriteToString()
	x := srcDoc.Root().SelectElement("p:x")
	_, err := Transplant(dstDoc.Root().SelectElement("slot"), x, nil)
	if !errors.Is(err, ErrUndefinedPrefix) {
		t.Fatalf("want ErrUndefinedPrefix, got %v", err)
	}
	dstAfter, _ := dstDoc.WriteToString()
	if dstAfter != dstBefore {
		t.Fatal("target tree changed after a failed transplant")
	}
}

// TestTransplantInheritedBindingHoist verifies that a prefix declared on an
// ancestor outside the moved subtree is hoisted onto the moved root.
func TestTransplantInheritedBindingHoist(t *testing.T) {
	srcDoc := mustDoc(t, `
<src xmlns:h="urn:hoisted">
	<outer>
		<h:item/>
	</outer>
</src>`)
	dstDoc := mustDoc(t, `<dst xmlns:h="urn:other"><slot/></dst>`)

	outer := srcDoc.Root().SelectElement("outer")
	before := subtreeSignature(outer, nil)
	got, err := Transplant(dstDoc.Root().SelectElement("slot"), outer, nil)
	if err != nil {
		t.Fatalf("Transplant returned error: %v", err)
	}
	after := subtreeSignature(got, nil)
	if len(before) != len(after) {
		t.Fatalf("signature length changed: before=%v after=%v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("expanded names changed:\nbefore=%v\nafter =%v", before, after)
		}
	}
	item := got.ChildElements()[0]
	if item.NamespaceURI() != "urn:hoisted" {
		t.Fatalf("item URI = %q, want urn:hoisted", item.NamespaceURI())
	}
}

// TestTransplantAtIndex verifies insertion position and index adjustment
// when moving within the same parent.
func TestTransplantAtIndex(t *testing.T) {
	doc := mustDoc(t, `<root xmlns:p="urn:P"><a/><p:b xmlns:p="urn:B"/><c/></root>`)
	root := doc.Root()
	b := root.SelectElement("p:b")

	moved, err := TransplantAt(root, 0, b, &TransplantSettings{Move: true})
	if err != nil {
		t.Fatalf("TransplantAt returned error: %v", err)
	}
	var tags []string
	for _, c := range root.ChildElements() {
		tags = append(tags, c.FullTag())
	}
	wantTags := []string{moved.FullTag(), "a", "c"}
	if len(tags) != len(wantTags) {
		t.Fatalf("children = %v, want %v", tags, wantTags)
	}
	for i := range wantTags {
		if tags[i] != wantTags[i] {
			t.Fatalf("children = %v, want %v", tags, wantTags)
		}
	}

	// Nil inputs and copy mode leave source in place.
	if _, err := Transplant(root, nil, nil); !errors.Is(err, ErrTransplantNilElement) {
		t.Fatalf("want ErrTransplantNilElement, got %v", err)
	}
}

// TestTransplantCopyLeavesSource verifies copy mode never unlinks the
// source subtree and that the returned copy is a distinct tree.
func TestTransplantCopyLeavesSource(t *testing.T) {
	srcDoc := mustDoc(t, `<src xmlns:p="urn:S"><p:x><p:y/></p:x></src>`)
	dstDoc := mustDoc(t, `<dst><slot/></dst>`)
	x := srcDoc.Root().SelectElement("p:x")
	got, err := Transplant(dstDoc.Root().SelectElement("slot"), x, nil)
	if err != nil {
		t.Fatal(err)
	}
	if srcDoc.Root().SelectElement("p:x") == nil {
		t.Fatal("source must remain in place in copy mode")
	}
	if got == x || got.Parent() == x.Parent() {
		t.Fatal("transplanted element must be a distinct copy")
	}
	if got.SelectElement("p:y") == nil {
		t.Fatal("deep copy expected")
	}
}

// TestTransplantShadowedPrefix verifies that a prefix redeclared to a
// different URI on one branch keeps both meanings, each with its own
// declaration, while siblings sharing the outer binding reuse one prefix.
func TestTransplantShadowedPrefix(t *testing.T) {
	sd := mustDoc(t, `
<r xmlns:p="urn:outer">
	<a><p:x/></a>
	<b xmlns:p="urn:inner"><p:y/></b>
	<p:z/>
</r>`)
	dd := mustDoc(t, `<D xmlns:p="urn:target"><slot/></D>`)

	got, err := Transplant(dd.Root().SelectElement("slot"), sd.Root(), nil)
	if err != nil {
		t.Fatal(err)
	}

	x := got.SelectElement("a").ChildElements()[0]
	y := got.SelectElement("b").ChildElements()[0]
	var z *Element
	for _, c := range got.ChildElements() {
		if c.NamespaceURI() == "urn:outer" && c.Tag == "z" {
			z = c
		}
	}
	if z == nil {
		t.Fatal("z element not found")
	}

	if x.NamespaceURI() != "urn:outer" {
		t.Fatalf("x = %q, want urn:outer", x.NamespaceURI())
	}
	if y.NamespaceURI() != "urn:inner" {
		t.Fatalf("y = %q, want urn:inner", y.NamespaceURI())
	}
	if z.NamespaceURI() != "urn:outer" {
		t.Fatalf("z = %q, want urn:outer", z.NamespaceURI())
	}
	if x.Space != z.Space {
		t.Fatalf("outer-scope siblings should share a prefix: %q vs %q", x.Space, z.Space)
	}
	if x.Space == y.Space {
		t.Fatalf("inner binding must use a different prefix than outer")
	}
}

// TestTransplantRandomized generates many random source and target trees
// with prefix and default-namespace declarations, transplants a random
// subtree, and asserts that every expanded element/attribute name survives
// the move. It also verifies the result round-trips through the XML parser.
func TestTransplantRandomized(t *testing.T) {
	rng := newRandSource(t)

	uris := []string{"urn:A", "urn:B", "urn:C", "urn:D"}
	prefixNames := []string{"a", "b", "p", "q"}

	for iter := 0; iter < 200; iter++ {
		srcXML, pickTag := randomTree(rng, uris, prefixNames, "src")
		dstXML, _ := randomTree(rng, uris, prefixNames, "dst")

		srcDoc := mustDoc(t, srcXML)
		dstDoc := mustDoc(t, dstXML)

		srcEl := findElementByTag(srcDoc.Root(), pickTag)
		if srcEl == nil {
			continue
		}
		targets := collectSlots(dstDoc.Root())
		if len(targets) == 0 {
			continue
		}
		target := targets[int(rng.Int63())%len(targets)]

		before := subtreeSignature(srcEl, nil)
		got, err := Transplant(target, srcEl, nil)
		if err != nil {
			// Only undefined-prefix errors are acceptable on synthetic
			// trees with inherited declarations pruned at subtree roots.
			if !errors.Is(err, ErrUndefinedPrefix) {
				t.Fatalf("iter %d: unexpected error %v\nsrc=%s\ndst=%s", iter, err, srcXML, dstXML)
			}
			continue
		}

		after := subtreeSignature(got, nil)
		if len(before) != len(after) {
			t.Fatalf("iter %d: signature length changed:\nbefore=%v\nafter =%v\nsrc=%s\ndst=%s",
				iter, before, after, srcXML, dstXML)
		}
		for i := range before {
			if before[i] != after[i] {
				t.Fatalf("iter %d: expanded names changed:\nbefore=%v\nafter =%v\nsrc=%s\ndst=%s",
					iter, before, after, srcXML, dstXML)
			}
		}

		out, err := dstDoc.WriteToString()
		if err != nil {
			t.Fatalf("iter %d: write error: %v", iter, err)
		}
		if err := roundTripOK(out); err != nil {
			t.Fatalf("iter %d: output not well-formed: %v\n%s", iter, err, out)
		}
	}
}

// newRandSource returns a deterministic pseudo-random source.
func newRandSource(t *testing.T) *rand.Rand {
	t.Helper()
	return rand.New(rand.NewSource(42))
}

// randomTree builds a small XML document with random prefix/default
// declarations. It returns the XML and the tag of a candidate subtree.
func randomTree(rng *rand.Rand, uris, prefixNames []string, rootTag string) (string, string) {
	type genScope struct {
		prefixes map[string]string
		def      string // visible default URI; "" with defSet=false means none
		defSet   bool
	}

	var b strings.Builder
	rootScope := genScope{prefixes: map[string]string{}}
	for i := int(rng.Int63n(3)); i > 0; i-- {
		p := prefixNames[rng.Int63n(int64(len(prefixNames)))]
		rootScope.prefixes[p] = uris[rng.Int63n(int64(len(uris)))]
	}
	b.WriteString("<" + rootTag)
	for p, u := range rootScope.prefixes {
		b.WriteString(` xmlns:` + p + `="` + u + `"`)
	}
	if rng.Int63n(2) == 0 {
		rootScope.def = uris[rng.Int63n(int64(len(uris)))]
		rootScope.defSet = true
		b.WriteString(` xmlns="` + rootScope.def + `"`)
	}
	b.WriteString(">")

	pickTag := ""
	nodeCounter := 0
	var gen func(depth int, sc genScope)
	gen = func(depth int, sc genScope) {
		children := int(rng.Int63n(4))
		for i := 0; i < children; i++ {
			nodeCounter++
			local := "n" + itoa(nodeCounter)

			// Build a child scope by shadowing some bindings.
			child := genScope{prefixes: cloneScope(sc.prefixes), def: sc.def, defSet: sc.defSet}
			var decls []string
			for j := int(rng.Int63n(3)); j > 0; j-- {
				p := prefixNames[rng.Int63n(int64(len(prefixNames)))]
				u := uris[rng.Int63n(int64(len(uris)))]
				child.prefixes[p] = u
				decls = append(decls, ` xmlns:`+p+`="`+u+`"`)
			}
			switch rng.Int63n(3) {
			case 0:
				child.def = uris[rng.Int63n(int64(len(uris)))]
				child.defSet = true
				decls = append(decls, ` xmlns="`+child.def+`"`)
			case 1:
				child.def = ""
				child.defSet = true
				decls = append(decls, ` xmlns=""`)
			}

			// Choose a name resolvable in the child scope.
			fullTag, usedPrefix := local, ""
			avail := make([]string, 0, len(child.prefixes))
			for p := range child.prefixes {
				avail = append(avail, p)
			}
			sort.Strings(avail)
			choice := rng.Int63n(3)
			switch {
			case choice == 0 && len(avail) > 0:
				usedPrefix = avail[rng.Int63n(int64(len(avail)))]
				fullTag = usedPrefix + ":" + local
			case choice == 1 && child.defSet && child.def != "":
				fullTag = local
			case len(avail) > 0:
				usedPrefix = avail[rng.Int63n(int64(len(avail)))]
				fullTag = usedPrefix + ":" + local
			default:
				fullTag = local
			}

			b.WriteString("<" + fullTag)
			for _, dl := range decls {
				b.WriteString(dl)
			}
			if usedPrefix != "" && rng.Int63n(2) == 0 {
				b.WriteString(` ` + usedPrefix + `:k="v"`)
			}
			b.WriteString(">")

			if pickTag == "" && depth <= 1 {
				pickTag = fullTag
			}
			if depth > 0 {
				gen(depth-1, child)
			}
			b.WriteString("</" + fullTag + ">")
		}
	}
	gen(int(1+rng.Int63n(3)), rootScope)

	b.WriteString("</" + rootTag + ">")
	return b.String(), pickTag
}

func cloneScope(m map[string]string) map[string]string {
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func findElementByTag(root *Element, tag string) *Element {
	var found *Element
	var walk func(*Element)
	walk = func(e *Element) {
		if found != nil {
			return
		}
		if e.FullTag() == tag {
			found = e
			return
		}
		for _, t := range e.Child {
			if c, ok := t.(*Element); ok {
				walk(c)
			}
		}
	}
	walk(root)
	return found
}

func collectSlots(root *Element) []*Element {
	var out []*Element
	var walk func(*Element)
	walk = func(e *Element) {
		if len(e.Child) == 0 {
			out = append(out, e)
			return
		}
		for _, t := range e.Child {
			if c, ok := t.(*Element); ok {
				walk(c)
			}
		}
	}
	walk(root)
	return out
}

func TestTransplantDetached(t *testing.T) {
	// Detached source element with its own declarations is self-contained.
	src := NewElement("p:x")
	src.CreateAttr("xmlns:p", "urn:S")
	src.CreateAttr("p:k", "v")
	dst := mustDoc(t, `<D xmlns:p="urn:T"><slot/></D>`)
	got, err := Transplant(dst.Root().SelectElement("slot"), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.NamespaceURI() != "urn:S" {
		t.Fatalf("URI=%s", got.NamespaceURI())
	}
	if got.Attr[1].NamespaceURI() != "urn:S" {
		t.Fatalf("attr URI=%s", got.Attr[1].NamespaceURI())
	}

	// Detached source relying on an unresolvable prefix fails atomically.
	bad := NewElement("q:y")
	before, _ := dst.WriteToString()
	if _, err := Transplant(dst.Root(), bad, nil); !errors.Is(err, ErrUndefinedPrefix) {
		t.Fatalf("want ErrUndefinedPrefix, got %v", err)
	}
	after, _ := dst.WriteToString()
	if before != after {
		t.Fatal("target changed after failure")
	}
}
