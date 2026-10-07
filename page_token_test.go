//  Copyright (c) 2017 Couchbase, Inc.
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
	"errors"
	"testing"

	"github.com/blevesearch/vellum/regexp"
)

// pageTestKeys is kept in lexicographic order and includes an empty key
// and keys containing zero bytes.
var pageTestKeys = []string{
	"", "\x00", "\x00\x00", "a", "a\x00", "a\x00b", "ab", "abc",
	"b", "b\x00", "ba", "c", "ca", "cb", "d", "mon", "thurs",
	"tues", "tye", "z", "zz",
}

func buildPageTestFST(t *testing.T, keys []string) (*FST, []byte) {
	t.Helper()

	var buf bytes.Buffer
	b, err := New(&buf, nil)
	if err != nil {
		t.Fatalf("error creating builder: %v", err)
	}
	for i, k := range keys {
		err = b.Insert([]byte(k), uint64(i%5))
		if err != nil {
			t.Fatalf("error inserting %q: %v", k, err)
		}
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

func collectAll(t *testing.T, fst *FST, aut Automaton,
	start, end []byte) ([][]byte, []uint64) {
	t.Helper()

	var keys [][]byte
	var vals []uint64
	itr, err := fst.Search(aut, start, end)
	if err == ErrIteratorDone {
		return nil, nil
	}
	if err != nil {
		t.Fatalf("error creating iterator: %v", err)
	}
	for err == nil {
		k, v := itr.Current()
		keys = append(keys, append([]byte(nil), k...))
		vals = append(vals, v)
		err = itr.Next()
	}
	if err != ErrIteratorDone {
		t.Fatalf("iterator error: %v", err)
	}
	return keys, vals
}

func collectPaged(t *testing.T, fst *FST, aut Automaton, queryID string,
	start, end []byte, limit int) ([][]byte, []uint64) {
	t.Helper()

	var keys [][]byte
	var vals []uint64
	var token []byte
	for pages := 0; ; pages++ {
		if pages > len(pageTestKeys)+2 {
			t.Fatalf("pagination did not terminate")
		}
		pageKeys, pageVals, next, err :=
			fst.SearchPage(aut, queryID, start, end, token, limit)
		if err != nil {
			t.Fatalf("page %d error: %v", pages, err)
		}
		if len(pageKeys) > limit {
			t.Fatalf("page %d returned %d keys, limit %d",
				pages, len(pageKeys), limit)
		}
		if len(pageKeys) == 0 && next != nil {
			t.Fatalf("page %d returned no keys but a next token", pages)
		}
		keys = append(keys, pageKeys...)
		vals = append(vals, pageVals...)
		if next == nil {
			return keys, vals
		}
		token = next
	}
}

func expectSameKeys(t *testing.T, want, got [][]byte, wantVals, gotVals []uint64) {
	t.Helper()

	if len(want) != len(got) {
		t.Fatalf("expected %d keys, got %d\nwant: %q\ngot:  %q",
			len(want), len(got), want, got)
	}
	for i := range want {
		if !bytes.Equal(want[i], got[i]) {
			t.Fatalf("key %d: expected %q, got %q", i, want[i], got[i])
		}
		if wantVals[i] != gotVals[i] {
			t.Fatalf("val %d: expected %d, got %d",
				i, wantVals[i], gotVals[i])
		}
	}
}

func TestSearchPageVsFullIteration(t *testing.T) {
	fst, _ := buildPageTestFST(t, pageTestKeys)

	wantKeys, wantVals := collectAll(t, fst, nil, nil, nil)
	if len(wantKeys) != len(pageTestKeys) {
		t.Fatalf("expected %d keys in full iteration, got %d",
			len(pageTestKeys), len(wantKeys))
	}

	for _, limit := range []int{1, 2, 3, 5, 7, 16, 21, 1000} {
		gotKeys, gotVals := collectPaged(t, fst, nil, "q", nil, nil, limit)
		expectSameKeys(t, wantKeys, gotKeys, wantVals, gotVals)
	}
}

func TestSearchPageRange(t *testing.T) {
	keys := make([]string, 0, 26)
	for c := byte('a'); c <= 'z'; c++ {
		keys = append(keys, string([]byte{c}))
	}
	fst, _ := buildPageTestFST(t, keys)

	start, end := []byte("c"), []byte("w")
	wantKeys, wantVals := collectAll(t, fst, nil, start, end)
	if len(wantKeys) != 20 {
		t.Fatalf("expected 20 keys in range, got %d", len(wantKeys))
	}

	for _, limit := range []int{1, 2, 3, 4, 5, 6, 7, 19, 20, 21} {
		gotKeys, gotVals := collectPaged(t, fst, nil, "q", start, end, limit)
		expectSameKeys(t, wantKeys, gotKeys, wantVals, gotVals)
	}

	// page size 5 divides the range exactly: the last page ends right
	// before the exclusive end key, and must terminate without an
	// extra empty page
	pages := 0
	var token []byte
	for {
		pageKeys, _, next, err :=
			fst.SearchPage(nil, "q", start, end, token, 5)
		if err != nil {
			t.Fatalf("page %d error: %v", pages, err)
		}
		pages++
		if next == nil {
			if len(pageKeys) != 5 {
				t.Fatalf("expected final page of 5 keys, got %d",
					len(pageKeys))
			}
			break
		}
		token = next
	}
	if pages != 4 {
		t.Fatalf("expected exactly 4 pages, got %d", pages)
	}
}

func TestSearchPageEmptyAndZeroByteKeys(t *testing.T) {
	fst, _ := buildPageTestFST(t, pageTestKeys)

	// page size 1 forces tokens whose last delivered key is the empty
	// key and keys containing zero bytes
	gotKeys, _ := collectPaged(t, fst, nil, "q", nil, nil, 1)
	if string(gotKeys[0]) != "" {
		t.Fatalf("expected first key to be empty, got %q", gotKeys[0])
	}
	if string(gotKeys[1]) != "\x00" {
		t.Fatalf("expected second key to be zero byte, got %q", gotKeys[1])
	}
	for i, k := range pageTestKeys {
		if string(gotKeys[i]) != k {
			t.Fatalf("key %d: expected %q, got %q", i, k, gotKeys[i])
		}
	}
}

func TestSearchPageAcrossProcesses(t *testing.T) {
	fst1, data := buildPageTestFST(t, pageTestKeys)

	// first page from one "process"
	keys1, vals1, token, err := fst1.SearchPage(nil, "q", nil, nil, nil, 7)
	if err != nil {
		t.Fatalf("error reading first page: %v", err)
	}
	if token == nil {
		t.Fatalf("expected a continuation token")
	}

	// another "process" re-opens the same content and resumes
	fst2, err := Load(data)
	if err != nil {
		t.Fatalf("error reloading fst: %v", err)
	}

	var keys2 [][]byte
	var vals2 []uint64
	for token != nil {
		var pageKeys [][]byte
		var pageVals []uint64
		pageKeys, pageVals, token, err =
			fst2.SearchPage(nil, "q", nil, nil, token, 7)
		if err != nil {
			t.Fatalf("error resuming: %v", err)
		}
		keys2 = append(keys2, pageKeys...)
		vals2 = append(vals2, pageVals...)
	}

	wantKeys, wantVals := collectAll(t, fst1, nil, nil, nil)
	expectSameKeys(t, wantKeys,
		append(keys1, keys2...), wantVals, append(vals1, vals2...))
}

func TestSearchPageCorruptToken(t *testing.T) {
	fst, _ := buildPageTestFST(t, pageTestKeys)

	_, _, token, err := fst.SearchPage(nil, "q", nil, nil, nil, 3)
	if err != nil {
		t.Fatalf("error reading first page: %v", err)
	}

	flipped := append([]byte(nil), token...)
	flipped[len(flipped)/2] ^= 0xff

	badCRC := append([]byte(nil), token...)
	badCRC[len(badCRC)-1] ^= 0xff

	badVersion := append([]byte(nil), token...)
	badVersion[2] = 0xff

	corrupt := [][]byte{
		{},
		[]byte("junk"),
		token[:len(token)-1],
		token[:2],
		flipped,
		badCRC,
		badVersion,
	}
	for i, tok := range corrupt {
		_, _, _, err := fst.SearchPage(nil, "q", nil, nil, tok, 3)
		if !errors.Is(err, ErrPageTokenCorrupt) {
			t.Errorf("corrupt token %d: expected ErrPageTokenCorrupt, got %v",
				i, err)
		}
	}
}

func TestSearchPageMismatch(t *testing.T) {
	fst, _ := buildPageTestFST(t, pageTestKeys)

	_, _, token, err := fst.SearchPage(nil, "q1", nil, nil, nil, 3)
	if err != nil {
		t.Fatalf("error reading first page: %v", err)
	}

	// different dictionary content
	otherKeys := append([]string(nil), pageTestKeys...)
	otherKeys[len(otherKeys)-1] = "zzz"
	fstOther, _ := buildPageTestFST(t, otherKeys)
	_, _, _, err = fstOther.SearchPage(nil, "q1", nil, nil, token, 3)
	if !errors.Is(err, ErrPageTokenMismatch) {
		t.Errorf("different FST: expected ErrPageTokenMismatch, got %v", err)
	}

	// different query id
	_, _, _, err = fst.SearchPage(nil, "q2", nil, nil, token, 3)
	if !errors.Is(err, ErrPageTokenMismatch) {
		t.Errorf("different query id: expected ErrPageTokenMismatch, got %v",
			err)
	}

	// different start key
	_, _, _, err = fst.SearchPage(nil, "q1", []byte("b"), nil, token, 3)
	if !errors.Is(err, ErrPageTokenMismatch) {
		t.Errorf("different start: expected ErrPageTokenMismatch, got %v", err)
	}

	// different end key
	_, _, _, err = fst.SearchPage(nil, "q1", nil, []byte("z"), token, 3)
	if !errors.Is(err, ErrPageTokenMismatch) {
		t.Errorf("different end: expected ErrPageTokenMismatch, got %v", err)
	}

	// nil end key vs empty (non-nil) end key
	_, _, _, err = fst.SearchPage(nil, "q1", nil, []byte{}, token, 3)
	if !errors.Is(err, ErrPageTokenMismatch) {
		t.Errorf("nil vs empty end: expected ErrPageTokenMismatch, got %v",
			err)
	}

	// sanity: the same parameters must be accepted
	_, _, _, err = fst.SearchPage(nil, "q1", nil, nil, token, 3)
	if err != nil {
		t.Errorf("same parameters: expected no error, got %v", err)
	}
}

func TestSearchPageTokenIndependentOfBuffers(t *testing.T) {
	fst, _ := buildPageTestFST(t, pageTestKeys)

	keys1, _, token, err := fst.SearchPage(nil, "q", nil, nil, nil, 3)
	if err != nil {
		t.Fatalf("error reading first page: %v", err)
	}
	if len(keys1) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(keys1))
	}

	// scribble over the returned keys; the token must not alias them
	// (nor the iterator's internal buffers)
	for _, k := range keys1 {
		for i := range k {
			k[i] ^= 0xff
		}
	}

	keys2, _, _, err := fst.SearchPage(nil, "q", nil, nil, token, 3)
	if err != nil {
		t.Fatalf("error resuming: %v", err)
	}
	want := []string{"a", "a\x00", "a\x00b"}
	if len(keys2) != len(want) {
		t.Fatalf("expected %d keys, got %d", len(want), len(keys2))
	}
	for i := range want {
		if string(keys2[i]) != want[i] {
			t.Fatalf("key %d: expected %q, got %q", i, want[i], keys2[i])
		}
	}
}

