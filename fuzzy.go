// Copyright IBM Corp. 2015, 2025
// SPDX-License-Identifier: MPL-2.0

package iradix

import (
	"bytes"
	"sort"
)

// FuzzyMatch is a single result of FuzzySearch. It holds a copy of the
// matched key, its associated value, and the Levenshtein edit distance
// (counting byte insertions, deletions and substitutions) between the
// key and the query.
type FuzzyMatch[T any] struct {
	Key      []byte
	Val      T
	Distance int
}

// FuzzySearch returns the keys of the tree within maxDist byte edits
// (insertions, deletions and substitutions) of the query. The dynamic
// programming state is carried along the compressed edges of the tree so
// that branches which cannot possibly contain a match are pruned without
// ever materializing or comparing their keys. The tree and its values are
// only read, never modified.
//
// Results are sorted by ascending edit distance, breaking ties by key
// byte order. If limit is positive, at most limit results are returned;
// they are always the true top matches according to that ordering, and
// the limit is also used to tighten pruning as the search proceeds. A
// limit <= 0 means no limit.
func (t *Tree[T]) FuzzySearch(query []byte, maxDist int, limit int) []FuzzyMatch[T] {
	matches, _ := t.fuzzySearch(query, maxDist, limit)
	return matches
}

// fuzzySearch implements FuzzySearch and additionally reports the number
// of tree nodes that were examined, which demonstrates how much of the
// tree was pruned away.
func (t *Tree[T]) fuzzySearch(query []byte, maxDist int, limit int) ([]FuzzyMatch[T], int) {
	if maxDist < 0 {
		return nil, 0
	}
	s := &fuzzySearcher[T]{
		query:   query,
		maxDist: maxDist,
		limit:   limit,
	}

	// The initial row is the distance between the empty key prefix and
	// every prefix of the query.
	row := make([]int, len(query)+1)
	for i := range row {
		row[i] = i
	}
	s.walk(t.root, row)
	return s.results, s.visited
}

// fuzzySearcher holds the state of a single FuzzySearch.
type fuzzySearcher[T any] struct {
	query   []byte
	maxDist int
	limit   int

	// results is kept sorted by (distance, key) and truncated to limit.
	results []FuzzyMatch[T]

	// visited counts the nodes examined, used to verify pruning.
	visited int
}

// threshold is the effective pruning and acceptance bound. Once the
// result set is full, anything worse than the current worst result can
// never make the cut, so the bound tightens as matches are found.
func (s *fuzzySearcher[T]) threshold() int {
	if s.limit > 0 && len(s.results) == s.limit {
		if d := s.results[len(s.results)-1].Distance; d < s.maxDist {
			return d
		}
	}
	return s.maxDist
}

// walk advances the DP row along the compressed prefix of n and recurses
// into the children of n in key order.
func (s *fuzzySearcher[T]) walk(n *Node[T], row []int) {
	s.visited++

	// Push the DP state along every byte of the compressed edge. The
	// minimum of a row never decreases as more key bytes are consumed,
	// so once it exceeds the threshold the whole subtree is hopeless.
	for _, c := range n.prefix {
		row = s.advance(row, c)
		if minRow(row) > s.threshold() {
			return
		}
	}

	// A leaf on this node is a key exactly matching the path so far.
	if n.leaf != nil {
		if d := row[len(s.query)]; d <= s.threshold() {
			s.add(n.leaf, d)
		}
	}

	for _, e := range n.edges {
		s.walk(e.node, row)
	}
}

// advance computes the next DP row after consuming the key byte c.
// row[j] is the edit distance between the key prefix consumed so far and
// query[:j].
func (s *fuzzySearcher[T]) advance(prev []int, c byte) []int {
	m := len(s.query)
	next := make([]int, m+1)
	next[0] = prev[0] + 1
	for j := 1; j <= m; j++ {
		cost := 1
		if s.query[j-1] == c {
			cost = 0
		}
		next[j] = minInt3(
			prev[j]+1,      // delete c from the key
			next[j-1]+1,    // insert c into the key
			prev[j-1]+cost, // keep or substitute
		)
	}
	return next
}

// add records a match, keeping results sorted by (distance, key) and
// bounded by the limit. Keys are visited in ascending order, so a full
// result set can only be displaced by a strictly smaller distance.
func (s *fuzzySearcher[T]) add(leaf *leafNode[T], d int) {
	if s.limit > 0 && len(s.results) == s.limit && d >= s.results[len(s.results)-1].Distance {
		return
	}
	key := make([]byte, len(leaf.key))
	copy(key, leaf.key)
	match := FuzzyMatch[T]{Key: key, Val: leaf.val, Distance: d}

	idx := sort.Search(len(s.results), func(i int) bool {
		r := s.results[i]
		if r.Distance != d {
			return r.Distance > d
		}
		return bytes.Compare(r.Key, key) >= 0
	})
	s.results = append(s.results, FuzzyMatch[T]{})
	copy(s.results[idx+1:], s.results[idx:])
	s.results[idx] = match

	if s.limit > 0 && len(s.results) > s.limit {
		s.results = s.results[:s.limit]
	}
}

func minRow(row []int) int {
	m := row[0]
	for _, v := range row[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func minInt3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
