package xpath

import (
	"sort"
	"strings"
	"testing"
)

// patternTestNode identifies an element or attribute node in a test tree.
// Elements are identified by their *TNode pointer; attributes by the owning
// element pointer together with the attribute index.
type patternTestNode struct {
	element *TNode
	attr    int // -1 for elements
}

func patternNodeOf(nav *TNodeNavigator) patternTestNode {
	return patternTestNode{element: nav.curr, attr: nav.attr}
}

func (n patternTestNode) navigator(root *TNode) *TNodeNavigator {
	return &TNodeNavigator{curr: n.element, root: root, attr: n.attr}
}

// patternNodeLabel renders an element-or-attribute key for diagnostics.
func patternNodeLabel(n patternTestNode) string {
	if n.attr != -1 {
		return elementPath(n.element) + "/@" + n.element.Attr[n.attr].Key
	}
	return elementPath(n.element)
}

func elementPath(n *TNode) string {
	var parts []string
	for node := n; node != nil && node.Type != RootNode; node = node.Parent {
		parts = append(parts, node.Data)
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return "/" + strings.Join(parts, "/")
}

// buildPatternTestTree builds:
//
//	<root lang="en" type="doc">
//	  <a>
//	    <b><c>x</c><c>y</c><d/></b>
//	    <b><c>z</c><d/></b>
//	    <d id="only"/>
//	  </a>
//	  <a>
//	    <b><c>q</c><ns:e xmlns:ns="urn:x"/></b>
//	    <d/>
//	  </a>
//	</root>
func buildPatternTestTree() *TNode {
	doc := createNode("", RootNode)
	root := doc.createChildNode("root", ElementNode)
	root.addAttribute("lang", "en")
	root.addAttribute("type", "doc")

	a1 := addElement(root, "a")
	b1 := addElement(a1, "b")
	addElement(b1, "c").createChildNode("x", TextNode)
	addElement(b1, "c").createChildNode("y", TextNode)
	addElement(b1, "d")
	b2 := addElement(a1, "b")
	addElement(b2, "c").createChildNode("z", TextNode)
	addElement(b2, "d")
	d1 := addElement(a1, "d")
	d1.addAttribute("id", "only")

	a2 := addElement(root, "a")
	b3 := addElement(a2, "b")
	addElement(b3, "c").createChildNode("q", TextNode)
	ns := addElement(b3, "ns:e")
	ns.NamespaceURL = "urn:x"
	ns.addAttribute("xmlns:ns", "urn:x")
	addElement(a2, "d")
	return doc
}

func addElement(parent *TNode, name string) *TNode {
	return parent.createChildNode(name, ElementNode)
}

// collectPatternCandidates enumerates the document node plus every element
// and attribute node in the tree.
func collectPatternCandidates(root *TNode) []patternTestNode {
	var nodes []patternTestNode
	var walk func(*TNode)
	walk = func(node *TNode) {
		if node.Type == RootNode {
			nodes = append(nodes, patternTestNode{element: node, attr: -1})
		} else if node.Type == ElementNode {
			nodes = append(nodes, patternTestNode{element: node, attr: -1})
			for i := range node.Attr {
				nodes = append(nodes, patternTestNode{element: node, attr: i})
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return nodes
}

// selectReference runs an XPath selection against the document root and
// collects the resulting element/attribute nodes.
func selectReference(t *testing.T, root *TNode, expr string) map[patternTestNode]bool {
	t.Helper()
	compiled, err := Compile(expr)
	if err != nil {
		t.Fatalf("reference compile %q: %v", expr, err)
	}
	iter := compiled.Select(createNavigator(root))
	result := make(map[patternTestNode]bool)
	for iter.MoveNext() {
		nav, ok := iter.Current().(*TNodeNavigator)
		if !ok {
			t.Fatalf("unexpected navigator type for %q", expr)
		}
		result[patternNodeOf(nav)] = true
	}
	return result
}

func matchPatternNodes(t *testing.T, root *TNode, p *Pattern, nodes []patternTestNode) map[patternTestNode]bool {
	t.Helper()
	result := make(map[patternTestNode]bool)
	for _, node := range nodes {
		if p.Match(node.navigator(root)) {
			result[node] = true
		}
	}
	return result
}

func assertNodeSetsEqual(t *testing.T, pattern string, got, want map[patternTestNode]bool) {
	t.Helper()
	var missing, extra []string
	for node := range want {
		if !got[node] {
			missing = append(missing, patternNodeLabel(node))
		}
	}
	for node := range got {
		if !want[node] {
			extra = append(extra, patternNodeLabel(node))
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) != 0 || len(extra) != 0 {
		t.Fatalf("pattern %q mismatch\nmissing: %v\nextra:   %v", pattern, missing, extra)
	}
}

// TestPatternMatchesReference compares CompilePattern/Match results against
// the corresponding XPath selection executed from the document root, for a
// range of patterns covering the declared subset.
func TestPatternMatchesReference(t *testing.T) {
	doc := buildPatternTestTree()
	candidates := collectPatternCandidates(doc)

	// Each entry pairs a pattern with the XPath expression that selects the
	// same nodes when evaluated against the document root.
	cases := []struct {
		pattern   string
		reference string
	}{
		{"c", "//c"},
		{"d", "//d"},
		{"*", "//*"},
		{"a/b", "//a/b"},
		{"/root/a/b/c", "/root/a/b/c"},
		{"a//c", "//a//c"},
		{"//c", "//c"},
		{"//a/d", "//a/d"},
		{"b/c | b/d", "//b/c | //b/d"},
		{"a/b/c | a/d", "//a/b/c | //a/d"},
		{"c | //d", "//c | //d"},
		{"/", "/"},
		{"@lang", "//@lang"},
		{"@*", "//@*"},
		{"//@id", "//@id"},
		{"a/d/@id", "//a/d/@id"},
		{"b/c[1]", "//b/c[1]"},
		{"b/c[2]", "//b/c[2]"},
		{"b/c[last()]", "//b/c[last()]"},
		{"a//c[1]", "//a//c[1]"},
		{"a/d[1]", "//a/d[1]"},
		{"a[1]/b[1]/c[2]", "//a[1]/b[1]/c[2]"},
		{"c[1 and position()=1]", "//c[1 and position()=1]"},
		{"c[last()!=1]", "//c[last()!=1]"},
		{"c[position()>1]", "//c[position()>1]"},
		{"b[position()=last()]", "//b[position()=last()]"},
		{"c[last()-1]", "//c[last()-1]"},
		{"c[1] | d[1]", "//c[1] | //d[1]"},
		{"//ns:e", "//ns:e"},
	}

	for _, tc := range cases {
		p, err := CompilePattern(tc.pattern)
		if err != nil {
			t.Fatalf("CompilePattern(%q) error: %v", tc.pattern, err)
		}
		want := selectReference(t, doc, tc.reference)
		got := matchPatternNodes(t, doc, p, candidates)
		assertNodeSetsEqual(t, tc.pattern, got, want)
	}
}

// TestPatternRejectedEntries verifies that entry forms outside the declared
// subset fail at compile time rather than silently producing wrong matches.
func TestPatternRejectedEntries(t *testing.T) {
	rejected := []string{
		"id('x')",
		"key('k', 'v')",
		"text()",
		"node()",
		"comment()",
		"c/ancestor::a",
		"../a",
		".",
		"$x",
		"(a|b)/c",
		"c[count(preceding-sibling::*)>0]",
		"c[position()=count(../c)]",
		"c[@id='x' and string(@v)='y']",
		"d[@id='only']",
		"d[position()>1 and @id='x']",
		"c[position()=1 or @id]",
		"c[position() > '1']",
		"c[true()]",
		"c//",
		"/a/@b/c",
		"/@x",
	}
	for _, expr := range rejected {
		if _, err := CompilePattern(expr); err == nil {
			t.Fatalf("CompilePattern(%q) expected error, got nil", expr)
		}
	}
}

// TestPatternCompileEdgeCases covers trivial/duplicate compile inputs.
func TestPatternCompileEdgeCases(t *testing.T) {
	if _, err := CompilePattern(""); err == nil {
		t.Fatal("empty pattern must fail to compile")
	}
	p := MustCompilePattern("a|b")
	if p.String() != "a|b" {
		t.Fatalf("unexpected String(): %q", p.String())
	}
	if p.Match(nil) {
		t.Fatal("Match(nil) must be false")
	}
}

// TestPatternRootAndNames verifies the document-root pattern, plain name
// tests and prefixed (namespace) name tests.
func TestPatternRootAndNames(t *testing.T) {
	doc := buildPatternTestTree()
	rootPattern := MustCompilePattern("/")
	if !rootPattern.Match(createNavigator(doc)) {
		t.Fatal("/ must match the document node")
	}
	if rootPattern.Match(createNavigator(doc.LastChild)) {
		t.Fatal("/ must not match the document element")
	}

	candidates := collectPatternCandidates(doc)
	assertNodeSetsEqual(t, "root",
		matchPatternNodes(t, doc, MustCompilePattern("root"), candidates),
		map[patternTestNode]bool{{element: doc.LastChild, attr: -1}: true})
	assertNodeSetsEqual(t, "ns:e",
		matchPatternNodes(t, doc, MustCompilePattern("ns:e"), candidates),
		selectReference(t, doc, "//ns:e"))
}

// TestPatternAttributePredicates checks positional predicates on the
// attribute axis, including that position() and last() reflect the full
// attribute set and do not collapse to 1.
func TestPatternAttributePredicates(t *testing.T) {
	doc := buildPatternTestTree()
	root := doc.LastChild
	lang := patternTestNode{element: root, attr: 0}
	typ := patternTestNode{element: root, attr: 1}

	first := MustCompilePattern("@*[1]")
	if !first.Match(lang.navigator(doc)) || first.Match(typ.navigator(doc)) {
		t.Fatal("@*[1] must match only the first attribute")
	}
	last := MustCompilePattern("@*[last()]")
	if last.Match(lang.navigator(doc)) || !last.Match(typ.navigator(doc)) {
		t.Fatal("@*[last()] must match only the final attribute")
	}
	byName := MustCompilePattern("@type[1]")
	if !byName.Match(typ.navigator(doc)) || byName.Match(lang.navigator(doc)) {
		t.Fatal("@type[1] must match the type attribute only")
	}
}

// TestPatternDoesNotMutateCurrent verifies the documented guarantee that
// Match leaves the passed navigator in place.
func TestPatternDoesNotMutateCurrent(t *testing.T) {
	doc := buildPatternTestTree()
	target := doc.LastChild.LastChild.LastChild // root/a/b
	before := patternNodeOf(createNavigator(target))

	nav := createNavigator(target)
	for _, expr := range []string{"b/c", "//c", "/root/a/b/c", "x", "c[last()]"} {
		p := MustCompilePattern(expr)
		p.Match(nav)
		after := patternNodeOf(nav)
		if after != before {
			t.Fatalf("pattern %q moved the navigator: %v -> %v", expr, before, after)
		}
	}
}

// countingNavigator wraps a TNodeNavigator and counts the tree-walking
// operations performed on every navigator in the Copy chain (copies share
// the counter).
type countingNavigator struct {
	*TNodeNavigator
	counter *navCounter
}

type navCounter struct {
	moveToChild    int
	moveToNext     int
	moveToPrevious int
	moveToParent   int
	moveToFirst    int
	moveToNextAttr int
	moveToRoot     int
}

func newCountingNavigator(n *TNodeNavigator) *countingNavigator {
	return &countingNavigator{TNodeNavigator: n, counter: &navCounter{}}
}

func (n *countingNavigator) Copy() NodeNavigator {
	return &countingNavigator{TNodeNavigator: n.TNodeNavigator.Copy().(*TNodeNavigator), counter: n.counter}
}

func (n *countingNavigator) MoveToChild() bool {
	n.counter.moveToChild++
	return n.TNodeNavigator.MoveToChild()
}

func (n *countingNavigator) MoveToNext() bool {
	n.counter.moveToNext++
	return n.TNodeNavigator.MoveToNext()
}

func (n *countingNavigator) MoveToPrevious() bool {
	n.counter.moveToPrevious++
	return n.TNodeNavigator.MoveToPrevious()
}

func (n *countingNavigator) MoveToParent() bool {
	n.counter.moveToParent++
	return n.TNodeNavigator.MoveToParent()
}

func (n *countingNavigator) MoveToFirst() bool {
	n.counter.moveToFirst++
	return n.TNodeNavigator.MoveToFirst()
}

func (n *countingNavigator) MoveToNextAttribute() bool {
	n.counter.moveToNextAttr++
	return n.TNodeNavigator.MoveToNextAttribute()
}

func (n *countingNavigator) MoveToRoot() {
	n.counter.moveToRoot++
	n.TNodeNavigator.MoveToRoot()
}

// TestPatternDeepChainAccessCount ensures matching against a long ancestor
// chain only walks the ancestors of the candidate: a full-tree search would
// visit every node of the other deep branches.
func TestPatternDeepChainAccessCount(t *testing.T) {
	const depth = 200
	doc := createNode("", RootNode)
	parent := doc.createChildNode("root", ElementNode)
	// Two parallel chains of equal depth. Matching a node at the bottom of
	// the first chain against a first-chain-only pattern must not touch the
	// second chain.
	first := parent
	second := parent.createChildNode("noise", ElementNode)
	for i := 0; i < depth; i++ {
		first = first.createChildNode("level", ElementNode)
		second = second.createChildNode("level", ElementNode)
	}

	pattern := "/root" + strings.Repeat("/level", depth)
	p := MustCompilePattern(pattern)
	nav := newCountingNavigator(createNavigator(first))
	if !p.Match(nav) {
		t.Fatal("deep chain node must match the absolute pattern")
	}
	// Only ancestor ascents are expected (a constant handful per ancestor).
	if nav.counter.moveToChild+nav.counter.moveToNext != 0 {
		t.Fatalf("Match descended into siblings/children: child=%d next=%d",
			nav.counter.moveToChild, nav.counter.moveToNext)
	}
	if nav.counter.moveToParent > depth*3 {
		t.Fatalf("Match performed %d parent moves for depth %d", nav.counter.moveToParent, depth)
	}

	// A leaf of the second chain must not match, still without any descent.
	other := newCountingNavigator(createNavigator(second))
	if p.Match(other) {
		t.Fatal("noise-chain node must not match /root/level/level/level")
	}
	if other.counter.moveToChild+other.counter.moveToNext != 0 {
		t.Fatalf("negative Match descended into siblings/children: child=%d next=%d",
			other.counter.moveToChild, other.counter.moveToNext)
	}
}

// TestPatternWideSiblingAccessCount ensures matching a node among many
// siblings never enumerates the whole document; predicates that only need
// the position (no last()) must not even walk the following siblings.
func TestPatternWideSiblingAccessCount(t *testing.T) {
	const siblings = 500
	doc := createNode("", RootNode)
	root := doc.createChildNode("root", ElementNode)
	// Each item carries a large subtree that a correct Match must never
	// enter; a full-tree-search implementation would visit all of it.
	var middle *TNode
	for i := 0; i < siblings; i++ {
		item := root.createChildNode("item", ElementNode)
		for j := 0; j < 20; j++ {
			deep := item.createChildNode("payload", ElementNode)
			deep.createChildNode("leaf", ElementNode)
		}
		if i == siblings/2 {
			middle = item
		}
	}

	// Pattern without last(): preceding-sibling walk only; zero MoveToNext.
	positional := MustCompilePattern("item[position()=251]")
	nav := newCountingNavigator(createNavigator(middle))
	if !positional.Match(nav) {
		t.Fatal("middle sibling must match item[position()=251]")
	}
	if nav.counter.moveToNext != 0 {
		t.Fatalf("position-only predicate walked %d following siblings", nav.counter.moveToNext)
	}
	if nav.counter.moveToChild != 0 {
		t.Fatalf("position-only predicate descended into %d children", nav.counter.moveToChild)
	}

	// Pattern with last(): a following-sibling walk bounded by the sibling
	// count, still no descent into any payload subtree.
	last := MustCompilePattern("item[last()]")
	firstChild := root.FirstChild
	firstNav := newCountingNavigator(createNavigator(firstChild))
	if last.Match(firstNav) {
		t.Fatal("first sibling must not match item[last()]")
	}
	if firstNav.counter.moveToChild != 0 {
		t.Fatalf("last() predicate descended into %d children of sibling subtrees", firstNav.counter.moveToChild)
	}
	if firstNav.counter.moveToNext > siblings {
		t.Fatalf("last() predicate walked %d siblings for %d total", firstNav.counter.moveToNext, siblings)
	}
}

// TestPatternReusableAndConcurrent verifies one compiled pattern can be
// applied repeatedly from different nodes without cross-call state.
func TestPatternReusableAndConcurrent(t *testing.T) {
	doc := buildPatternTestTree()
	candidates := collectPatternCandidates(doc)
	p := MustCompilePattern("b/c[1] | d[last()]")

	first := matchPatternNodes(t, doc, p, candidates)
	done := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- true }()
			for j := 0; j < 20; j++ {
				matchPatternNodes(t, doc, p, candidates)
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	again := matchPatternNodes(t, doc, p, candidates)
	assertNodeSetsEqual(t, "b/c[1] | d[last()]", again, first)
}

// fallbackNavigator hides the optional IsSameNode method so positional
// attribute predicates resolve through the structural-hash identity path.
type fallbackNavigator struct {
	inner TNodeNavigator
}

func (n *fallbackNavigator) Copy() NodeNavigator {
	copied := n.inner.Copy().(*TNodeNavigator)
	return &fallbackNavigator{inner: *copied}
}

func (n *fallbackNavigator) NodeType() NodeType { return n.inner.NodeType() }
func (n *fallbackNavigator) LocalName() string  { return n.inner.LocalName() }
func (n *fallbackNavigator) Prefix() string     { return n.inner.Prefix() }
func (n *fallbackNavigator) Value() string      { return n.inner.Value() }
func (n *fallbackNavigator) MoveToRoot()        { n.inner.MoveToRoot() }
func (n *fallbackNavigator) MoveToParent() bool { return n.inner.MoveToParent() }
func (n *fallbackNavigator) MoveToNextAttribute() bool {
	return n.inner.MoveToNextAttribute()
}
func (n *fallbackNavigator) MoveToChild() bool    { return n.inner.MoveToChild() }
func (n *fallbackNavigator) MoveToFirst() bool    { return n.inner.MoveToFirst() }
func (n *fallbackNavigator) MoveToNext() bool     { return n.inner.MoveToNext() }
func (n *fallbackNavigator) MoveToPrevious() bool { return n.inner.MoveToPrevious() }
func (n *fallbackNavigator) MoveTo(other NodeNavigator) bool {
	if node, ok := other.(*fallbackNavigator); ok {
		return n.inner.MoveTo(&node.inner)
	}
	return false
}

func TestPatternAttributeIdentityFallback(t *testing.T) {
	doc := buildPatternTestTree()
	root := doc.LastChild

	p := MustCompilePattern("@*[2]")
	second := &fallbackNavigator{inner: TNodeNavigator{curr: root, root: doc, attr: 1}}
	if !p.Match(second) {
		t.Fatal("@*[2] must match the second attribute via hash identity fallback")
	}
	first := &fallbackNavigator{inner: TNodeNavigator{curr: root, root: doc, attr: 0}}
	if p.Match(first) {
		t.Fatal("@*[2] must not match the first attribute")
	}
}
