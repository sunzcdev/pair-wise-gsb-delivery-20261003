package xpath

import (
	"strconv"
	"testing"
)

// buildPatternTree constructs:
//
//	root
//	└─ root-el
//	   ├─ item (a=1, k=A)
//	   │  ├─ child x (twice)
//	   │  └─ deep/nested/item
//	   ├─ item (a=2, k=B)
//	   │  └─ child x (twice)
//	   ├─ other
//	   └─ item (a=3, k=A)
//	      └─ child x (twice)
func buildPatternTree() *TNode {
	doc := createNode("", RootNode)
	rootEl := doc.createChildNode("root-el", ElementNode)
	for i := 1; i <= 3; i++ {
		item := rootEl.createChildNode("item", ElementNode)
		item.addAttribute("a", strconv.Itoa(i))
		if i != 2 {
			item.addAttribute("k", "A")
		} else {
			item.addAttribute("k", "B")
		}
		c1 := item.createChildNode("child", ElementNode)
		c1.createChildNode("x", ElementNode)
		c1.createChildNode("x", ElementNode)
		if i == 1 {
			deep := item.createChildNode("deep", ElementNode)
			nested := deep.createChildNode("nested", ElementNode)
			nested.createChildNode("item", ElementNode)
		}
	}
	rootEl.createChildNode("other", ElementNode)
	return doc
}

