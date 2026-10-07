// Copyright IBM Corp. 2015, 2025
// SPDX-License-Identifier: MPL-2.0

package iradix

import (
	"bytes"
	"fmt"
	"sort"
	"testing"
)

// levDistance is a plain reference implementation of the byte-wise
// Levenshtein distance used to cross-check the tree walk.
func levDistance(a, b []byte) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for _, ca := range a {
		next := make([]int, len(b)+1)
		next[0] = prev[0] + 1
		for j := 1; j <= len(b); j++ {
			cost := 1
			if ca == b[j-1] {
				cost = 0
			}
			next[j] = minInt3(prev[j]+1, next[j-1]+1, prev[j-1]+cost)
		}
		prev = next
	}
	return prev[len(b)]
}

// bruteForceFuzzy computes the expected result by exporting every key and
// comparing it against the query, the approach FuzzySearch is meant to
// avoid.
func bruteForceFuzzy[T any](t *Tree[T], query []byte, maxDist, limit int) []FuzzyMatch[T] {
	var all []FuzzyMatch[T]
	t.Root().Walk(func(k []byte, v T) bool {
		if d := levDistance(k, query); d <= maxDist {
			key := make([]byte, len(k))
			copy(key, k)
			all = append(all, FuzzyMatch[T]{Key: key, Val: v, Distance: d})
		}
		return false
	})
	sort.Slice(all, func(i, j int) bool {
		if all[i].Distance != all[j].Distance {
			return all[i].Distance < all[j].Distance
		}
		return bytes.Compare(all[i].Key, all[j].Key) < 0
	})
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all
}

