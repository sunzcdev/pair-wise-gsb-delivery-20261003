package xpath

import (
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

// This file provides a self-contained XML adapter built on encoding/xml and
// uses it to exercise the XPath namespace:: axis. The adapter implements the
// optional NamespaceNavigator capability: it enumerates the namespace
// bindings that are in scope for an element (its own declarations plus the
// non-shadowed bindings inherited from ancestors), honors default-namespace
// undeclaration (xmlns=""), and always exposes the implicit "xml" prefix.

const xmlNamespaceURI = "http://www.w3.org/XML/1998/namespace"

type xmlNSDecl struct {
	prefix string // "" is the default namespace
	uri    string
}

type xmlNSNode struct {
	parent, firstChild, lastChild, prevSibling, nextSibling *xmlNSNode

	kind    NodeType
	local   string
	prefix  string
	uri     string
	text    string
	attrs   []xmlNSAttr
	nsDecls []xmlNSDecl
}

type xmlNSAttr struct {
	local, uri, value string
}

// parseXML builds a tiny DOM from an XML document using encoding/xml,
// keeping namespace declarations separate from ordinary attributes.
func parseXML(t *testing.T, src string) *xmlNSNode {
	t.Helper()
	doc := &xmlNSNode{kind: RootNode}

	type frame struct {
		el      *xmlNSNode
		uri2pfx map[string]string // best-effort prefix lookup for the element
	}
	dec := xml.NewDecoder(strings.NewReader(src))
	var stack []*frame
	current := doc

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		switch v := tok.(type) {
		case xml.StartElement:
			el := &xmlNSNode{kind: ElementNode, local: v.Name.Local, uri: v.Name.Space}
			el.parent = current
			appendXMLChild(current, el)

			uri2pfx := map[string]string{}
			if len(stack) > 0 {
				for k, val := range stack[len(stack)-1].uri2pfx {
					uri2pfx[k] = val
				}
			}
			for _, a := range v.Attr {
				switch {
				case a.Name.Space == "xmlns":
					el.nsDecls = append(el.nsDecls, xmlNSDecl{prefix: a.Name.Local, uri: a.Value})
					uri2pfx[a.Value] = a.Name.Local
				case a.Name.Space == "" && a.Name.Local == "xmlns":
					el.nsDecls = append(el.nsDecls, xmlNSDecl{prefix: "", uri: a.Value})
				default:
					el.attrs = append(el.attrs, xmlNSAttr{local: a.Name.Local, uri: a.Name.Space, value: a.Value})
				}
			}
			if el.uri != "" {
				if pf, ok := uri2pfx[el.uri]; ok {
					el.prefix = pf
				}
			}

			current = el
			stack = append(stack, &frame{el: el, uri2pfx: uri2pfx})
		case xml.EndElement:
			stack = stack[:len(stack)-1]
			current = current.parent
		case xml.CharData:
			text := string(v)
			if strings.TrimSpace(text) == "" {
				continue
			}
			appendXMLChild(current, &xmlNSNode{kind: TextNode, text: text, parent: current})
		}
	}
	return doc
}

func appendXMLChild(parent, child *xmlNSNode) {
	if parent.lastChild == nil {
		parent.firstChild = child
	} else {
		parent.lastChild.nextSibling = child
		child.prevSibling = parent.lastChild
	}
	parent.lastChild = child
}

// xmlNSNavigator is an encoding/xml backed NodeNavigator implementing
// NamespaceNavigator.
type xmlNSNavigator struct {
	root, curr *xmlNSNode

	// -1: on the element (or a non-element node); otherwise index of the
	// currently selected attribute or in-scope namespace node.
	attr int
	ns   int

	// mode selects which secondary axis attr indexes: false = attributes,
	// true = in-scope namespace nodes.
	onNS bool

	namespaces []xmlNSDecl // lazily enumerated in-scope bindings
}

func newXMLNSNavigator(doc *xmlNSNode) *xmlNSNavigator {
	return &xmlNSNavigator{root: doc, curr: doc, attr: -1, ns: -1}
}

func (n *xmlNSNavigator) NodeType() NodeType {
	if n.onNS {
		return NamespaceNode
	}
	if n.attr != -1 {
		return AttributeNode
	}
	return n.curr.kind
}

func (n *xmlNSNavigator) LocalName() string {
	if n.onNS {
		return n.namespaces[n.ns].prefix
	}
	if n.attr != -1 {
		return n.curr.attrs[n.attr].local
	}
	return n.curr.local
}

