package jsonquery

import "github.com/antchfx/xpath"

// A View is a restricted, read-only projection of a JSON document tree that
// treats a single node as its virtual root.
//
// The view never cuts, rewires or copies the underlying tree: every result
// node returned by a view query is still the original *Node. Only navigation
// is confined: the parent, ancestor, sibling, following and preceding axes all
// stop at the virtual root, and absolute paths resolve from it, so nodes
// outside the view can never be reached.
//
// Multiple views, even views created over the same node, are independent and
// may be used in parallel; navigators from different views never interact.
type View struct {
	root *Node
}

// NewView creates a restricted View with the specified node as its virtual
// root. A nil node produces a nil view.
func NewView(root *Node) *View {
	if root == nil {
		return nil
	}
	return &View{root: root}
}

// Root returns the original node that serves as the virtual root of the view.
func (v *View) Root() *Node {
	if v == nil {
		return nil
	}
	return v.root
}

// CreateXPathNavigator creates a new xpath.NodeNavigator positioned on the
// view's virtual root. Every axis of the returned navigator stays inside the
// view.
func (v *View) CreateXPathNavigator() *NodeNavigator {
	if v == nil {
		return nil
	}
	return &NodeNavigator{cur: v.root, root: v.root, view: v}
}

// FindAll is like QueryAll but panics if expr cannot be parsed.
func (v *View) FindAll(expr string) []*Node {
	nodes, err := v.QueryAll(expr)
	if err != nil {
		panic(err)
	}
	return nodes
}

// FindOne is like Query but panics if expr cannot be parsed.
func (v *View) FindOne(expr string) *Node {
	node, err := v.Query(expr)
	if err != nil {
		panic(err)
	}
	return node
}

// QueryAll searches the nodes that match the specified XPath expr inside the
// view. The returned nodes are the original nodes of the document tree; only
// the search scope is restricted.
func (v *View) QueryAll(expr string) ([]*Node, error) {
	exp, err := getQuery(expr)
	if err != nil {
		return nil, err
	}
	return v.QuerySelectorAll(exp), nil
}

// Query searches the first node that matches the specified XPath expr inside
// the view and returns it.
func (v *View) Query(expr string) (*Node, error) {
	exp, err := getQuery(expr)
	if err != nil {
		return nil, err
	}
	return v.QuerySelector(exp), nil
}

// QuerySelectorAll searches all of the nodes that match the specified XPath
// selector inside the view.
func (v *View) QuerySelectorAll(selector *xpath.Expr) []*Node {
	if v == nil {
		return nil
	}
	t := selector.Select(v.CreateXPathNavigator())
	var elems []*Node
	for t.MoveNext() {
		elems = append(elems, t.Current().(*NodeNavigator).cur)
	}
	return elems
}

// QuerySelector returns the first node that matches the specified XPath
// selector inside the view.
func (v *View) QuerySelector(selector *xpath.Expr) *Node {
	if v == nil {
		return nil
	}
	t := selector.Select(v.CreateXPathNavigator())
	if t.MoveNext() {
		return t.Current().(*NodeNavigator).cur
	}
	return nil
}
