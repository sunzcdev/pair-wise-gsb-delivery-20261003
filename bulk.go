// Copyright 2020 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package btree

import (
	"errors"
	"fmt"
	"unsafe"
)

// DuplicatePolicy tells BulkLoad how to handle items with equal keys.
type DuplicatePolicy int

const (
	// RejectDuplicates refuses the whole load when a duplicate key is found.
	// This is the zero-value default because it is the safest choice.
	RejectDuplicates DuplicatePolicy = iota
	// KeepFirst keeps the first occurrence of a duplicate key and discards
	// every later item with the same key.
	KeepFirst
	// KeepLast keeps the last occurrence of a duplicate key, replacing the
	// earlier item in place.
	KeepLast
)

// BulkOptions are the options for a BulkLoad call.
type BulkOptions struct {
	// Duplicates selects how equal keys are treated.
	// Defaults to RejectDuplicates.
	Duplicates DuplicatePolicy
}

// BulkStats reports statistics collected by a single BulkLoad call. The
// counters are updated while the stream is consumed, so they stay useful even
// when the load fails.
type BulkStats struct {
	// Items is the number of items consumed from the iterator.
	Items int
	// Loaded is the number of items that ended up in the new tree. It is less
	// than Items when KeepFirst or KeepLast skipped duplicates.
	Loaded int
	// Duplicates is the number of equal-key items observed after the first
	// one.
	Duplicates int
	// Comparisons is the number of times the tree comparator was invoked,
	// both while validating the input order and during final verification.
	Comparisons int64
	// Nodes is the number of nodes allocated for the new tree.
	Nodes int
	// PeakBytes is an upper bound on the heap bytes occupied by the
	// temporary tree while it is being built. The previous tree is not
	// counted.
	PeakBytes uintptr
}

var (
	// ErrOutOfOrder is returned when an iterator yields an item whose key is
	// less than the key of the previous item.
	ErrOutOfOrder = errors.New("btree: bulk load items are not in ascending order")
	// ErrDuplicate is returned with RejectDuplicates when two equal keys are
	// encountered.
	ErrDuplicate = errors.New("btree: bulk load encountered a duplicate key")
	// ErrNilIterator is returned when the iterator argument is nil.
	ErrNilIterator = errors.New("btree: bulk load iterator is nil")
	// ErrNilItem is returned when an iterator yields a nil item.
	ErrNilItem = errors.New("btree: bulk load iterator yielded a nil item")
)

// BulkLoad atomically builds a tree from a stream of ascending items and
// replaces the previous contents only after every item has been consumed and
// the resulting tree has been verified.
//
// The iterator is called repeatedly until it returns false; the total number
// of items never has to be known up front. The items are not buffered into an
// extra slice: the tree is laid out directly, bottom up, using the configured
// node capacity (degree). Nodes split on the same median the tree uses for
// ordinary inserts, and when the trailing level does not fill a complete node
// its keys or subtrees are merged with the previous sibling by demoting the
// separator between them, keeping every non-root node within the configured
// occupancy.
//
// The new tree is fully constructed in private temporary structures while the
// existing tree, including any Copy of it, keeps working unchanged. If the
// stream or the final verification fails, the temporary tree is discarded and
// the previous tree is left intact.
func (tr *BTreeG[T]) BulkLoad(iter func() (T, bool), opts *BulkOptions) (BulkStats, error) {
	if iter == nil {
		return BulkStats{}, ErrNilIterator
	}
	policy := RejectDuplicates
	if opts != nil {
		policy = opts.Duplicates
	}
	if policy < RejectDuplicates || policy > KeepLast {
		return BulkStats{}, fmt.Errorf("btree: invalid duplicate policy %d", policy)
	}
	if tr.lock(true) {
		defer tr.unlock(true)
	}
	tr.init(0)

	b := bulkBuilder[T]{
		tr:        tr,
		policy:    policy,
		leafCap:   tr.max,
		internCap: tr.max + 1,
		minItems:  tr.min,
	}
	b.openLeaf = b.newLeaf(make([]T, 0, tr.max))

	var prev T
	havePrev := false
	for {
		item, ok := iter()
		if !ok {
			break
		}
		if interface{}(item) == nil {
			return b.stats, ErrNilItem
		}
		b.stats.Items++
		if havePrev {
			b.stats.Comparisons++
			if !b.less(prev, item) {
				if b.equal(prev, item) {
					b.stats.Duplicates++
					switch policy {
					case RejectDuplicates:
						return b.stats, fmt.Errorf("%w at item #%d",
							ErrDuplicate, b.stats.Items)
					case KeepFirst:
						continue
					case KeepLast:
						// In a sorted stream a duplicate is always the last
						// item of the open leaf, so overwrite it in place.
						leaf := b.openLeaf.items
						leaf[len(leaf)-1] = item
						prev = item
						continue
					}
				}
				return b.stats, fmt.Errorf("%w at item #%d",
					ErrOutOfOrder, b.stats.Items)
			}
		}
		b.push(item)
		b.stats.Loaded++
		prev = item
		havePrev = true
	}

	root, err := b.finish()
	if err != nil {
		return b.stats, err
	}

	// Publish only after the whole tree exists and verifies. Until this
	// assignment tr.root still points at the previous tree.
	tr.root = root
	tr.count = b.stats.Loaded
	return b.stats, nil
}

