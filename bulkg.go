// Copyright 2020 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package btree

import "unsafe"

// BulkLoad atomically replaces the contents of tr with a tree built from the
// items produced by iter. The iterator must yield items in strictly ascending
// order according to tr's less function; the total count does not need to be
// known in advance.
//
// Duplicate keys are handled according to opts.Duplicates (RejectDuplicates,
// KeepFirst, or KeepLast).
//
// The new tree is assembled entirely in temporary memory and only replaces
// the old tree after the whole iterator has been consumed and validated. On
// error the temporary structure is released, the receiver is left untouched,
// and any earlier Copy of tr keeps working.
//
// Items are packed bottom-up according to the configured node capacity. The
// final underfilled leaf is redistributed with its predecessor, and internal
// levels are grouped with balanced boundaries, so every non-root node
// respects the minimum occupancy.
func (tr *BTreeG[T]) BulkLoad(iter func(yield func(item T) bool),
	opts *BulkOptions,
) (BulkStats, error) {
	if iter == nil {
		return BulkStats{}, ErrNilIterator
	}
	if tr.lock(true) {
		defer tr.unlock(true)
	}
	tr.init(0)

	policy := RejectDuplicates
	if opts != nil {
		policy = opts.Duplicates
	}
	b := bulkBuilderG[T]{tr: tr, isoid: newIsoID(), policy: policy}

	var loadErr error
	iter(func(item T) bool {
		if err := b.add(item); err != nil {
			loadErr = err
			return false
		}
		return true
	})
	if loadErr != nil {
		return BulkStats{}, loadErr
	}

	root := b.finish()
	if root != nil {
		b.setCounts(root)
		if root.count != b.loaded {
			return BulkStats{}, ErrOutOfOrder
		}
		b.adopt(root)
	}

	stats := BulkStats{
		Loaded:      b.loaded,
		Duplicates:  b.duplicates,
		Comparisons: b.comparisons,
		Nodes:       b.nodes,
		ItemSlots:   b.itemSlots,
		PeakBytes:   b.peakBytes,
	}
	tr.root = root
	tr.count = b.loaded
	return stats, nil
}

// bulkNodeG is a packed node pending assembly into an internal level.
type bulkNodeG[T any] struct {
	node *node[T]
	sep  T // lifted maximum item (absent from node)
}

type bulkBuilderG[T any] struct {
	tr     *BTreeG[T]
	isoid  uint64
	policy DuplicatePolicy

	// leaf is the currently open leaf; leaves holds packed leaves, each
	// paired with its lifted boundary item.
	leaf   *node[T]
	leaves []bulkNodeG[T]

	last     T
	hasLast  bool
	lastSlot *T

	loaded      int
	duplicates  int
	comparisons int64
	nodes       int64
	itemSlots   int64
	peakBytes   uintptr
}

func (b *bulkBuilderG[T]) addBytes(n uintptr) {
	b.peakBytes += n
}

func (b *bulkBuilderG[T]) newLeaf() *node[T] {
	n := &node[T]{isoid: b.isoid}
	n.items = make([]T, 0, b.tr.max)
	b.nodes++
	b.addBytes(unsafe.Sizeof(node[T]{}) +
		uintptr(b.tr.max)*unsafe.Sizeof(n.items[0]))
	b.itemSlots += int64(b.tr.max)
	return n
}

func (b *bulkBuilderG[T]) newInternal() *node[T] {
	n := &node[T]{isoid: b.isoid}
	n.children = new([]*node[T])
	*n.children = make([]*node[T], 0, b.tr.max+1)
	n.items = make([]T, 0, b.tr.max)
	b.nodes++
	b.addBytes(unsafe.Sizeof(node[T]{}) +
		unsafe.Sizeof([]*node[T]{}) +
		uintptr(b.tr.max+1)*unsafe.Sizeof((*node[T])(nil)) +
		uintptr(b.tr.max)*unsafe.Sizeof(n.items[0]))
	b.itemSlots += int64(b.tr.max)
	return n
}

func (b *bulkBuilderG[T]) add(item T) error {
	if b.hasLast {
		b.comparisons += 2
		if b.tr.less(item, b.last) {
			return ErrOutOfOrder
		}
		if !b.tr.less(b.last, item) {
			b.duplicates++
			switch b.policy {
			case RejectDuplicates:
				return ErrDuplicateKey
			case KeepFirst:
				return nil
			default: // KeepLast
				*b.lastSlot = item
				b.last = item
				return nil
			}
		}
	}
	b.loaded++

	if b.leaf == nil {
		b.leaf = b.newLeaf()
	}
	if len(b.leaf.items) == b.tr.max {
		b.sealLeaf()
		b.leaf = b.newLeaf()
	}
	b.leaf.items = append(b.leaf.items, item)
	b.last = item
	b.lastSlot = &b.leaf.items[len(b.leaf.items)-1]
	b.hasLast = true
	return nil
}

// sealLeaf finishes the current full leaf, lifting its last item as the
// boundary separator and recording the leaf for level assembly.
func (b *bulkBuilderG[T]) sealLeaf() {
	lf := b.leaf
	sep := lf.items[b.tr.max-1]
	lf.items[b.tr.max-1] = b.tr.empty
	lf.items = lf.items[:b.tr.max-1]
	b.leaves = append(b.leaves, bulkNodeG[T]{node: lf, sep: sep})
	b.addBytes(unsafe.Sizeof(bulkNodeG[T]{}))
}

