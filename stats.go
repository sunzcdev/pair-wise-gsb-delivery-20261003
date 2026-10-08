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

import "sort"

// outRange describes the residual output reachable from a single FST state.
//
// The values are deliberately path independent: they describe ONLY the output
// that can be contributed from the state itself downwards (the state's own
// final output plus every transition output leading to a final state).  They
// never include any output accumulated by a path above the state, which is why
// the same state can be shared by many different prefixes that arrive at it
// with different accumulated values.
type outRange struct {
	min uint64
	max uint64
}

// StateStats holds read-only, state-level auxiliary statistics for an
// immutable FST.  The statistics are built entirely in memory and never alter
// the on-disk FST representation, so a single StateStats value may be built
// once and shared across any number of concurrent range queries.
type StateStats struct {
	f *FST

	// ranges maps an encoded state address to its residual output range.
	ranges map[int]outRange
}

// NewStateStats builds residual output statistics for the FST by visiting each
// reachable state exactly once.  It does not modify the FST or its backing
// bytes.  The returned StateStats is valid until the FST is closed.
func NewStateStats(f *FST) (*StateStats, error) {
	root := f.decoder.getRoot()

	// Collect every reachable state address with an explicit depth-first
	// traversal.  States are written bottom-up by the builder, so a child
	// always has a smaller address than its parent.  Sorting the addresses
	// ascending therefore gives a valid post-order for computing the
	// residual ranges.
	addrs := make([]int, 0, 256)
	seen := make(map[int]struct{}, 256)

	// Address emptyAddr (0) is the shared implicit final state and is a
	// valid transition destination, so seed it explicitly.
	seen[emptyAddr] = struct{}{}
	addrs = append(addrs, emptyAddr)

	stack := []int{root}
	for len(stack) > 0 {
		addr := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		addrs = append(addrs, addr)

		state, err := f.decoder.stateAt(addr, nil)
		if err != nil {
			return nil, err
		}
		for i := 0; i < state.NumTransitions(); i++ {
			ch := state.TransitionAt(i)
			_, dest, _ := state.TransitionFor(ch)
			if dest == noneAddr {
				continue
			}
			if _, ok := seen[dest]; !ok {
				stack = append(stack, dest)
			}
		}
	}

	sort.Ints(addrs)

	s := &StateStats{
		f:      f,
		ranges: make(map[int]outRange, len(addrs)),
	}

	// The implicit empty state is final with a zero output and contributes
	// nothing on its own.
	s.ranges[emptyAddr] = outRange{min: 0, max: 0}

	for _, addr := range addrs {
		if addr == emptyAddr {
			continue
		}
		state, err := f.decoder.stateAt(addr, nil)
		if err != nil {
			return nil, err
		}
		s.ranges[addr] = s.computeRange(state)
	}

	return s, nil
}

// computeRange derives the residual output range of a state from the already
// computed ranges of its transition destinations (smaller addresses).
func (s *StateStats) computeRange(state fstState) outRange {
	var r outRange
	have := false

	if state.Final() {
		v := state.FinalOutput()
		r = outRange{min: v, max: v}
		have = true
	}

	for i := 0; i < state.NumTransitions(); i++ {
		ch := state.TransitionAt(i)
		_, dest, out := state.TransitionFor(ch)
		if dest == noneAddr {
			continue
		}
		child, ok := s.ranges[dest]
		if !ok {
			// Unreachable destination; ignore.  Every reachable
			// destination is present due to post-order processing.
			continue
		}
		cand := outRange{
			min: out + child.min,
			max: saturatingAdd(out, child.max),
		}
		if !have {
			r = cand
			have = true
			continue
		}
		if cand.min < r.min {
			r.min = cand.min
		}
		if cand.max > r.max {
			r.max = cand.max
		}
	}

	return r
}

// saturatingAdd returns a+b, saturating at math.MaxUint64 instead of
// wrapping around.
func saturatingAdd(a, b uint64) uint64 {
	sum := a + b
	if sum < a { // overflow
		return ^uint64(0)
	}
	return sum
}
