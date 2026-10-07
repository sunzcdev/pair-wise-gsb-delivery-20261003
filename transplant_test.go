package etree

import (
	"errors"
	"strings"
	"testing"
)

// resolveQName resolves a QName value against the namespace bindings in
// scope at element 'e', returning the expanded name's URI and local part.
func resolveQName(t *testing.T, e *Element, value string) (string, string) {
	t.Helper()
	prefix, local := spaceDecompose(value)
	if prefix == "" {
		return e.findDefaultNamespaceURI(), local
	}
	return e.findLocalNamespaceURI(prefix), local
}

// roundTrip serializes the document and reparses it, so that tests can
// verify expanded names survive serialization.
func roundTrip(t *testing.T, doc *Document) *Document {
	t.Helper()
	s, err := doc.WriteToString()
	if err != nil {
		t.Fatalf("etree: failed to serialize document: %v", err)
	}
	return newDocumentFromString(t, s)
}

// selectAttrByKey returns the element's first attribute with the given
// local key, regardless of namespace prefix.
func selectAttrByKey(t *testing.T, e *Element, key string) *Attr {
	t.Helper()
	for i := range e.Attr {
		if e.Attr[i].Key == key {
			return &e.Attr[i]
		}
	}
	t.Fatalf("etree: attribute %q not found on element %q", key, e.FullTag())
	return nil
}

// countNsDecls returns the number of namespace declaration attributes on
// the element.
func countNsDecls(e *Element) int {
	n := 0
	for i := range e.Attr {
		if isNsDecl(&e.Attr[i]) {
			n++
		}
	}
	return n
}

func TestTransplantCopyPrefixConflict(t *testing.T) {
	srcDoc := newDocumentFromString(t,
		`<root xmlns:p="urn:a"><p:item p:attr="1"><p:sub/></p:item></root>`)
	dstDoc := newDocumentFromString(t,
		`<dst xmlns:p="urn:b"><p:existing/></dst>`)

	srcBefore, err := srcDoc.WriteToString()
	if err != nil {
		t.Fatal(err)
	}

	dst := dstDoc.Root()
	src := srcDoc.Root().ChildElements()[0]

	moved, err := dst.TransplantChild(src, TransplantCopy, nil)
	if err != nil {
		t.Fatalf("etree: transplant failed: %v", err)
	}

	// The transplanted copy must keep its expanded names even though the
	// destination binds prefix "p" to a different URI.
	checkStrEq(t, moved.NamespaceURI(), "urn:a")
	checkStrEq(t, moved.ChildElements()[0].NamespaceURI(), "urn:a")
	checkStrEq(t, selectAttrByKey(t, moved, "attr").NamespaceURI(), "urn:a")

	// The destination's own elements must be unaffected.
	checkStrEq(t, dst.FindElement("./p:existing").NamespaceURI(), "urn:b")

	// Copy mode must leave the source tree untouched.
	checkElementEq(t, src.Parent(), srcDoc.Root())
	srcAfter, err := srcDoc.WriteToString()
	if err != nil {
		t.Fatal(err)
	}
	checkStrEq(t, srcAfter, srcBefore)

	// Expanded names must survive serialization.
	dst2 := roundTrip(t, dstDoc)
	root2 := dst2.Root()
	checkStrEq(t, root2.ChildElements()[0].NamespaceURI(), "urn:b")
	moved2 := root2.ChildElements()[1]
	checkStrEq(t, moved2.NamespaceURI(), "urn:a")
	checkStrEq(t, moved2.ChildElements()[0].NamespaceURI(), "urn:a")
	checkStrEq(t, selectAttrByKey(t, moved2, "attr").NamespaceURI(), "urn:a")
}

func TestTransplantMoveNoNamespace(t *testing.T) {
	srcDoc := newDocumentFromString(t,
		`<root><item><name>x</name></item></root>`)
	dstDoc := newDocumentFromString(t,
		`<dst xmlns="urn:target"><existing/></dst>`)

	dst := dstDoc.Root()
	src := srcDoc.Root().ChildElements()[0]

	moved, err := dst.TransplantChild(src, TransplantMove, nil)
	if err != nil {
		t.Fatalf("etree: transplant failed: %v", err)
	}

	// The destination's default namespace must not capture the moved
	// subtree's no-namespace elements.
	checkStrEq(t, moved.NamespaceURI(), "")
	checkStrEq(t, moved.ChildElements()[0].NamespaceURI(), "")
	checkStrEq(t, moved.SelectAttrValue("xmlns", "<missing>"), "")

	// Move mode must detach the source element from its old parent.
	checkElementEq(t, moved, src)
	checkIntEq(t, len(srcDoc.Root().ChildElements()), 0)

	// The destination's own elements must be unaffected.
	checkStrEq(t, dst.FindElement("./existing").NamespaceURI(), "urn:target")

	// Expanded names must survive serialization.
	dst2 := roundTrip(t, dstDoc)
	moved2 := dst2.Root().ChildElements()[1]
	checkStrEq(t, moved2.NamespaceURI(), "")
	checkStrEq(t, moved2.ChildElements()[0].NamespaceURI(), "")
}

