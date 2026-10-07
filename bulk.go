// Copyright 2021 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package rtree

import (
	"errors"
	"math"
	"sort"
	"sync"
)

// ErrInvalidRect is returned by bulk loading operations when one of the
// provided items contains a NaN coordinate or a minimum value that is greater
// than its maximum value.
var ErrInvalidRect = errors.New("rtree: invalid rectangle: " +
	"NaN coordinate or min greater than max")

// Item is a single bulk loading entry that pairs a rectangle with arbitrary
// data.
type Item[N numeric, T any] struct {
	Min  [2]N
	Max  [2]N
	Data T
}

// validRect reports whether the rectangle is finite (no NaN) and not
// inverted. The expression "v != v" evaluates to true only for NaN floats and
// compiles to a constant false for every integer N.
func validRect[N numeric](min, max [2]N) bool {
	for i := 0; i < 2; i++ {
		lo, hi := min[i], max[i]
		if lo != lo || hi != hi || lo > hi {
			return false
		}
	}
	return true
}

type bulkItem[N numeric, T any] struct {
	r    rect[N]
	data T
}

func rectLessX[N numeric](a, b rect[N]) bool {
	if a.min[0] != b.min[0] {
		return a.min[0] < b.min[0]
	}
	if a.min[1] != b.min[1] {
		return a.min[1] < b.min[1]
	}
	if a.max[0] != b.max[0] {
		return a.max[0] < b.max[0]
	}
	return a.max[1] < b.max[1]
}

func rectLessY[N numeric](a, b rect[N]) bool {
	if a.min[1] != b.min[1] {
		return a.min[1] < b.min[1]
	}
	if a.min[0] != b.min[0] {
		return a.min[0] < b.min[0]
	}
	if a.max[1] != b.max[1] {
		return a.max[1] < b.max[1]
	}
	return a.max[0] < b.max[0]
}

// ceilSqrt returns ceil(sqrt(n)).
func ceilSqrt(n int) int {
	s := int(math.Sqrt(float64(n)))
	for s*s < n {
		s++
	}
	for s > 1 && (s-1)*(s-1) >= n {
		s--
	}
	return s
}

func ceilDiv(a, b int) int {
	return (a + b - 1) / b
}

// groupSizes partitions total ordered entries inside one STR vertical slice
// into node groups of at most maxEntries entries each. Every group holds at
// least two entries: when a single entry would trail the slice, the final 65
// entries are split into groups of 33 and 32 instead of 64 and 1.
func groupSizes(total int) []int {
	ngroups := ceilDiv(total, maxEntries)
	sizes := make([]int, ngroups)
	for i := range sizes {
		sizes[i] = maxEntries
	}
	sizes[ngroups-1] = total - (ngroups-1)*maxEntries
	if sizes[ngroups-1] == 1 && ngroups > 1 {
		sizes[ngroups-2] = (maxEntries + 2) / 2 // 33
		sizes[ngroups-1] = (maxEntries + 1) / 2 // 32
	}
	return sizes
}

// strSlices partitions n globally-x-sorted entries into STR vertical slices
// of at most numSlices*maxEntries entries each and returns their [start,end)
// bounds. When the trailing slice would hold a single entry it is merged into
// the previous slice so that every produced node (except the single root of a
// one-item tree) has at least two children.
func strSlices(n, numSlices int) [][2]int {
	capEach := numSlices * maxEntries
	var slices [][2]int
	for start := 0; start < n; start += capEach {
		end := start + capEach
		if end > n {
			end = n
		}
		slices = append(slices, [2]int{start, end})
	}
	if len(slices) > 1 {
		last := slices[len(slices)-1]
		if last[1]-last[0] == 1 {
			slices[len(slices)-2][1] = last[1]
			slices = slices[:len(slices)-1]
		}
	}
	return slices
}

