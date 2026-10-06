// Copyright IBM Corp. 2015, 2025
// SPDX-License-Identifier: MPL-2.0

package iradix

import (
	"bytes"
	"sort"
)

// PrefixMatch is the result of a single longest-prefix query issued
// through LongestPrefixBatch.
type PrefixMatch[T any] struct {
	// Key is the key stored in the tree that forms the longest matched
	// prefix of the query. It is only set when Found is true. The slice
	// aliases the key held by the tree, so it stays valid across calls
	// and must not be modified.
	Key []byte

	// Value is the value associated with Key.
	Value T

	// Found reports whether any prefix of the query was found. An empty
	// query only matches an empty key stored at the root, so an empty
	// query against a tree without an empty key reports Found as false,
	// which is distinguishable from a match on the empty key.
	Found bool
}

// prefixMatchOf builds the result for a query from the deepest leaf seen
// along its path, or a zero result if no prefix matched.
func prefixMatchOf[T any](leaf *leafNode[T]) PrefixMatch[T] {
	if leaf == nil {
		return PrefixMatch[T]{}
	}
	return PrefixMatch[T]{Key: leaf.key, Value: leaf.val, Found: true}
}

// LongestPrefixBatch performs a longest-prefix lookup for every key in
// queries, which may be given in any order and may contain duplicates.
// Keys sharing common prefixes traverse the shared part of the tree only
// once, instead of each query walking from the root on its own.
//
// The returned slice has the same length as queries and results[i] holds
// the match for queries[i], preserving the input order one-to-one. The
// returned keys alias keys stored in the tree and are never backed by
// scratch buffers reused by later calls.
func (n *Node[T]) LongestPrefixBatch(queries [][]byte) []PrefixMatch[T] {
	results, _ := n.longestPrefixBatch(queries)
	return results
}

// longestPrefixBatch is the internal form of LongestPrefixBatch that also
// reports how many tree nodes were visited, which is used to verify that
// shared prefixes are only walked once.
func (n *Node[T]) longestPrefixBatch(queries [][]byte) ([]PrefixMatch[T], int) {
	results := make([]PrefixMatch[T], len(queries))
	visits := 0
	if len(queries) == 0 {
		return results, visits
	}

	// Process the queries in byte-wise order so that queries sharing a
	// path through the tree form contiguous groups. The original indexes
	// are carried along so results map back to the input order.
	order := make([]int, len(queries))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		return bytes.Compare(queries[order[i]], queries[order[j]]) < 0
	})

	sorted := make([][]byte, len(queries))
	for i, idx := range order {
		sorted[i] = queries[idx]
	}

	n.walkLongestPrefixBatch(sorted, order, nil, results, &visits)
	return results, visits
}

// walkLongestPrefixBatch resolves the queries in rem, which are sorted
// byte-wise and aligned with order, against the subtree rooted at n. The
// remaining query suffixes in rem all share the path from the root down
// to n, and last is the deepest leaf seen along that path so far.
func (n *Node[T]) walkLongestPrefixBatch(rem [][]byte, order []int, last *leafNode[T], results []PrefixMatch[T], visits *int) {
	*visits++

	// Split off the queries that diverge inside the compressed prefix of
	// this node. Because the group is sorted, the queries matching the
	// whole prefix form a contiguous run in the middle, with mismatches
	// on both sides. Mismatches fall back to the nearest valued ancestor.
	lo := 0
	for lo < len(rem) && !bytes.HasPrefix(rem[lo], n.prefix) {
		lo++
	}
	hi := lo
	for hi < len(rem) && bytes.HasPrefix(rem[hi], n.prefix) {
		hi++
	}
	for _, idx := range order[:lo] {
		results[idx] = prefixMatchOf(last)
	}
	for _, idx := range order[hi:] {
		results[idx] = prefixMatchOf(last)
	}
	rem = rem[lo:hi]
	order = order[lo:hi]
	if len(rem) == 0 {
		return
	}

	// Strip the matched prefix. Suffixes of a sorted group that share a
	// prefix remain sorted, so the groups below stay contiguous.
	stripped := make([][]byte, len(rem))
	for i, r := range rem {
		stripped[i] = r[len(n.prefix):]
	}
	rem = stripped

	// A full-key match at this node takes priority over shorter prefixes
	// found higher up.
	if n.leaf != nil {
		last = n.leaf
	}

	// Empty suffixes sort first: these queries end exactly at this node.
	i := 0
	for i < len(rem) && len(rem[i]) == 0 {
		results[order[i]] = prefixMatchOf(last)
		i++
	}

	// The remaining suffixes group by their first byte into contiguous
	// runs thanks to the byte-wise ordering, and each run descends into
	// the matching edge at most once.
	for i < len(rem) {
		j := i + 1
		for j < len(rem) && rem[j][0] == rem[i][0] {
			j++
		}
		_, child := n.getEdge(rem[i][0])
		if child == nil {
			for _, idx := range order[i:j] {
				results[idx] = prefixMatchOf(last)
			}
		} else {
			child.walkLongestPrefixBatch(rem[i:j], order[i:j], last, results, visits)
		}
		i = j
	}
}

// LongestPrefixBatch is used to perform a longest-prefix lookup for every
// key in queries in a single shared traversal. See Node.LongestPrefixBatch
// for details.
func (t *Tree[T]) LongestPrefixBatch(queries [][]byte) []PrefixMatch[T] {
	return t.root.LongestPrefixBatch(queries)
}

// LongestPrefixBatch is used to perform a longest-prefix lookup for every
// key in queries in a single shared traversal. See Node.LongestPrefixBatch
// for details.
func (t *Txn[T]) LongestPrefixBatch(queries [][]byte) []PrefixMatch[T] {
	return t.root.LongestPrefixBatch(queries)
}