func TestTransplantDefaultNamespace(t *testing.T) {
	srcDoc := newDocumentFromString(t,
		`<root xmlns="urn:def"><item><sub/></item></root>`)
	dstDoc := newDocumentFromString(t,
		`<dst xmlns="urn:other"><existing/></dst>`)

	dst := dstDoc.Root()
	src := srcDoc.Root().ChildElements()[0]

	moved, err := dst.TransplantChild(src, TransplantCopy, nil)
	if err != nil {
		t.Fatalf("etree: transplant failed: %v", err)
	}

	// The subtree's default namespace must be redeclared on the moved root
	// so it is not shadowed by the destination's default namespace.
	checkStrEq(t, moved.NamespaceURI(), "urn:def")
	checkStrEq(t, moved.ChildElements()[0].NamespaceURI(), "urn:def")
	checkStrEq(t, moved.SelectAttrValue("xmlns", "<missing>"), "urn:def")
	checkStrEq(t, dst.FindElement("./existing").NamespaceURI(), "urn:other")

	dst2 := roundTrip(t, dstDoc)
	moved2 := dst2.Root().ChildElements()[1]
	checkStrEq(t, moved2.NamespaceURI(), "urn:def")
	checkStrEq(t, moved2.ChildElements()[0].NamespaceURI(), "urn:def")
}

func TestTransplantQNameAttrs(t *testing.T) {
	srcDoc := newDocumentFromString(t,
		`<root xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:t="urn:types">`+
			`<t:item xsi:type="t:Widget" refs="t:a t:b">`+
			`<t:part xmlns:t="urn:redecl"><t:inner/></t:part>`+
			`</t:item></root>`)
	dstDoc := newDocumentFromString(t,
		`<dst xmlns:t="urn:dst-types" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><t:e/></dst>`)

	settings := &TransplantSettings{
		QNameAttrs: map[string]QNameAttrMode{
			"xsi:type": QNameValue,
			"refs":     QNameListValue,
		},
	}

	dst := dstDoc.Root()
	src := srcDoc.Root().ChildElements()[0]

	moved, err := dst.TransplantChild(src, TransplantMove, settings)
	if err != nil {
		t.Fatalf("etree: transplant failed: %v", err)
	}

	checkExpandedNames := func(m *Element) {
		t.Helper()

		// The element name used prefix "t" bound to urn:types, which
		// conflicts with the destination's "t" binding.
		checkStrEq(t, m.NamespaceURI(), "urn:types")

		// The registered QName attribute values must resolve to the same
		// expanded names as before the transplant.
		uri, local := resolveQName(t, m, m.SelectAttrValue("xsi:type", ""))
		checkStrEq(t, uri, "urn:types")
		checkStrEq(t, local, "Widget")

		tokens := strings.Fields(m.SelectAttrValue("refs", ""))
		checkIntEq(t, len(tokens), 2)
		for i, want := range []string{"a", "b"} {
			uri, local := resolveQName(t, m, tokens[i])
			checkStrEq(t, uri, "urn:types")
			checkStrEq(t, local, want)
		}

		// The nested redeclaration of "t" must keep its own meaning.
		part := m.ChildElements()[0]
		checkStrEq(t, part.NamespaceURI(), "urn:redecl")
		checkStrEq(t, part.ChildElements()[0].NamespaceURI(), "urn:redecl")
	}

	checkExpandedNames(moved)

	// The destination's own elements must be unaffected.
	checkStrEq(t, dst.FindElement("./t:e").NamespaceURI(), "urn:dst-types")

	// Only one new namespace declaration should be needed: a fresh prefix
	// for urn:types. The xsi prefix is already bound by the destination,
	// and urn:redecl travels with the subtree's own declaration.
	checkIntEq(t, countNsDecls(moved), 1)

	// Expanded names and QName values must survive serialization.
	dst2 := roundTrip(t, dstDoc)
	checkExpandedNames(dst2.Root().ChildElements()[1])
}

