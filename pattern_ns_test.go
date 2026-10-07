package xpath

import "testing"

func buildNamespacePatternTree() *TNode {
	doc := createNode("", RootNode)
	root := doc.createChildNode("root", ElementNode)
	a := root.createChildNode("a:thing", ElementNode)
	a.addAttribute("xmlns:a", "urn:a")
	b := root.createChildNode("b:thing", ElementNode)
	b.addAttribute("xmlns:b", "urn:b")
	c := root.createChildNode("plain", ElementNode)
	c.addAttribute("a:id", "1")
	return doc
}

func TestPatternNamespaceAndWildcard(t *testing.T) {
	doc := buildNamespacePatternTree()

	// Without a binding, prefix:* matches on the literal prefix name.
	literal, err := CompilePattern("a:*")
	assertNoErr(t, err)
	assertTrue(t, literal.Match(createNavigator(doc.FirstChild.FirstChild)))
	assertFalse(t, literal.Match(createNavigator(doc.FirstChild.FirstChild.NextSibling)))

	// With a namespace binding, things in urn:a match but urn:b do not.
	bound, err := CompilePatternWithNS("x:thing", map[string]string{"x": "urn:a"})
	assertNoErr(t, err)
	assertTrue(t, bound.Match(createNavigator(doc.FirstChild.FirstChild)))
	assertFalse(t, bound.Match(createNavigator(doc.FirstChild.FirstChild.NextSibling)))

	// Bare wildcard matches every element in the ancestor walk.
	star, err := CompilePattern("*")
	assertNoErr(t, err)
	assertTrue(t, star.Match(createNavigator(doc.FirstChild.LastChild)))
}

func TestPatternMultipleMatchNoMutation(t *testing.T) {
	doc := buildPatternTree()
	p, err := CompilePattern("item[2]")
	assertNoErr(t, err)

	// Repeated Match calls against different nodes must stay independent,
	// since predicates carry a shared positional context per compiled step.
	items := doc.FirstChild
	first := items.FirstChild
	second := first.NextSibling

	assertFalse(t, p.Match(createNavigator(first)))
	assertTrue(t, p.Match(createNavigator(second)))
	assertFalse(t, p.Match(createNavigator(first)))
	assertTrue(t, p.Match(createNavigator(second)))
}
