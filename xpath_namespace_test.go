package xpath

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"testing"
)

// nsTestNode is a minimal XML tree node for namespace axis tests. xmlns
// declarations written on an element are kept in decls, separate from
// ordinary attributes.
type nsTestNode struct {
	parent, firstChild, lastChild, prevSibling, nextSibling *nsTestNode

	typ    NodeType
	name   string // local name for elements, character data for text nodes
	prefix string
	decls  []NamespaceBinding
}

func (n *nsTestNode) createChild(name string, typ NodeType, decls ...NamespaceBinding) *nsTestNode {
	m := &nsTestNode{typ: typ, name: name, parent: n, decls: decls}
	if n.firstChild == nil {
		n.firstChild = m
	} else {
		n.lastChild.nextSibling = m
		m.prevSibling = n.lastChild
	}
	n.lastChild = m
	return m
}

// nsTestNavigator navigates a nsTestNode tree and implements
// NamespaceScopeNavigator, resolving ancestor inheritance, prefix
// shadowing and default-namespace undeclaration.
type nsTestNavigator struct {
	curr, root *nsTestNode
}

func createNSNavigator(n *nsTestNode) *nsTestNavigator {
	return &nsTestNavigator{curr: n, root: n}
}

func (n *nsTestNavigator) NodeType() NodeType { return n.curr.typ }

func (n *nsTestNavigator) LocalName() string { return n.curr.name }

func (n *nsTestNavigator) Prefix() string { return n.curr.prefix }

func (n *nsTestNavigator) Value() string {
	if n.curr.typ == TextNode {
		return n.curr.name
	}
	var buf bytes.Buffer
	var walk func(e *nsTestNode)
	walk = func(e *nsTestNode) {
		if e.typ == TextNode {
			buf.WriteString(e.name)
		}
		for c := e.firstChild; c != nil; c = c.nextSibling {
			walk(c)
		}
	}
	walk(n.curr)
	return buf.String()
}

func (n *nsTestNavigator) Copy() NodeNavigator {
	m := *n
	return &m
}

func (n *nsTestNavigator) MoveToRoot() { n.curr = n.root }

func (n *nsTestNavigator) MoveToParent() bool {
	if p := n.curr.parent; p != nil {
		n.curr = p
		return true
	}
	return false
}

func (n *nsTestNavigator) MoveToNextAttribute() bool { return false }

func (n *nsTestNavigator) MoveToChild() bool {
	if c := n.curr.firstChild; c != nil {
		n.curr = c
		return true
	}
	return false
}

func (n *nsTestNavigator) MoveToFirst() bool {
	if n.curr.prevSibling == nil {
		return false
	}
	for n.curr.prevSibling != nil {
		n.curr = n.curr.prevSibling
	}
	return true
}

func (n *nsTestNavigator) MoveToNext() bool {
	if s := n.curr.nextSibling; s != nil {
		n.curr = s
		return true
	}
	return false
}

func (n *nsTestNavigator) MoveToPrevious() bool {
	if s := n.curr.prevSibling; s != nil {
		n.curr = s
		return true
	}
	return false
}

func (n *nsTestNavigator) MoveTo(other NodeNavigator) bool {
	o, ok := other.(*nsTestNavigator)
	if !ok || o.root != n.root {
		return false
	}
	n.curr = o.curr
	return true
}

// InScopeNamespaces merges the xmlns declarations of every element from
// the outermost ancestor down to the current element: inner declarations
// shadow outer ones for the same prefix, and an empty URI undeclares the
// binding (default-namespace cancellation).
func (n *nsTestNavigator) InScopeNamespaces() []NamespaceBinding {
	if n.curr.typ != ElementNode {
		return nil
	}
	var chain []*nsTestNode
	for e := n.curr; e != nil && e.typ == ElementNode; e = e.parent {
		chain = append(chain, e)
	}
	var out []NamespaceBinding
	index := make(map[string]int)
	for i := len(chain) - 1; i >= 0; i-- {
		for _, d := range chain[i].decls {
			if pos, ok := index[d.Prefix]; ok {
				if d.URI == "" {
					out = append(out[:pos], out[pos+1:]...)
					index = make(map[string]int)
					for j, b := range out {
						index[b.Prefix] = j
					}
				} else {
					out[pos].URI = d.URI
				}
			} else if d.URI != "" {
				index[d.Prefix] = len(out)
				out = append(out, d)
			}
		}
	}
	return out
}

func (n *nsTestNavigator) NamespaceNodeID(prefix string) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%p\x00%s", n.curr, prefix)
	return h.Sum64()
}