func TestTransplantReuseDestinationPrefix(t *testing.T) {
	srcDoc := newDocumentFromString(t,
		`<root xmlns:q="urn:a"><q:item q:x="1"/></root>`)
	dstDoc := newDocumentFromString(t,
		`<dst xmlns:z="urn:a"/>`)

	dst := dstDoc.Root()
	src := srcDoc.Root().ChildElements()[0]

	moved, err := dst.TransplantChild(src, TransplantCopy, nil)
	if err != nil {
		t.Fatalf("etree: transplant failed: %v", err)
	}

	// The destination already binds a prefix to urn:a, so no new
	// declarations are necessary; the source prefix is renamed to it.
	checkStrEq(t, moved.NamespaceURI(), "urn:a")
	checkStrEq(t, moved.Space, "z")
	checkStrEq(t, selectAttrByKey(t, moved, "x").NamespaceURI(), "urn:a")
	checkIntEq(t, countNsDecls(moved), 0)

	dst2 := roundTrip(t, dstDoc)
	moved2 := dst2.Root().ChildElements()[0]
	checkStrEq(t, moved2.NamespaceURI(), "urn:a")
	checkStrEq(t, selectAttrByKey(t, moved2, "x").NamespaceURI(), "urn:a")
}

func TestTransplantUnboundPrefix(t *testing.T) {
	for _, mode := range []TransplantMode{TransplantCopy, TransplantMove} {
		srcDoc := newDocumentFromString(t, `<root><p:item/></root>`)
		dstDoc := newDocumentFromString(t, `<dst/>`)

		srcBefore, _ := srcDoc.WriteToString()
		dstBefore, _ := dstDoc.WriteToString()

		_, err := dstDoc.Root().TransplantChild(
			srcDoc.Root().ChildElements()[0], mode, nil)
		if !errors.Is(err, ErrUnboundPrefix) {
			t.Fatalf("etree: expected ErrUnboundPrefix, got %v", err)
		}

		// On failure, both trees must remain unmodified.
		srcAfter, _ := srcDoc.WriteToString()
		dstAfter, _ := dstDoc.WriteToString()
		checkStrEq(t, srcAfter, srcBefore)
		checkStrEq(t, dstAfter, dstBefore)
	}
}

func TestTransplantCycle(t *testing.T) {
	doc := newDocumentFromString(t, `<root><a><b/></a></root>`)
	before, _ := doc.WriteToString()

	a := doc.Root().ChildElements()[0]
	b := a.ChildElements()[0]

	if _, err := b.TransplantChild(a, TransplantMove, nil); !errors.Is(err, ErrTransplantCycle) {
		t.Fatalf("etree: expected ErrTransplantCycle, got %v", err)
	}
	if _, err := a.TransplantChild(a, TransplantCopy, nil); !errors.Is(err, ErrTransplantCycle) {
		t.Fatalf("etree: expected ErrTransplantCycle, got %v", err)
	}

	after, _ := doc.WriteToString()
	checkStrEq(t, after, before)
}

func TestTransplantMoveToAncestor(t *testing.T) {
	// Moving a subtree to one of its own ancestors is legal; only moving
	// an element into itself or a descendant creates a cycle.
	doc := newDocumentFromString(t,
		`<root xmlns:p="urn:a"><wrapper><p:item p:attr="1"/></wrapper></root>`)

	root := doc.Root()
	src := root.FindElement("./wrapper/p:item")

	moved, err := root.TransplantChild(src, TransplantMove, nil)
	if err != nil {
		t.Fatalf("etree: transplant failed: %v", err)
	}

	checkElementEq(t, moved, src)
	checkElementEq(t, moved.Parent(), root)
	checkStrEq(t, moved.NamespaceURI(), "urn:a")
	checkStrEq(t, selectAttrByKey(t, moved, "attr").NamespaceURI(), "urn:a")
	checkIntEq(t, len(root.FindElement("./wrapper").ChildElements()), 0)

	doc2 := roundTrip(t, doc)
	moved2 := doc2.Root().ChildElements()[1]
	checkStrEq(t, moved2.NamespaceURI(), "urn:a")
	checkStrEq(t, selectAttrByKey(t, moved2, "attr").NamespaceURI(), "urn:a")
}