func (n *xmlNSNavigator) Prefix() string {
	// A namespace node has no prefix of its own; its local name is the
	// prefix of the binding it represents.
	if n.onNS {
		return ""
	}
	if n.attr != -1 {
		return ""
	}
	return n.curr.prefix
}

func (n *xmlNSNavigator) NamespaceURL() string {
	if n.onNS {
		return ""
	}
	if n.attr != -1 {
		return n.curr.attrs[n.attr].uri
	}
	return n.curr.uri
}

func (n *xmlNSNavigator) Value() string {
	if n.onNS {
		return n.namespaces[n.ns].uri
	}
	if n.attr != -1 {
		return n.curr.attrs[n.attr].value
	}
	if n.curr.kind == TextNode {
		return n.curr.text
	}
	var sb strings.Builder
	for ch := n.curr.firstChild; ch != nil; ch = ch.nextSibling {
		collectXMLText(ch, &sb)
	}
	return sb.String()
}

func collectXMLText(node *xmlNSNode, sb *strings.Builder) {
	if node.kind == TextNode {
		sb.WriteString(node.text)
	}
	for ch := node.firstChild; ch != nil; ch = ch.nextSibling {
		collectXMLText(ch, sb)
	}
}

func (n *xmlNSNavigator) Copy() NodeNavigator {
	n2 := *n
	if n.namespaces != nil {
		n2.namespaces = append([]xmlNSDecl(nil), n.namespaces...)
	}
	return &n2
}

func (n *xmlNSNavigator) MoveToRoot() {
	*n = xmlNSNavigator{root: n.root, curr: n.root, attr: -1, ns: -1}
}

func (n *xmlNSNavigator) MoveToParent() bool {
	if n.onNS || n.attr != -1 {
		// The parent of a namespace or attribute node is the queried
		// element, not the ancestor that first declared the binding.
		n.onNS = false
		n.attr = -1
		n.ns = -1
		n.namespaces = nil
		return true
	}
	if n.curr.parent == nil {
		return false
	}
	n.curr = n.curr.parent
	return true
}

func (n *xmlNSNavigator) MoveToNextAttribute() bool {
	if n.curr.kind != ElementNode || n.attr >= len(n.curr.attrs)-1 {
		return false
	}
	n.onNS = false
	n.ns = -1
	n.attr++
	return true
}

// MoveToNextNamespace implements the optional NamespaceNavigator capability.
func (n *xmlNSNavigator) MoveToNextNamespace() bool {
	if n.curr.kind != ElementNode {
		return false
	}
	if !n.onNS {
		// Starting a fresh enumeration on the current element.
		n.namespaces = inScopeNamespaces(n.curr)
		n.onNS = true
		n.attr = -1
		n.ns = -1
	}
	if n.ns >= len(n.namespaces)-1 {
		return false
	}
	n.ns++
	return true
}

// inScopeNamespaces computes the namespace bindings in scope for el by
// walking the ancestor chain rootwards, accumulating declarations in
// document order (outermost first). The nearest declaration of a prefix
// wins (prefix shadowing); a declaration with an empty URI (notably
// xmlns="") removes that prefix from scope. The implicit xml binding is
// always present.
func inScopeNamespaces(el *xmlNSNode) []xmlNSDecl {
	var chain []*xmlNSNode
	for cur := el; cur != nil; cur = cur.parent {
		if cur.kind == ElementNode {
			chain = append(chain, cur)
		}
	}

	var decls []xmlNSDecl // outermost declarations first
	for i := len(chain) - 1; i >= 0; i-- {
		decls = append(decls, chain[i].nsDecls...)
	}

	var result []xmlNSDecl
	pos := map[string]int{} // position of each in-scope prefix in result
	removed := map[string]bool{}
	for _, d := range decls {
		if d.uri == "" {
			// Undeclaration (xmlns="") removes the prefix from scope.
			if i, ok := pos[d.prefix]; ok {
				result = append(result[:i], result[i+1:]...)
				delete(pos, d.prefix)
				for j, r := range result {
					pos[r.prefix] = j
				}
			}
			removed[d.prefix] = true
			continue
		}
		// Re-appearing after an undeclaration re-enters scope.
		delete(removed, d.prefix)
		if i, ok := pos[d.prefix]; ok {
			result[i] = d // prefix shadowing keeps the original position
		} else {
			pos[d.prefix] = len(result)
			result = append(result, d)
		}
	}

	if !removed["xml"] {
		result = append(result, xmlNSDecl{prefix: "xml", uri: xmlNamespaceURI})
	}
	return result
}

