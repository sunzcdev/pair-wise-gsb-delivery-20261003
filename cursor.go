package jsonquery

import (
	"fmt"
	"sort"
	"strings"
)

// A Cursor is a persistent position within a JSON document tree.
//
// Cursors allow multiple independent "branches" of edits to be derived from
// a single parsed snapshot. Navigation (Down, Up, Next, Prev) and edits
// (SetValue, Rename, Replace, InsertChildAt, Remove) never mutate the
// underlying tree: every operation returns a new Cursor value, and Root
// materializes a new document root that shares all untouched subtrees with
// the original snapshot. Old roots and old cursors remain valid and
// queryable at all times.
//
// Only the nodes along the edited path (and their siblings, whose
// Parent/Prev/Next links must belong to the new version) are copied; shared
// content nodes are never rewritten to fix up parent pointers, so the old
// tree is never disturbed.
type Cursor struct {
	node  *Node
	path  *cursorPath
	dirty bool
	// pv maps shared boundary nodes to their parent in this cursor's
	// version. It is created by the first edit and shared (by reference) by
	// every cursor derived from it, and is attached to the root by Root.
	pv map[*Node]*Node
}

// cursorPath is one level of the persistent ancestor stack: the parent node
// and the focus' left/right siblings at the moment the cursor descended.
type cursorPath struct {
	up     *cursorPath
	parent *Node
	lefts  []*Node
	rights []*Node
}

// NewCursor returns a Cursor positioned at the root of the document.
func NewCursor(root *Node) *Cursor {
	return &Cursor{node: root}
}

// Node returns the node the cursor is currently positioned on.
func (c *Cursor) Node() *Node {
	return c.node
}

// Value returns the JSON value of the current node.
func (c *Cursor) Value() interface{} {
	return c.node.Value()
}

// Path returns the deterministic XPath-like location of the cursor, e.g.
// "/store/book/*[2]/price". Array items (which have no member name) are
// addressed by their 1-based position. The path always reflects the cursor's
// own version of the tree, so it tracks renames and array insertions and
// deletions made through this cursor.
func (c *Cursor) Path() string {
	if c.path == nil {
		return "/"
	}
	var segs []string
	for node, p := c.node, c.path; p != nil; node, p = p.parent, p.up {
		if node.Data != "" {
			segs = append(segs, node.Data)
		} else {
			segs = append(segs, fmt.Sprintf("*[%d]", len(p.lefts)+1))
		}
	}
	for i, j := 0, len(segs)-1; i < j; i, j = i+1, j-1 {
		segs[i], segs[j] = segs[j], segs[i]
	}
	return "/" + strings.Join(segs, "/")
}

// Root returns the root node of the cursor's version of the document.
//
// If the cursor (or the cursor it was derived from) performed edits, Root
// rebuilds only the ancestor chain of the edit, producing a new root that
// shares every untouched subtree with the original snapshot. If no edits
// were made, Root returns the original root node unchanged.
func (c *Cursor) Root() *Node {
	if !c.dirty {
		n := c.node
		for p := c.path; p != nil; p = p.up {
			n = p.parent
		}
		return n
	}
	n := c.node
	for p := c.path; p != nil; p = p.up {
		n = rebuildParent(p, n, c.pv)
	}
	n.vparents = c.pv
	return n
}

// Down returns a cursor positioned on the first child of the current node.
func (c *Cursor) Down() (*Cursor, bool) {
	return c.DownIndex(0)
}

// DownIndex returns a cursor positioned on the i-th child of the current
// node. It reports false if the index is out of range.
func (c *Cursor) DownIndex(i int) (*Cursor, bool) {
	children := c.node.ChildNodes()
	if i < 0 || i >= len(children) {
		return nil, false
	}
	return c.childCursor(children, i), true
}

// DownName returns a cursor positioned on the first child element with the
// specified member name. It reports false if there is no such child.
func (c *Cursor) DownName(name string) (*Cursor, bool) {
	children := c.node.ChildNodes()
	for i, ch := range children {
		if ch.Type == ElementNode && ch.Data == name {
			return c.childCursor(children, i), true
		}
	}
	return nil, false
}

func (c *Cursor) childCursor(children []*Node, i int) *Cursor {
	lefts := make([]*Node, i)
	copy(lefts, children[:i])
	rights := make([]*Node, len(children)-i-1)
	copy(rights, children[i+1:])
	return &Cursor{
		node:  children[i],
		path:  &cursorPath{up: c.path, parent: c.node, lefts: lefts, rights: rights},
		dirty: c.dirty,
		pv:    c.pv,
	}
}

// Up returns a cursor positioned on the parent of the current node. It
// reports false if the cursor is already at the root.
func (c *Cursor) Up() (*Cursor, bool) {
	p := c.path
	if p == nil {
		return nil, false
	}
	if !c.dirty {
		return &Cursor{node: p.parent, path: p.up}, true
	}
	return &Cursor{node: rebuildParent(p, c.node, c.pv), path: p.up, dirty: true, pv: c.pv}, true
}

