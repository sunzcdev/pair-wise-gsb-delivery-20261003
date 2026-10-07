package vellum

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
)

// paginationKeys includes the edge cases required for resumption tokens:
// the empty key, keys containing zero bytes and a run of generated keys.
var paginationKeys = [][]byte{
	{},
	{0x00},
	{0x00, 0x00},
	{'a'},
	{'a', 0x00},
	{'a', 0x00, 'b'},
	{'b'},
}

func init() {
	for i := 0; i < 100; i++ {
		paginationKeys = append(paginationKeys, []byte(fmt.Sprintf("key%04d", i)))
	}
	sort.Slice(paginationKeys, func(i, j int) bool {
		return bytes.Compare(paginationKeys[i], paginationKeys[j]) < 0
	})
}

func buildPaginationFST(t *testing.T, keys [][]byte) (*FST, []byte) {
	t.Helper()
	var buf bytes.Buffer
	b, err := New(&buf, nil)
	if err != nil {
		t.Fatalf("error creating builder: %v", err)
	}
	for i, k := range keys {
		if err := b.Insert(k, uint64(i+1)); err != nil {
			t.Fatalf("error inserting %q: %v", k, err)
		}
	}
	if err := b.Close(); err != nil {
		t.Fatalf("error closing builder: %v", err)
	}
	fst, err := Load(buf.Bytes())
	if err != nil {
		t.Fatalf("error loading fst: %v", err)
	}
	return fst, buf.Bytes()
}