// Load performs a one-time bulk (packed) load of all items into the tree
// using the Sort-Tile-Recursive (STR) algorithm.
//
// The leaves and branches are packed from spatially sorted entries, so the
// resulting tree shape depends only on the rectangle coordinates, never on
// the order of the input. Duplicate rectangles are retained as distinct
// entries.
//
// Every item is validated before any node is built: an item with a NaN
// coordinate or an inverted (min > max) rectangle causes Load to return
// ErrInvalidRect without mutating the tree. Loading zero items clears the
// tree. Any previous contents are replaced.
func (tr *RTreeGN[N, T]) Load(items []Item[N, T]) error {
	for i := range items {
		if !validRect[N](items[i].Min, items[i].Max) {
			return ErrInvalidRect
		}
	}
	if tr.qpool == nil {
		tr.qpool = &sync.Pool{
			New: func() any { return &queue[N, T]{} },
		}
	}
	if len(items) == 0 {
		tr.Clear()
		return nil
	}
	entries := make([]bulkItem[N, T], len(items))
	for i := range items {
		entries[i] = bulkItem[N, T]{
			r:    rect[N]{items[i].Min, items[i].Max},
			data: items[i].Data,
		}
	}
	level := tr.packLeaves(entries)
	for len(level) > 1 {
		level = tr.packBranches(level)
	}
	root := level[0]
	tr.root = root
	tr.rect = root.rect()
	tr.count = len(items)
	return nil
}

// packLeaves spatially sorts the entries and packs them into leaf nodes.
func (tr *RTreeGN[N, T]) packLeaves(
	entries []bulkItem[N, T],
) []*node[N, T] {
	n := len(entries)
	numLeaves := ceilDiv(n, maxEntries)
	nodes := make([]*node[N, T], 0, numLeaves)
	// STR: global sort by x.
	sort.Slice(entries, func(i, j int) bool {
		return rectLessX(entries[i].r, entries[j].r)
	})
	for _, sl := range strSlices(n, ceilSqrt(numLeaves)) {
		start, end := sl[0], sl[1]
		// Re-sort this vertical slice by y, then cut into leaf groups.
		sort.Slice(entries[start:end], func(i, j int) bool {
			return rectLessY(entries[start+i].r, entries[start+j].r)
		})
		g := start
		for _, size := range groupSizes(end - start) {
			leaf := tr.newNode(true)
			for k := g; k < g+size; k++ {
				leaf.rects[leaf.count] = entries[k].r
				leaf.items()[leaf.count] = entries[k].data
				leaf.count++
			}
			nodes = append(nodes, leaf)
			g += size
		}
	}
	return nodes
}

// sortBranch orders the child pointers and rectangles of a branch by the
// child minimum x. A stable sort keeps equal (duplicate) rectangles in a
// deterministic, non-decreasing order.
func (tr *RTreeGN[N, T]) sortBranch(b *node[N, T]) {
	count := int(b.count)
	idx := make([]int, count)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool {
		return rectLessX(b.rects[idx[i]], b.rects[idx[j]])
	})
	oldRects := append([]rect[N]{}, b.rects[:count]...)
	oldChildren := append([]*node[N, T]{}, b.children()[:count]...)
	for i, j := range idx {
		b.rects[i] = oldRects[j]
		b.children()[i] = oldChildren[j]
	}
}

// packBranches spatially sorts the child nodes and packs them into parent
// branch nodes. A single parent is returned when all children fit.
func (tr *RTreeGN[N, T]) packBranches(
	children []*node[N, T],
) []*node[N, T] {
	c := len(children)
	if c <= maxEntries {
		return []*node[N, T]{tr.newBranch(children)}
	}
	numParents := ceilDiv(c, maxEntries)
	sort.Slice(children, func(i, j int) bool {
		return rectLessX(children[i].rect(), children[j].rect())
	})
	nodes := make([]*node[N, T], 0, numParents)
	for _, sl := range strSlices(c, ceilSqrt(numParents)) {
		start, end := sl[0], sl[1]
		sort.Slice(children[start:end], func(i, j int) bool {
			return rectLessY(children[start+i].rect(),
				children[start+j].rect())
		})
		g := start
		for _, size := range groupSizes(end - start) {
			nodes = append(nodes, tr.newBranch(children[g:g+size]))
			g += size
		}
	}
	return nodes
}

func (tr *RTreeGN[N, T]) newBranch(children []*node[N, T]) *node[N, T] {
	b := tr.newNode(false)
	for i, ch := range children {
		b.rects[i] = ch.rect()
		b.children()[i] = ch
	}
	b.count = int16(len(children))
	tr.sortBranch(b)
	return b
}
