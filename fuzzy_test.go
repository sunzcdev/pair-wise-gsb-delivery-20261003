// Copyright IBM Corp. 2015, 2025
// SPDX-License-Identifier: MPL-2.0

package iradix

import (
	"bytes"
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"testing"
)

// naiveLevenshtein computes the same byte-wise Levenshtein distance as
// FuzzyLookup, without using the tree, to serve as an oracle.
func naiveLevenshtein(a, b []byte) int {
	la, lb := len(a), len(b)
	row := make([]int, lb+1)
	for j := range row {
		row[j] = j
	}
	for i := 1; i <= la; i++ {
		prev := row[0]
		row[0] = i
		for j := 1; j <= lb; j++ {
			cur := row[j]
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			d := row[j] + 1
			if row[j-1]+1 < d {
				d = row[j-1] + 1
			}
			if prev+cost < d {
				d = prev + cost
			}
			row[j] = d
			prev = cur
		}
	}
	return row[lb]
}

// naiveFuzzy is the reference implementation: enumerate every key and
// filter/sort the same way FuzzyLookup promises.
func naiveFuzzy[T any](tree *Tree[T], query []byte, maxDist, limit int) []FuzzyMatch[T] {
	var all []FuzzyMatch[T]
	tree.Root().Walk(func(k []byte, v T) bool {
		if d := naiveLevenshtein(query, k); d <= maxDist {
			all = append(all, FuzzyMatch[T]{
				Key:      append([]byte(nil), k...),
				Value:    v,
				Distance: d,
			})
		}
		return false
	})
	sort.Slice(all, func(i, j int) bool {
		return fuzzyWorse(all[j], all[i])
	})
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all
}

func copyMatches(ms []FuzzyMatch[string]) []FuzzyMatch[string] {
	out := make([]FuzzyMatch[string], len(ms))
	for i, m := range ms {
		out[i] = FuzzyMatch[string]{
			Key:      append([]byte(nil), m.Key...),
			Value:    m.Value,
			Distance: m.Distance,
		}
	}
	return out
}

func matchesEqual(a, b []FuzzyMatch[string]) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Distance != b[i].Distance || !bytes.Equal(a[i].Key, b[i].Key) || a[i].Value != b[i].Value {
			return false
		}
	}
	return true
}

// allByteKeys enumerates every key over alphabet with length in [0,maxLen].
func allByteKeys(alphabet []byte, maxLen int) [][]byte {
	var keys [][]byte
	var gen func(prefix []byte)
	gen = func(prefix []byte) {
		key := make([]byte, len(prefix))
		copy(key, prefix)
		keys = append(keys, key)
		if len(prefix) == maxLen {
			return
		}
		for _, b := range alphabet {
			gen(append(prefix, b))
		}
	}
	gen(nil)
	return keys
}

// TestFuzzyLookupExhaustive builds a small dense key space (including the
// empty key and prefix relationships) and checks FuzzyLookup against the
// naive implementation for every query in the same key space, multiple
// thresholds and limits.
func TestFuzzyLookupExhaustive(t *testing.T) {
	alphabet := []byte{'a', 'b'}
	const maxLen = 3

	tree := New[string]()
	for i, k := range allByteKeys(alphabet, maxLen) {
		tree, _, _ = tree.Insert(k, "v"+strconv.Itoa(i))
	}

	queries := allByteKeys(alphabet, maxLen+1)
	for _, maxDist := range []int{0, 1, 2, 3} {
		for _, limit := range []int{0, 1, 2, 5, 100} {
			for _, q := range queries {
				want := naiveFuzzy(tree, q, maxDist, limit)
				got := tree.FuzzyLookup(q, maxDist, limit)
				if !matchesEqual(got, want) {
					t.Fatalf("q=%q maxDist=%d limit=%d\n got=%v\nwant=%v",
						string(q), maxDist, limit, got, want)
				}
			}
		}
	}
}