func (n *xmlNSNavigator) MoveToChild() bool {
	if n.onNS || n.attr != -1 {
		return false
	}
	if n.curr.firstChild == nil {
		return false
	}
	n.curr = n.curr.firstChild
	return true
}

func (n *xmlNSNavigator) MoveToFirst() bool {
	if n.onNS || n.attr != -1 || n.curr.prevSibling == nil {
		return false
	}
	for n.curr.prevSibling != nil {
		n.curr = n.curr.prevSibling
	}
	return true
}

func (n *xmlNSNavigator) MoveToNext() bool {
	if n.onNS || n.attr != -1 || n.curr.nextSibling == nil {
		return false
	}
	n.curr = n.curr.nextSibling
	return true
}

func (n *xmlNSNavigator) MoveToPrevious() bool {
	if n.onNS || n.attr != -1 || n.curr.prevSibling == nil {
		return false
	}
	n.curr = n.curr.prevSibling
	return true
}

func (n *xmlNSNavigator) MoveTo(other NodeNavigator) bool {
	o, ok := other.(*xmlNSNavigator)
	if !ok || o.root != n.root {
		return false
	}
	*n = *o
	if o.namespaces != nil {
		n.namespaces = append([]xmlNSDecl(nil), o.namespaces...)
	}
	return true
}

// Three layers of prefix redefinition, plus default-namespace handling:
//
//	<a xmlns="urn:default" xmlns:p="urn:p1" xmlns:q="urn:q1">
//	  <b xmlns:p="urn:p2">
//	    <c xmlns:p="urn:p3" xmlns:q="urn:q2" xmlns="">
//	      <d/>
//	      <e xmlns:p="urn:p4"/>
//	    </c>
//	  </b>
//	</a>
//
// In scope for <d>: default undeclared, p=urn:p3 (inherited from c),
// q=urn:q2 (rebound on c), plus the implicit xml prefix.
const threeTierNSXML = `
<a xmlns="urn:default" xmlns:p="urn:p1" xmlns:q="urn:q1">
  <b xmlns:p="urn:p2">
    <c xmlns:p="urn:p3" xmlns:q="urn:q2" xmlns="">
      <d/>
      <e xmlns:p="urn:p4">text</e>
    </c>
  </b>
</a>`

type nsPair struct {
	name, uri string
}

func collectNamespaces(t *testing.T, doc *xmlNSNode, expr string) []nsPair {
	t.Helper()
	e, err := Compile(expr)
	assertNoErr(t, err)
	iter := e.Select(newXMLNSNavigator(doc))
	var pairs []nsPair
	for iter.MoveNext() {
		cur := iter.Current()
		if cur.NodeType() != NamespaceNode {
			t.Fatalf("expected NamespaceNode, got %v", cur.NodeType())
		}
		pairs = append(pairs, nsPair{name: cur.LocalName(), uri: cur.Value()})
	}
	return pairs
}

func TestNamespaceAxisInScopeBindings(t *testing.T) {
	doc := parseXML(t, threeTierNSXML)

	// On the root element <a>: default, p (first definition), q and the
	// implicit xml binding.
	got := collectNamespaces(t, doc, `/a/namespace::*`)
	assertEqual(t, []nsPair{
		{"", "urn:default"},
		{"p", "urn:p1"},
		{"q", "urn:q1"},
		{"xml", xmlNamespaceURI},
	}, got)

	// <b>: p is shadowed by the second definition; default and q are
	// inherited from the ancestor.
	got = collectNamespaces(t, doc, `//b/namespace::*`)
	assertEqual(t, []nsPair{
		{"", "urn:default"},
		{"p", "urn:p2"},
		{"q", "urn:q1"},
		{"xml", xmlNamespaceURI},
	}, got)

	// <d>: p is shadowed for the third time (declared on ancestor <c>),
	// q rebound, the default namespace canceled with xmlns="".
	got = collectNamespaces(t, doc, `//d/namespace::*`)
	assertEqual(t, []nsPair{
		{"p", "urn:p3"},
		{"q", "urn:q2"},
		{"xml", xmlNamespaceURI},
	}, got)

	// <e>: the third-tier binding p=urn:p3 is itself shadowed by a fourth
	// declaration on the element; the canceled default stays absent.
	got = collectNamespaces(t, doc, `//e/namespace::*`)
	assertEqual(t, []nsPair{
		{"p", "urn:p4"},
		{"q", "urn:q2"},
		{"xml", xmlNamespaceURI},
	}, got)
}

