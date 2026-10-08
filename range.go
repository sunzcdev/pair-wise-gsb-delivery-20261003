//  Copyright (c) 2026 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 		http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vellum

import "math"

// stateRange describes the minimum and maximum residual output reachable
// from a single FST state.  The values are relative to the state itself
// (i.e. they do NOT include outputs accumulated along any particular path
// leading to the state), so they remain valid no matter how many
// different prefixes share the state.
type stateRange struct {
	min uint64
	max uint64
}

// OutputStats holds per-state auxiliary statistics computed from an
// immutable FST.  For every state that can reach at least one final
// state, it records the minimum and maximum residual output (the sum of
// transition outputs plus the final output) over all paths from that
// state to a final state.  Because the values are residual, states shared
// by multiple suffixes carry a single, path-independent description.
//
// Building the statistics is a read-only operation: it never mutates the
// FST, its underlying data, or its file format.  The statistics may be
// reused for any number of range queries against the FST they were built
// from.
type OutputStats struct {
	ranges map[int]stateRange
}

// Len returns the number of states for which statistics were computed.
func (s *OutputStats) Len() int {
	if s == nil {
		return 0
	}
	return len(s.ranges)
}

// satAdd adds two uint64 values, saturating to math.MaxUint64 on
// overflow.  It is used only for bound computations, so saturation can
// never make a reachable range smaller than it truly is.
func satAdd(a, b uint64) uint64 {
	s := a + b
	if s < a {
		return math.MaxUint64
	}
	return s
}

// BuildOutputStats computes residual min/max output statistics for every
// state of the provided immutable FST.  The FST is only read, never
// modified.  The returned statistics can be passed to RangeIterator to
// prune branches that cannot possibly satisfy a value range.
func BuildOutputStats(f *FST) (*OutputStats, error) {
	rv := &OutputStats{
		ranges: make(map[int]stateRange),
	}

	rootState, err := f.decoder.stateAt(f.decoder.getRoot(), nil)
	if err != nil {
		return nil, err
	}

	type frame struct {
		state fstState
		next  int
	}

	// Iterative post-order traversal of the state graph.  The graph is a
	// DAG, so a state already on the stack can never be encountered again
	// as a descendant; a state seen more than once is always fully
	// computed before its second visit.
	stack := []frame{{state: rootState}}
	for len(stack) > 0 {
		top := &stack[len(stack)-1]

		if top.next < top.state.NumTransitions() {
			t := top.state.TransitionAt(top.next)
			top.next++
			_, dest, _ := top.state.TransitionFor(t)
			if _, ok := rv.ranges[dest]; ok {
				continue
			}
			child, err := f.decoder.stateAt(dest, nil)
			if err != nil {
				return nil, err
			}
			stack = append(stack, frame{state: child})
			continue
		}

		// All children of this state have been computed, fold their
		// residual ranges (plus the transition outputs) into this
		// state's own residual range.
		var lo, hi uint64
		found := false
		if top.state.Final() {
			lo, hi = top.state.FinalOutput(), top.state.FinalOutput()
			found = true
		}
		for j := 0; j < top.state.NumTransitions(); j++ {
			t := top.state.TransitionAt(j)
			_, dest, out := top.state.TransitionFor(t)
			cr, ok := rv.ranges[dest]
			if !ok {
				// child cannot reach any final state
				continue
			}
			clo := satAdd(out, cr.min)
			chi := satAdd(out, cr.max)
			if !found || clo < lo {
				lo = clo
			}
			if !found || chi > hi {
				hi = chi
			}
			found = true
		}
		if found {
			rv.ranges[top.state.Address()] = stateRange{min: lo, max: hi}
		}

		stack = stack[:len(stack)-1]
	}

	return rv, nil
}

// RangeIterator enumerates, in lexicographic key order, the key/value
// pairs of an FST whose key starts with a given prefix and whose value
// falls within an inclusive [minVal, maxVal] range.  It should be
// constructed with the RangeIterator method on the FST.
type RangeIterator struct {
	f      *FST
	stats  *OutputStats
	minVal uint64
	maxVal uint64

	statesStack []fstState
	keysStack   []byte
	valsStack   []uint64
	posStack    []int

	baseDepth int

	currVal uint64
	done    bool
}