func collectFullIteration(t *testing.T, fst *FST, start, end []byte) ([][]byte, []uint64) {
	t.Helper()
	itr, err := fst.Iterator(start, end)
	if err == ErrIteratorDone {
		return nil, nil
	}
	if err != nil {
		t.Fatalf("error creating iterator: %v", err)
	}
	defer itr.Close()
	var keys [][]byte
	var vals []uint64
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

// collectPaged reads all pages, round-tripping the token through
// Encode/DecodePageToken between pages to simulate the handoff to another
// process.  If reload is set, a fresh FST instance is used for every page.
func collectPaged(t *testing.T, fst *FST, data []byte, reload bool,
	queryID string, start, end []byte, pageSize int) ([][]byte, []uint64) {
	t.Helper()
	token := fst.NewPageToken(queryID, start, end)
	var keys [][]byte
	var vals []uint64
	for {
		enc, err := token.Encode()
		if err != nil {
			t.Fatalf("error encoding token: %v", err)
		}
		token, err = DecodePageToken(enc)
		if err != nil {
			t.Fatalf("error decoding token: %v", err)
		}
		pageFST := fst
		if reload {
			pageFST, err = Load(data)
			if err != nil {
				t.Fatalf("error reloading fst: %v", err)
			}
		}
		pageKeys, pageVals, next, err := pageFST.NextPage(nil, queryID, pageSize, token)
		if reload {
			pageFST.Close()
		}
		if err != nil {
			t.Fatalf("error reading page: %v", err)
		}
		if len(pageKeys) == 0 {
			break
		}
		if len(pageKeys) > pageSize {
			t.Fatalf("page larger than pageSize: %d > %d", len(pageKeys), pageSize)
		}
		keys = append(keys, pageKeys...)
		vals = append(vals, pageVals...)
		token = next
	}
	return keys, vals
}

func TestPaginationMatchesFullIteration(t *testing.T) {
	fst, data := buildPaginationFST(t, paginationKeys)
	defer fst.Close()

	wantKeys, wantVals := collectFullIteration(t, fst, nil, nil)
	if len(wantKeys) != len(paginationKeys) {
		t.Fatalf("expected %d keys from full iteration, got %d",
			len(paginationKeys), len(wantKeys))
	}

	for _, pageSize := range []int{1, 2, 3, 7, 50, len(paginationKeys), len(paginationKeys) + 1} {
		for _, reload := range []bool{false, true} {
			gotKeys, gotVals := collectPaged(t, fst, data, reload, "q1", nil, nil, pageSize)
			if !reflect.DeepEqual(wantKeys, gotKeys) {
				t.Fatalf("pageSize %d reload %v: keys mismatch\nwant %q\ngot %q",
					pageSize, reload, wantKeys, gotKeys)
			}
			if !reflect.DeepEqual(wantVals, gotVals) {
				t.Fatalf("pageSize %d reload %v: vals mismatch\nwant %v\ngot %v",
					pageSize, reload, wantVals, gotVals)
			}
		}
	}
}

func TestPaginationRangeEndingAtUpperBound(t *testing.T) {
	fst, data := buildPaginationFST(t, paginationKeys)
	defer fst.Close()

	// [a, key0010) holds exactly 14 keys: a, a\x00, a\x00b, b, key0000..key0009
	start, end := []byte("a"), []byte("key0010")
	wantKeys, wantVals := collectFullIteration(t, fst, start, end)
	if len(wantKeys) != 14 {
		t.Fatalf("expected 14 keys in range, got %d", len(wantKeys))
	}

	// page sizes that divide the range evenly, so the last page ends
	// exactly at the upper bound, plus uneven sizes
	for _, pageSize := range []int{1, 2, 7, 14, 5, 100} {
		gotKeys, gotVals := collectPaged(t, fst, data, false, "q-range", start, end, pageSize)
		if !reflect.DeepEqual(wantKeys, gotKeys) {
			t.Fatalf("pageSize %d: keys mismatch\nwant %q\ngot %q", pageSize, wantKeys, gotKeys)
		}
		if !reflect.DeepEqual(wantVals, gotVals) {
			t.Fatalf("pageSize %d: vals mismatch", pageSize)
		}
	}

	// a page ending exactly at the upper bound must be followed by an
	// empty page and a token that does not advance
	token := fst.NewPageToken("q-range", start, end)
	pageKeys, _, token, err := fst.NextPage(nil, "q-range", 14, token)
	if err != nil {
		t.Fatalf("error reading page: %v", err)
	}
	if len(pageKeys) != 14 {
		t.Fatalf("expected 14 keys, got %d", len(pageKeys))
	}
	pageKeys, _, next, err := fst.NextPage(nil, "q-range", 14, token)
	if err != nil {
		t.Fatalf("error reading final page: %v", err)
	}
	if len(pageKeys) != 0 {
		t.Fatalf("expected empty final page, got %d keys", len(pageKeys))
	}
	if !reflect.DeepEqual(token, next) {
		t.Fatalf("expected token to remain unchanged after exhaustion")
	}
}

func TestPaginationEmptyKeyAtPageBoundary(t *testing.T) {
	fst, _ := buildPaginationFST(t, paginationKeys)
	defer fst.Close()

	// first page of size 1 delivers exactly the empty key
	token := fst.NewPageToken("q", nil, nil)
	pageKeys, _, token, err := fst.NextPage(nil, "q", 1, token)
	if err != nil {
		t.Fatalf("error reading first page: %v", err)
	}
	if len(pageKeys) != 1 || len(pageKeys[0]) != 0 {
		t.Fatalf("expected first page to hold only the empty key, got %q", pageKeys)
	}

	// resuming must skip the empty key and deliver 0x00 next
	pageKeys, _, _, err = fst.NextPage(nil, "q", 1, token)
	if err != nil {
		t.Fatalf("error reading second page: %v", err)
	}
	if len(pageKeys) != 1 || !bytes.Equal(pageKeys[0], []byte{0x00}) {
		t.Fatalf("expected second page to hold key 0x00, got %q", pageKeys)
	}
}

func TestPageTokenMismatch(t *testing.T) {
	fst, _ := buildPaginationFST(t, paginationKeys)
	defer fst.Close()

	token := fst.NewPageToken("q1", nil, nil)
	_, _, token, err := fst.NextPage(nil, "q1", 3, token)
	if err != nil {
		t.Fatalf("error reading page: %v", err)
	}

	// resuming with a different query identifier must be rejected
	_, _, _, err = fst.NextPage(nil, "q2", 3, token)
	if !errors.Is(err, ErrPageTokenMismatch) {
		t.Fatalf("expected ErrPageTokenMismatch for different query ID, got %v", err)
	}

	// resuming against a different FST must be rejected
	other, _ := buildPaginationFST(t, paginationKeys[:len(paginationKeys)-1])
	defer other.Close()
	_, _, _, err = other.NextPage(nil, "q1", 3, token)
	if !errors.Is(err, ErrPageTokenMismatch) {
		t.Fatalf("expected ErrPageTokenMismatch for different FST, got %v", err)
	}

	// same query, same FST: still works
	pageKeys, _, _, err := fst.NextPage(nil, "q1", 3, token)
	if err != nil {
		t.Fatalf("error resuming with matching token: %v", err)
	}
	if len(pageKeys) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(pageKeys))
	}
}

