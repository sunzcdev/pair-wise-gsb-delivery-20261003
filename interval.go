// Copyright 2020 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package btree

// End is an interval endpoint over an ordered coordinate type K.
//
// Boundary semantics:
//
//   - Infinite reports an unbounded endpoint. An interval with an
//     infinite start extends to -infinity; one with an infinite end
//     extends to +infinity. The Value field is ignored when Infinite is
//     true.
//   - Inclusive reports whether the Value belongs to the interval. A
//     closed endpoint is inclusive, an open endpoint is exclusive. The
//     field is ignored for infinite endpoints.
//
// Two endpoints only touch when both are inclusive and carry the same
// finite value; an open endpoint sitting on the same coordinate does not
// contain that coordinate.
type End[K any] struct {
	Value     K
	Inclusive bool
	Infinite  bool
}

// Interval is a half-open-or-closed, possibly unbounded range with an
// independent identity.
//
// The identity is separate from the bounds: two intervals may share the
// same Start and End and still be stored as distinct records. The tree is
// ordered by (Start, Start boundary, ID), where closed starts sort before
// open starts on the same value, and ties are broken with the ID
// comparator supplied to NewIntervalTreeG.
//
// An interval is empty (Empty returns true) when both ends are finite and
// either Start.Value > End.Value, or they are equal and at least one end
// is open. Empty intervals may be stored and keep their identity, but they
// contain no coordinate and therefore never overlap any other interval,
// including another empty interval: emptiness is the empty set, not a
// point.
//
// T can be any payload type; interval operations only inspect the bounds
// and the ID, so T may safely be a zero-sized struct.
type Interval[K comparable, T any] struct {
	Start End[K]
	End   End[K]
	// ID uniquely identifies this interval independently of its bounds.
	ID K
	// Item is an arbitrary payload.
	Item T
}

// endBound is the augmented information attached to every tree node: the
// greatest end endpoint anywhere in the subtree. It is ordered so that
// greater coordinates dominate, and, at the same coordinate, a closed end
// dominates an open end (closed reaches one coordinate further).
// Infinite ends dominate everything. A zero value sorts below any real
// endpoint and is used for empty children slots.
type endBound[K any] struct {
	value     K
	inclusive bool
	infinite  bool
	valid     bool
}

func endOf[K comparable, T any](iv Interval[K, T]) endBound[K] {
	return endBound[K]{
		value:     iv.End.Value,
		inclusive: iv.End.Inclusive,
		infinite:  iv.End.Infinite,
		valid:     true,
	}
}

// absorb sets b to the greater of the two ends.
func (b *endBound[K]) absorb(o endBound[K], less func(a, c K) bool) {
	if !o.valid {
		return
	}
	if !b.valid || o.infinite ||
		(!b.infinite && (less(b.value, o.value) ||
			(!less(o.value, b.value) && !b.inclusive && o.inclusive))) {
		*b = o
	}
}

// IntervalTreeG is an interval index backed by the generic B-tree. Each
// node caches the maximum end of its subtree, so overlap queries can skip
// whole subtrees that cannot contain a matching record instead of
// scanning forward by start point.
type IntervalTreeG[K comparable, T any] struct {
	tr     *BTreeG[Interval[K, T]]
	lessK  func(a, b K) bool
	lessID func(a, b K) bool
}

// NewIntervalTreeG returns an interval index.
//
// lessK is the coordinate comparator. lessID compares interval
// identities and must define a strict total order consistent with lessK
// on equal values; it is only used to break ties between intervals that
// share a start endpoint. Degree behaves as in NewBTreeGOptions; pass zero
// for the default.
func NewIntervalTreeG[K comparable, T any](
	lessK func(a, b K) bool,
	lessID func(a, b K) bool,
	degree int,
) *IntervalTreeG[K, T] {
	if lessK == nil || lessID == nil {
		panic("nil less")
	}
	it := &IntervalTreeG[K, T]{lessK: lessK, lessID: lessID}
	opts := Options{Degree: degree, NoLocks: true}
	tr := NewBTreeGOptions(it.intervalLess, opts)
	tr.reaug = it.recompute
	it.tr = tr
	return it
}

// intervalLess is the total item order: start coordinate, then closed
// start before open start on the same coordinate, then identity.
func (it *IntervalTreeG[K, T]) intervalLess(a, b Interval[K, T]) bool {
	if a.Start.Infinite != b.Start.Infinite {
		return a.Start.Infinite
	}
	if !a.Start.Infinite {
		if it.lessK(a.Start.Value, b.Start.Value) {
			return true
		}
		if it.lessK(b.Start.Value, a.Start.Value) {
			return false
		}
		if a.Start.Inclusive != b.Start.Inclusive {
			// closed sorts before open: touching intervals with closed
			// starts are reached before open ones at the same coordinate.
			return a.Start.Inclusive
		}
	}
	return it.lessID(a.ID, b.ID)
}

