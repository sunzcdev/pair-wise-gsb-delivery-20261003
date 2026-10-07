package jsonquery

import "fmt"

// A Cursor is a persistent, versioned position inside a JSON document tree.
//
// A cursor never mutates the nodes it was created from. Every edit operation
// (SetValue, Rename, InsertChild, Remove) performs path-copying: only the
// nodes along the path from the root to the edited position (plus their
// direct children, so that Parent/PrevSibling/NextSibling relations stay
// consistent within the new version) are cloned. Unchanged subtrees are
// shared between versions, so deriving a new branch costs O(depth*width)
// instead of copying the whole document. The old root and any other cursors
// remain valid and keep seeing the old version.
//
// Cursor navigation (Parent, Child, Next, Prev) is driven by the cursor's
// own path, so it always reflects the version the cursor belongs to even
// though content nodes may be shared with other versions.
//
// Because shared nodes keep the Parent pointers of the version they were
// created in, the package-level Find/Query functions must only be used on
// trees that were never branched. To query a version produced by cursor
// edits, use the cursor's own Find/FindOne/Query/QueryAll methods, which
// resolve parent links against the correct version.
type Cursor struct {
	root    *Node
	parents map[*Node]*Node // version-specific parent overrides for shared nodes
	path    []*Node         // path[0] is the root, path[len-1] is the current node
}

// NewCursor creates a cursor positioned at the root of the document.
func NewCursor(root *Node) *Cursor {
	return &Cursor{root: root, path: []*Node{root}}
}

// Clone returns an independent cursor at the same position and version.
// Edits made through one cursor never affect its clones.
func (c *Cursor) Clone() *Cursor {
	path := make([]*Node, len(c.path))
	copy(path, c.path)
	return &Cursor{root: c.root, parents: c.parents, path: path}
}

// Find is like the package-level Find but evaluates expr against the
// version this cursor belongs to, starting from its root.
func (c *Cursor) Find(expr string) []*Node {
	nodes, err := c.QueryAll(expr)
	if err != nil {
		panic(err)
	}
	return nodes
}

// FindOne is like the package-level FindOne but evaluates expr against the
// version this cursor belongs to, starting from its root.
func (c *Cursor) FindOne(expr string) *Node {
	node, err := c.Query(expr)
	if err != nil {
		panic(err)
	}
	return node
}

// QueryAll searches the cursor's version for nodes matching expr,
// starting from the version root.
func (c *Cursor) QueryAll(expr string) ([]*Node, error) {
	exp, err := getQuery(expr)
	if err != nil {
		return nil, err
	}
	t := exp.Select(&NodeNavigator{cur: c.root, root: c.root, parents: c.parents})
	var elems []*Node
	for t.MoveNext() {
		elems = append(elems, (t.Current().(*NodeNavigator)).cur)
	}
	return elems, nil
}

// Query is like QueryAll but returns the first matched node.
func (c *Cursor) Query(expr string) (*Node, error) {
	exp, err := getQuery(expr)
	if err != nil {
		return nil, err
	}
	t := exp.Select(&NodeNavigator{cur: c.root, root: c.root, parents: c.parents})
	if t.MoveNext() {
		return (t.Current().(*NodeNavigator)).cur, nil
	}
	return nil, nil
}

// Root returns the root node of the version this cursor currently sees.
func (c *Cursor) Root() *Node {
	return c.root
}

// Node returns the node the cursor is currently positioned at.
func (c *Cursor) Node() *Node {
	return c.path[len(c.path)-1]
}

// Path returns the chain of nodes from the root down to the current node.
func (c *Cursor) Path() []*Node {
	path := make([]*Node, len(c.path))
	copy(path, c.path)
	return path
}

// Depth returns the distance of the current node from the root.
func (c *Cursor) Depth() int {
	return len(c.path) - 1
}

// Index returns the position of the current node among its siblings,
// or 0 when the cursor is at the root.
func (c *Cursor) Index() int {
	if len(c.path) < 2 {
		return 0
	}
	return childIndex(c.path[len(c.path)-2], c.Node())
}

