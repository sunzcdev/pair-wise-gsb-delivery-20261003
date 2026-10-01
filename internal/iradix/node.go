package iradix

import (
	"bytes"
	"sort"
)

// WalkFn is used when walking the tree. Takes a
// key and value, returning if iteration should
// be terminated.
type WalkFn func(k []byte, v interface{}) bool

// leafNode is used to represent a value
type leafNode struct {
	mutateCh chan struct{}
	key      []byte
	val      interface{}
}

// edge is used to represent an edge node
type edge struct {
	label byte
	node  *Node
}

// Node is an immutable node in the radix tree
type Node struct {
	// mutateCh is closed if this node is modified
	mutateCh chan struct{}

	// leaf is used to store possible leaf
	leaf *leafNode

	// prefix is the common prefix we ignore
	prefix []byte

	// size is the number of leaves in the subtree rooted at this node,
	// including the leaf on this node if any. It is maintained on every
	// mutation so that Rank and Select can be answered without walking
	// the tree.
	size int

	// Edges should be stored in-order for iteration.
	// We avoid a fully materialized slice to save memory,
	// since in most cases we expect to be sparse
	edges edges
}

func (n *Node) isLeaf() bool {
	return n.leaf != nil
}

func (n *Node) addEdge(e edge) {
	num := len(n.edges)
	idx := sort.Search(num, func(i int) bool {
		return n.edges[i].label >= e.label
	})
	n.edges = append(n.edges, e)
	if idx != num {
		copy(n.edges[idx+1:], n.edges[idx:num])
		n.edges[idx] = e
	}
}

func (n *Node) replaceEdge(e edge) {
	num := len(n.edges)
	idx := sort.Search(num, func(i int) bool {
		return n.edges[i].label >= e.label
	})
	if idx < num && n.edges[idx].label == e.label {
		n.edges[idx].node = e.node
		return
	}
	panic("replacing missing edge")
}

func (n *Node) getEdge(label byte) (int, *Node) {
	num := len(n.edges)
	idx := sort.Search(num, func(i int) bool {
		return n.edges[i].label >= label
	})
	if idx < num && n.edges[idx].label == label {
		return idx, n.edges[idx].node
	}
	return -1, nil
}

func (n *Node) getLowerBoundEdge(label byte) (int, *Node) {
	num := len(n.edges)
	idx := sort.Search(num, func(i int) bool {
		return n.edges[i].label >= label
	})
	// we want lower bound behavior so return even if it's not an exact match
	if idx < num {
		return idx, n.edges[idx].node
	}
	return -1, nil
}

func (n *Node) delEdge(label byte) {
	num := len(n.edges)
	idx := sort.Search(num, func(i int) bool {
		return n.edges[i].label >= label
	})
	if idx < num && n.edges[idx].label == label {
		copy(n.edges[idx:], n.edges[idx+1:])
		n.edges[len(n.edges)-1] = edge{}
		n.edges = n.edges[:len(n.edges)-1]
	}
}

func (n *Node) GetWatch(k []byte) (<-chan struct{}, interface{}, bool) {
	search := k
	watch := n.mutateCh
	for {
		// Check for key exhaustion
		if len(search) == 0 {
			if n.isLeaf() {
				return n.leaf.mutateCh, n.leaf.val, true
			}
			break
		}

		// Look for an edge
		_, n = n.getEdge(search[0])
		if n == nil {
			break
		}

		// Update to the finest granularity as the search makes progress
		watch = n.mutateCh

		// Consume the search prefix
		if bytes.HasPrefix(search, n.prefix) {
			search = search[len(n.prefix):]
		} else {
			break
		}
	}
	return watch, nil, false
}

func (n *Node) Get(k []byte) (interface{}, bool) {
	_, val, ok := n.GetWatch(k)
	return val, ok
}

// LongestPrefix is like Get, but instead of an
// exact match, it will return the longest prefix match.
func (n *Node) LongestPrefix(k []byte) ([]byte, interface{}, bool) {
	var last *leafNode
	search := k
	for {
		// Look for a leaf node
		if n.isLeaf() {
			last = n.leaf
		}

		// Check for key exhaution
		if len(search) == 0 {
			break
		}

		// Look for an edge
		_, n = n.getEdge(search[0])
		if n == nil {
			break
		}

		// Consume the search prefix
		if bytes.HasPrefix(search, n.prefix) {
			search = search[len(n.prefix):]
		} else {
			break
		}
	}
	if last != nil {
		return last.key, last.val, true
	}
	return nil, nil, false
}

// Minimum is used to return the minimum value in the tree
func (n *Node) Minimum() ([]byte, interface{}, bool) {
	for {
		if n.isLeaf() {
			return n.leaf.key, n.leaf.val, true
		}
		if len(n.edges) > 0 {
			n = n.edges[0].node
		} else {
			break
		}
	}
	return nil, nil, false
}

// Maximum is used to return the maximum value in the tree
func (n *Node) Maximum() ([]byte, interface{}, bool) {
	for {
		if num := len(n.edges); num > 0 {
			n = n.edges[num-1].node
			continue
		}
		if n.isLeaf() {
			return n.leaf.key, n.leaf.val, true
		} else {
			break
		}
	}
	return nil, nil, false
}

// Len returns the number of leaves in the subtree rooted at this node.
func (n *Node) Len() int {
	return n.size
}