// Next returns a cursor positioned on the next sibling of the current node.
func (c *Cursor) Next() (*Cursor, bool) {
	p := c.path
	if p == nil || len(p.rights) == 0 {
		return nil, false
	}
	lefts := make([]*Node, len(p.lefts)+1)
	copy(lefts, p.lefts)
	lefts[len(p.lefts)] = c.node
	return &Cursor{
		node:  p.rights[0],
		path:  &cursorPath{up: p.up, parent: p.parent, lefts: lefts, rights: p.rights[1:]},
		dirty: c.dirty,
		pv:    c.pv,
	}, true
}

// Prev returns a cursor positioned on the previous sibling of the current
// node.
func (c *Cursor) Prev() (*Cursor, bool) {
	p := c.path
	if p == nil || len(p.lefts) == 0 {
		return nil, false
	}
	rights := make([]*Node, 0, len(p.rights)+1)
	rights = append(rights, c.node)
	rights = append(rights, p.rights...)
	return &Cursor{
		node:  p.lefts[len(p.lefts)-1],
		path:  &cursorPath{up: p.up, parent: p.parent, lefts: p.lefts[:len(p.lefts)-1], rights: rights},
		dirty: c.dirty,
		pv:    c.pv,
	}, true
}

// versioned returns the parent-override map shared by this cursor's edit
// branch, creating one on the branch's first edit. The new map is seeded
// with a copy of the base version's overrides (never the base map itself)
// so that branches derived from an already-versioned root keep resolving
// inherited boundary nodes without polluting the base version.
func (c *Cursor) versioned() map[*Node]*Node {
	if c.pv != nil {
		return c.pv
	}
	pv := make(map[*Node]*Node)
	root := c.node
	for p := c.path; p != nil; p = p.up {
		root = p.parent
	}
	for k, v := range root.vparents {
		pv[k] = v
	}
	return pv
}

// Replace returns a cursor whose current node is n, leaving every other
// cursor and the original tree untouched. The new root is obtained with Root.
func (c *Cursor) Replace(n *Node) *Cursor {
	return &Cursor{node: n, path: c.path, dirty: true, pv: c.versioned()}
}

// SetValue returns a cursor whose current node holds the new JSON value.
// The node's children are regenerated from v using the same shape as Parse:
// maps become object members, slices become array items and scalars become a
// single text child.
func (c *Cursor) SetValue(v interface{}) *Cursor {
	n := &Node{Type: c.node.Type, Data: c.node.Data, level: c.node.level, value: v}
	buildChildren(n, v)
	return c.Replace(n)
}

// Rename returns a cursor whose current node has the new member name. The
// node's value and children are shared with the old node; only the name
// changes. The cursor's Path reflects the new name immediately.
func (c *Cursor) Rename(name string) *Cursor {
	n := *c.node
	n.Data = name
	n.Parent, n.PrevSibling, n.NextSibling = nil, nil, nil
	return c.Replace(&n)
}

// InsertChildAt returns a cursor whose current node has child inserted at
// index i among its children, shifting later children right. Use
// NewElement("", v) to build array items. The cursor stays on the same
// (rebuilt) node; descend again to address children by their new positions.
func (c *Cursor) InsertChildAt(i int, child *Node) (*Cursor, bool) {
	children := c.node.ChildNodes()
	if i < 0 || i > len(children) {
		return nil, false
	}
	merged := make([]*Node, 0, len(children)+1)
	merged = append(merged, children[:i]...)
	merged = append(merged, child)
	merged = append(merged, children[i:]...)
	n := &Node{Type: c.node.Type, Data: c.node.Data, level: c.node.level}
	pv := c.versioned()
	linkChildren(n, merged, pv)
	n.value = valueFromChildren(c.node.value, merged)
	return &Cursor{node: n, path: c.path, dirty: true, pv: pv}, true
}

// AppendChild returns a cursor whose current node has child appended as its
// last child.
func (c *Cursor) AppendChild(child *Node) *Cursor {
	cur, _ := c.InsertChildAt(len(c.node.ChildNodes()), child)
	return cur
}

// Remove deletes the current node from its parent and returns a cursor at a
// deterministic fallback position: the next sibling if there is one,
// otherwise the previous sibling, otherwise the (childless) parent. It
// reports false when positioned on the root, which cannot be removed.
func (c *Cursor) Remove() (*Cursor, bool) {
	p := c.path
	if p == nil {
		return nil, false
	}
	pv := c.versioned()
	switch {
	case len(p.rights) > 0:
		return &Cursor{
			node:  p.rights[0],
			path:  &cursorPath{up: p.up, parent: p.parent, lefts: p.lefts, rights: p.rights[1:]},
			dirty: true,
			pv:    pv,
		}, true
	case len(p.lefts) > 0:
		return &Cursor{
			node:  p.lefts[len(p.lefts)-1],
			path:  &cursorPath{up: p.up, parent: p.parent, lefts: p.lefts[:len(p.lefts)-1]},
			dirty: true,
			pv:    pv,
		}, true
	default:
		parent := &Node{Type: p.parent.Type, Data: p.parent.Data, level: p.parent.level}
		parent.value = valueFromChildren(p.parent.value, nil)
		return &Cursor{node: parent, path: p.up, dirty: true, pv: pv}, true
	}
}