func TestSearchPageWithAutomaton(t *testing.T) {
	fst, _ := buildPageTestFST(t, pageTestKeys)

	r, err := regexp.New(`t.*`)
	if err != nil {
		t.Fatalf("error building regexp automaton: %v", err)
	}

	wantKeys, wantVals := collectAll(t, fst, r, nil, nil)
	if len(wantKeys) != 3 {
		t.Fatalf("expected 3 matching keys, got %d", len(wantKeys))
	}

	for _, limit := range []int{1, 2, 100} {
		gotKeys, gotVals := collectPaged(t, fst, r, "regexp:t.*", nil, nil,
			limit)
		expectSameKeys(t, wantKeys, gotKeys, wantVals, gotVals)
	}
}

func TestSearchPageEmptyRange(t *testing.T) {
	fst, _ := buildPageTestFST(t, pageTestKeys)

	keys, vals, token, err :=
		fst.SearchPage(nil, "q", []byte("x"), []byte("y"), nil, 10)
	if err != nil {
		t.Fatalf("expected no error for empty range, got %v", err)
	}
	if len(keys) != 0 || len(vals) != 0 || token != nil {
		t.Fatalf("expected empty result, got keys=%q vals=%v token=%v",
			keys, vals, token)
	}
}

func TestSearchPageInvalidLimit(t *testing.T) {
	fst, _ := buildPageTestFST(t, pageTestKeys)

	for _, limit := range []int{0, -1} {
		_, _, _, err := fst.SearchPage(nil, "q", nil, nil, nil, limit)
		if err == nil {
			t.Errorf("expected error for limit %d", limit)
		}
	}
}