// createNamespaceExample builds a document with three levels of prefix
// redefinition and a default namespace that is cancelled mid-tree:
//
//	<root xmlns="urn:default-1" xmlns:p="urn:p1" xmlns:q="urn:q">
//	  <mid xmlns="urn:default-2" xmlns:p="urn:p2">
//	    <leaf xmlns="" xmlns:p="urn:p3">
//	      <item/>
//	    </leaf>
//	  </mid>
//	</root>
func createNamespaceExample() (*nsTestNode, map[string]*nsTestNode) {
	doc := &nsTestNode{typ: RootNode}
	root := doc.createChild("root", ElementNode,
		NamespaceBinding{Prefix: "", URI: "urn:default-1"},
		NamespaceBinding{Prefix: "p", URI: "urn:p1"},
		NamespaceBinding{Prefix: "q", URI: "urn:q"},
	)
	mid := root.createChild("mid", ElementNode,
		NamespaceBinding{Prefix: "", URI: "urn:default-2"},
		NamespaceBinding{Prefix: "p", URI: "urn:p2"},
	)
	leaf := mid.createChild("leaf", ElementNode,
		NamespaceBinding{Prefix: "", URI: ""},
		NamespaceBinding{Prefix: "p", URI: "urn:p3"},
	)
	item := leaf.createChild("item", ElementNode)
	item.createChild("text", TextNode)
	return doc, map[string]*nsTestNode{
		"root": root, "mid": mid, "leaf": leaf, "item": item,
	}
}

func selectNSNodes(t *testing.T, root *nsTestNode, expr string) []NodeNavigator {
	t.Helper()
	exp, err := Compile(expr)
	if err != nil {
		t.Fatalf("Compile(%q) failed: %v", expr, err)
	}
	iter := exp.Select(createNSNavigator(root))
	var nodes []NodeNavigator
	for iter.MoveNext() {
		nodes = append(nodes, iter.Current().Copy())
	}
	return nodes
}

func evalNS(t *testing.T, root *nsTestNode, expr string) interface{} {
	t.Helper()
	exp, err := Compile(expr)
	if err != nil {
		t.Fatalf("Compile(%q) failed: %v", expr, err)
	}
	return exp.Evaluate(createNSNavigator(root))
}

func TestNamespaceAxisInScopeBindings(t *testing.T) {
	doc, _ := createNamespaceExample()

	// item inherits p (redefined at leaf), q (from root) and the implicit
	// xml binding; the default namespace was undeclared at leaf.
	assertEqual(t, float64(3), evalNS(t, doc, "count(//item/namespace::*)"))
	// leaf itself: p, q, xml (default undeclared on leaf).
	assertEqual(t, float64(3), evalNS(t, doc, "count(//leaf/namespace::*)"))
	// mid: default, p, q, xml.
	assertEqual(t, float64(4), evalNS(t, doc, "count(//mid/namespace::*)"))
	// root: default, p, q, xml.
	assertEqual(t, float64(4), evalNS(t, doc, "count(//root/namespace::*)"))

	// The namespace axis of a non-element node is empty.
	assertEqual(t, float64(0), evalNS(t, doc, "count(//item/text()/namespace::*)"))
}

func TestNamespaceAxisPrefixShadowing(t *testing.T) {
	doc, _ := createNamespaceExample()

	// The p prefix is redefined on three levels; each element must see the
	// innermost binding, not the one written on itself or the outermost.
	assertEqual(t, "urn:p1", evalNS(t, doc, "string(//root/namespace::p)"))
	assertEqual(t, "urn:p2", evalNS(t, doc, "string(//mid/namespace::p)"))
	assertEqual(t, "urn:p3", evalNS(t, doc, "string(//leaf/namespace::p)"))
	assertEqual(t, "urn:p3", evalNS(t, doc, "string(//item/namespace::p)"))
	// q is only declared on root and inherited all the way down.
	assertEqual(t, "urn:q", evalNS(t, doc, "string(//item/namespace::q)"))
}

func TestNamespaceAxisDefaultUndeclaration(t *testing.T) {
	doc, _ := createNamespaceExample()

	// leaf cancels the default namespace with xmlns="", so neither leaf
	// nor item have a default-namespace node.
	assertEqual(t, float64(0), evalNS(t, doc, "count(//leaf/namespace::*[name()=''])"))
	assertEqual(t, float64(0), evalNS(t, doc, "count(//item/namespace::*[name()=''])"))
	// mid redefines the default namespace.
	assertEqual(t, float64(1), evalNS(t, doc, "count(//mid/namespace::*[name()=''])"))
	assertEqual(t, "urn:default-2", evalNS(t, doc, "string(//mid/namespace::*[name()=''])"))
	assertEqual(t, "urn:default-1", evalNS(t, doc, "string(//root/namespace::*[name()=''])"))
}