// TestFuzzyLookupHandPicked covers long common prefixes, keys that are
// prefixes of others and the empty key with explicit assertions.
func TestFuzzyLookupHandPicked(t *testing.T) {
	keys := []string{
		"",
		"a",
		"abcdefghij",
		"abcdefghik",
		"abcdefghijkl",
		"abcxxxxxxx",
		"zebra",
		"zero",
	}
	tree := New[string]()
	for _, k := range keys {
		tree, _, _ = tree.Insert([]byte(k), "val:"+k)
	}

	// Exact match of the empty key only when the threshold is zero.
	got := tree.FuzzyLookup(nil, 0, 0)
	if len(got) != 1 || len(got[0].Key) != 0 || got[0].Distance != 0 {
		t.Fatalf("empty query, d=0: %+v", got)
	}

	// A key that is a strict prefix of another matches at distance zero.
	got = tree.FuzzyLookup([]byte("a"), 0, 0)
	if len(got) != 1 || string(got[0].Key) != "a" {
		t.Fatalf("exact prefix key, d=0: %+v", got)
	}

	// Threshold zero never produces fuzzy results.
	got = tree.FuzzyLookup([]byte("abcdefghijX"), 0, 0)
	if len(got) != 0 {
		t.Fatalf("no exact key, d=0: %+v", got)
	}

	// One substitution on the long shared prefix; distance ordering wins
	// over key order.
	got = tree.FuzzyLookup([]byte("abcdefghijX"), 1, 0)
	if len(got) != 1 || string(got[0].Key) != "abcdefghij" || got[0].Distance != 1 {
		t.Fatalf("long prefix d=1: %+v", got)
	}

	// At distance 2 the longer key becomes reachable through one
	// substitution (X->k) plus one insertion (l), and it sorts after the
	// distance-1 hit.
	got = tree.FuzzyLookup([]byte("abcdefghijX"), 2, 0)
	wantPairs := []struct {
		key string
		d   int
	}{
		{"abcdefghij", 1},
		{"abcdefghijkl", 2},
		{"abcdefghik", 2},
	}
	if len(got) != len(wantPairs) {
		t.Fatalf("long prefix d=2: %+v", got)
	}
	for i, want := range wantPairs {
		if string(got[i].Key) != want.key || got[i].Distance != want.d {
			t.Fatalf("long prefix d=2, result %d: got %q d=%d want %q d=%d",
				i, got[i].Key, got[i].Distance, want.key, want.d)
		}
	}

	// Limit still returns the true top items.
	full := tree.FuzzyLookup([]byte("abcdefghijX"), 2, 0)
	for _, limit := range []int{1, 2, 3} {
		limited := tree.FuzzyLookup([]byte("abcdefghijX"), 2, limit)
		if !matchesEqual(limited, full[:minInt(limit, len(full))]) {
			t.Fatalf("limit=%d: got=%v want=%v", limit, limited, full[:limit])
		}
	}

	// Invalid thresholds are rejected.
	if tree.FuzzyLookup([]byte("a"), -1, 0) != nil {
		t.Fatalf("negative maxDist should return nil")
	}
	if tree.FuzzyLookup([]byte("a"), maxFuzzyDistance+1, 0) != nil {
		t.Fatalf("excessive maxDist should return nil")
	}
}

// countNodes returns the total number of nodes reachable from n.
func countNodes[T any](n *Node[T]) int {
	c := 1
	for i := range n.edges {
		c += countNodes(n.edges[i].node)
	}
	return c
}

// TestFuzzyLookupPruning shows that branches that cannot contain a match
// are skipped: the visited node count is strictly below the total node
// count, and the entire far-away "z" subtree is never entered.
func TestFuzzyLookupPruning(t *testing.T) {
	keys := []string{
		"aardvark",
		"abacus",
		"abandon",
		"absolutely",
		"abstract",
		"academy",
		"accent",
		"accident",
		"zebra",
		"zero",
		"zoo",
		"zoology",
	}
	tree := New[string]()
	for _, k := range keys {
		tree, _, _ = tree.Insert([]byte(k), k)
	}

	total := countNodes(tree.root)

	s := &fuzzySearch[string]{query: []byte("aardvark"), cutoff: 1}
	s.dfs(tree.root, make([]int, len("aardvark")+1))

	if s.visited >= total {
		t.Fatalf("expected pruning: visited=%d total=%d", s.visited, total)
	}
	t.Logf("visited %d of %d nodes for a distance-1 query", s.visited, total)

	// None of the "z" keys are within distance 1, and the branch leading
	// to them must have been pruned. Locate the z-edge subtree and confirm
	// that none of the returned keys live there, then bound the visit count.
	_, znode := tree.root.getEdge('z')
	if znode == nil {
		t.Fatalf("test setup missing z subtree")
	}
	zSubtree := countNodes(znode)

	var got []string
	for _, m := range s.matches {
		got = append(got, string(m.Key))
	}
	for _, k := range got {
		if len(k) > 0 && k[0] == 'z' {
			t.Fatalf("impossible z-branch key returned: %q", k)
		}
	}
	// The root is visited but no node of the z subtree is, so the visit
	// count cannot exceed total minus the skipped subtree.
	if s.visited > total-zSubtree {
		t.Fatalf("z subtree was entered: visited=%d bound=%d", s.visited, total-zSubtree)
	}

	// Threshold zero only walks the exact-match path, far below total size.
	s0 := &fuzzySearch[string]{query: []byte("accident"), cutoff: 0}
	s0.dfs(tree.root, make([]int, len("accident")+1))
	if s0.visited >= total {
		t.Fatalf("distance-0 query should only touch the exact path: %d of %d", s0.visited, total)
	}
	t.Logf("visited %d of %d nodes for a distance-0 query", s0.visited, total)
}