// Parent moves the cursor to the parent of the current node.
// It returns false if the cursor is already at the root.
func (c *Cursor) Parent() bool {
	if len(c.path) < 2 {
		return false
	}
	c.path = c.path[:len(c.path)-1]
	return true
}

// Child moves the cursor to the i-th child of the current node.
func (c *Cursor) Child(i int) bool {
	children := c.Node().ChildNodes()
	if i < 0 || i >= len(children) {
		return false
	}
	c.path = append(c.path, children[i])
	return true
}

// ChildElement moves the cursor to the first child element with the
// specified name.
func (c *Cursor) ChildElement(name string) bool {
	for _, child := range c.Node().ChildNodes() {
		if child.Data == name {
			c.path = append(c.path, child)
			return true
		}
	}
	return false
}

// Next moves the cursor to the next sibling of the current node.
func (c *Cursor) Next() bool {
	return c.moveSibling(1)
}

// Prev moves the cursor to the previous sibling of the current node.
func (c *Cursor) Prev() bool {
	return c.moveSibling(-1)
}

func (c *Cursor) moveSibling(offset int) bool {
	if len(c.path) < 2 {
		return false
	}
	parent := c.path[len(c.path)-2]
	children := parent.ChildNodes()
	i := childIndex(parent, c.Node()) + offset
	if i < 0 || i >= len(children) {
		return false
	}
	c.path[len(c.path)-1] = children[i]
	return true
}

// SetValue replaces the value of the current node with v and returns the
// root of the new version. The cursor keeps pointing at the replaced node
// in the new version; the old version is left untouched.
func (c *Cursor) SetValue(v interface{}) *Node {
	if len(c.path) == 1 {
		doc := &Node{Type: DocumentNode}
		parseValue(v, doc, 1)
		c.root = doc
		c.parents = nil
		c.path = []*Node{doc}
		return doc
	}
	cur := c.Node()
	n := buildValue(cur.Data, v)
	n.level = cur.level
	d := len(c.path) - 2
	idx := c.Index()
	kids := shallowCloneList(c.path[d].ChildNodes())
	kids[idx] = n
	c.rebuild(d, kids, idx)
	return c.root
}

// Rename changes the name (object member key) of the current node and
// returns the root of the new version. The cursor keeps pointing at the
// renamed node, so its path reflects the new key.
func (c *Cursor) Rename(name string) *Node {
	if len(c.path) == 1 {
		return c.root
	}
	n := shallowClone(c.Node())
	n.Data = name
	d := len(c.path) - 2
	idx := c.Index()
	kids := shallowCloneList(c.path[d].ChildNodes())
	kids[idx] = n
	c.rebuild(d, kids, idx)
	return c.root
}

// InsertChild inserts a new array element holding v at position i of the
// current (array) node and returns the root of the new version. The cursor
// stays on the current node with its path updated to the new version.
func (c *Cursor) InsertChild(i int, v interface{}) *Node {
	cur := c.Node()
	kids := shallowCloneList(cur.ChildNodes())
	if i < 0 {
		i = 0
	}
	if i > len(kids) {
		i = len(kids)
	}
	n := buildValue("", v)
	n.level = cur.level + 1
	kids = append(kids, nil)
	copy(kids[i+1:], kids[i:])
	kids[i] = n
	c.rebuild(len(c.path)-1, kids, -1)
	return c.root
}

// Remove deletes the current node from its parent and returns the root of
// the new version. The cursor moves to a deterministic position: the node
// that shifts into the removed index (the former next sibling), or the new
// last sibling when the tail was removed, or the parent when no siblings
// remain. Removing the root is a no-op.
func (c *Cursor) Remove() *Node {
	if len(c.path) == 1 {
		return c.root
	}
	d := len(c.path) - 2
	idx := c.Index()
	orig := c.path[d].ChildNodes()
	kids := make([]*Node, 0, len(orig)-1)
	for i, k := range orig {
		if i != idx {
			kids = append(kids, shallowClone(k))
		}
	}
	cursorIdx := -1
	if len(kids) > 0 {
		cursorIdx = idx
		if cursorIdx >= len(kids) {
			cursorIdx = len(kids) - 1
		}
	}
	c.rebuild(d, kids, cursorIdx)
	return c.root
}