// RangeIterator returns an iterator over the key/value pairs whose key
// starts with prefix and whose value satisfies minVal <= value <= maxVal.
// Results are produced in lexicographic key order.  An empty interval
// (minVal > maxVal) yields no results.  If stats is non-nil it MUST have
// been built from this same FST with BuildOutputStats, and is used to
// skip branches that cannot contain a matching value; if stats is nil no
// pruning is performed and every candidate is visited.
func (f *FST) RangeIterator(stats *OutputStats, prefix []byte,
	minVal, maxVal uint64) (*RangeIterator, error) {

	rv := &RangeIterator{
		f:      f,
		stats:  stats,
		minVal: minVal,
		maxVal: maxVal,
	}

	if minVal > maxVal {
		// empty interval matches nothing
		rv.done = true
		return rv, nil
	}

	// descend along the prefix, accumulating transition outputs
	curr, err := f.decoder.stateAt(f.decoder.getRoot(), nil)
	if err != nil {
		return nil, err
	}
	var total uint64
	rv.statesStack = append(rv.statesStack, curr)
	rv.valsStack = append(rv.valsStack, 0)
	rv.posStack = append(rv.posStack, -1)
	for _, c := range prefix {
		_, nextAddr, out := curr.TransitionFor(c)
		if nextAddr == noneAddr {
			// prefix not present, no results
			rv.done = true
			return rv, nil
		}
		total += out
		curr, err = f.decoder.stateAt(nextAddr, nil)
		if err != nil {
			return nil, err
		}
		rv.statesStack = append(rv.statesStack, curr)
		rv.keysStack = append(rv.keysStack, c)
		rv.valsStack = append(rv.valsStack, total)
		rv.posStack = append(rv.posStack, -1)
	}

	// the whole subtree under the prefix may be prunable
	if rv.prunes(total, curr.Address()) {
		rv.done = true
	}

	// never backtrack past the states making up the prefix
	rv.baseDepth = len(rv.statesStack)

	return rv, nil
}

// prunes reports whether the subtree rooted at the state with the given
// address, reached with accumulated value acc, can be skipped because no
// value under it can fall inside [minVal, maxVal].
func (i *RangeIterator) prunes(acc uint64, addr int) bool {
	if i.stats == nil {
		return false
	}
	r, ok := i.stats.ranges[addr]
	if !ok {
		// no final state is reachable from here
		return true
	}
	lo := satAdd(acc, r.min)
	hi := satAdd(acc, r.max)
	return hi < i.minVal || lo > i.maxVal
}

// Current returns the key and value currently pointed to by the iterator.
// The []byte of the key is ONLY guaranteed to be valid until another call
// to Next/Close.  If you need it beyond that point you MUST make a copy.
func (i *RangeIterator) Current() ([]byte, uint64) {
	return i.keysStack, i.currVal
}

// Next advances the iterator to the next key/value pair satisfying the
// prefix and value range constraints.  If none exists, ErrIteratorDone is
// returned.
func (i *RangeIterator) Next() error {
	if i.done {
		return ErrIteratorDone
	}

	for len(i.statesStack) >= i.baseDepth {
		depth := len(i.statesStack) - 1
		curr := i.statesStack[depth]

		if i.posStack[depth] < 0 {
			// first visit to this state, check its own final output
			i.posStack[depth] = 0
			if curr.Final() {
				total := i.valsStack[depth] + curr.FinalOutput()
				if i.minVal <= total && total <= i.maxVal {
					i.currVal = total
					return nil
				}
			}
		}

		// find the next child worth descending into
		descended := false
		for j := i.posStack[depth]; j < curr.NumTransitions(); j++ {
			t := curr.TransitionAt(j)
			_, nextAddr, out := curr.TransitionFor(t)
			nextTotal := i.valsStack[depth] + out
			if i.prunes(nextTotal, nextAddr) {
				continue
			}
			next, err := i.f.decoder.stateAt(nextAddr, nil)
			if err != nil {
				return err
			}
			i.posStack[depth] = j + 1
			i.statesStack = append(i.statesStack, next)
			i.keysStack = append(i.keysStack, t)
			i.valsStack = append(i.valsStack, nextTotal)
			i.posStack = append(i.posStack, -1)
			descended = true
			break
		}
		if descended {
			continue
		}

		// exhausted this state, backtrack
		i.statesStack = i.statesStack[:depth]
		if depth > 0 {
			i.keysStack = i.keysStack[:depth-1]
		}
		i.valsStack = i.valsStack[:depth]
		i.posStack = i.posStack[:depth]
	}

	i.done = true
	return ErrIteratorDone
}

// Close frees any resources held by this iterator.
func (i *RangeIterator) Close() error {
	// at the moment we don't do anything,
	// but wanted this for API completeness
	return nil
}