func TestDecodePageTokenCorrupt(t *testing.T) {
	fst, _ := buildPaginationFST(t, paginationKeys)
	defer fst.Close()

	valid, err := fst.NewPageToken("q1", nil, nil).Encode()
	if err != nil {
		t.Fatalf("error encoding token: %v", err)
	}

	corrupted := map[string][]byte{
		"garbage":          []byte("this is not a token"),
		"empty":            {},
		"truncated":        valid[:len(valid)/2],
		"wrong version":    []byte(`{"version":99,"fingerprint":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`),
		"bad fingerprint":  []byte(`{"version":1,"fingerprint":"AA=="}`),
		"missing fields":   []byte(`{"version":1}`),
		"trailing garbage": append(append([]byte(nil), valid...), 0x00),
	}
	for name, data := range corrupted {
		_, err := DecodePageToken(data)
		if !errors.Is(err, ErrInvalidPageToken) {
			t.Errorf("%s: expected ErrInvalidPageToken, got %v", name, err)
		}
	}

	// a structurally valid token for another FST is not "corrupt" but
	// must be rejected on use
	other, _ := buildPaginationFST(t, paginationKeys[1:])
	defer other.Close()
	foreign, err := other.NewPageToken("q1", nil, nil).Encode()
	if err != nil {
		t.Fatalf("error encoding foreign token: %v", err)
	}
	token, err := DecodePageToken(foreign)
	if err != nil {
		t.Fatalf("foreign token should decode cleanly: %v", err)
	}
	_, _, _, err = fst.NextPage(nil, "q1", 1, token)
	if !errors.Is(err, ErrPageTokenMismatch) {
		t.Fatalf("expected ErrPageTokenMismatch, got %v", err)
	}
}

func TestPageTokenDoesNotBorrowIteratorBuffers(t *testing.T) {
	fst, _ := buildPaginationFST(t, paginationKeys)
	defer fst.Close()

	token := fst.NewPageToken("q", nil, nil)
	var all [][]byte
	for {
		pageKeys, _, next, err := fst.NextPage(nil, "q", 4, token)
		if err != nil {
			t.Fatalf("error reading page: %v", err)
		}
		if len(pageKeys) == 0 {
			break
		}
		all = append(all, pageKeys...)
		token = next
	}
	// if keys or tokens had borrowed the iterator's internal buffers,
	// later pages would have mutated the earlier results
	if len(all) != len(paginationKeys) {
		t.Fatalf("expected %d keys, got %d", len(paginationKeys), len(all))
	}
	for i := range all {
		if !bytes.Equal(all[i], paginationKeys[i]) {
			t.Fatalf("key %d mutated after delivery: want %q, got %q",
				i, paginationKeys[i], all[i])
		}
	}
}
