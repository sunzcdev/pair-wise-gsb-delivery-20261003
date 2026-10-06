// Copyright IBM Corp. 2015, 2025
// SPDX-License-Identifier: MPL-2.0

package iradix

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// countLongestPrefixVisits mirrors the walk done by Node.LongestPrefix and
// counts how many nodes a single lookup touches.
func countLongestPrefixVisits[T any](n *Node[T], k []byte) int {
	visits := 0
	search := k
	for {
		visits++
		if len(search) == 0 {
			break
		}
		_, next := n.getEdge(search[0])
		if next == nil {
			break
		}
		n = next
		if bytes.HasPrefix(search, n.prefix) {
			search = search[len(n.prefix):]
		} else {
			break
		}
	}
	return visits
}

func TestLongestPrefixBatch_MatchesLongestPrefix(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	alphabet := []byte("abc/")
	randKey := func() []byte {
		k := make([]byte, rng.Intn(12))
		for i := range k {
			k[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return k
	}

	r := New[int]()
	keys := make(map[string]int)
	for i := 0; i < 300; i++ {
		k := randKey()
		if _, ok := keys[string(k)]; ok {
			continue
		}
		keys[string(k)] = i
		r, _, _ = r.Insert(k, i)
	}

	// Build a shuffled query list mixing tree keys, random keys, empty
	// keys, and duplicates.
	var queries [][]byte
	for k := range keys {
		queries = append(queries, []byte(k))
	}
	for i := 0; i < 300; i++ {
		queries = append(queries, randKey())
	}
	queries = append(queries, []byte{}, []byte{})
	for i := 0; i < 50; i++ {
		queries = append(queries, queries[rng.Intn(len(queries))])
	}
	rng.Shuffle(len(queries), func(i, j int) {
		queries[i], queries[j] = queries[j], queries[i]
	})

	results := r.LongestPrefixBatch(queries)
	if len(results) != len(queries) {
		t.Fatalf("got %d results for %d queries", len(results), len(queries))
	}
	for i, q := range queries {
		wantKey, wantVal, wantOk := r.Root().LongestPrefix(q)
		got := results[i]
		if got.Found != wantOk {
			t.Fatalf("query %d %q: found=%v, want %v", i, q, got.Found, wantOk)
		}
		if !wantOk {
			continue
		}
		if !bytes.Equal(got.Key, wantKey) || got.Value != wantVal {
			t.Fatalf("query %d %q: got (%q, %d), want (%q, %d)",
				i, q, got.Key, got.Value, wantKey, wantVal)
		}
	}
}

func TestLongestPrefixBatch_MidEdgeMismatch(t *testing.T) {
	r := New[int]()
	r, _, _ = r.Insert([]byte("a"), 1)
	r, _, _ = r.Insert([]byte("abc"), 2)

	queries := [][]byte{
		[]byte("abd"), // diverges inside the compressed edge "bc"
		[]byte("ab"),  // stops short inside the compressed edge "bc"
		[]byte("abc"), // full-key hit beats the shorter prefix "a"
		[]byte("z"),   // no edge at all
	}
	results := r.LongestPrefixBatch(queries)

	if !results[0].Found || string(results[0].Key) != "a" || results[0].Value != 1 {
		t.Fatalf("abd: got %+v, want match on a/1", results[0])
	}
	if !results[1].Found || string(results[1].Key) != "a" || results[1].Value != 1 {
		t.Fatalf("ab: got %+v, want match on a/1", results[1])
	}
	if !results[2].Found || string(results[2].Key) != "abc" || results[2].Value != 2 {
		t.Fatalf("abc: got %+v, want match on abc/2", results[2])
	}
	if results[3].Found {
		t.Fatalf("z: got %+v, want no match", results[3])
	}
}

func TestLongestPrefixBatch_EmptyKeyVsNoMatch(t *testing.T) {
	r := New[int]()
	r, _, _ = r.Insert([]byte("foo"), 1)

	// Empty query against a tree without an empty key: no match.
	results := r.LongestPrefixBatch([][]byte{{}})
	if len(results) != 1 || results[0].Found {
		t.Fatalf("empty query without empty key: got %+v, want no match", results[0])
	}

	// Insert the empty key: the empty query now matches it exactly.
	r, _, _ = r.Insert([]byte{}, 2)
	results = r.LongestPrefixBatch([][]byte{{}, []byte("f")})
	if !results[0].Found || len(results[0].Key) != 0 || results[0].Value != 2 {
		t.Fatalf("empty query with empty key: got %+v, want match on empty key/2", results[0])
	}
	if !results[1].Found || string(results[1].Key) != "" || results[1].Value != 2 {
		t.Fatalf("f: got %+v, want match on empty key/2", results[1])
	}
}

func TestLongestPrefixBatch_DuplicatesAndOrder(t *testing.T) {
	r := New[int]()
	r, _, _ = r.Insert([]byte("foo"), 1)
	r, _, _ = r.Insert([]byte("foobar"), 2)

	queries := [][]byte{
		[]byte("foozip"),
		[]byte("foobarbaz"),
		[]byte("foozip"),
		[]byte("bar"),
		[]byte("foozip"),
	}
	results := r.LongestPrefixBatch(queries)
	if len(results) != len(queries) {
		t.Fatalf("got %d results for %d queries", len(results), len(queries))
	}
	for _, i := range []int{0, 2, 4} {
		if !results[i].Found || string(results[i].Key) != "foo" || results[i].Value != 1 {
			t.Fatalf("result %d: got %+v, want foo/1", i, results[i])
		}
	}
	if !results[1].Found || string(results[1].Key) != "foobar" || results[1].Value != 2 {
		t.Fatalf("result 1: got %+v, want foobar/2", results[1])
	}
	if results[3].Found {
		t.Fatalf("result 3: got %+v, want no match", results[3])
	}
}

func TestLongestPrefixBatch_ResultKeysStable(t *testing.T) {
	r := New[int]()
	r, _, _ = r.Insert([]byte("foo"), 1)
	r, _, _ = r.Insert([]byte("bar"), 2)

	first := r.LongestPrefixBatch([][]byte{[]byte("foozip"), []byte("barfly")})
	if !first[0].Found || !first[1].Found {
		t.Fatalf("unexpected misses: %+v", first)
	}

	// Later calls must not overwrite keys returned by earlier calls.
	for i := 0; i < 10; i++ {
		r.LongestPrefixBatch([][]byte{[]byte("bar"), []byte("foo"), []byte("zzz")})
	}
	if string(first[0].Key) != "foo" || string(first[1].Key) != "bar" {
		t.Fatalf("results changed after later calls: %q, %q", first[0].Key, first[1].Key)
	}

	// Mutating the original query slice must not corrupt results either.
	q := []byte("foozip")
	got := r.LongestPrefixBatch([][]byte{q})
	q[0] = 'x'
	if string(got[0].Key) != "foo" {
		t.Fatalf("result key changed after mutating query: %q", got[0].Key)
	}
}

func TestLongestPrefixBatch_SharedVisits(t *testing.T) {
	r := New[int]()

	// Build routes with a long shared prefix so per-query lookups repeat
	// a deep walk from the root.
	const prefix = "/api/v1/tenants/region/"
	for i := 0; i < 64; i++ {
		k := fmt.Sprintf("%s%02d", prefix, i)
		r, _, _ = r.Insert([]byte(k), i)
	}

	// Queries all share the long common prefix, in arbitrary order.
	const numQueries = 512
	queries := make([][]byte, 0, numQueries)
	want := make([]string, 0, numQueries)
	for i := numQueries - 1; i >= 0; i-- {
		queries = append(queries, []byte(fmt.Sprintf("%s%02d/extra", prefix, i%64)))
		want = append(want, fmt.Sprintf("%s%02d", prefix, i%64))
	}

	results, batchVisits := r.Root().longestPrefixBatch(queries)
	if len(results) != numQueries {
		t.Fatalf("got %d results for %d queries", len(results), numQueries)
	}
	for i, q := range queries {
		if !results[i].Found || string(results[i].Key) != want[i] {
			t.Fatalf("query %q: got %+v, want key %q", q, results[i], want[i])
		}
	}

	soloVisits := 0
	for _, q := range queries {
		soloVisits += countLongestPrefixVisits(r.Root(), q)
	}

	// The shared walk should visit barely more nodes than a single
	// lookup, while walking each query on its own repeats the shared
	// path for every query.
	if batchVisits*4 >= soloVisits {
		t.Fatalf("batch visited %d nodes, solo lookups visited %d; want batch < solo/4",
			batchVisits, soloVisits)
	}
}

func TestLongestPrefixBatch_SortedInput(t *testing.T) {
	// Guard the contiguous-group assumption: feeding already sorted keys
	// must produce the same results as shuffled keys.
	r := New[int]()
	keys := []string{"a", "ab", "abc", "b", "ba", "cbb"}
	for i, k := range keys {
		r, _, _ = r.Insert([]byte(k), i)
	}

	queries := [][]byte{
		[]byte("a"), []byte("abx"), []byte("abd"), []byte("ac"),
		[]byte("b"), []byte("bb"), []byte("cba"), []byte("d"),
	}
	sorted := make([][]byte, len(queries))
	copy(sorted, queries)
	sort.Slice(sorted, func(i, j int) bool { return bytes.Compare(sorted[i], sorted[j]) < 0 })

	gotSorted := r.LongestPrefixBatch(sorted)
	for i, q := range sorted {
		wantKey, _, wantOk := r.Root().LongestPrefix(q)
		if gotSorted[i].Found != wantOk || !bytes.Equal(gotSorted[i].Key, wantKey) {
			t.Fatalf("sorted query %q: got %+v, want key %q found %v",
				q, gotSorted[i], wantKey, wantOk)
		}
	}
}
