// Copyright 2020 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package btree

import "reflect"

// DiffKind describes the kind of change reported by a Diff iterator.
type DiffKind int

const (
	// DiffAdded means the item exists in the newer tree but not the older.
	DiffAdded DiffKind = iota + 1
	// DiffDeleted means the item exists in the older tree but not the newer.
	DiffDeleted
	// DiffModified means the same key exists in both trees but the value
	// differs according to the equality function given to DiffIter.
	DiffModified
)

// Diff represents a single ordered difference between two trees.
//
// The Old field holds the item from the older tree and the New field holds
// the item from the newer tree. Only New is populated for DiffAdded, only
// Old for DiffDeleted, and both for DiffModified.
type Diff[T any] struct {
	Kind DiffKind
	Old  T
	New  T
}

// DiffIterG is an ordered iterator over the differences between two
// copy-on-write B-tree versions.
//
// Create one with BTreeG.DiffIter and call Next until it returns false,
// reading each change with Item. Release must be called when done.
//
// Comparator compatibility
//
// Keys are aligned with the receiver tree's "less" function. Both trees
// must use the same comparator value; a different function that merely
// defines the same ordering is rejected with a panic, because equivalence
// of two function values cannot be established at runtime.
//
// Shared nodes and mutable values
//
// Copy and IsoCopy produce versions that share most of their nodes through
// copy-on-write. When both versions reach the same subtree node (the same
// *node pointer), the iterator skips the entire subtree without inspecting
// its items or calling equals; that is why a diff visits only the
// structurally changed parts instead of scanning every key.
//
// A shared node only proves that the node itself was not rewritten through
// the tree API. It is not a promise that the item values reachable through
// the node are immutable forever: if an inserted item is a pointer or
// contains pointers and the caller mutates the referenced value in place,
// the change is visible through every version sharing the node and the
// subtree is skipped without being reported. Values must therefore be
// treated as immutable snapshots, or the item type must implement
// Copy/IsoCopy so that every version stores its own copy.
//
// The equals function reports whether two items sharing a key are equal and
// must be an equivalence relation consistent with "less". A nil equals
// treats items as equal when neither is less than the other, comparing
// ordering only.
type DiffIterG[T any] struct {
	older *BTreeG[T]
	newer *BTreeG[T]
	less  func(a, b T) bool
	eq    func(a, b T) bool

	lockedOlder bool
	lockedNewer bool

	stackO  [16]diffFrame[T]
	stackN  [16]diffFrame[T]
	framesO []diffFrame[T]
	framesN []diffFrame[T]

	done    bool
	current Diff[T]
}

type diffFrame[T any] struct {
	n *node[T]
	// i indexes the in-order position of the frame.
	// For a leaf it is a plain item index.
	// For an internal node even positions are children (i/2) and odd
	// positions are separator items (i/2).
	i int
}

const (
	diffEOF = iota
	diffItem
	diffSubtree
)

// DiffIter returns an ordered iterator over the differences between tr
// (the older version) and newer, where newer is typically obtained with
// Copy and then mutated.
//
// equals decides whether two items sharing a key are equal; pass nil to
// compare ordering only. See DiffIterG for the comparator and shared-node
// contract.
//
// DiffIter panics if either tree is nil or if the trees were created with
// different "less" functions. The iterator holds read locks on both trees
// until Release; do not mutate either tree while iterating.
func (tr *BTreeG[T]) DiffIter(newer *BTreeG[T], equals func(a, b T) bool) *DiffIterG[T] {
	if tr == nil || newer == nil {
		panic("nil tree")
	}
	if reflect.ValueOf(tr.less).Pointer() != reflect.ValueOf(newer.less).Pointer() {
		panic("incompatible comparators: both trees must use the same less function")
	}
	d := new(DiffIterG[T])
	d.older = tr
	d.newer = newer
	d.less = tr.less
	d.eq = equals
	// Lock in isoid order so that concurrent diffs between the same two
	// trees cannot deadlock. A tree and its copy always have distinct
	// isoids, and a tree cannot be diffed against itself.
	if d.older.isoid <= d.newer.isoid {
		d.lockedOlder = d.older.lock(false)
		d.lockedNewer = d.newer.lock(false)
	} else {
		d.lockedNewer = d.newer.lock(false)
		d.lockedOlder = d.older.lock(false)
	}
	d.framesO = d.stackO[:0]
	d.framesN = d.stackN[:0]
	if tr.root != nil {
		d.framesO = append(d.framesO, diffFrame[T]{tr.root, 0})
	}
	if newer.root != nil {
		d.framesN = append(d.framesN, diffFrame[T]{newer.root, 0})
	}
	return d
}

// frameLimit returns one past the last valid position of a frame.
func frameLimit[T any](n *node[T]) int {
	if n.leaf() {
		return len(n.items)
	}
	return 2*len(n.items) + 1
}

