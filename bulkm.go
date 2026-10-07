// Copyright 2020 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package btree

import "unsafe"

// BulkLoad atomically replaces the contents of m with a map built from the
// key/value pairs produced by iter. The iterator must yield keys in strictly
// ascending order; the total count does not need to be known in advance.
//
// Duplicate keys are handled according to opts.Duplicates (RejectDuplicates,
// KeepFirst, or KeepLast).
//
// The new tree is assembled entirely in temporary memory and only replaces
// the old tree after the whole iterator has been consumed and validated. On
// error the temporary structure is released, the receiver is left untouched,
// and any earlier Copy of m keeps working.
func (tr *Map[K, V]) BulkLoad(iter func(yield func(key K, value V) bool),
	opts *BulkOptions,
) (BulkStats, error) {
	if iter == nil {
		return BulkStats{}, ErrNilIterator
	}
	tr.init(0)

	policy := RejectDuplicates
	if opts != nil {
		policy = opts.Duplicates
	}
	b := bulkBuilderM[K, V]{tr: tr, isoid: newIsoID(), policy: policy}

	var loadErr error
	iter(func(key K, value V) bool {
		if err := b.add(key, value); err != nil {
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

type bulkNodeM[K ordered, V any] struct {
	node *mapNode[K, V]
	sep  mapPair[K, V]
}

type bulkBuilderM[K ordered, V any] struct {
	tr     *Map[K, V]
	isoid  uint64
	policy DuplicatePolicy

	leaf     *mapNode[K, V]
	leaves   []bulkNodeM[K, V]
	last     mapPair[K, V]
	hasLast  bool
	lastSlot *mapPair[K, V]

	loaded      int
	duplicates  int
	comparisons int64
	nodes       int64
	itemSlots   int64
	peakBytes   uintptr
}

func (b *bulkBuilderM[K, V]) addBytes(n uintptr) {
	b.peakBytes += n
}

func (b *bulkBuilderM[K, V]) newLeaf() *mapNode[K, V] {
	n := &mapNode[K, V]{isoid: b.isoid}
	n.items = make([]mapPair[K, V], 0, b.tr.max)
	b.nodes++
	b.addBytes(unsafe.Sizeof(mapNode[K, V]{}) +
		uintptr(b.tr.max)*unsafe.Sizeof(n.items[0]))
	b.itemSlots += int64(b.tr.max)
	return n
}

func (b *bulkBuilderM[K, V]) newInternal() *mapNode[K, V] {
	n := &mapNode[K, V]{isoid: b.isoid}
	n.children = new([]*mapNode[K, V])
	*n.children = make([]*mapNode[K, V], 0, b.tr.max+1)
	n.items = make([]mapPair[K, V], 0, b.tr.max)
	b.nodes++
	b.addBytes(unsafe.Sizeof(mapNode[K, V]{}) +
		unsafe.Sizeof([]*mapNode[K, V]{}) +
		uintptr(b.tr.max+1)*unsafe.Sizeof((*mapNode[K, V])(nil)) +
		uintptr(b.tr.max)*unsafe.Sizeof(n.items[0]))
	b.itemSlots += int64(b.tr.max)
	return n
}

func (b *bulkBuilderM[K, V]) add(key K, value V) error {
	pair := mapPair[K, V]{key: key, value: value}
	if b.hasLast {
		b.comparisons += 2
		if key < b.last.key {
			return ErrOutOfOrder
		}
		if !(b.last.key < key) {
			b.duplicates++
			switch b.policy {
			case RejectDuplicates:
				return ErrDuplicateKey
			case KeepFirst:
				return nil
			default:
				*b.lastSlot = pair
				b.last = pair
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
	b.leaf.items = append(b.leaf.items, pair)
	b.last = pair
	b.lastSlot = &b.leaf.items[len(b.leaf.items)-1]
	b.hasLast = true
	return nil
}

func (b *bulkBuilderM[K, V]) sealLeaf() {
	lf := b.leaf
	sep := lf.items[b.tr.max-1]
	lf.items[b.tr.max-1] = b.tr.empty
	lf.items = lf.items[:b.tr.max-1]
	b.leaves = append(b.leaves, bulkNodeM[K, V]{node: lf, sep: sep})
	b.addBytes(unsafe.Sizeof(bulkNodeM[K, V]{}))
}

func (b *bulkBuilderM[K, V]) finish() *mapNode[K, V] {
	if b.loaded == 0 {
		return nil
	}
	b.finishLeaves()
	cur := b.leaves

	if len(cur) == 1 {
		return cur[0].node
	}
	for len(cur) > 1 {
		cur = b.groupLevel(cur)
	}
	root := cur[0].node
	b.sink(root, cur[0].sep)
	if len(*root.children) == 1 {
		return (*root.children)[0]
	}
	return root
}

func (b *bulkBuilderM[K, V]) finishLeaves() {
	if b.leaf == nil {
		return
	}
	if len(b.leaves) == 0 {
		b.leaves = append(b.leaves, bulkNodeM[K, V]{node: b.leaf})
		b.leaf = nil
		return
	}

	pred := b.leaves[len(b.leaves)-1]
	predLeaf := pred.node
	total := len(predLeaf.items) + 1 + len(b.leaf.items)
	pool := make([]mapPair[K, V], total)
	copy(pool, predLeaf.items)
	pool[len(predLeaf.items)] = pred.sep
	copy(pool[len(predLeaf.items)+1:], b.leaf.items)
	b.addBytes(uintptr(total) * unsafe.Sizeof(pool[0]))
	b.itemSlots += int64(total)

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
	b.leaves = append(b.leaves, bulkNodeM[K, V]{
		node: last, sep: pool[total-1],
	})
	b.leaf = nil
}

func (b *bulkBuilderM[K, V]) groupLevel(
	in []bulkNodeM[K, V],
) []bulkNodeM[K, V] {
	maxChildren := b.tr.max + 1
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

	out := make([]bulkNodeM[K, V], 0, len(boundaries)-1)
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
		out = append(out, bulkNodeM[K, V]{
			node: node, sep: children[len(children)-1].sep,
		})
	}
	return out
}

func (b *bulkBuilderM[K, V]) sink(n *mapNode[K, V],
	item mapPair[K, V],
) {
	cur := n
	for !cur.leaf() {
		cur = (*cur.children)[len(*cur.children)-1]
	}
	cur.items = append(cur.items, item)
}

func (b *bulkBuilderM[K, V]) setCounts(n *mapNode[K, V]) {
	n.count = len(n.items)
	if !n.leaf() {
		for _, child := range *n.children {
			b.setCounts(child)
			n.count += child.count
		}
	}
}

func (b *bulkBuilderM[K, V]) adopt(n *mapNode[K, V]) {
	n.isoid = b.tr.isoid
	if !n.leaf() {
		for _, child := range *n.children {
			b.adopt(child)
		}
	}
}
