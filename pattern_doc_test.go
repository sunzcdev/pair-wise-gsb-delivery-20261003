package xpath_test

import (
	"fmt"

	"github.com/antchfx/xpath"
)

// patternExampleNode is a minimal tree node used by the external example.
type patternExampleNode struct {
	Parent, FirstChild, LastChild, PrevSibling, NextSibling *patternExampleNode

	Type xpath.NodeType
	Data string
}

func (n *patternExampleNode) add(child *patternExampleNode) *patternExampleNode {
	child.Parent = n
	if n.FirstChild == nil {
		n.FirstChild = child
	} else {
		n.LastChild.NextSibling = child
		child.PrevSibling = n.LastChild
	}
	n.LastChild = child
	return child
}

type patternExampleNavigator struct {
	curr, root *patternExampleNode
}

func (n *patternExampleNavigator) NodeType() xpath.NodeType { return n.curr.Type }
func (n *patternExampleNavigator) LocalName() string        { return n.curr.Data }
func (n *patternExampleNavigator) Prefix() string           { return "" }
func (n *patternExampleNavigator) Value() string            { return "" }
func (n *patternExampleNavigator) Copy() xpath.NodeNavigator {
	copied := *n
	return &copied
}
func (n *patternExampleNavigator) MoveToRoot() { n.curr = n.root }
func (n *patternExampleNavigator) MoveToParent() bool {
	if n.curr.Parent == nil {
		return false
	}
	n.curr = n.curr.Parent
	return true
}
func (n *patternExampleNavigator) MoveToNextAttribute() bool { return false }
func (n *patternExampleNavigator) MoveToChild() bool {
	if n.curr.FirstChild == nil {
		return false
	}
	n.curr = n.curr.FirstChild
	return true
}
func (n *patternExampleNavigator) MoveToFirst() bool {
	for n.curr.PrevSibling != nil {
		n.curr = n.curr.PrevSibling
	}
	return true
}
func (n *patternExampleNavigator) MoveToNext() bool {
	if n.curr.NextSibling == nil {
		return false
	}
	n.curr = n.curr.NextSibling
	return true
}
func (n *patternExampleNavigator) MoveToPrevious() bool {
	if n.curr.PrevSibling == nil {
		return false
	}
	n.curr = n.curr.PrevSibling
	return true
}
func (n *patternExampleNavigator) MoveTo(other xpath.NodeNavigator) bool {
	node, ok := other.(*patternExampleNavigator)
	if !ok {
		return false
	}
	n.curr = node.curr
	return true
}

// ExampleCompilePattern shows compiling an XSLT pattern and testing nodes
// against it without selecting from the document root for every node.
func ExampleCompilePattern() {
	doc := &patternExampleNode{Type: xpath.RootNode}
	book := doc.add(&patternExampleNode{Type: xpath.ElementNode, Data: "book"})
	chapter := book.add(&patternExampleNode{Type: xpath.ElementNode, Data: "chapter"})
	first := chapter.add(&patternExampleNode{Type: xpath.ElementNode, Data: "item"})
	first.add(&patternExampleNode{Type: xpath.TextNode, Data: "first"})
	second := chapter.add(&patternExampleNode{Type: xpath.ElementNode, Data: "item"})
	second.add(&patternExampleNode{Type: xpath.TextNode, Data: "second"})

	p, err := xpath.CompilePattern("book/chapter/item[1]")
	if err != nil {
		fmt.Println("compile error:", err)
		return
	}

	var walk func(*patternExampleNode)
	walk = func(node *patternExampleNode) {
		if node.Type == xpath.ElementNode {
			nav := &patternExampleNavigator{curr: node, root: doc}
			if p.Match(nav) {
				fmt.Println("match:", node.FirstChild.Data)
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)

	if _, err := xpath.CompilePattern("id('x')"); err != nil {
		fmt.Println("rejected: id()")
	}

	// Output:
	// match: first
	// rejected: id()
}