func TestNamespaceAxisNameTests(t *testing.T) {
	doc := parseXML(t, threeTierNSXML)

	// Select a binding by prefix; name/local-name/string-value semantics.
	got := collectNamespaces(t, doc, `//d/namespace::p`)
	assertEqual(t, []nsPair{{"p", "urn:p3"}}, got)

	// The default namespace node has an empty local name.
	got = collectNamespaces(t, doc, `/a/namespace::*[local-name()=""]`)
	assertEqual(t, []nsPair{{"", "urn:default"}}, got)

	// Once canceled with xmlns="", no empty-prefix namespace node exists.
	assertEqual(t, 0, len(collectNamespaces(t, doc, `//c/namespace::*[local-name()=""]`)))

	// node() on the namespace axis matches namespace nodes too.
	assertEqual(t, 3, len(collectNamespaces(t, doc, `//d/namespace::node()`)))

	// The implicit xml prefix is always present with its fixed URI.
	got = collectNamespaces(t, doc, `//d/namespace::xml`)
	assertEqual(t, []nsPair{{"xml", xmlNamespaceURI}}, got)

	// Bindings can be filtered by their string value (the namespace URI).
	got = collectNamespaces(t, doc, `//d/namespace::*[.="urn:p3"]`)
	assertEqual(t, []nsPair{{"p", "urn:p3"}}, got)

	// The axis is empty for non-element and document context nodes.
	evalXML(t, doc, `count(//text()/namespace::*)`, float64(0))
	evalXML(t, doc, `count(/namespace::*)`, float64(0))

	// Boolean existence of a binding.
	evalXML(t, doc, `boolean(//d/namespace::p)`, true)
	evalXML(t, doc, `boolean(//d/namespace::zz)`, false)
}

func TestNamespaceAxisParentIsQueriedElement(t *testing.T) {
	doc := parseXML(t, threeTierNSXML)
	e, err := Compile(`//d/namespace::p`)
	assertNoErr(t, err)
	iter := e.Select(newXMLNSNavigator(doc))
	if !iter.MoveNext() {
		t.Fatal("expected the p namespace node")
	}
	ns := iter.Current()

	// The namespace node's parent must be <d>, even though p was first
	// declared on the remote ancestor <a> and rebound twice in between.
	parent := ns.Copy()
	if !parent.MoveToParent() {
		t.Fatal("namespace node must have a parent")
	}
	assertEqual(t, ElementNode, parent.NodeType())
	assertEqual(t, "d", parent.LocalName())

	// Moving to the parent through the parent axis yields the same node.
	got := collectElements(t, doc, `//d/namespace::p/parent::*`)
	assertEqual(t, []string{"d"}, got)

	if iter.MoveNext() {
		t.Fatal("expected exactly one p namespace node")
	}
}

func collectElements(t *testing.T, doc *xmlNSNode, expr string) []string {
	t.Helper()
	e, err := Compile(expr)
	assertNoErr(t, err)
	iter := e.Select(newXMLNSNavigator(doc))
	var names []string
	for iter.MoveNext() {
		cur := iter.Current()
		if cur.NodeType() != ElementNode {
			t.Fatalf("expected ElementNode, got %v", cur.NodeType())
		}
		names = append(names, cur.LocalName())
	}
	return names
}

func evalXML(t *testing.T, doc *xmlNSNode, expr string, want interface{}) {
	t.Helper()
	e, err := Compile(expr)
	assertNoErr(t, err)
	assertEqual(t, want, e.Evaluate(newXMLNSNavigator(doc)))
}

func TestNamespaceAxisNodeFunctions(t *testing.T) {
	doc := parseXML(t, threeTierNSXML)

	// name() of a prefixed namespace node is the prefix; the string value
	// of the node-set is the URI.
	evalXML(t, doc, `string(//d/namespace::p)`, "urn:p3")
	evalXML(t, doc, `name(//d/namespace::p)`, "p")
	evalXML(t, doc, `local-name(//d/namespace::p)`, "p")
	evalXML(t, doc, `name(/a/namespace::*[local-name()=""])`, "")
	// A namespace node is not itself in any namespace.
	evalXML(t, doc, `namespace-uri(//d/namespace::p)`, "")
}

