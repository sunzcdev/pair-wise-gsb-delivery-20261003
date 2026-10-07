package jsonquery

import (
	"github.com/antchfx/xpath"
)

// A View is a restricted XPath scope rooted at a node of an existing
// document. The root node of the view acts as a virtual document root:
// every navigation axis (parent, ancestor, sibling, absolute path, ...)
// stays inside the view, so nodes outside the boundary are unreachable
// and invisible to queries.
//
// The original tree is neither modified nor copied, and nodes returned
// by view queries are the same *Node values as in the original tree.
// Views with different boundaries are independent of each other and may
// be used concurrently.
type View struct {
	root *Node
}

// NewView creates a restricted view using top as its virtual root.
func NewView(top *Node) *View {
	return &View{root: top}
}

// Root returns the virtual root node of the view. It is a node of the
// original tree, not a copy.
func (v *View) Root() *Node {
	return v.root
}

// Navigator creates a new XPath navigator confined to the view.
func (v *View) Navigator() *NodeNavigator {
	return &NodeNavigator{cur: v.root, root: v.root, boundary: v.root}
}

// Find is like QueryAll but will panics if `expr` cannot be parsed.
func (v *View) Find(expr string) []*Node {
	nodes, err := v.QueryAll(expr)
	if err != nil {
		panic(err)
	}
	return nodes
}

// FindOne is like Query but will panics if `expr` cannot be parsed.
func (v *View) FindOne(expr string) *Node {
	node, err := v.Query(expr)
	if err != nil {
		panic(err)
	}
	return node
}

// QueryAll searches the Node that matches by the specified XPath expr,
// restricted to the view. Return an error if the expression `expr`
// cannot be parsed.
func (v *View) QueryAll(expr string) ([]*Node, error) {
	exp, err := getQuery(expr)
	if err != nil {
		return nil, err
	}
	return v.QuerySelectorAll(exp), nil
}

// Query searches the Node that matches by the specified XPath expr,
// restricted to the view, and returns first element of matched.
func (v *View) Query(expr string) (*Node, error) {
	exp, err := getQuery(expr)
	if err != nil {
		return nil, err
	}
	return v.QuerySelector(exp), nil
}

// QuerySelectorAll searches all of the Node that matches the specified
// XPath selectors, restricted to the view.
func (v *View) QuerySelectorAll(selector *xpath.Expr) []*Node {
	return querySelectorAll(v.Navigator(), selector)
}

// QuerySelector returns the first matched Node by the specified XPath
// selector, restricted to the view.
func (v *View) QuerySelector(selector *xpath.Expr) *Node {
	return querySelector(v.Navigator(), selector)
}
