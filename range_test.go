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

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"sort"
	"testing"
)

func buildRangeTestFST(t *testing.T, entries map[string]uint64) (*FST, []byte) {
	t.Helper()

	var buf bytes.Buffer
	b, err := New(&buf, nil)
	if err != nil {
		t.Fatalf("error creating builder: %v", err)
	}
	err = insertStringMap(b, entries)
	if err != nil {
		t.Fatalf("error building: %v", err)
	}
	err = b.Close()
	if err != nil {
		t.Fatalf("error closing builder: %v", err)
	}

	data := buf.Bytes()
	fst, err := Load(data)
	if err != nil {
		t.Fatalf("error loading fst: %v", err)
	}
	return fst, data
}

// enumerateAndFilter is the reference implementation: full enumeration
// followed by prefix and value-range filtering.
func enumerateAndFilter(t *testing.T, fst *FST, prefix []byte,
	minVal, maxVal uint64) map[string]uint64 {
	t.Helper()

	rv := map[string]uint64{}
	itr, err := fst.Iterator(nil, nil)
	for err == nil {
		key, val := itr.Current()
		if bytes.HasPrefix(key, prefix) && minVal <= val && val <= maxVal {
			rv[string(key)] = val
		}
		err = itr.Next()
	}
	if err != ErrIteratorDone {
		t.Fatalf("iterator error: %v", err)
	}
	return rv
}

func collectRange(t *testing.T, fst *FST, stats *OutputStats, prefix []byte,
	minVal, maxVal uint64) map[string]uint64 {
	t.Helper()

	rv := map[string]uint64{}
	itr, err := fst.RangeIterator(stats, prefix, minVal, maxVal)
	if err != nil {
		t.Fatalf("error creating range iterator: %v", err)
	}
	err = itr.Next()
	for err == nil {
		key, val := itr.Current()
		rv[string(key)] = val
		err = itr.Next()
	}
	if err != ErrIteratorDone {
		t.Fatalf("range iterator error: %v", err)
	}
	err = itr.Close()
	if err != nil {
		t.Fatalf("error closing range iterator: %v", err)
	}
	return rv
}

func checkRangeQuery(t *testing.T, fst *FST, stats *OutputStats, prefix []byte,
	minVal, maxVal uint64) {
	t.Helper()

	expect := enumerateAndFilter(t, fst, prefix, minVal, maxVal)

	got := collectRange(t, fst, stats, prefix, minVal, maxVal)
	if !reflect.DeepEqual(expect, got) {
		t.Errorf("with stats, prefix %q range [%d,%d]: expected %v, got %v",
			prefix, minVal, maxVal, expect, got)
	}

	got = collectRange(t, fst, nil, prefix, minVal, maxVal)
	if !reflect.DeepEqual(expect, got) {
		t.Errorf("without stats, prefix %q range [%d,%d]: expected %v, got %v",
			prefix, minVal, maxVal, expect, got)
	}

	// results must be produced in lexicographic key order
	itr, err := fst.RangeIterator(stats, prefix, minVal, maxVal)
	if err != nil {
		t.Fatalf("error creating range iterator: %v", err)
	}
	var prev []byte
	err = itr.Next()
	for err == nil {
		key, _ := itr.Current()
		if prev != nil && bytes.Compare(prev, key) >= 0 {
			t.Errorf("prefix %q range [%d,%d]: keys out of order: %q then %q",
				prefix, minVal, maxVal, prev, key)
		}
		prev = append(prev[:0], key...)
		err = itr.Next()
	}
	if err != ErrIteratorDone {
		t.Fatalf("range iterator error: %v", err)
	}
}