func TestNamespaceAxisSetDedup(t *testing.T) {
	doc := parseXML(t, threeTierNSXML)

	// Repeating the same axis step in a union must not duplicate the
	// namespace nodes of one element.
	pairs := collectNamespaces(t, doc, `//d/namespace::* | //d/namespace::*`)
	assertEqual(t, 3, len(pairs))

	// The same prefix bound on different elements are distinct nodes, and
	// navigating the parent axis from any of them lands on <d> (the engine
	// de-duplicates at set operations, not across individual path steps,
	// just like for the attribute axis).
	parents := collectElements(t, doc, `//d/namespace::p/parent::* | //d/namespace::q/parent::*`)
	assertEqual(t, []string{"d"}, parents)

	// Counting the namespace axis across elements exercises
	// ancestor/union style aggregation without collapsing distinct nodes.
	evalXML(t, doc, `count(//d/namespace::*)`, float64(3))
	evalXML(t, doc, `count(/a/namespace::*)`, float64(4))

	// Stable identity: the same binding revisited on the same element hashes
	// equal, while the same prefix/URI on a different element stays distinct.
	dp := mustSelectOne(t, doc, `//d/namespace::p`)
	dpAgain := mustSelectOne(t, doc, `//d/namespace::p`)
	ep := mustSelectOne(t, doc, `//e/namespace::p`)
	assertEqual(t, getHashCode(dp.Copy()), getHashCode(dpAgain.Copy()))
	if getHashCode(dp.Copy()) == getHashCode(ep.Copy()) {
		t.Fatal("namespace nodes of different elements must not share identity")
	}
	xmlp := mustSelectOne(t, doc, `//d/namespace::xml`)
	if getHashCode(xmlp.Copy()) == getHashCode(dp.Copy()) {
		t.Fatal("distinct prefixes of one element must not share identity")
	}
}

func mustSelectOne(t *testing.T, doc *xmlNSNode, expr string) NodeNavigator {
	t.Helper()
	e, err := Compile(expr)
	assertNoErr(t, err)
	iter := e.Select(newXMLNSNavigator(doc))
	if !iter.MoveNext() {
		t.Fatalf("expected one node for %q", expr)
	}
	return iter.Current().Copy()
}

func TestNamespaceAxisLegacyAdapterError(t *testing.T) {
	// A navigator that does not implement NamespaceNavigator surfaces the
	// explicit unsupported-result error at query time.
	legacy := createNavigator(employee_example)
	if _, ok := NodeNavigator(legacy).(NamespaceNavigator); ok {
		t.Fatal("test adapter unexpectedly implements NamespaceNavigator")
	}

	e, err := Compile(`//name/namespace::*`)
	assertNoErr(t, err)

	var recovered interface{}
	func() {
		defer func() { recovered = recover() }()
		iter := e.Select(legacy)
		for iter.MoveNext() {
		}
	}()
	if recovered != ErrNamespaceAxisUnsupported {
		t.Fatalf("expected ErrNamespaceAxisUnsupported, got %v", recovered)
	}
}

func TestNamespaceAxisCopiesAreStable(t *testing.T) {
	doc := parseXML(t, threeTierNSXML)
	e, err := Compile(`//d/namespace::p`)
	assertNoErr(t, err)
	iter := e.Select(newXMLNSNavigator(doc))
	if !iter.MoveNext() {
		t.Fatal("expected namespace node")
	}
	first := iter.Current()
	snapshot := first.Copy()

	// Continuing iteration on the live iterator must not move a copy.
	if iter.MoveNext() {
		t.Fatal("unexpected extra namespace node")
	}
	assertEqual(t, NamespaceNode, snapshot.NodeType())
	assertEqual(t, "p", snapshot.LocalName())
	assertEqual(t, "urn:p3", snapshot.Value())

	// A fresh navigator can be moved onto the snapshot position.
	fresh := newXMLNSNavigator(doc)
	if !fresh.MoveTo(snapshot) {
		t.Fatal("MoveTo onto a namespace node failed")
	}
	assertEqual(t, NamespaceNode, fresh.NodeType())
	assertEqual(t, "p", fresh.LocalName())
}

func TestNamespaceAxisUndeclareAndRedeclare(t *testing.T) {
	// A prefix declared on an ancestor, undeclared on a descendant with an
	// empty URI, and re-declared deeper down must resolve to the newest
	// binding in scope.
	src := `<a xmlns:p="urn:1"><b xmlns:p=""><c xmlns:p="urn:2"><d/></c></b></a>`
	doc := parseXML(t, src)

	assertEqual(t, 0, len(collectNamespaces(t, doc, `//b/namespace::p`)))
	assertEqual(t, []nsPair{
		{"p", "urn:2"},
		{"xml", xmlNamespaceURI},
	}, collectNamespaces(t, doc, `//d/namespace::*`))
}