func checkFuzzyMatches[T comparable](t *testing.T, got, want []FuzzyMatch[T]) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d matches, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i].Distance != want[i].Distance || !bytes.Equal(got[i].Key, want[i].Key) || got[i].Val != want[i].Val {
			t.Fatalf("match %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

// TestFuzzySearch_Exhaustive builds a tree from every string over a small
// alphabet up to a fixed length and cross-checks FuzzySearch against the
// brute-force export-and-compare approach for many queries, thresholds
// and limits.
func TestFuzzySearch_Exhaustive(t *testing.T) {
	var keys [][]byte
	alphabet := []byte{'a', 'b', 'c'}
	var gen func(prefix []byte, depth int)
	gen = func(prefix []byte, depth int) {
		keys = append(keys, append([]byte(nil), prefix...))
		if depth == 0 {
			return
		}
		for _, c := range alphabet {
			gen(append(prefix, c), depth-1)
		}
	}
	gen(nil, 3)

	tree := New[int]()
	for i, k := range keys {
		tree, _, _ = tree.Insert(k, i)
	}

	queries := [][]byte{
		nil, {}, {'a'}, {'b'}, {'a', 'b'}, {'b', 'a'}, {'a', 'b', 'c'},
		{'c', 'b', 'a'}, {'a', 'a', 'a', 'a'}, {'x'}, {'x', 'y', 'z'},
		{'a', 'x', 'b'}, {'c', 'c'}, {'\x00'}, {'\xff', 'a'},
	}
	limits := []int{0, 1, 2, 5, 1000}

	for _, q := range queries {
		for maxDist := 0; maxDist <= 4; maxDist++ {
			for _, limit := range limits {
				got := tree.FuzzySearch(q, maxDist, limit)
				want := bruteForceFuzzy(tree, q, maxDist, limit)
				if len(got) != len(want) {
					t.Fatalf("query %q maxDist %d limit %d: got %d matches, want %d\ngot:  %v\nwant: %v",
						q, maxDist, limit, len(got), len(want), got, want)
				}
				for i := range got {
					if got[i].Distance != want[i].Distance || !bytes.Equal(got[i].Key, want[i].Key) || got[i].Val != want[i].Val {
						t.Fatalf("query %q maxDist %d limit %d match %d: got %v, want %v",
							q, maxDist, limit, i, got[i], want[i])
					}
				}
			}
		}
	}
}

func TestFuzzySearch_EmptyKeyAndQuery(t *testing.T) {
	tree := New[int]()
	tree, _, _ = tree.Insert([]byte(""), 1)
	tree, _, _ = tree.Insert([]byte("a"), 2)
	tree, _, _ = tree.Insert([]byte("ab"), 3)

	// Empty query: distances are the key lengths.
	got := tree.FuzzySearch(nil, 1, 0)
	want := []FuzzyMatch[int]{
		{Key: []byte(""), Val: 1, Distance: 0},
		{Key: []byte("a"), Val: 2, Distance: 1},
	}
	checkFuzzyMatches(t, got, want)

	// The empty key is one deletion away from "a".
	got = tree.FuzzySearch([]byte("a"), 1, 0)
	want = []FuzzyMatch[int]{
		{Key: []byte("a"), Val: 2, Distance: 0},
		{Key: []byte(""), Val: 1, Distance: 1},
		{Key: []byte("ab"), Val: 3, Distance: 1},
	}
	checkFuzzyMatches(t, got, want)
}

func TestFuzzySearch_KeyPrefixOfAnother(t *testing.T) {
	tree := New[int]()
	tree, _, _ = tree.Insert([]byte("foo"), 1)
	tree, _, _ = tree.Insert([]byte("foobar"), 2)
	tree, _, _ = tree.Insert([]byte("foobaz"), 3)

	got := tree.FuzzySearch([]byte("fooba"), 2, 0)
	want := []FuzzyMatch[int]{
		{Key: []byte("foobar"), Val: 2, Distance: 1},
		{Key: []byte("foobaz"), Val: 3, Distance: 1},
		{Key: []byte("foo"), Val: 1, Distance: 2},
	}
	checkFuzzyMatches(t, got, want)

	// Query is a strict prefix of every key.
	got = tree.FuzzySearch([]byte("fo"), 1, 0)
	want = []FuzzyMatch[int]{
		{Key: []byte("foo"), Val: 1, Distance: 1},
	}
	checkFuzzyMatches(t, got, want)
}

func TestFuzzySearch_LongCommonPrefix(t *testing.T) {
	prefix := bytes.Repeat([]byte("p"), 128)
	keyA := append(append([]byte(nil), prefix...), 'x')
	keyB := append(append([]byte(nil), prefix...), 'y')
	query := append(append([]byte(nil), prefix...), 'x')

	tree := New[int]()
	tree, _, _ = tree.Insert(keyA, 1)
	tree, _, _ = tree.Insert(keyB, 2)
	tree, _, _ = tree.Insert([]byte("zzz"), 3)

	got := tree.FuzzySearch(query, 1, 0)
	want := []FuzzyMatch[int]{
		{Key: keyA, Val: 1, Distance: 0},
		{Key: keyB, Val: 2, Distance: 1},
	}
	checkFuzzyMatches(t, got, want)

	// A typo buried deep in the common prefix is still found.
	typo := append([]byte(nil), prefix...)
	typo[100] = 'Q'
	typo = append(typo, 'x')
	got = tree.FuzzySearch(typo, 1, 0)
	want = []FuzzyMatch[int]{
		{Key: keyA, Val: 1, Distance: 1},
	}
	checkFuzzyMatches(t, got, want)
}

func TestFuzzySearch_ZeroThreshold(t *testing.T) {
	tree := New[int]()
	tree, _, _ = tree.Insert([]byte(""), 1)
	tree, _, _ = tree.Insert([]byte("abc"), 2)
	tree, _, _ = tree.Insert([]byte("abd"), 3)

	// Distance zero means exact matches only.
	got := tree.FuzzySearch([]byte("abc"), 0, 0)
	want := []FuzzyMatch[int]{
		{Key: []byte("abc"), Val: 2, Distance: 0},
	}
	checkFuzzyMatches(t, got, want)

	got = tree.FuzzySearch([]byte("ab"), 0, 0)
	if len(got) != 0 {
		t.Fatalf("expected no matches, got %v", got)
	}

	got = tree.FuzzySearch(nil, 0, 0)
	want = []FuzzyMatch[int]{
		{Key: []byte(""), Val: 1, Distance: 0},
	}
	checkFuzzyMatches(t, got, want)
}

func TestFuzzySearch_LimitKeepsTopMatches(t *testing.T) {
	tree := New[int]()
	keys := []string{"cat", "car", "cart", "cat", "cut", "bat", "cats", "scat"}
	for i, k := range keys {
		tree, _, _ = tree.Insert([]byte(k), i)
	}

	full := tree.FuzzySearch([]byte("cat"), 2, 0)
	for limit := 1; limit <= len(full); limit++ {
		got := tree.FuzzySearch([]byte("cat"), 2, limit)
		checkFuzzyMatches(t, got, full[:limit])
	}
}

func countNodes[T any](n *Node[T]) int {
	count := 1
	for _, e := range n.edges {
		count += countNodes(e.node)
	}
	return count
}

func TestFuzzySearch_PrunesImpossibleBranches(t *testing.T) {
	tree := New[int]()
	tree, _, _ = tree.Insert([]byte("config"), 1)
	tree, _, _ = tree.Insert([]byte("confog"), 2)
	// A large subtree that shares nothing with the query.
	for i := 0; i < 200; i++ {
		tree, _, _ = tree.Insert([]byte(fmt.Sprintf("zzzzzzzzzz-%04d", i)), i)
	}

	matches, visited := tree.fuzzySearch([]byte("config"), 1, 0)
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %v", matches)
	}

	total := countNodes(tree.Root())
	if visited >= total {
		t.Fatalf("expected pruning to skip nodes: visited %d of %d", visited, total)
	}
	// The matching branch is tiny; the z-subtree must be skipped almost
	// entirely instead of walking all of its nodes.
	if visited > total/10 {
		t.Fatalf("expected to visit a small fraction of the tree: visited %d of %d", visited, total)
	}
}

func TestFuzzySearch_LimitTightensPruning(t *testing.T) {
	tree := New[int]()
	tree, _, _ = tree.Insert([]byte("config"), 1)
	for i := 0; i < 200; i++ {
		tree, _, _ = tree.Insert([]byte(fmt.Sprintf("donfih-%04d", i)), i)
	}

	matches, visited := tree.fuzzySearch([]byte("config"), 2, 1)
	if len(matches) != 1 || string(matches[0].Key) != "config" || matches[0].Distance != 0 {
		t.Fatalf("unexpected matches: %v", matches)
	}
	total := countNodes(tree.Root())
	if visited > total/10 {
		t.Fatalf("expected limit to tighten pruning: visited %d of %d", visited, total)
	}
}

func TestFuzzySearch_ReadOnly(t *testing.T) {
	tree := New[int]()
	keys := [][]byte{[]byte(""), []byte("a"), []byte("ab"), []byte("abc"), []byte("b"), []byte("ba")}
	for i, k := range keys {
		tree, _, _ = tree.Insert(k, i)
	}
	rootBefore := tree.Root()
	sizeBefore := tree.Len()

	matches := tree.FuzzySearch([]byte("ab"), 2, 0)
	if len(matches) == 0 {
		t.Fatal("expected matches")
	}

	// Mutating the returned keys must not disturb the tree.
	for _, m := range matches {
		for i := range m.Key {
			m.Key[i] = 'X'
		}
	}

	if tree.Root() != rootBefore || tree.Len() != sizeBefore {
		t.Fatal("tree changed by FuzzySearch")
	}
	for i, k := range keys {
		v, ok := tree.Get(k)
		if !ok || v != i {
			t.Fatalf("key %q: got (%v, %v), want (%v, true)", k, v, ok, i)
		}
	}
}

func TestFuzzySearch_NegativeDistance(t *testing.T) {
	tree := New[int]()
	tree, _, _ = tree.Insert([]byte("a"), 1)
	if got := tree.FuzzySearch([]byte("a"), -1, 0); got != nil {
		t.Fatalf("expected nil for negative distance, got %v", got)
	}
}