// elem returns the element at the current frame position. Exhausted frames
// must first be removed with norm.
func (d *DiffIterG[T]) elem(frames []diffFrame[T]) (kind int, item T, child *node[T]) {
	f := &frames[len(frames)-1]
	if f.n.leaf() || f.i&1 == 1 {
		return diffItem, f.n.items[f.i>>1], nil
	}
	return diffSubtree, d.older.empty, (*f.n.children)[f.i>>1]
}

// norm pops exhausted frames, advancing each parent frame past the finished
// child.
func (d *DiffIterG[T]) norm(frames []diffFrame[T]) []diffFrame[T] {
	for len(frames) > 0 && frames[len(frames)-1].i >= frameLimit(frames[len(frames)-1].n) {
		frames = frames[:len(frames)-1]
		if len(frames) > 0 {
			frames[len(frames)-1].i++
		}
	}
	return frames
}

// descend pushes the path below node n, ending on its leftmost leaf.
func (d *DiffIterG[T]) descend(frames []diffFrame[T], n *node[T]) []diffFrame[T] {
	for !n.leaf() {
		frames = append(frames, diffFrame[T]{n, 0})
		n = (*n.children)[0]
	}
	return append(frames, diffFrame[T]{n, 0})
}

// emitRemaining drains one stream after the other stream is exhausted,
// reporting every remaining item as added or deleted. No key comparisons or
// equals calls are required.
func (d *DiffIterG[T]) emitRemaining(frames []diffFrame[T], added bool) []diffFrame[T] {
	frames = d.norm(frames)
	if len(frames) == 0 {
		d.done = true
		return frames
	}
	kind, item, child := d.elem(frames)
	if kind == diffSubtree {
		frames = d.descend(frames, child)
		frames = d.norm(frames)
		_, item, _ = d.elem(frames)
	}
	if added {
		d.current = Diff[T]{Kind: DiffAdded, New: item}
	} else {
		d.current = Diff[T]{Kind: DiffDeleted, Old: item}
	}
	frames[len(frames)-1].i++
	return frames
}

// Next advances to the next difference, returning false when both trees
// have been fully aligned.
func (d *DiffIterG[T]) Next() bool {
	if d.done {
		return false
	}
	for {
		d.framesO = d.norm(d.framesO)
		d.framesN = d.norm(d.framesN)
		if len(d.framesO) == 0 || len(d.framesN) == 0 {
			if len(d.framesN) > 0 {
				d.framesN = d.emitRemaining(d.framesN, true)
				return true
			}
			if len(d.framesO) > 0 {
				d.framesO = d.emitRemaining(d.framesO, false)
				return true
			}
			d.done = true
			return false
		}
		kindO, itemO, childO := d.elem(d.framesO)
		kindN, itemN, childN := d.elem(d.framesN)
		if kindO == diffSubtree && kindN == diffSubtree && childO == childN {
			// The subtree is shared verbatim by both versions. Skip it
			// without touching any item; copy-on-write guarantees the node
			// was not rewritten. In-place mutation of a referenced value
			// cannot be detected here; see the DiffIterG docs.
			d.framesO[len(d.framesO)-1].i++
			d.framesN[len(d.framesN)-1].i++
			continue
		}
		if kindO == diffSubtree {
			d.framesO = d.descend(d.framesO, childO)
			continue
		}
		if kindN == diffSubtree {
			d.framesN = d.descend(d.framesN, childN)
			continue
		}
		// Both streams are on items. Advance both, then rewind the side
		// whose item was smaller so it is reconsidered on the next pass.
		d.framesO[len(d.framesO)-1].i++
		d.framesN[len(d.framesN)-1].i++
		switch {
		case d.less(itemO, itemN):
			d.current = Diff[T]{Kind: DiffDeleted, Old: itemO}
			d.framesN[len(d.framesN)-1].i--
			return true
		case d.less(itemN, itemO):
			d.current = Diff[T]{Kind: DiffAdded, New: itemN}
			d.framesO[len(d.framesO)-1].i--
			return true
		}
		// Equal keys: report a modification unless values are equivalent.
		equal := d.eq != nil && d.eq(itemO, itemN)
		if d.eq == nil {
			equal = !d.less(itemO, itemN) && !d.less(itemN, itemO)
		}
		if equal {
			continue
		}
		d.current = Diff[T]{Kind: DiffModified, Old: itemO, New: itemN}
		return true
	}
}

// Item returns the current difference. It is only valid after Next returns
// true.
func (d *DiffIterG[T]) Item() Diff[T] {
	return d.current
}

// Release unlocks both trees. It must be called when iteration is finished.
func (d *DiffIterG[T]) Release() {
	if d.lockedOlder {
		d.older.unlock(false)
		d.lockedOlder = false
	}
	if d.lockedNewer {
		d.newer.unlock(false)
		d.lockedNewer = false
	}
}