// TestRangeIteratorSharedSuffix uses a dictionary in which the same
// suffix states are shared by different prefixes whose accumulated values
// differ, so any auxiliary statistic caching absolute per-path values
// (instead of residual per-state values) would prune or keep the wrong
// branches.
func TestRangeIteratorSharedSuffix(t *testing.T) {
	entries := map[string]uint64{
		// shared "at"/"ar" suffixes with different accumulated values
		"bat": 600, "cat": 150, "rat": 300,
		"bar": 700, "car": 50, "far": 450,
		// prefix of another key
		"cats": 400, "catsup": 90,
		// zero and max values
		"dog": 0, "dogs": math.MaxUint64,
		// deep shared suffix chain
		"walking": 120, "talking": 480, "stalking": 260,
	}

	fst, data := buildRangeTestFST(t, entries)
	defer func() {
		err := fst.Close()
		if err != nil {
			t.Fatalf("error closing fst: %v", err)
		}
	}()

	snapshot := append([]byte(nil), data...)

	stats, err := BuildOutputStats(fst)
	if err != nil {
		t.Fatalf("error building output stats: %v", err)
	}
	if stats.Len() == 0 {
		t.Fatalf("expected non-empty stats")
	}

	// building stats must not modify the FST data
	if !bytes.Equal(snapshot, data) {
		t.Fatalf("building stats modified the FST data")
	}

	prefixes := []string{
		"", "b", "ba", "bat", "c", "ca", "cat", "cats", "d", "dog",
		"r", "walk", "talking", "z", "catsup!",
	}
	ranges := [][2]uint64{
		{0, 0},                           // only zero
		{0, math.MaxUint64},              // everything
		{100, 500},                       // the motivating example
		{0, 149},                         // below some values
		{600, 600},                       // single exact value
		{math.MaxUint64, math.MaxUint64}, // only max uint64
		{501, 599},                       // matches nothing
		{500, 100},                       // empty interval
		{150, 150},                       // boundary inclusive
	}

	for _, prefix := range prefixes {
		for _, r := range ranges {
			checkRangeQuery(t, fst, stats, []byte(prefix), r[0], r[1])
		}
	}
}

func TestRangeIteratorRandom(t *testing.T) {
	r := rand.New(rand.NewSource(42))

	entries := map[string]uint64{}
	for len(entries) < 500 {
		key := make([]byte, 1+r.Intn(6))
		for j := range key {
			key[j] = byte('a' + r.Intn(8))
		}
		var val uint64
		switch r.Intn(10) {
		case 0:
			val = 0
		case 1:
			val = math.MaxUint64
		default:
			val = uint64(r.Intn(1000))
		}
		entries[string(key)] = val
	}

	fst, _ := buildRangeTestFST(t, entries)
	defer func() {
		err := fst.Close()
		if err != nil {
			t.Fatalf("error closing fst: %v", err)
		}
	}()

	stats, err := BuildOutputStats(fst)
	if err != nil {
		t.Fatalf("error building output stats: %v", err)
	}

	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for n := 0; n < 200; n++ {
		var prefix []byte
		switch r.Intn(3) {
		case 0:
			prefix = nil
		case 1:
			prefix = []byte{byte('a' + r.Intn(8))}
		default:
			k := keys[r.Intn(len(keys))]
			prefix = []byte(k[:r.Intn(len(k)+1)])
		}

		var minVal, maxVal uint64
		switch r.Intn(8) {
		case 0:
			minVal, maxVal = 0, math.MaxUint64
		case 1:
			minVal, maxVal = 0, 0
		case 2:
			minVal, maxVal = math.MaxUint64, math.MaxUint64
		case 3:
			minVal, maxVal = 700, 100 // empty interval
		default:
			minVal = uint64(r.Intn(1000))
			maxVal = minVal + uint64(r.Intn(1000))
		}

		checkRangeQuery(t, fst, stats, prefix, minVal, maxVal)
	}
}

func TestRangeIteratorEmptyFST(t *testing.T) {
	fst, _ := buildRangeTestFST(t, map[string]uint64{})
	defer func() {
		err := fst.Close()
		if err != nil {
			t.Fatalf("error closing fst: %v", err)
		}
	}()

	stats, err := BuildOutputStats(fst)
	if err != nil {
		t.Fatalf("error building output stats: %v", err)
	}

	got := collectRange(t, fst, stats, nil, 0, math.MaxUint64)
	if len(got) != 0 {
		t.Errorf("expected no results from empty fst, got %v", got)
	}
}

func ExampleFST_RangeIterator() {
	var buf bytes.Buffer
	b, err := New(&buf, nil)
	if err != nil {
		panic(err)
	}
	entries := map[string]uint64{
		"apple": 50, "banana": 200, "cherry": 450, "date": 700,
	}
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := b.Insert([]byte(k), entries[k]); err != nil {
			panic(err)
		}
	}
	if err := b.Close(); err != nil {
		panic(err)
	}

	fst, err := Load(buf.Bytes())
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := fst.Close(); err != nil {
			panic(err)
		}
	}()

	stats, err := BuildOutputStats(fst)
	if err != nil {
		panic(err)
	}

	itr, err := fst.RangeIterator(stats, nil, 100, 500)
	if err != nil {
		panic(err)
	}
	err = itr.Next()
	for err == nil {
		key, val := itr.Current()
		fmt.Printf("%s %d\n", key, val)
		err = itr.Next()
	}

	// Output:
	// banana 200
	// cherry 450
}
