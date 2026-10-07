// Copyright 2021 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package rtree

import "errors"

// ErrWeightOverflow is returned when maintaining the optional weight
// aggregation would overflow an int64.
var ErrWeightOverflow = errors.New("rtree: weight aggregation overflow")

// ErrNegativeWeight is returned when the weight function assigned with
// SetWeightFunc returns a negative weight. Weights must be non-negative.
var ErrNegativeWeight = errors.New("rtree: weight must be non-negative")

// checkedAddInt64 adds two int64 values, reporting false on overflow.
func checkedAddInt64(a, b int64) (int64, bool) {
	s := a + b
	if (b > 0 && s < a) || (b < 0 && s > a) {
		return 0, false
	}
	return s, true
}

// SetWeightFunc assigns the function used to derive the non-negative weight
// of an item, enabling weight aggregation for the Report method. It must be
// called before any items are inserted into the tree; it panics otherwise,
// because aggregates are maintained incrementally by Insert, Delete, Replace
// and copy-on-write and are never recomputed by rescanning the tree.
func (tr *RTreeGN[N, T]) SetWeightFunc(fn func(T) int64) {
	if tr.count != 0 {
		panic("rtree: SetWeightFunc must be called on an empty tree")
	}
	tr.weightFn = fn
}

// weightOf returns the weight of an item, or zero when no weight function
// has been assigned.
func (tr *RTreeGN[N, T]) weightOf(data T) int64 {
	if tr.weightFn == nil {
		return 0
	}
	return tr.weightFn(data)
}

// Report returns the number of items that fully fall inside the provided
// window along with the sum of their weights. Items touching the window
// boundary are included (closed interval). When the window fully contains
// the bounding box of a subtree, the precomputed statistics of that subtree
// are used directly instead of descending further, so a window covering the
// entire tree only visits a constant number of nodes.
func (tr *RTreeGN[N, T]) Report(min, max [2]N) (count, weight int64) {
	count, weight, _ = tr.report(min, max, nil)
	return count, weight
}

// report is the internal implementation of Report. When visited is not nil
// it counts the number of nodes the query touches.
func (tr *RTreeGN[N, T]) report(min, max [2]N, visited *int,
) (count, weight int64, nodes int) {
	target := rect[N]{min, max}
	if tr.root == nil || !target.intersects(&tr.rect) {
		return 0, 0, 0
	}
	count, weight = tr.nodeReport(tr.root, &tr.rect, &target, visited)
	if visited != nil {
		nodes = *visited
	}
	return count, weight, nodes
}

func (tr *RTreeGN[N, T]) nodeReport(n *node[N, T], nr *rect[N],
	target *rect[N], visited *int,
) (count, weight int64) {
	if visited != nil {
		*visited++
	}
	if target.contains(nr) {
		// The window fully contains this node's bounding box; take the
		// subtree statistics directly.
		return n.subCount, n.subWeight
	}
	rects := n.rects[:n.count]
	if n.leaf() {
		items := n.items()
		for i := 0; i < len(rects); i++ {
			if target.contains(&rects[i]) {
				count++
				weight += tr.weightOf(items[i])
			}
		}
		return count, weight
	}
	children := n.children()
	for i := 0; i < len(rects); i++ {
		if !target.intersects(&rects[i]) {
			continue
		}
		c, w := tr.nodeReport(children[i], &rects[i], target, visited)
		count += c
		weight += w
	}
	return count, weight
}

// SetWeightFunc assigns the function used to derive the non-negative weight
// of an item, enabling weight aggregation for the Report method. It must be
// called before any items are inserted into the tree.
func (tr *RTreeG[T]) SetWeightFunc(fn func(T) int64) {
	tr.base.SetWeightFunc(fn)
}

// InsertChecked is the same as Insert but returns an error instead of
// panicking when the weight aggregation cannot be maintained.
func (tr *RTreeG[T]) InsertChecked(min, max [2]float64, data T) error {
	return tr.base.InsertChecked(min, max, data)
}

// ReplaceChecked is the same as Replace but returns an error instead of
// panicking when the weight aggregation cannot be maintained. On error the
// old item is restored, leaving the tree unmodified.
func (tr *RTreeG[T]) ReplaceChecked(
	oldMin, oldMax [2]float64, oldData T,
	newMin, newMax [2]float64, newData T,
) error {
	return tr.base.ReplaceChecked(
		oldMin, oldMax, oldData,
		newMin, newMax, newData,
	)
}

// Report returns the number of items that fully fall inside the provided
// window along with the sum of their weights. Items touching the window
// boundary are included (closed interval).
func (tr *RTreeG[T]) Report(min, max [2]float64) (count, weight int64) {
	return tr.base.Report(min, max)
}

// SetWeightFunc assigns the function used to derive the non-negative weight
// of an item, enabling weight aggregation for the Report method. It must be
// called before any items are inserted into the tree.
func (tr *RTree) SetWeightFunc(fn func(any) int64) {
	tr.base.SetWeightFunc(fn)
}

// InsertChecked is the same as Insert but returns an error instead of
// panicking when the weight aggregation cannot be maintained.
func (tr *RTree) InsertChecked(min, max [2]float64, data any) error {
	return tr.base.InsertChecked(min, max, data)
}

// ReplaceChecked is the same as Replace but returns an error instead of
// panicking when the weight aggregation cannot be maintained. On error the
// old item is restored, leaving the tree unmodified.
func (tr *RTree) ReplaceChecked(
	oldMin, oldMax [2]float64, oldData any,
	newMin, newMax [2]float64, newData any,
) error {
	return tr.base.ReplaceChecked(
		oldMin, oldMax, oldData,
		newMin, newMax, newData,
	)
}

// Report returns the number of items that fully fall inside the provided
// window along with the sum of their weights. Items touching the window
// boundary are included (closed interval).
func (tr *RTree) Report(min, max [2]float64) (count, weight int64) {
	return tr.base.Report(min, max)
}