// collectAllNodes returns every element and attribute navigator in document
// order, including the document root.
func collectAllNodes(n *TNode) []NodeNavigator {
	var out []NodeNavigator
	var walk func(node *TNode)
	walk = func(node *TNode) {
		nav := createNavigator(node)
		out = append(out, nav.Copy())
		for i := range node.Attr {
			attrNav := createNavigator(node)
			attrNav.attr = i
			out = append(out, attrNav.Copy())
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

// referenceMatch selects nodes with an equivalent XPath and reports membership
// by node identity.
func referenceMatch(t *testing.T, doc *TNode, xpaths []string) map[string]bool {
	t.Helper()
	set := make(map[string]bool)
	for _, x := range xpaths {
		exp, err := Compile(x)
		assertNoErr(t, err)
		iter := exp.Select(createNavigator(doc))
		for iter.MoveNext() {
			set[nodeKey(iter.Current().Copy().(*TNodeNavigator))] = true
		}
	}
	return set
}

// nodeKey returns a stable identity key for the current navigator position.
// Element keys are the sequence of child indices from the document root;
// attribute keys append the attribute index.
func nodeKey(nav NodeNavigator) string {
	n, ok := nav.(*TNodeNavigator)
	if !ok {
		return strconv.FormatUint(getHashCode(nav), 10)
	}
	var chain []string
	cur := n.curr
	for cur != nil && cur.Type != RootNode {
		idx := 0
		for p := cur.PrevSibling; p != nil; p = p.PrevSibling {
			idx++
		}
		chain = append(chain, cur.Data+"#"+strconv.Itoa(idx))
		cur = cur.Parent
	}
	// Reverse to document order.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	key := "/" + joinStrings(chain, "/")
	if n.attr != -1 {
		key += "@attr#" + strconv.Itoa(n.attr)
	}
	return key
}

func joinStrings(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

func TestPatternSubset(t *testing.T) {
	doc := buildPatternTree()

	patterns := []string{
		"item",
		"*",
		"@a",
		"item/@a",
		"/root-el",
		"/root-el/item",
		"item/child",
		"item//item",
		"//item",
		"item | other",
		"item[1]",
		"item[2]",
		"item[last()]",
		"item[position()=2]",
		"item[@k='A']",
		"item[@a>1]",
		"item[@a][2]",
		"item/child/x[2]",
		"item/child/x[last()]",
		"@a[1]",
		"item[@k='A'][2]",
		"item[last()-1]",
		"item[position()!=1]",
		"item | @a",
		"deep//item",
		"other | item/child/x",
	}

	for _, src := range patterns {
		src := src
		t.Run(src, func(t *testing.T) {
			p, err := CompilePattern(src)
			assertNoErr(t, err)

			refs := referenceExpressions(src)
			expected := referenceMatch(t, doc, refs)

			for _, node := range collectAllNodes(doc) {
				got := p.Match(node)
				_, want := expected[nodeKey(node.Copy().(*TNodeNavigator))]
				if got != want {
					t.Fatalf("pattern %q on %s: Match=%v want %v",
						src, nodeDescription(node), got, want)
				}
			}
		})
	}
}

// referenceExpressions returns the XPath expressions that select exactly the
// nodes matched by a (possibly union) relative pattern.
func referenceExpressions(pattern string) []string {
	var branches []string
	splitUnion(pattern, &branches)
	out := make([]string, 0, len(branches))
	for _, b := range branches {
		if len(b) > 0 && b[0] == '/' {
			out = append(out, b)
		} else {
			out = append(out, "//"+b)
		}
	}
	return out
}

func splitUnion(s string, out *[]string) {
	// Patterns in this test set do not use '|' inside predicates; a top-level
	// split is sufficient for the reference.
	depth := 0
	quote := rune(0)
	start := 0
	for i, r := range s {
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		case '[', '(':
			depth++
		case ']', ')':
			depth--
		case '|':
			if depth == 0 {
				*out = append(*out, s[start:i])
				start = i + len("|")
			}
		}
	}
	*out = append(*out, s[start:])
}

func nodeDescription(n NodeNavigator) string {
	switch n.NodeType() {
	case RootNode:
		return "/"
	case AttributeNode:
		return "@" + n.LocalName()
	default:
		return n.LocalName()
	}
}

func TestPatternCompileErrors(t *testing.T) {
	bad := []string{
		"id('x')",
		"key('k','v')/item",
		"document('x')",
		"item[id('x')]",
		".",
		"..",
		"item/..",
		"text()",
		"item/text()",
		"comment()",
		"item/ancestor::x",
		"item/parent::x",
		"(item)",
		"$x",
		"'literal'",
		"1+1",
	}
	for _, src := range bad {
		if _, err := CompilePattern(src); err == nil {
			t.Fatalf("expected compile error for %q, got nil", src)
		}
	}
}

func TestPatternRoot(t *testing.T) {
	doc := buildPatternTree()
	p, err := CompilePattern("/")
	assertNoErr(t, err)
	assertTrue(t, p.Match(createNavigator(doc)))
	assertFalse(t, p.Match(createNavigator(doc.FirstChild)))
}

func TestPatternMatchDoesNotMoveNavigator(t *testing.T) {
	doc := buildPatternTree()
	item := doc.FirstChild.FirstChild // root-el/item[1]
	nav := createNavigator(item)

	p, err := CompilePattern("item[2]")
	assertNoErr(t, err)
	beforeNode, beforeAttr := nav.curr, nav.attr
	_ = p.Match(nav)
	assertEqual(t, beforeNode, nav.curr)
	assertEqual(t, beforeAttr, nav.attr)
}

// countingNavigator wraps a TNodeNavigator and records navigation calls so the
// tests can prove Match never walks unrelated subtrees.
type countingNavigator struct {
	NodeNavigator
	moves map[string]int
}

func newCountingNavigator(inner NodeNavigator) *countingNavigator {
	return &countingNavigator{NodeNavigator: inner, moves: make(map[string]int)}
}

func (c *countingNavigator) Copy() NodeNavigator {
	return &countingNavigator{NodeNavigator: c.NodeNavigator.Copy(), moves: c.moves}
}

func (c *countingNavigator) MoveToChild() bool {
	c.moves["MoveToChild"]++
	return c.NodeNavigator.MoveToChild()
}

func (c *countingNavigator) MoveToNext() bool {
	c.moves["MoveToNext"]++
	return c.NodeNavigator.MoveToNext()
}

func (c *countingNavigator) MoveToPrevious() bool {
	c.moves["MoveToPrevious"]++
	return c.NodeNavigator.MoveToPrevious()
}

func (c *countingNavigator) MoveToParent() bool {
	c.moves["MoveToParent"]++
	return c.NodeNavigator.MoveToParent()
}

func (c *countingNavigator) MoveToFirst() bool {
	c.moves["MoveToFirst"]++
	return c.NodeNavigator.MoveToFirst()
}

// buildDeepChain builds root > level1 > ... > levelN, with target being the
// deepest element.
func buildDeepChain(n int) (*TNode, *TNode) {
	doc := createNode("", RootNode)
	cur := doc.createChildNode("level", ElementNode)
	for i := 1; i < n; i++ {
		cur = cur.createChildNode("level", ElementNode)
	}
	return doc, cur
}

// buildWideTree builds root > parent > item{1..n}, each item carrying one
// child, so a full-tree search is clearly distinguishable.
func buildWideTree(n int) (*TNode, *TNode) {
	doc := createNode("", RootNode)
	parent := doc.createChildNode("parent", ElementNode)
	var middle *TNode
	for i := 0; i < n; i++ {
		item := parent.createChildNode("item", ElementNode)
		item.createChildNode("leaf", ElementNode)
		if i == n/2 {
			middle = item
		}
	}
	return doc, middle
}

func TestPatternMatchDeepChainAccessCount(t *testing.T) {
	const depth = 500
	_, target := buildDeepChain(depth)

	p, err := CompilePattern("//level")
	assertNoErr(t, err)

	counter := newCountingNavigator(createNavigator(target))
	if !p.Match(counter) {
		t.Fatal("expected deepest level to match //level")
	}

	// The ancestor chain is O(depth); crucially MoveToChild is never used, so
	// no subtree is explored.
	if got := counter.moves["MoveToChild"]; got != 0 {
		t.Fatalf("Match must not descend into subtrees, MoveToChild=%d", got)
	}
	if got := counter.moves["MoveToParent"]; got > depth+1 {
		t.Fatalf("ancestor walk exceeded chain length: %d > %d", got, depth+1)
	}
}

func TestPatternMatchWideSiblingAccessCount(t *testing.T) {
	const n = 2000
	doc, middle := buildWideTree(n)

	p, err := CompilePattern("parent/item[last()]")
	assertNoErr(t, err)

	// Matching the middle item must not require walking children of every
	// sibling, and must never descend into their subtrees.
	counter := newCountingNavigator(createNavigator(middle))
	assertFalse(t, p.Match(counter))
	// One MoveToChild positions on the parent's first child; it never
	// descends into an item's own subtree.
	if got := counter.moves["MoveToChild"]; got != 1 {
		t.Fatalf("expected a single sibling-walk MoveToChild, got %d", got)
	}
	// Sibling scanning is bounded by the number of siblings, independent of
	// the subtrees hanging off them (each item carries its own leaf here).
	if got := counter.moves["MoveToNext"]; got > n {
		t.Fatalf("sibling scan exceeded sibling count: %d > %d", got, n)
	}
	// The matcher never enumerates preceding siblings via MoveToFirst for the
	// child axis (document-order enumeration only).
	if got := counter.moves["MoveToFirst"]; got != 0 {
		t.Fatalf("unexpected MoveToFirst: %d", got)
	}

	// last() item matches: still no subtree descent.
	last := doc.FirstChild.LastChild
	counter2 := newCountingNavigator(createNavigator(last))
	assertTrue(t, p.Match(counter2))
	if got := counter2.moves["MoveToChild"]; got != 1 {
		t.Fatalf("MoveToChild=%d, expected 1", got)
	}

	// A pattern without predicates on a deep chain does not even scan siblings:
	// assert MoveToNext stays zero for a pure ancestor walk.
	q, err := CompilePattern("//item")
	assertNoErr(t, err)
	counter3 := newCountingNavigator(createNavigator(middle))
	assertTrue(t, q.Match(counter3))
	if got := counter3.moves["MoveToNext"]; got != 0 {
		t.Fatalf("predicate-free match scanned siblings, MoveToNext=%d", got)
	}
	if got := counter3.moves["MoveToChild"]; got != 0 {
		t.Fatalf("MoveToChild=%d, expected 0", got)
	}
}