// TestFuzzyLookupLimitPruning verifies that a full bounded result set
// tightens pruning but keeps the genuine top items.
func TestFuzzyLookupLimitPruning(t *testing.T) {
	tree := New[string]()
	all := []string{"cat", "car", "cab", "can", "cap", "bat", "bad", "bar"}
	for i, k := range all {
		tree, _, _ = tree.Insert([]byte(k), strconv.Itoa(i))
	}

	full := tree.FuzzyLookup([]byte("cat"), 2, 0)
	for _, limit := range []int{1, 2, 3, len(all)} {
		got := tree.FuzzyLookup([]byte("cat"), 2, limit)
		want := copyMatches(full[:minInt(limit, len(full))])
		if !matchesEqual(got, want) {
			t.Fatalf("limit=%d\n got=%v\nwant=%v", limit, got, want)
		}
	}
}

// TestFuzzyLookupReadOnly ensures the query does not mutate the tree or
// its values: contents and Len are identical before and after.
func TestFuzzyLookupReadOnly(t *testing.T) {
	keys := []string{"", "a", "ab", "abc", "longcommonprefix"}
	before := New[int]()
	for i, k := range keys {
		before, _, _ = before.Insert([]byte(k), i)
	}

	for _, q := range []string{"", "a", "ax", "abc", "longcommonpreeee", "zzz"} {
		for _, d := range []int{0, 1, 2, 4} {
			res := before.FuzzyLookup([]byte(q), d, 3)
			for _, m := range res {
				if v, ok := before.Get(m.Key); !ok || v != m.Value {
					t.Fatalf("stale value for %q: got %v (%v)", m.Key, m.Value, v)
				}
			}
		}
	}

	if before.Len() != len(keys) {
		t.Fatalf("Len changed: %d", before.Len())
	}
	var seen []string
	before.Root().Walk(func(k []byte, _ int) bool {
		seen = append(seen, string(k))
		return false
	})
	if !reflect.DeepEqual(seen, keys) {
		t.Fatalf("keys changed: %v", seen)
	}
}

// TestFuzzyLookupRandomSets builds many small random trees (over an
// alphabet large enough to exercise sparse branching) and checks every
// query/threshold/limit combination against the naive oracle.
func TestFuzzyLookupRandomSets(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	alphabet := []byte{'a', 'b', 'c', 'x', 'y'}

	for iter := 0; iter < 200; iter++ {
		numKeys := r.Intn(12) + 1
		seen := make(map[string]bool)
		var keys [][]byte
		for len(keys) < numKeys {
			l := r.Intn(5) // includes length 0
			k := make([]byte, l)
			for i := range k {
				k[i] = alphabet[r.Intn(len(alphabet))]
			}
			if !seen[string(k)] {
				seen[string(k)] = true
				keys = append(keys, k)
			}
		}

		tree := New[int]()
		for i, k := range keys {
			tree, _, _ = tree.Insert(k, i)
		}

		queries := append([][]byte{}, keys...)
		for i := 0; i < 5; i++ {
			l := r.Intn(5)
			q := make([]byte, l)
			for j := range q {
				q[j] = alphabet[r.Intn(len(alphabet))]
			}
			queries = append(queries, q)
		}

		for _, q := range queries {
			for _, maxDist := range []int{0, 1, 2} {
				for _, limit := range []int{0, 1, 3} {
					got := tree.FuzzyLookup(q, maxDist, limit)
					want := naiveFuzzy(tree, q, maxDist, limit)
					if len(got) != len(want) {
						t.Fatalf("iter=%d q=%q d=%d limit=%d len got=%d want=%d",
							iter, q, maxDist, limit, len(got), len(want))
					}
					for i := range want {
						if got[i].Distance != want[i].Distance ||
							!bytes.Equal(got[i].Key, want[i].Key) ||
							got[i].Value != want[i].Value {
							t.Fatalf("iter=%d q=%q d=%d limit=%d\n got=%v\nwant=%v",
								iter, q, maxDist, limit, got, want)
						}
					}
				}
			}
		}
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