// bulkBuilder lays out a B-tree directly from a sorted stream.
//
// While streaming, items only ever touch the open leaf. When the open leaf
// reaches capacity the next item splits it on its median: the left half is
// sealed, the median is recorded in leafSeps as the separator toward the
// right half, which becomes the new open leaf. At finish the leaves and
// separators are grouped bottom up into internal nodes; the trailing short
// leaf/group borrows entries from its previous sibling. Keys are stored
// exactly once (either in a node or as a promoted separator) and no extra
// full-copy slice is required.
type bulkBuilder[T any] struct {
	tr        *BTreeG[T]
	policy    DuplicatePolicy
	leafCap   int // max items in a node
	internCap int // max children of an internal node
	minItems  int // min items in a non-root node

	leaves   []*node[T] // sealed leaves
	leafSeps []T        // promoted median following each sealed leaf
	openLeaf *node[T]   // trailing open leaf

	stats BulkStats
	bytes uintptr
}

func (b *bulkBuilder[T]) less(a, c T) bool {
	return b.tr.less(a, c)
}

func (b *bulkBuilder[T]) equal(a, c T) bool {
	b.stats.Comparisons++
	return !b.less(a, c) && !b.less(c, a)
}

func sliceCapBytes[T any](s []T) uintptr {
	var zero T
	return uintptr(cap(s)) * unsafe.Sizeof(zero)
}

// charge accounts for freshly allocated tree storage and updates the peak.
func (b *bulkBuilder[T]) charge(nbytes uintptr) {
	b.bytes += nbytes
	if b.bytes > b.stats.PeakBytes {
		b.stats.PeakBytes = b.bytes
	}
}

// newLeaf allocates a leaf node over the given items slice.
func (b *bulkBuilder[T]) newLeaf(items []T) *node[T] {
	n := &node[T]{isoid: b.tr.isoid, items: items}
	b.stats.Nodes++
	b.charge(unsafe.Sizeof(node[T]{}) + sliceCapBytes(items))
	return n
}

// newInternal allocates an internal node over the given items/children.
func (b *bulkBuilder[T]) newInternal(items []T, children []*node[T]) *node[T] {
	ch := new([]*node[T])
	*ch = children
	n := &node[T]{isoid: b.tr.isoid, items: items, children: ch}
	b.stats.Nodes++
	b.charge(unsafe.Sizeof(node[T]{}) +
		sliceCapBytes(items) +
		unsafe.Sizeof([]*node[T]{}) +
		sliceCapBytes(children))
	return n
}

// push adds the next sorted item. Once the open leaf holds leafCap items, the
// next item triggers a median split (mirroring nodeSplit): the left half is
// sealed, the median is promoted as a separator and the right half becomes
// the new open leaf.
func (b *bulkBuilder[T]) push(item T) {
	if len(b.openLeaf.items) < b.leafCap {
		b.openLeaf.items = append(b.openLeaf.items, item)
		return
	}
	all := make([]T, 0, b.internCap)
	all = append(all, b.openLeaf.items...)
	all = append(all, item)
	i := b.leafCap / 2
	median := all[i]
	left := b.newLeaf(append([]T{}, all[:i]...))
	right := b.newLeaf(append([]T{}, all[i+1:]...))
	b.leaves = append(b.leaves, left)
	b.leafSeps = append(b.leafSeps, median)
	b.openLeaf = right
}

// finish assembles internal levels bottom up, fixes the trailing short leaf,
// verifies the tree and returns the root.
func (b *bulkBuilder[T]) finish() (*node[T], error) {
	if b.stats.Loaded == 0 {
		return nil, nil
	}

	// leaves are the median-split sealed leaves followed by the open trailing
	// leaf. leafSeps[i] is the promoted median separating leaf i from leaf
	// i+1; that key lives in neither leaf.
	leaves := append(append([]*node[T]{}, b.leaves...), b.openLeaf)
	seps := append([]T{}, b.leafSeps...)

	// Borrow keys into a short trailing leaf from the previous leaf. Moving a
	// keys across the boundary keeps order: prev's tail becomes last's head.
	// The separator becomes the new first key of the trailing leaf.
	if len(leaves) >= 2 {
		last := leaves[len(leaves)-1]
		prev := leaves[len(leaves)-2]
		if need := b.minItems - len(last.items); need > 0 {
			if len(prev.items)-need < b.minItems {
				return nil, errors.New(
					"btree: bulk load cannot balance leaf level")
			}
			splitAt := len(prev.items) - need
			moved := append([]T{}, prev.items[splitAt:]...)
			prev.items = prev.items[:splitAt]
			last.items = append(moved, last.items...)
			seps[len(seps)-1] = last.items[0]
		}
	}

	// Group into internal levels until a single root remains. Each grouping
	// consumes the separators between child nodes as the internal items.
	nodes := leaves
	for len(nodes) > 1 {
		nodes, seps = b.groupLevel(nodes, seps)
	}

	root := nodes[0]
	if err := b.verify(root); err != nil {
		return nil, err
	}
	return root, nil
}