// Rank returns the number of keys in the tree that sort strictly before k,
// which is the 0-based position k would occupy in the sorted order of keys.
// It runs in time proportional to the length of k rather than the rank
// itself, since subtree sizes are tracked on every node.
func (n *Node) Rank(k []byte) int {
	search := k
	rank := 0
	for {
		// If the search key is exhausted, a leaf on this node (if any) has
		// exactly key k, which is not strictly less than k.
		if len(search) == 0 {
			return rank
		}

		// A leaf on this node has a key that is a proper prefix of k, so it
		// sorts strictly before k.
		if n.leaf != nil {
			rank++
		}

		// Accumulate the sizes of all edges that sort before the next byte
		// of the search key.
		label := search[0]
		num := len(n.edges)
		idx := sort.Search(num, func(i int) bool {
			return n.edges[i].label >= label
		})
		for i := 0; i < idx; i++ {
			rank += n.edges[i].node.size
		}
		if idx == num || n.edges[idx].label != label {
			return rank
		}

		child := n.edges[idx].node
		if bytes.HasPrefix(search, child.prefix) {
			search = search[len(child.prefix):]
			n = child
			continue
		}

		// The search key diverges from the child prefix. Every key under the
		// child shares child.prefix, so either all of them sort before k or
		// none do. Note that child.prefix cannot be a prefix of search here,
		// and search[0] == child.prefix[0] since they share the edge label.
		limit := len(search)
		if len(child.prefix) < limit {
			limit = len(child.prefix)
		}
		i := 1
		for i < limit && search[i] == child.prefix[i] {
			i++
		}
		if i < limit && child.prefix[i] < search[i] {
			rank += child.size
		}
		return rank
	}
}

// Select returns the key and value at the given 0-based rank in the sorted
// order of the tree. It runs in time proportional to the height of the tree
// rather than the rank itself, since subtree sizes are tracked on every
// node. The third return value is false if the rank is out of range.
func (n *Node) Select(rank int) ([]byte, interface{}, bool) {
	if rank < 0 || rank >= n.size {
		return nil, nil, false
	}
	for {
		if n.leaf != nil {
			if rank == 0 {
				return n.leaf.key, n.leaf.val, true
			}
			rank--
		}
		descended := false
		for _, e := range n.edges {
			if e.node.size > rank {
				n = e.node
				descended = true
				break
			}
			rank -= e.node.size
		}
		if !descended {
			// Unreachable if subtree sizes are consistent.
			return nil, nil, false
		}
	}
}

// Iterator is used to return an iterator at
// the given node to walk the tree
func (n *Node) Iterator() *Iterator {
	return &Iterator{node: n}
}

// ReverseIterator is used to return an iterator at
// the given node to walk the tree backwards
func (n *Node) ReverseIterator() *ReverseIterator {
	return NewReverseIterator(n)
}

// rawIterator is used to return a raw iterator at the given node to walk the
// tree.
func (n *Node) rawIterator() *rawIterator {
	iter := &rawIterator{node: n}
	iter.Next()
	return iter
}

// Walk is used to walk the tree
func (n *Node) Walk(fn WalkFn) {
	recursiveWalk(n, fn)
}

// WalkBackwards is used to walk the tree in reverse order
func (n *Node) WalkBackwards(fn WalkFn) {
	reverseRecursiveWalk(n, fn)
}

// WalkPrefix is used to walk the tree under a prefix
func (n *Node) WalkPrefix(prefix []byte, fn WalkFn) {
	search := prefix
	for {
		// Check for key exhaution
		if len(search) == 0 {
			recursiveWalk(n, fn)
			return
		}

		// Look for an edge
		_, n = n.getEdge(search[0])
		if n == nil {
			break
		}

		// Consume the search prefix
		if bytes.HasPrefix(search, n.prefix) {
			search = search[len(n.prefix):]

		} else if bytes.HasPrefix(n.prefix, search) {
			// Child may be under our search prefix
			recursiveWalk(n, fn)
			return
		} else {
			break
		}
	}
}

// WalkPath is used to walk the tree, but only visiting nodes
// from the root down to a given leaf. Where WalkPrefix walks
// all the entries *under* the given prefix, this walks the
// entries *above* the given prefix.
func (n *Node) WalkPath(path []byte, fn WalkFn) {
	search := path
	for {
		// Visit the leaf values if any
		if n.leaf != nil && fn(n.leaf.key, n.leaf.val) {
			return
		}

		// Check for key exhaution
		if len(search) == 0 {
			return
		}

		// Look for an edge
		_, n = n.getEdge(search[0])
		if n == nil {
			return
		}

		// Consume the search prefix
		if bytes.HasPrefix(search, n.prefix) {
			search = search[len(n.prefix):]
		} else {
			break
		}
	}
}

// recursiveWalk is used to do a pre-order walk of a node
// recursively. Returns true if the walk should be aborted
func recursiveWalk(n *Node, fn WalkFn) bool {
	// Visit the leaf values if any
	if n.leaf != nil && fn(n.leaf.key, n.leaf.val) {
		return true
	}

	// Recurse on the children
	for _, e := range n.edges {
		if recursiveWalk(e.node, fn) {
			return true
		}
	}
	return false
}

// reverseRecursiveWalk is used to do a reverse pre-order
// walk of a node recursively. Returns true if the walk
// should be aborted
func reverseRecursiveWalk(n *Node, fn WalkFn) bool {
	// Visit the leaf values if any
	if n.leaf != nil && fn(n.leaf.key, n.leaf.val) {
		return true
	}

	// Recurse on the children in reverse order
	for i := len(n.edges) - 1; i >= 0; i-- {
		e := n.edges[i]
		if reverseRecursiveWalk(e.node, fn) {
			return true
		}
	}
	return false
}