// NewElement builds a new element node (with its subtree) from a Go value,
// using the same shape as Parse. Pass an empty name for array items.
func NewElement(name string, value interface{}) *Node {
	n := &Node{Data: name, Type: ElementNode, value: value}
	buildChildren(n, value)
	return n
}

// rebuildParent creates the new-version parent node for one unwound path
// level: a fresh node whose children are lefts + [node] + rights.
func rebuildParent(p *cursorPath, node *Node, pv map[*Node]*Node) *Node {
	parent := &Node{Type: p.parent.Type, Data: p.parent.Data, level: p.parent.level}
	children := make([]*Node, 0, len(p.lefts)+1+len(p.rights))
	children = append(children, p.lefts...)
	children = append(children, node)
	children = append(children, p.rights...)
	linkChildren(parent, children, pv)
	parent.value = valueFromChildren(p.parent.value, children)
	return parent
}

// linkChildren shallow-copies each of children and links the copies into a
// fresh, consistent Parent/Prev/Next chain under parent (which must itself
// be a new node). Copying the siblings keeps the new version's navigation
// relations self-contained without mutating nodes that may still be shared
// with older versions; the copies share their subtrees with the originals.
//
// The children of each copy stay shared with the original tree, so their
// Parent pointer still refers to the original version; for every such
// boundary node an entry is recorded in pv (when non-nil) so that
// navigators of the new version resolve the correct parent.
func linkChildren(parent *Node, children []*Node, pv map[*Node]*Node) {
	var prev *Node
	for _, orig := range children {
		cp := new(Node)
		*cp = *orig
		cp.Parent = parent
		cp.PrevSibling = prev
		cp.NextSibling = nil
		cp.level = parent.level + 1
		if pv != nil {
			cp.vparents = pv
			for ch := cp.FirstChild; ch != nil; ch = ch.NextSibling {
				pv[ch] = cp
			}
		}
		if prev == nil {
			parent.FirstChild = cp
		} else {
			prev.NextSibling = cp
		}
		prev = cp
	}
	parent.LastChild = prev
}

// valueFromChildren recomputes the JSON value of a rebuilt node from its
// children, preserving the container kind (object vs array vs scalar) of the
// old value. Child values themselves are shared, never deep-copied.
func valueFromChildren(old interface{}, children []*Node) interface{} {
	if _, ok := old.(map[string]interface{}); ok {
		m := make(map[string]interface{}, len(children))
		for _, ch := range children {
			m[ch.Data] = ch.value
		}
		return m
	}
	if _, ok := old.([]interface{}); ok {
		s := make([]interface{}, len(children))
		for i, ch := range children {
			s[i] = ch.value
		}
		return s
	}
	if len(children) == 0 {
		return old
	}
	if children[0].Type == TextNode {
		return children[0].value
	}
	// The old value carried no container type information (e.g. the document
	// node); infer the shape from the children.
	for _, ch := range children {
		if ch.Data != "" {
			m := make(map[string]interface{}, len(children))
			for _, c := range children {
				m[c.Data] = c.value
			}
			return m
		}
	}
	s := make([]interface{}, len(children))
	for i, ch := range children {
		s[i] = ch.value
	}
	return s
}

// buildChildren populates parent with child nodes representing v, mirroring
// the node shapes produced by Parse.
func buildChildren(parent *Node, v interface{}) {
	switch vv := v.(type) {
	case []interface{}:
		for _, item := range vv {
			child := &Node{Type: ElementNode, level: parent.level + 1, value: item}
			appendChild(parent, child)
			buildChildren(child, item)
		}
	case map[string]interface{}:
		keys := make([]string, 0, len(vv))
		for k := range vv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := &Node{Data: k, Type: ElementNode, level: parent.level + 1, value: vv[k]}
			appendChild(parent, child)
			buildChildren(child, vv[k])
		}
	default:
		child := &Node{Data: fmt.Sprintf("%v", v), Type: TextNode, level: parent.level + 1, value: v}
		appendChild(parent, child)
	}
}

func appendChild(parent, child *Node) {
	child.Parent = parent
	if parent.FirstChild == nil {
		parent.FirstChild = child
	} else {
		parent.LastChild.NextSibling = child
		child.PrevSibling = parent.LastChild
	}
	parent.LastChild = child
}
