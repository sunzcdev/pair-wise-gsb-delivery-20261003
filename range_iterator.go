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

// RangeIterator enumerates, in lexicographic key order, the keys that start
// with a given prefix AND whose associated uint64 value falls within the
// inclusive interval [minVal, maxVal].
//
// It is a read-only view over an immutable FST.  State-level residual output
// statistics (StateStats) are used to prune whole branches whose values
// cannot satisfy the interval, without ever descending into them.
//
// A RangeIterator is meant for single-goroutine use, but many iterators may
// safely share the same underlying FST and StateStats.
type RangeIterator struct {
	f      *FST
	stats  *StateStats
	prefix []byte
	minVal uint64
	maxVal uint64

	// Per-depth traversal frames.  states[k] is the FST state reached after
	// consuming key[0:k]; idx[k] is the next transition index to try.
	states      []fstState
	idx         []int
	testedFinal []bool

	// Current key path and the output accumulated along it.
	key  []byte
	vals []uint64

	// curKey/curVal are valid only after a successful Next()/First().
	curKey []byte
	curVal uint64
	done   bool
}

// RangeIterator returns a read-only iterator over the keys beginning with
// prefix whose associated value is within the inclusive interval
// [minVal, maxVal], returned in lexicographic key order.
//
// The residual output statistics are shared (not mutated) and used to prune
// branches that cannot satisfy the interval.  Boundary cases are supported:
// minVal may be zero, maxVal may be math.MaxUint64, and an empty interval
// (minVal > maxVal) yields an iterator that is immediately done.
//
// A nil prefix enumerates the entire FST.  The returned iterator is for
// single-goroutine use; many iterators may share the same StateStats.
func (s *StateStats) RangeIterator(prefix []byte, minVal, maxVal uint64) (*RangeIterator, error) {
	return newRangeIterator(s.f, s, prefix, minVal, maxVal)
}

// newRangeIterator positions the iterator at the first matching key.  An
// empty range (minVal > maxVal) yields an immediately-done iterator.
func newRangeIterator(f *FST, stats *StateStats, prefix []byte,
	minVal, maxVal uint64) (*RangeIterator, error) {

	it := &RangeIterator{
		f:      f,
		stats:  stats,
		prefix: append([]byte(nil), prefix...),
		minVal: minVal,
		maxVal: maxVal,
	}

	if minVal > maxVal {
		it.done = true
		return it, nil
	}

	err := it.seekPrefix()
	if err != nil {
		return nil, err
	}
	if !it.done {
		err = it.advance()
		if err != nil {
			return nil, err
		}
	}
	return it, nil
}

// seekPrefix walks the mandatory prefix bytes.  On success the root frame of
// the prefix subtree is the sole frame; otherwise the iterator is marked done.
func (it *RangeIterator) seekPrefix() error {
	root, err := it.f.decoder.stateAt(it.f.decoder.getRoot(), nil)
	if err != nil {
		return err
	}
	it.states = append(it.states, root)
	it.idx = append(it.idx, 0)
	it.testedFinal = append(it.testedFinal, false)

	curr := root
	for _, c := range it.prefix {
		_, nextAddr, out := curr.TransitionFor(c)
		if nextAddr == noneAddr {
			it.done = true
			return nil
		}
		next, err := it.f.decoder.stateAt(nextAddr, nil)
		if err != nil {
			return err
		}
		it.states = append(it.states, next)
		it.idx = append(it.idx, 0)
		it.testedFinal = append(it.testedFinal, false)
		it.vals = append(it.vals, out)
		curr = next
	}

	// The key buffer always contains the matched prefix; descendants extend
	// it.  The accumulated prefix output lives on vals.
	it.key = append(it.key[:0], it.prefix...)

	// Even the prefix state itself may be a match (prefix is itself a key).
	// advance() begins by testing the current frame as a final state.
	return nil
}

