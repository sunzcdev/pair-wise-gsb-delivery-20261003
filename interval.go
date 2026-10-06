// Copyright 2020 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package btree

import (
	"math"
	"sync"
)

// Interval is a one-dimensional interval with a unique identity.
//
// Lo and Hi are the lower and upper endpoints. An endpoint may be
// unbounded by using math.Inf(-1) for Lo or math.Inf(1) for Hi.
// LoOpen and HiOpen declare the corresponding endpoint as open
// (excluded from the interval); by default endpoints are closed
// (included). The openness of an unbounded endpoint is meaningless and
// ignored.
//
// An interval is empty when it contains no points: Lo > Hi, or
// Lo == Hi with at least one open endpoint, or Lo == Hi at an infinity
// (infinities are not points of the domain). Empty intervals may be
// stored and deleted like any other interval, but they never overlap
// anything, including other empty intervals.
//
// Every interval stored in an IntervalIndex carries a unique ID,
// assigned by Insert, so intervals with identical bounds are distinct
// entries. NaN endpoints are invalid and cause Insert and the query
// methods to panic.
type Interval struct {
	ID     uint64
	Lo     float64
	Hi     float64
	LoOpen bool
	HiOpen bool
}

// Closed returns the closed interval [lo, hi].
func Closed(lo, hi float64) Interval {
	return Interval{Lo: lo, Hi: hi}
}

// Open returns the open interval (lo, hi).
func Open(lo, hi float64) Interval {
	return Interval{Lo: lo, Hi: hi, LoOpen: true, HiOpen: true}
}

// ClosedOpen returns the half-open interval [lo, hi).
func ClosedOpen(lo, hi float64) Interval {
	return Interval{Lo: lo, Hi: hi, HiOpen: true}
}

// OpenClosed returns the half-open interval (lo, hi].
func OpenClosed(lo, hi float64) Interval {
	return Interval{Lo: lo, Hi: hi, LoOpen: true}
}

// Every returns the unbounded interval (-inf, +inf), which overlaps
// every non-empty interval.
func Every() Interval {
	return Interval{Lo: math.Inf(-1), Hi: math.Inf(1)}
}

// Empty reports whether the interval contains no points.
func (iv Interval) Empty() bool {
	if iv.Lo > iv.Hi {
		return true
	}
	if iv.Lo == iv.Hi {
		if iv.LoOpen || iv.HiOpen {
			return true
		}
		// a degenerate interval at an infinity contains no domain point
		if math.IsInf(iv.Lo, 0) {
			return true
		}
	}
	return false
}

// Overlaps reports whether two intervals share at least one point.
// Empty intervals never overlap anything.
func (iv Interval) Overlaps(o Interval) bool {
	if iv.Empty() || o.Empty() {
		return false
	}
	if iv.Lo > o.Hi || (iv.Lo == o.Hi && (iv.LoOpen || o.HiOpen)) {
		return false
	}
	if o.Lo > iv.Hi || (o.Lo == iv.Hi && (o.LoOpen || iv.HiOpen)) {
		return false
	}
	return true
}

// effectiveHi is the upper endpoint used for augmented pruning. Empty
// intervals can never match a query, so they do not contribute.
func (iv Interval) effectiveHi() float64 {
	if iv.Empty() {
		return math.Inf(-1)
	}
	return iv.Hi
}

// intervalLess is the total order used by the index: by lower endpoint,
// with a closed endpoint sorting before an open one at the same value
// (it starts earlier), then by identity. This makes scans deterministic
// and independent of insertion or map iteration order.
func intervalLess(a, b Interval) bool {
	if a.Lo != b.Lo {
		return a.Lo < b.Lo
	}
	if a.LoOpen != b.LoOpen {
		return b.LoOpen
	}
	return a.ID < b.ID
}

// intervalAugmentor maintains, on every tree node, the maximum
// effectiveHi of all intervals in the node's subtree. A subtree cannot
// contain a match for a query when its maximum is below the query's
// lower endpoint, which allows the query to skip the subtree entirely.
type intervalAugmentor struct{}

func (intervalAugmentor) augment(items []Interval,
	children *[]*node[Interval],
) any {
	max := math.Inf(-1)
	for i := range items {
		if h := items[i].effectiveHi(); h > max {
			max = h
		}
	}
	if children != nil {
		for _, c := range *children {
			if h, ok := c.aug.(float64); ok && h > max {
				max = h
			}
		}
	}
	return max
}

// IntervalIndex is an interval index built on the B-tree. It answers
// overlap queries ("find all intervals overlapping a range") without
// scanning past intervals that cannot match: each tree node tracks the
// maximum upper endpoint of its subtree, so whole subtrees are pruned
// during a query. Results are returned in a stable order, sorted by
// lower endpoint and identity.
//
// Create one with NewIntervalIndex. An IntervalIndex is safe for
// concurrent use.
type IntervalIndex struct {
	mu   sync.RWMutex
	tr   *BTreeG[Interval]
	byID map[uint64]Interval
	seq  uint64
}