// finish assembles packed leaves into internal levels and returns the root.
func (b *bulkBuilderG[T]) finish() *node[T] {
	if b.loaded == 0 {
		return nil
	}

	// Redistribute the open final leaf with its sealed predecessor so both
	// meet the minimum occupancy, then record the final leaf.
	b.finishLeaves()

	cur := b.leaves

	if len(cur) == 1 && cur[0].node.leaf() {
		// Single leaf with no lifted boundary; it is already complete.
		return cur[0].node
	}
	for len(cur) > 1 {
		cur = b.groupLevel(cur)
	}
	root := cur[0].node
	// The final separator is the global maximum and belongs in the rightmost
	// leaf; it was lifted only during assembly.
	b.sink(root, cur[0].sep)
	if len(*root.children) == 1 {
		return (*root.children)[0]
	}
	return root
}

// finishLeaves seals the open final leaf, redistributing it with the previous
// leaf when it would otherwise be underfilled.
func (b *bulkBuilderG[T]) finishLeaves() {
	if b.leaf == nil {
		return
	}
	if len(b.leaves) == 0 {
		// Everything fits in one leaf; it keeps all its items (no lifted
		// boundary because it is the final and only node).
		b.leaves = append(b.leaves, bulkNodeG[T]{node: b.leaf})
		b.leaf = nil
		return
	}

	pred := b.leaves[len(b.leaves)-1]
	predLeaf := pred.node
	// Ordered pool: pred items + pred boundary + open leaf items.
	total := len(predLeaf.items) + 1 + len(b.leaf.items)
	pool := make([]T, total)
	copy(pool, predLeaf.items)
	pool[len(predLeaf.items)] = pred.sep
	copy(pool[len(predLeaf.items)+1:], b.leaf.items)
	b.addBytes(uintptr(total) * unsafe.Sizeof(pool[0]))
	b.itemSlots += int64(total)

	// Choose boundary a so that leafA keeps a-1 physical items (its max
	// pool[a-1] lifted) and leafB keeps total-a-1 physical items (its max
	// pool[total-1] lifted). Both physical counts must be in [min, max-1].
	a := total / 2
	if a < b.tr.min+1 {
		a = b.tr.min + 1
	}
	if total-a-1 < b.tr.min {
		a = total - b.tr.min
	}

	predLeaf.items = append(predLeaf.items[:0], pool[:a-1]...)
	b.leaves[len(b.leaves)-1].sep = pool[a-1]

	last := b.leaf
	last.items = append(last.items[:0], pool[a:total-1]...)
	b.leaves = append(b.leaves, bulkNodeG[T]{
		node: last, sep: pool[total-1],
	})
	b.leaf = nil
}

// groupLevel packs one level of finished subtrees into internal nodes. Each
// internal node holds up to max+1 children and max separator items; the last
// group may be smaller but is kept at or above the minimum by balancing the
// boundary with the preceding group.
func (b *bulkBuilderG[T]) groupLevel(in []bulkNodeG[T]) []bulkNodeG[T] {
	// Determine group boundaries. With c children a node stores c-1 items;
	// full nodes take max+1 children. Choose the number of children per group
	// so the final group also has minChildren..max+1 children.
	maxChildren := b.tr.max + 1

	// Number of groups needed, and spread children so every group is in
	// [minChildren, maxChildren].
	n := len(in)
	groups := 1
	for groups*maxChildren < n {
		groups++
	}
	boundaries := make([]int, 0, groups+1)
	for g := 0; g < groups; g++ {
		boundaries = append(boundaries, g*n/groups)
	}
	boundaries = append(boundaries, n)

	out := make([]bulkNodeG[T], 0, len(boundaries)-1)
	for g := 0; g < len(boundaries)-1; g++ {
		s, e := boundaries[g], boundaries[g+1]
		children := in[s:e]
		node := b.newInternal()
		for j := 0; j < len(children); j++ {
			*node.children = append(*node.children, children[j].node)
			if j < len(children)-1 {
				node.items = append(node.items, children[j].sep)
			}
		}
		// The group's lifted maximum is the sep of its last child.
		out = append(out, bulkNodeG[T]{node: node, sep: children[len(children)-1].sep})
	}
	return out
}

// sink pushes a lifted maximum down the right edge into the final leaf.
func (b *bulkBuilderG[T]) sink(n *node[T], item T) {
	cur := n
	for !cur.leaf() {
		cur = (*cur.children)[len(*cur.children)-1]
	}
	cur.items = append(cur.items, item)
}

func (b *bulkBuilderG[T]) setCounts(n *node[T]) {
	n.count = len(n.items)
	if !n.leaf() {
		for _, child := range *n.children {
			b.setCounts(child)
			n.count += child.count
		}
	}
}

// adopt stamps the temporary tree with the owner's isolation id.
func (b *bulkBuilderG[T]) adopt(n *node[T]) {
	n.isoid = b.tr.isoid
	if !n.leaf() {
		for _, child := range *n.children {
			b.adopt(child)
		}
	}
}