// recompute rebuilds a node's cached maximum end from its items and
// children. It is a pure function of the subtree contents, so it remains
// correct after Copy and keeps sharing safe.
func (it *IntervalTreeG[K, T]) recompute(n *node[Interval[K, T]]) {
	var max endBound[K]
	for i := 0; i < len(n.items); i++ {
		max.absorb(endOf(n.items[i]), it.lessK)
	}
	if !n.leaf() {
		for i := 0; i < len(*n.children); i++ {
			if c, ok := (*n.children)[i].meta.(endBound[K]); ok {
				max.absorb(c, it.lessK)
			}
		}
	}
	n.meta = max
}

// Set inserts or replaces the interval. Replacement matches the total
// item order, i.e. same start endpoint and same ID; the end and payload
// may change.
func (it *IntervalTreeG[K, T]) Set(iv Interval[K, T]) (prev Interval[K, T], replaced bool) {
	return it.tr.Set(iv)
}

// Load bulk-loads a pre-sorted interval, as in BTreeG.Load.
func (it *IntervalTreeG[K, T]) Load(iv Interval[K, T]) (prev Interval[K, T], replaced bool) {
	return it.tr.Load(iv)
}

// Delete removes the interval with the same start endpoint and ID. It
// returns the removed interval and whether one existed.
func (it *IntervalTreeG[K, T]) Delete(iv Interval[K, T]) (Interval[K, T], bool) {
	// The item order ignores the end, so locate the stored record through
	// its (start, ID) key and delete the exact value found; passing iv
	// directly could steer the search when its end differs.
	cur, ok := it.tr.Get(iv)
	if !ok {
		return iv, false
	}
	return it.tr.Delete(cur)
}

// Len returns the number of stored intervals.
func (it *IntervalTreeG[K, T]) Len() int {
	return it.tr.Len()
}

// Copy returns a copy-on-write snapshot. Modifications to either tree do
// not affect the other, and both keep correct augmented information.
func (it *IntervalTreeG[K, T]) Copy() *IntervalTreeG[K, T] {
	cp := &IntervalTreeG[K, T]{
		tr:     it.tr.Copy(),
		lessK:  it.lessK,
		lessID: it.lessID,
	}
	cp.tr.reaug = cp.recompute
	return cp
}

// Clear removes all intervals.
func (it *IntervalTreeG[K, T]) Clear() {
	it.tr.Clear()
}

// Empty reports whether the interval contains no coordinate.
func (iv Interval[K, T]) Empty(less func(a, b K) bool) bool {
	if iv.Start.Infinite || iv.End.Infinite {
		return false
	}
	if less(iv.End.Value, iv.Start.Value) {
		return true
	}
	if !less(iv.Start.Value, iv.End.Value) &&
		(!iv.Start.Inclusive || !iv.End.Inclusive) {
		return true
	}
	return false
}

// Overlaps reports whether two intervals share at least one coordinate.
// Empty intervals never overlap anything.
func (iv Interval[K, T]) Overlaps(other Interval[K, T], less func(a, b K) bool) bool {
	if iv.Empty(less) || other.Empty(less) {
		return false
	}
	// lo = max of the two starts, hi = min of the two ends.
	// No overlap iff lo > hi, or lo == hi and the touch point is excluded
	// by either side.
	if iv.Start.Infinite {
		// lo = other.Start; only the other end can bound it on the right.
		return endNotBeforeStart(other, iv.End, less)
	}
	if other.Start.Infinite {
		return endNotBeforeStart(iv, other.End, less)
	}
	// Intersection is non-empty iff neither end stops before the other
	// interval's start. Both finite ends must satisfy this; an infinite
	// end always does.
	return endNotBeforeStart(iv, other.End, less) &&
		endNotBeforeStart(other, iv.End, less)
}

// endNotBeforeStart handles the case where one start is -infinity: the
// intersection is non-empty iff the finite end does not stop before the
// other (finite) start.
func endNotBeforeStart[K comparable, T any](finiteStart Interval[K, T], bound End[K],
	less func(a, b K) bool,
) bool {
	if bound.Infinite {
		return true
	}
	if finiteStart.Start.Infinite {
		// the other interval starts at -infinity: every finite end reaches it
		return true
	}
	if less(bound.Value, finiteStart.Start.Value) {
		return false
	}
	if !less(finiteStart.Start.Value, bound.Value) {
		return bound.Inclusive && finiteStart.Start.Inclusive
	}
	return true
}

