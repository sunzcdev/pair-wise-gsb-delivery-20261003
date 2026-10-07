// Copyright IBM Corp. 2015, 2025
// SPDX-License-Identifier: MPL-2.0

package iradix

import "sort"

// maxFuzzyDistance bounds the amount of edit-distance state kept per
// active branch. Queries whose threshold exceeds this are rejected before
// any node is visited so the scan cannot allocate unbounded state.
const maxFuzzyDistance = 64

// FuzzyMatch is a single result of a fuzzy key lookup.
type FuzzyMatch[T any] struct {
	// Key is the stored key. The slice is shared with the immutable tree
	// and must not be modified by the caller.
	Key []byte

	// Value is the value stored under Key.
	Value T

	// Distance is the Levenshtein edit distance between the query key and
	// Key, measured over bytes using insert, delete and substitute
	// operations, each costing one edit.
	Distance int
}

// FuzzyLookup returns the stored keys whose byte-wise Levenshtein edit
// distance from k is at most maxDist. Results are ordered by distance
// first and then by key in ascending byte order.
//
// The dynamic-programming state is advanced one byte at a time along the
// compressed node prefixes, so branches whose minimum possible distance
// already exceeds the threshold are skipped without ever materializing
// or comparing the stored keys.
//
// If limit is positive, at most limit results are returned. When more
// than limit keys qualify, the returned results are still the true first
// limit items under the (distance, key) ordering. Every branch within
// maxDist is still explored; a bounded max-heap retains the best limit
// matches so ties in distance are resolved correctly by key order. A
// non-positive limit returns every matching key.
//
// The tree is never mutated. maxDist must not be negative and must not
// exceed 64; otherwise FuzzyLookup returns nil.
func (t *Tree[T]) FuzzyLookup(k []byte, maxDist, limit int) []FuzzyMatch[T] {
	if maxDist < 0 || maxDist > maxFuzzyDistance {
		return nil
	}
	s := &fuzzySearch[T]{
		query:  k,
		limit:  limit,
		cutoff: maxDist,
	}
	row := make([]int, len(k)+1)
	for j := range row {
		row[j] = j
	}
	rootRow, ok := s.advancePrefix(t.root.prefix, row)
	if ok {
		s.dfs(t.root, rootRow)
	}

	// matches may be held in heap order; establish (distance, key) order.
	sort.Slice(s.matches, func(i, j int) bool {
		return fuzzyWorse(s.matches[j], s.matches[i])
	})
	return s.matches
}

// fuzzySearch carries the state of a fuzzy lookup. matches doubles as a
// bounded max-heap keyed by (distance, key descending) so its root is the
// worst accepted result; it is only bounded when a positive limit is set.
type fuzzySearch[T any] struct {
	query   []byte
	limit   int
	cutoff  int
	matches []FuzzyMatch[T]
	visited int
}

// fuzzyWorse reports whether a ranks strictly after b in the desired
// output ordering, i.e. larger distance, or equal distance and a larger
// key in byte order.
func fuzzyWorse[T any](a, b FuzzyMatch[T]) bool {
	if a.Distance != b.Distance {
		return a.Distance > b.Distance
	}
	return compareBytes(a.Key, b.Key) > 0
}

// compareBytes returns -1, 0 or 1 comparing two byte slices lexicographically.
func compareBytes(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	default:
		return 0
	}
}

// accept records a qualifying leaf, maintaining the bounded heap when a
// limit is in effect. The traversal cutoff stays at the caller's threshold:
// a branch tied with the current worst result may still hold a key that is
// lexicographically smaller, so it must be explored and lose in the heap.
func (s *fuzzySearch[T]) accept(m FuzzyMatch[T]) {
	if s.limit <= 0 {
		s.matches = append(s.matches, m)
		return
	}
	if len(s.matches) < s.limit {
		s.matches = append(s.matches, m)
		s.heapUp(len(s.matches) - 1)
	} else if fuzzyWorse(m, s.matches[0]) {
		// m is worse than the current worst retained result; drop it.
		return
	} else {
		s.matches[0] = m
		s.heapDown(0)
	}
}

// heapUp and heapDown maintain a max-heap ordered by fuzzyWorse.
func (s *fuzzySearch[T]) heapUp(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if fuzzyWorse(s.matches[i], s.matches[parent]) {
			s.matches[parent], s.matches[i] = s.matches[i], s.matches[parent]
			i = parent
			continue
		}
		return
	}
}

func (s *fuzzySearch[T]) heapDown(i int) {
	for {
		left := 2*i + 1
		right := left + 1
		worst := i
		if left < len(s.matches) && fuzzyWorse(s.matches[left], s.matches[worst]) {
			worst = left
		}
		if right < len(s.matches) && fuzzyWorse(s.matches[right], s.matches[worst]) {
			worst = right
		}
		if worst == i {
			return
		}
		s.matches[i], s.matches[worst] = s.matches[worst], s.matches[i]
		i = worst
	}
}

// dfs visits n with a row that already reflects n's compressed prefix.
// The row holds edit distances between every query prefix and the
// accumulated key path up to this node.
func (s *fuzzySearch[T]) dfs(n *Node[T], row []int) {
	s.visited++

	if n.leaf != nil {
		if d := row[len(row)-1]; d <= s.cutoff {
			s.accept(FuzzyMatch[T]{Key: n.leaf.key, Value: n.leaf.val, Distance: d})
		}
	}

	// Edges are stored in ascending label order. The DP state is pushed
	// through the child's whole compressed prefix before the child node is
	// entered, so an edge whose subtree cannot contain a match is skipped
	// without visiting its nodes or materializing its keys.
	for i := range n.edges {
		child := n.edges[i].node
		childRow, ok := s.advancePrefix(child.prefix, row)
		if !ok {
			continue
		}
		s.dfs(child, childRow)
	}
}

// advancePrefix pushes the edit-distance row through every byte of a
// compressed edge. It returns the resulting row, or ok == false once the
// minimum possible distance of any state cell exceeds the cutoff: every
// key past this point extends the path further and can only add edits.
func (s *fuzzySearch[T]) advancePrefix(prefix []byte, row []int) ([]int, bool) {
	for pi := 0; pi < len(prefix); pi++ {
		pb := prefix[pi]
		next := make([]int, len(row))
		next[0] = row[0] + 1
		minimum := next[0]
		for qi := 1; qi < len(row); qi++ {
			cost := 0
			if s.query[qi-1] != pb {
				cost = 1
			}
			del := row[qi] + 1      // delete a query byte
			ins := next[qi-1] + 1   // insert the prefix byte
			sub := row[qi-1] + cost // substitute (or match)
			d := del
			if ins < d {
				d = ins
			}
			if sub < d {
				d = sub
			}
			next[qi] = d
			if d < minimum {
				minimum = d
			}
		}
		if minimum > s.cutoff {
			return nil, false
		}
		row = next
	}
	return row, true
}