// NewIntervalIndex returns a new, empty IntervalIndex.
func NewIntervalIndex() *IntervalIndex {
	return newIntervalIndexOptions(Options{})
}

func newIntervalIndexOptions(opts Options) *IntervalIndex {
	opts.NoLocks = true // locking is managed by IntervalIndex itself
	tr := NewBTreeGOptions(intervalLess, opts)
	tr.aug = intervalAugmentor{}
	return &IntervalIndex{tr: tr, byID: make(map[uint64]Interval)}
}

func validEndpoint(x float64) {
	if math.IsNaN(x) {
		panic("interval endpoint is NaN")
	}
}

// Insert adds an interval to the index and returns its identity.
// The ID field of the provided interval is ignored; a fresh, unique
// identity is assigned and returned. Panics on NaN endpoints.
func (ix *IntervalIndex) Insert(iv Interval) uint64 {
	validEndpoint(iv.Lo)
	validEndpoint(iv.Hi)
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.seq++
	iv.ID = ix.seq
	ix.tr.Set(iv)
	ix.byID[iv.ID] = iv
	return iv.ID
}

// Delete removes the interval with the given identity.
// Returns false if no such interval exists.
func (ix *IntervalIndex) Delete(id uint64) bool {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	iv, ok := ix.byID[id]
	if !ok {
		return false
	}
	delete(ix.byID, id)
	ix.tr.Delete(iv)
	return true
}

// Get returns the interval with the given identity.
func (ix *IntervalIndex) Get(id uint64) (Interval, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	iv, ok := ix.byID[id]
	return iv, ok
}

// Len returns the number of intervals in the index.
func (ix *IntervalIndex) Len() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.tr.Len()
}

// Clear removes all intervals from the index.
func (ix *IntervalIndex) Clear() {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.tr.Clear()
	ix.byID = make(map[uint64]Interval)
}

// Copy returns an independent copy of the index. The underlying tree is
// copied copy-on-write, so it is cheap; later modifications of either
// index keep both correct.
func (ix *IntervalIndex) Copy() *IntervalIndex {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	byID := make(map[uint64]Interval, len(ix.byID))
	for id, iv := range ix.byID {
		byID[id] = iv
	}
	return &IntervalIndex{tr: ix.tr.Copy(), byID: byID, seq: ix.seq}
}

// Query returns all intervals overlapping q, sorted by lower endpoint
// and identity. An empty query interval matches nothing.
// Panics on NaN endpoints.
func (ix *IntervalIndex) Query(q Interval) []Interval {
	var out []Interval
	ix.QueryIter(q, func(iv Interval) bool {
		out = append(out, iv)
		return true
	})
	return out
}

// QueryIter calls iter for every interval overlapping q, in ascending
// order of lower endpoint and identity. Returning false from iter stops
// the iteration. The iterator must not modify the index.
func (ix *IntervalIndex) QueryIter(q Interval, iter func(Interval) bool) {
	validEndpoint(q.Lo)
	validEndpoint(q.Hi)
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if q.Empty() || ix.tr.root == nil {
		return
	}
	intervalNodeQuery(ix.tr.root, q, iter)
}

// subtreePruned reports whether a subtree whose maximum effective upper
// endpoint is maxHi can be skipped for the query: no interval in it can
// reach the query's lower endpoint.
func subtreePruned(maxHi float64, q Interval) bool {
	return maxHi < q.Lo || (maxHi == q.Lo && q.LoOpen)
}

// loBeyondQuery reports whether the interval's lower endpoint is past
// the query's upper endpoint. Because items are scanned in ascending
// order, everything after such an item cannot match either.
func loBeyondQuery(iv Interval, q Interval) bool {
	return iv.Lo > q.Hi || (iv.Lo == q.Hi && (iv.LoOpen || q.HiOpen))
}

func intervalNodeQuery(n *node[Interval], q Interval,
	iter func(Interval) bool,
) bool {
	if maxHi, ok := n.aug.(float64); ok && subtreePruned(maxHi, q) {
		return true
	}
	if n.leaf() {
		for _, iv := range n.items {
			if loBeyondQuery(iv, q) {
				return true
			}
			if iv.Overlaps(q) && !iter(iv) {
				return false
			}
		}
		return true
	}
	for i := 0; i < len(n.items); i++ {
		iv := n.items[i]
		if loBeyondQuery(iv, q) {
			// items and children to the right sort even higher, but
			// children[i] may still contain smaller keys.
			return intervalNodeQuery((*n.children)[i], q, iter)
		}
		if !intervalNodeQuery((*n.children)[i], q, iter) {
			return false
		}
		if iv.Overlaps(q) && !iter(iv) {
			return false
		}
	}
	return intervalNodeQuery((*n.children)[len(n.items)], q, iter)
}