// canReach reports whether a subtree whose maximum end is maxEnd may
// contain an interval whose start is at or beyond qStart.
func canReach[K any](maxEnd endBound[K], qStart End[K],
	less func(a, b K) bool,
) bool {
	if !maxEnd.valid || maxEnd.infinite {
		return maxEnd.valid
	}
	if qStart.Infinite {
		return true
	}
	if less(maxEnd.value, qStart.Value) {
		return false
	}
	if !less(qStart.Value, maxEnd.value) {
		// equal coordinates: an open query start needs a closed max end.
		return maxEnd.inclusive || !qStart.Inclusive
	}
	return true
}

// Scan calls iter for every stored interval in ascending order of
// (start, identity), including empty intervals. Return false to stop.
func (it *IntervalTreeG[K, T]) Scan(iter func(iv Interval[K, T]) bool) {
	if it.tr.lock(false) {
		defer it.tr.unlock(false)
	}
	if it.tr.root == nil {
		return
	}
	var walk func(cn **node[Interval[K, T]]) bool
	walk = func(cn **node[Interval[K, T]]) bool {
		n := *cn
		if n.leaf() {
			for i := range n.items {
				if !iter(n.items[i]) {
					return false
				}
			}
			return true
		}
		for i := 0; i < len(n.items); i++ {
			if !walk(&(*n.children)[i]) || !iter(n.items[i]) {
				return false
			}
		}
		return walk(&(*n.children)[len(*n.children)-1])
	}
	walk(&it.tr.root)
}

// Overlaps calls iter for every stored interval that overlaps q, in
// ascending order of (start, identity). Empty intervals never match.
// Return false from iter to stop.
func (it *IntervalTreeG[K, T]) Overlaps(q Interval[K, T],
	iter func(iv Interval[K, T]) bool,
) {
	if it.tr.lock(false) {
		defer it.tr.unlock(false)
	}
	if it.tr.root == nil || q.Empty(it.lessK) {
		return
	}
	it.nodeOverlaps(&it.tr.root, q, iter)
}

// nodeOverlaps performs an in-order, pruned traversal.
func (it *IntervalTreeG[K, T]) nodeOverlaps(cn **node[Interval[K, T]],
	q Interval[K, T], iter func(iv Interval[K, T]) bool,
) bool {
	n := *cn
	// Only items whose start does not lie beyond q.End can overlap.
	// Because items and children share an ordered layout, the candidates
	// are n.items[0:itemLimit] and, in a branch, children
	// 0..itemLimit inclusive; the rest of the subtree is pruned on the
	// query's right side.
	itemLimit := len(n.items)
	if !q.End.Infinite {
		for itemLimit > 0 {
			s := n.items[itemLimit-1].Start
			if s.Infinite {
				break
			}
			if it.lessK(s.Value, q.End.Value) ||
				(!it.lessK(q.End.Value, s.Value) &&
					s.Inclusive && q.End.Inclusive) {
				break
			}
			itemLimit--
		}
	}
	if n.leaf() {
		for i := 0; i < itemLimit; i++ {
			if n.items[i].Overlaps(q, it.lessK) {
				if !iter(n.items[i]) {
					return false
				}
			}
		}
		return true
	}
	for i := 0; i < itemLimit; i++ {
		// child i
		if it.childCanReach((*n.children)[i], q.Start) &&
			!it.nodeOverlaps(&(*n.children)[i], q, iter) {
			return false
		}
		if n.items[i].Overlaps(q, it.lessK) {
			if !iter(n.items[i]) {
				return false
			}
		}
	}
	if itemLimit == len(n.items) {
		// last child
		last := len(*n.children) - 1
		if it.childCanReach((*n.children)[last], q.Start) {
			return it.nodeOverlaps(&(*n.children)[last], q, iter)
		}
	} else {
		// Child itemLimit is between the last candidate item and the first
		// item beyond q.End. It may still hold starts equal to q.End, so
		// it is visited (and pruned by the max-end check); everything to
		// its right starts strictly beyond q.End.
		if it.childCanReach((*n.children)[itemLimit], q.Start) &&
			!it.nodeOverlaps(&(*n.children)[itemLimit], q, iter) {
			return false
		}
	}
	return true
}

// childCanReach applies the max-end pruning on the query's left side.
func (it *IntervalTreeG[K, T]) childCanReach(c *node[Interval[K, T]], qStart End[K]) bool {
	mb, _ := c.meta.(endBound[K])
	return canReach(mb, qStart, it.lessK)
}