// rebuild replaces the children of the node at path depth d with kids,
// propagating clones up to the root, and updates the cursor to the new
// version. If cursorIdx >= 0 the cursor descends one more level to the
// child at that index of the rebuilt node.
func (c *Cursor) rebuild(d int, kids []*Node, cursorIdx int) {
	idxs := make([]int, len(c.path))
	for i := 1; i < len(c.path); i++ {
		idxs[i] = childIndex(c.path[i-1], c.path[i])
	}
	parents := make(map[*Node]*Node, len(c.parents)+len(kids))
	for k, v := range c.parents {
		parents[k] = v
	}
	child := cloneWithChildren(c.path[d], kids, parents)
	for i := d - 1; i >= 0; i-- {
		parent := c.path[i]
		siblings := shallowCloneList(parent.ChildNodes())
		siblings[idxs[i+1]] = child
		child = cloneWithChildren(parent, siblings, parents)
	}
	c.root = child
	c.parents = parents
	path := []*Node{child}
	node := child
	for i := 1; i <= d; i++ {
		node = node.ChildNodes()[idxs[i]]
		path = append(path, node)
	}
	if cursorIdx >= 0 {
		node = node.ChildNodes()[cursorIdx]
		path = append(path, node)
	}
	c.path = path
}

// shallowClone copies a node without touching the original. The clone keeps
// the value and the subtree (FirstChild/LastChild) of the original, so the
// whole subtree below is shared between versions. Its Parent/Prev/Next
// links are (re)assigned by cloneWithChildren when the clone is linked into
// a new parent, leaving the old version's navigation relations intact.
func shallowClone(n *Node) *Node {
	return &Node{
		Type:       n.Type,
		Data:       n.Data,
		level:      n.level,
		value:      n.value,
		FirstChild: n.FirstChild,
		LastChild:  n.LastChild,
	}
}

func shallowCloneList(nodes []*Node) []*Node {
	clones := make([]*Node, len(nodes))
	for i, n := range nodes {
		clones[i] = shallowClone(n)
	}
	return clones
}

// cloneWithChildren returns a copy of parent whose children are kids, fixing
// all Parent/PrevSibling/NextSibling links among the clones and recomputing
// the parent's Go value from its new children. The children of the clones
// stay shared with the old version, so their Parent pointers still reference
// the old version's nodes; a version-specific override is recorded in
// parents for each of them instead of mutating the shared nodes.
func cloneWithChildren(parent *Node, kids []*Node, parents map[*Node]*Node) *Node {
	p := shallowClone(parent)
	p.FirstChild, p.LastChild = nil, nil
	for i, k := range kids {
		k.Parent = p
		if i == 0 {
			p.FirstChild = k
			k.PrevSibling = nil
		} else {
			kids[i-1].NextSibling = k
			k.PrevSibling = kids[i-1]
		}
		if i == len(kids)-1 {
			p.LastChild = k
			k.NextSibling = nil
		}
		for g := k.FirstChild; g != nil; g = g.NextSibling {
			parents[g] = k
		}
	}
	p.value = deriveValue(parent, kids)
	return p
}

// deriveValue rebuilds the Go value of a cloned parent from its new
// children without mutating the old version's maps or slices.
func deriveValue(parent *Node, kids []*Node) interface{} {
	switch parent.value.(type) {
	case map[string]interface{}:
		m := make(map[string]interface{}, len(kids))
		for _, k := range kids {
			m[k.Data] = k.value
		}
		return m
	case []interface{}:
		s := make([]interface{}, len(kids))
		for i, k := range kids {
			s[i] = k.value
		}
		return s
	default:
		return parent.value
	}
}

// buildValue constructs a fresh subtree for v, using the same node layout
// as Parse: an element node named data whose children represent v.
func buildValue(data string, v interface{}) *Node {
	n := &Node{Data: data, Type: ElementNode, level: 1, value: v}
	parseValue(v, n, 2)
	return n
}

func childIndex(parent, child *Node) int {
	for i, n := range parent.ChildNodes() {
		if n == child {
			return i
		}
	}
	panic(fmt.Sprintf("jsonquery: node %q is not a child of %q", child.Data, parent.Data))
}
