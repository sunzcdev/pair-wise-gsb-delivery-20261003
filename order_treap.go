// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import "bytes"

// tNode is a node in a persistent (immutable) randomized binary search tree
// (treap) augmented with the size of its subtree. The tree orders entries by
// their raw index key. For non-unique indexes the key is the index value
// suffixed with the primary key, which makes every key unique and produces a
// stable ordering when several objects share the same index value.
//
// The tree is persistent: insert and delete never mutate a node that is
// reachable from a previously published root, they allocate new nodes along
// the affected path and share the untouched subtrees. This lets an old read
// transaction keep referencing an older root while a writer publishes a new
// one, providing the same MVCC guarantees as the immutable radix indexes.
//
// The expected depth is O(log n) thanks to the random priorities (max-heap
// property: a parent's priority is >= its children's), so rank and select run
// in expected O(log n) rather than by scanning and discarding entries.
type tNode struct {
	key      []byte
	val      interface{}
	priority uint64
	size     int
	left     *tNode
	right    *tNode
}

// tSize returns the number of entries in the (possibly nil) subtree.
func tSize(n *tNode) int {
	if n == nil {
		return 0
	}
	return n.size
}

// tRecompute refreshes the cached subtree size after a child change.
func (n *tNode) recompute() *tNode {
	n.size = tSize(n.left) + tSize(n.right) + 1
	return n
}

// tClone returns a shallow copy of n so the caller can mutate the copy without
// touching a node that may be shared with an older snapshot.
func (n *tNode) clone() *tNode {
	return &tNode{
		key:      n.key,
		val:      n.val,
		priority: n.priority,
		size:     n.size,
		left:     n.left,
		right:    n.right,
	}
}

// tRotateRight performs the standard right rotation. Both affected nodes are
// copied because the structure is persistent.
func tRotateRight(n *tNode) *tNode {
	child := n.left
	re := child.right

	nc := n.clone()
	nc.left = re
	nc.recompute()

	cc := child.clone()
	cc.right = nc
	cc.recompute()
	return cc
}

// tRotateLeft performs the standard left rotation.
func tRotateLeft(n *tNode) *tNode {
	child := n.right
	le := child.left

	nc := n.clone()
	nc.right = le
	nc.recompute()

	cc := child.clone()
	cc.left = nc
	cc.recompute()
	return cc
}

// tInsert inserts key/val into the subtree rooted at n, returning the new
// root. If key already exists its value is replaced. rng supplies the priority
// for a newly created node. Only nodes on the changed path are allocated.
func tInsert(n *tNode, key []byte, val interface{}, rng *xorshift) *tNode {
	if n == nil {
		return &tNode{
			key:      key,
			val:      val,
			priority: rng.next(),
			size:     1,
		}
	}

	switch c := bytes.Compare(key, n.key); {
	case c < 0:
		nc := n.clone()
		nc.left = tInsert(n.left, key, val, rng)
		nc.recompute()
		if nc.left.priority > nc.priority {
			return tRotateRight(nc)
		}
		return nc

	case c > 0:
		nc := n.clone()
		nc.right = tInsert(n.right, key, val, rng)
		nc.recompute()
		if nc.right.priority > nc.priority {
			return tRotateLeft(nc)
		}
		return nc

	default:
		// Exact key already present: persist an in-place value update.
		nc := n.clone()
		nc.val = val
		return nc
	}
}

// tMerge joins two trees where every key in a is smaller than every key in b.
func tMerge(a, b *tNode) *tNode {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}

	if a.priority > b.priority {
		nc := a.clone()
		nc.right = tMerge(a.right, b)
		nc.recompute()
		return nc
	}

	nc := b.clone()
	nc.left = tMerge(a, b.left)
	nc.recompute()
	return nc
}

// tDelete removes key from the subtree if present. When the matching node is
// reached its two children are merged and the node is dropped.
func tDelete(n *tNode, key []byte) *tNode {
	if n == nil {
		return nil
	}

	switch c := bytes.Compare(key, n.key); {
	case c < 0:
		nc := n.clone()
		nc.left = tDelete(n.left, key)
		nc.recompute()
		return nc
	case c > 0:
		nc := n.clone()
		nc.right = tDelete(n.right, key)
		nc.recompute()
		return nc
	default:
		// Drop this node and join its children. They remain untouched and are
		// therefore safe to share with older snapshots.
		return tMerge(n.left, n.right)
	}
}

// tCountLess returns the number of entries whose key is strictly less than
// key. This is the ascending rank of key minus one.
func tCountLess(n *tNode, key []byte) int {
	count := 0
	for n != nil {
		switch c := bytes.Compare(key, n.key); {
		case c < 0:
			n = n.left
		case c > 0:
			count += tSize(n.left) + 1
			n = n.right
		default:
			count += tSize(n.left)
			return count
		}
	}
	return count
}

// tKth returns the node at the 1-based ascending position k (1 <= k <= size).
func tKth(n *tNode, k int) *tNode {
	for n != nil {
		left := tSize(n.left)
		switch {
		case k <= left:
			n = n.left
		case k == left+1:
			return n
		default:
			k -= left + 1
			n = n.right
		}
	}
	return nil
}

// tGet returns the value stored at key and whether the key was present.
func tGet(n *tNode, key []byte) (interface{}, bool) {
	for n != nil {
		switch c := bytes.Compare(key, n.key); {
		case c < 0:
			n = n.left
		case c > 0:
			n = n.right
		default:
			return n.val, true
		}
	}
	return nil, false
}

// xorshift is a tiny deterministic 64-bit PRNG used to assign treap
// priorities. Each sortable index owns its own instance so no global lock or
// shared rand.Source is required across concurrent writers (memdb already
// serializes writers, but keeping the state local also makes a committed
// orderIndex self-contained).
type xorshift struct {
	state uint64
}

// next returns the next pseudo-random value, never zero.
func (r *xorshift) next() uint64 {
	x := r.state
	if x == 0 {
		// xorshift64* cannot escape the zero state; seed a fallback.
		x = 0x9e3779b97f4a7c15
	}
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545F4914F6CDD1D
}
