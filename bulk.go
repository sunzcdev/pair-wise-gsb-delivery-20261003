// Copyright 2021 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package rtree

import (
	"errors"
	"math"
	"sort"
	"sync"
	"unsafe"
)

var (
	// ErrInvalidRect is returned by Load when an entry has a NaN coordinate
	// or a minimum that is greater than its maximum.
	ErrInvalidRect = errors.New("rtree: invalid rectangle")
	// ErrNotEmpty is returned by Load when the tree already contains items.
	ErrNotEmpty = errors.New("rtree: tree is not empty")
)

// Entry is a single rectangle/data pair used for bulk loading a tree.
type Entry[N numeric, T any] struct {
	Min  [2]N
	Max  [2]N
	Data T
}

// Load bulk loads all entries into the tree in a single pass using the
// Sort-Tile-Recursive (STR) algorithm. The entries are spatially sorted and
// tiled into leaves, and the resulting nodes are tiled again at every higher
// level, so the tree shape does not depend on the order of the input.
// Duplicate rectangles are kept as distinct entries.
//
// Every entry is validated before any nodes are created: a NaN coordinate or
// an inverted rectangle (min > max) causes Load to return ErrInvalidRect
// without modifying the tree. Load also returns ErrNotEmpty when the tree
// already contains items. An empty entry slice is a no-op.
func (tr *RTreeGN[N, T]) Load(entries []Entry[N, T]) error {
	if tr.root != nil || tr.count != 0 {
		return ErrNotEmpty
	}
	for i := range entries {
		if !validRect(entries[i].Min, entries[i].Max) {
			return ErrInvalidRect
		}
	}
	if len(entries) == 0 {
		return nil
	}
	if tr.qpool == nil {
		tr.qpool = &sync.Pool{
			New: func() any { return &queue[N, T]{} },
		}
	}

	tr.root = tr.strBuild(entries)
	tr.count = len(entries)
	tr.rect = tr.root.rect()
	return nil
}

// strBuild performs the recursive Sort-Tile-Recursive packing of entries and
// returns the root node.
func (tr *RTreeGN[N, T]) strBuild(entries []Entry[N, T]) *node[N, T] {
	nodes := tr.strPackItems(entries)
	for len(nodes) > 1 {
		nodes = tr.strPackNodes(nodes)
	}
	return nodes[0]
}

// strTile sorts units by their x center, splits them into about sqrt(S)
// vertical slices, and sorts each slice by its y center. The returned groups
// are the slices in x order, each already ordered on y; packing them in order
// produces spatially compact nodes. The ordering is a total order over the
// rectangles, so the result is independent of the original input order and
// duplicate rectangles remain individually addressable.
func strTile[N numeric](units []rect[N]) [][]int {
	s := len(units)
	nodesNeeded := (s + maxEntries - 1) / maxEntries
	slices := ceilSqrt(nodesNeeded)
	nodesPerSlice := (nodesNeeded + slices - 1) / slices
	sliceCap := nodesPerSlice * maxEntries

	order := make([]int, s)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		return rectCenterLessAxis(units[order[i]], units[order[j]], 0)
	})

	groups := make([][]int, 0, slices)
	for start := 0; start < s; start += sliceCap {
		end := start + sliceCap
		if end > s {
			end = s
		}
		group := order[start:end]
		sort.Slice(group, func(i, j int) bool {
			return rectCenterLessAxis(units[group[i]], units[group[j]], 1)
		})
		groups = append(groups, group)
	}
	return groups
}

func (tr *RTreeGN[N, T]) strPackItems(entries []Entry[N, T]) []*node[N, T] {
	units := make([]rect[N], len(entries))
	for i := range entries {
		units[i] = rect[N]{entries[i].Min, entries[i].Max}
	}
	groups := strTile(units)
	nodes := make([]*node[N, T], 0, len(entries)/maxEntries+1)
	for _, group := range groups {
		for start := 0; start < len(group); start += maxEntries {
			end := start + maxEntries
			if end > len(group) {
				end = len(group)
			}
			leaf := tr.newNode(true)
			for _, idx := range group[start:end] {
				leaf.rects[leaf.count] = units[idx]
				leaf.items()[leaf.count] = entries[idx].Data
				leaf.count++
			}
			nodes = append(nodes, leaf)
		}
	}
	return nodes
}

func (tr *RTreeGN[N, T]) strPackNodes(children []*node[N, T]) []*node[N, T] {
	units := make([]rect[N], len(children))
	for i, child := range children {
		units[i] = child.rect()
	}
	groups := strTile(units)
	nodes := make([]*node[N, T], 0, len(children)/maxEntries+1)
	for _, group := range groups {
		for start := 0; start < len(group); start += maxEntries {
			end := start + maxEntries
			if end > len(group) {
				end = len(group)
			}
			branch := tr.newNode(false)
			for _, idx := range group[start:end] {
				branch.rects[branch.count] = units[idx]
				branch.children()[branch.count] = children[idx]
				branch.count++
			}
			if orderBranches {
				branch.sort()
			}
			nodes = append(nodes, branch)
		}
	}
	return nodes
}

// validRect reports whether the rectangle is usable: no NaN coordinates and no
// inverted axes.
func validRect[N numeric](min, max [2]N) bool {
	for i := 0; i < 2; i++ {
		if isNaN(min[i]) || isNaN(max[i]) || min[i] > max[i] {
			return false
		}
	}
	return true
}

func isNaN[N numeric](v N) bool {
	switch x := any(v).(type) {
	case float64:
		return math.IsNaN(x)
	case float32:
		return math.IsNaN(float64(x))
	default:
		return false
	}
}

// rectCenterLessAxis orders rectangles by their center on axis, then the
// other axis, falling back to a lexicographic comparison of the corners. It is
// a total order over rectangles, so equal (duplicate) rectangles compare
// equal while every other pair has a deterministic relative order.
func rectCenterLessAxis[N numeric](a, b rect[N], axis int) bool {
	other := axis ^ 1
	if a.min[axis]+a.max[axis] != b.min[axis]+b.max[axis] {
		return a.min[axis]+a.max[axis] < b.min[axis]+b.max[axis]
	}
	if a.min[other]+a.max[other] != b.min[other]+b.max[other] {
		return a.min[other]+a.max[other] < b.min[other]+b.max[other]
	}
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

func ceilSqrt(n int) int {
	if n <= 1 {
		return 1
	}
	s := sort.Search(n, func(i int) bool { return i*i >= n })
	if s == 0 {
		return 1
	}
	return s
}

// EntryG is a single rectangle/data pair used for bulk loading an RTreeG or
// RTree.
type EntryG[T any] struct {
	Min  [2]float64
	Max  [2]float64
	Data T
}

// Load bulk loads all entries into the tree in a single pass using the
// Sort-Tile-Recursive (STR) algorithm. See RTreeGN.Load for details.
func (tr *RTreeG[T]) Load(entries []EntryG[T]) error {
	// EntryG[T] and Entry[float64, T] share the same memory layout.
	var base []Entry[float64, T]
	if len(entries) > 0 {
		base = unsafe.Slice((*Entry[float64, T])(unsafe.Pointer(&entries[0])),
			len(entries))
	}
	return tr.base.Load(base)
}

// Load bulk loads all entries into the tree in a single pass using the
// Sort-Tile-Recursive (STR) algorithm. See RTreeGN.Load for details.
func (tr *RTree) Load(entries []EntryG[any]) error {
	return tr.base.Load(entries)
}