// groupLevel partitions an ordered list of child nodes and the separators
// between them (len(seps) == len(nodes)-1) into internal nodes of at most
// internCap children. It returns the new level's nodes and the separators
// between those new nodes. The trailing group borrows children from the
// previous group so it keeps at least minItems+1 children; the borrowed
// boundary separator simply moves from the previous group's items to between
// the groups.
func (b *bulkBuilder[T]) groupLevel(nodes []*node[T], seps []T,
) ([]*node[T], []T) {
	n := len(nodes)

	// Initial full groups followed by a remainder.
	sizes := make([]int, 0, n/b.internCap+1)
	for rem := n; rem > 0; {
		size := b.internCap
		if rem < size {
			size = rem
		}
		sizes = append(sizes, size)
		rem -= size
	}

	// Borrow children into a short trailing group from the previous group,
	// which must retain at least minItems+1 children.
	for len(sizes) > 1 && sizes[len(sizes)-1]-1 < b.minItems {
		pgi := len(sizes) - 2
		if sizes[pgi]-1 <= b.minItems {
			break
		}
		sizes[pgi]--
		sizes[len(sizes)-1]++
	}

	out := make([]*node[T], 0, len(sizes))
	upSeps := make([]T, 0, len(sizes)-1)
	nodeOff := 0 // start offset into nodes of current group
	sepOff := 0  // start offset into seps of current group
	for gi, size := range sizes {
		groupNodes := nodes[nodeOff : nodeOff+size]
		groupSeps := seps[sepOff : sepOff+size-1]
		out = append(out, b.newInternal(
			append([]T{}, groupSeps...),
			append([]*node[T]{}, groupNodes...)))
		if gi+1 < len(sizes) {
			// Separator between this group and the next: it is the boundary
			// separator right after the current group's last child.
			upSeps = append(upSeps, seps[sepOff+size-1])
		}
		nodeOff += size
		sepOff += size // size-1 internal separators + 1 boundary separator
	}
	return out, upSeps
}

// verify recomputes node counts, checks structural and occupancy invariants,
// and performs a strict in-order comparison of every adjacent item.
func (b *bulkBuilder[T]) verify(root *node[T]) error {
	leafDepth := 0
	for n := root; !n.leaf(); n = (*n.children)[0] {
		leafDepth++
	}

	var check func(n *node[T], depth int, isRoot bool) (int, error)
	check = func(n *node[T], depth int, isRoot bool) (int, error) {
		isLeaf := n.leaf()
		if isLeaf != (depth == leafDepth) {
			return 0, errors.New(
				"btree: bulk load produced leaves at different depths")
		}
		if len(n.items) > b.leafCap {
			return 0, fmt.Errorf(
				"btree: bulk load node exceeds capacity: %d", len(n.items))
		}
		if !isRoot && len(n.items) < b.minItems {
			return 0, fmt.Errorf(
				"btree: bulk load node under minimum: %d", len(n.items))
		}
		count := len(n.items)
		if isLeaf {
			if len(n.items) == 0 {
				return 0, errors.New("btree: bulk load produced an empty leaf")
			}
			n.count = count
			return count, nil
		}
		if n.children == nil || len(*n.children) != len(n.items)+1 {
			return 0, errors.New(
				"btree: bulk load internal node children mismatch")
		}
		for _, child := range *n.children {
			cc, err := check(child, depth+1, false)
			if err != nil {
				return 0, err
			}
			count += cc
		}
		n.count = count
		return count, nil
	}

	count, err := check(root, 0, true)
	if err != nil {
		return err
	}
	if count != b.stats.Loaded {
		return fmt.Errorf(
			"btree: bulk load count mismatch: tree=%d loaded=%d",
			count, b.stats.Loaded)
	}

	// Strict in-order scan comparing every adjacent pair of items.
	type frame struct {
		n *node[T]
		i int
	}
	stack := []frame{{root, 0}}
	var prev T
	havePrev := false
	index := 0
	emit := func(item T) error {
		if havePrev {
			b.stats.Comparisons++
			if !b.less(prev, item) {
				return fmt.Errorf(
					"btree: bulk load verification failed between #%d and #%d",
					index, index+1)
			}
		}
		prev = item
		havePrev = true
		index++
		return nil
	}
	for len(stack) > 0 {
		f := &stack[len(stack)-1]
		n := f.n
		if n.leaf() {
			for i := 0; i < len(n.items); i++ {
				if err := emit(n.items[i]); err != nil {
					return err
				}
			}
			stack = stack[:len(stack)-1]
			continue
		}
		if f.i >= 2*len(n.items)+1 {
			stack = stack[:len(stack)-1]
			continue
		}
		i := f.i
		f.i++
		if i&1 == 0 {
			ci := i >> 1
			if ci >= len(*n.children) {
				stack = stack[:len(stack)-1]
				continue
			}
			stack = append(stack, frame{(*n.children)[ci], 0})
		} else if err := emit(n.items[i>>1]); err != nil {
			return err
		}
	}
	return nil
}