func TestNamespaceAxisXMLPrefix(t *testing.T) {
	doc, _ := createNamespaceExample()

	// The xml prefix is implicitly in scope on every element even though
	// the adapter never declares it.
	for _, expr := range []string{
		"string(//root/namespace::xml)",
		"string(//mid/namespace::xml)",
		"string(//leaf/namespace::xml)",
		"string(//item/namespace::xml)",
	} {
		assertEqual(t, "http://www.w3.org/XML/1998/namespace", evalNS(t, doc, expr))
	}
}

func TestNamespaceNodeNameAndValue(t *testing.T) {
	doc, _ := createNamespaceExample()

	assertEqual(t, "p", evalNS(t, doc, "name(//item/namespace::p)"))
	assertEqual(t, "p", evalNS(t, doc, "local-name(//item/namespace::p)"))
	assertEqual(t, "urn:p3", evalNS(t, doc, "string(//item/namespace::p)"))
	// A namespace node has no namespace URI of its own.
	assertEqual(t, "", evalNS(t, doc, "namespace-uri(//item/namespace::p)"))

	nodes := selectNSNodes(t, doc, "//item/namespace::*")
	assertEqual(t, 3, len(nodes))
	got := make(map[string]string)
	for _, n := range nodes {
		assertEqual(t, NamespaceNode, n.NodeType())
		got[n.LocalName()] = n.Value()
	}
	want := map[string]string{
		"p":   "urn:p3",
		"q":   "urn:q",
		"xml": "http://www.w3.org/XML/1998/namespace",
	}
	assertEqual(t, fmt.Sprintf("%v", want), fmt.Sprintf("%v", got))
}

func TestNamespaceNodeParent(t *testing.T) {
	doc, elems := createNamespaceExample()

	// The parent of a namespace node is the element the axis was
	// evaluated on, not the ancestor that declared the binding: q is
	// declared on root but in scope for item.
	assertEqual(t, "item", evalNS(t, doc, "local-name(//item/namespace::q/..)"))
	assertEqual(t, "item", evalNS(t, doc, "local-name(//item/namespace::q/parent::*)"))
	assertEqual(t, "leaf", evalNS(t, doc, "local-name(//item/namespace::p/../..)"))
	// Ancestors of the namespace node are the element itself and its
	// ancestors: item, leaf, mid and root.
	assertEqual(t, float64(4), evalNS(t, doc, "count(//item/namespace::q/ancestor::*)"))

	// The parent element node compares identical to the element itself.
	parent := selectNSNodes(t, doc, "//item/namespace::q/..")
	assertEqual(t, 1, len(parent))
	assertEqual(t, ElementNode, parent[0].NodeType())
	item := &nsTestNavigator{curr: elems["item"], root: doc}
	assertTrue(t, parent[0].MoveTo(item))
}

func TestNamespaceAxisDeduplication(t *testing.T) {
	doc, _ := createNamespaceExample()

	// Union with itself must not duplicate namespace nodes.
	assertEqual(t, float64(3), evalNS(t, doc, "count(//item/namespace::* | //item/namespace::*)"))
	// The same prefix on different elements yields distinct nodes.
	assertEqual(t, float64(3), evalNS(t, doc, "count(//mid//namespace::p)"))
	// ...but the item's p node is not duplicated by the union.
	assertEqual(t, float64(3), evalNS(t, doc, "count(//mid//namespace::p | //item/namespace::p)"))
}

func TestNamespaceAxisPositionalPredicate(t *testing.T) {
	doc, _ := createNamespaceExample()

	// Positions apply per context element: the first namespace node of
	// every element in document order.
	nodes := selectNSNodes(t, doc, "//mid//namespace::*[1]")
	assertEqual(t, 3, len(nodes)) // mid, leaf and item each contribute one
	for _, n := range nodes {
		assertEqual(t, NamespaceNode, n.NodeType())
	}
}

func TestNamespaceAxisUnsupportedNavigator(t *testing.T) {
	// TNodeNavigator does not implement NamespaceScopeNavigator: querying
	// the namespace axis compiles fine and yields a well-defined empty
	// node-set instead of failing.
	test_xpath_eval(t, book_example, "count(//book/namespace::*)", float64(0))
	test_xpath_count(t, book_example, "//book/namespace::*", 0)
	test_xpath_count(t, book_example, "//book/namespace::p", 0)
}