// canContain reports whether a state at the current depth, reached with the
// given accumulated output, can possibly produce a value inside the interval.
func (it *RangeIterator) canContain(addr int, acc uint64) bool {
	r, ok := it.stats.ranges[addr]
	if !ok {
		// No auxiliary info available; stay conservative and descend.
		return true
	}
	low := acc + r.min
	// The upper bound is clamped to MaxUint64 to avoid wrapping.
	high := saturatingAdd(acc, r.max)
	return high >= it.minVal && low <= it.maxVal
}

// Next advances to the next matching key/value pair in key order.  It returns
// ErrIteratorDone when no more matching pairs exist.
func (it *RangeIterator) Next() error {
	if it.done {
		return ErrIteratorDone
	}
	if err := it.advance(); err != nil {
		return err
	}
	if it.done {
		return ErrIteratorDone
	}
	return nil
}

// Current returns the key/value pair currently pointed to.  The returned key
// is only valid until the next call to Next(); copy it if you need it longer.
func (it *RangeIterator) Current() ([]byte, uint64) {
	if it.done {
		return nil, 0
	}
	return it.curKey, it.curVal
}

// Close releases resources held by the iterator.
func (it *RangeIterator) Close() error {
	return nil
}

// advance performs an in-order depth-first walk from the current position,
// pruning branches via the residual output statistics, and stops at the next
// final state whose total value lies within [minVal, maxVal].
func (it *RangeIterator) advance() error {
	for len(it.states) > 0 {
		depth := len(it.states) - 1
		state := it.states[depth]

		// On first arrival at a frame consider the state itself as a final
		// match.  When we return here after exploring a child, the flag is
		// already set so the final check is not repeated.
		if !it.testedFinal[depth] {
			it.testedFinal[depth] = true
			if state.Final() {
				total := it.acc() + state.FinalOutput()
				if total >= it.minVal && total <= it.maxVal {
					it.curKey = append(it.curKey[:0], it.key...)
					it.curVal = total
					return nil
				}
			}
		}

		n := state.NumTransitions()
		advanced := false

		for it.idx[depth] < n {
			t := state.TransitionAt(it.idx[depth])
			_, nextAddr, out := state.TransitionFor(t)
			it.idx[depth]++

			nextAcc := it.acc() + out
			if !it.canContain(nextAddr, nextAcc) {
				continue
			}

			next, err := it.f.decoder.stateAt(nextAddr, it.preallocState())
			if err != nil {
				return err
			}

			it.states = append(it.states, next)
			it.idx = append(it.idx, 0)
			it.testedFinal = append(it.testedFinal, false)
			it.key = append(it.key, t)
			it.vals = append(it.vals, out)
			advanced = true
			break
		}

		if advanced {
			continue
		}

		// Exhausted this state; pop back to the parent.
		if depth <= len(it.prefix) {
			// The frame at index len(prefix) is the prefix state itself.
			// Backtracking any further would leave the prefix subtree and
			// enumerate keys that do not share the prefix, so stop.
			it.done = true
			return nil
		}
		it.pop()
	}

	it.done = true
	return nil
}

func (it *RangeIterator) acc() uint64 {
	var total uint64
	for _, v := range it.vals {
		total += v
	}
	return total
}

func (it *RangeIterator) preallocState() fstState {
	if len(it.states) < cap(it.states) {
		if s, ok := it.states[0:cap(it.states)][len(it.states)].(*fstStateV1); ok && s != nil {
			return s
		}
	}
	return nil
}

func (it *RangeIterator) pop() {
	depth := len(it.states) - 1
	it.states = it.states[:depth]
	it.idx = it.idx[:depth]
	it.testedFinal = it.testedFinal[:depth]
	// Remove the suffix byte (and its output) added when entering this
	// state.  pop() is only reached for frames strictly below the prefix
	// state, so the trimmed byte is always a suffix byte, never a prefix
	// byte.
	it.key = it.key[:len(it.key)-1]
	it.vals = it.vals[:len(it.vals)-1]
}
