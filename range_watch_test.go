// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import (
	"testing"
	"time"
)

// testRangeSchema builds a schema with a non-unique integer index so that
// tests exercise duplicate index keys.
func testRangeSchema() *DBSchema {
	return &DBSchema{
		Tables: map[string]*TableSchema{
			"main": &TableSchema{
				Name: "main",
				Indexes: map[string]*IndexSchema{
					"id": &IndexSchema{
						Name:    "id",
						Unique:  true,
						Indexer: &StringFieldIndex{Field: "ID"},
					},
					"score": &IndexSchema{
						Name:    "score",
						Indexer: &IntFieldIndex{Field: "Int"},
					},
				},
			},
		},
	}
}

func testRangeDB(t *testing.T) *MemDB {
	db, err := NewMemDB(testRangeSchema())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	return db
}

func insertScores(t *testing.T, db *MemDB, scores map[string]int) {
	t.Helper()
	txn := db.Txn(true)
	for id, score := range scores {
		if err := txn.Insert("main", TestObject{ID: id, Int: score}); err != nil {
			t.Fatalf("err inserting: %s", err)
		}
	}
	txn.Commit()
}

func rangeResults(t *testing.T, iter ResultIterator) []TestObject {
	t.Helper()
	result := []TestObject{}
	for obj := iter.Next(); obj != nil; obj = iter.Next() {
		result = append(result, obj.(TestObject))
	}
	return result
}

func checkWatchFired(t *testing.T, watch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-watch:
	case <-time.After(10 * time.Second):
		t.Fatalf("watch should have fired: %s", msg)
	}
}

func checkWatchQuiet(t *testing.T, watch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-watch:
		t.Fatalf("watch should not have fired: %s", msg)
	default:
	}
}

func TestTxn_WatchRange(t *testing.T) {
	db := testRangeDB(t)
	insertScores(t, db, map[string]int{
		"a": 40,
		"b": 59,
		"c": 60,
		"d": 60, // duplicate index key
		"e": 65,
		"f": 79,
		"g": 80,
		"h": 100,
	})

	cases := []struct {
		Name  string
		Lower interface{}
		Upper interface{}
		Want  []TestObject
	}{
		{
			Name:  "closed lower open upper",
			Lower: 60,
			Upper: 80,
			Want: []TestObject{
				{ID: "c", Int: 60},
				{ID: "d", Int: 60},
				{ID: "e", Int: 65},
				{ID: "f", Int: 79},
			},
		},
		{
			Name:  "nil lower bound",
			Lower: nil,
			Upper: 60,
			Want: []TestObject{
				{ID: "a", Int: 40},
				{ID: "b", Int: 59},
			},
		},
		{
			Name:  "nil upper bound",
			Lower: 80,
			Upper: nil,
			Want: []TestObject{
				{ID: "g", Int: 80},
				{ID: "h", Int: 100},
			},
		},
		{
			Name:  "empty range",
			Lower: 80,
			Upper: 60,
			Want:  []TestObject{},
		},
		{
			Name:  "no matches",
			Lower: 81,
			Upper: 99,
			Want:  []TestObject{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			txn := db.Txn(false)
			defer txn.Abort()
			iter, err := txn.WatchRange("main", "score", tc.Lower, tc.Upper)
			if err != nil {
				t.Fatalf("err watch range: %s", err)
			}

			result := rangeResults(t, iter)
			if len(result) != len(tc.Want) {
				t.Fatalf(" got: %#v\nwant: %#v", result, tc.Want)
			}
			for i, obj := range tc.Want {
				if result[i].ID != obj.ID || result[i].Int != obj.Int {
					t.Fatalf(" got: %#v\nwant: %#v", result, tc.Want)
				}
			}
		})
	}
}

func TestTxn_WatchRange_Watch(t *testing.T) {
	db := testRangeDB(t)

	// Seed the DB with objects that all fall outside of the watched range
	// [60, 80), including duplicate index keys.
	insertScores(t, db, map[string]int{
		"a": 10,
		"b": 10,
		"c": 50,
		"d": 90,
		"e": 100,
	})

	// Subscribe to the range. The result is empty for now.
	txn := db.Txn(false)
	iter, err := txn.WatchRange("main", "score", 60, 80)
	if err != nil {
		t.Fatalf("err watch range: %s", err)
	}
	if result := rangeResults(t, iter); len(result) != 0 {
		t.Fatalf("bad: %#v", result)
	}
	watch := iter.WatchCh()
	txn.Abort()

	if n := db.watchers.count(); n != 1 {
		t.Fatalf("bad: %d", n)
	}

	// Writes outside of the range must not wake the watcher.
	insertScores(t, db, map[string]int{"f": 5, "g": 95})
	txn = db.Txn(true)
	if err := txn.Insert("main", TestObject{ID: "c", Int: 55}); err != nil {
		t.Fatalf("err inserting: %s", err)
	}
	txn.Commit()
	checkWatchQuiet(t, watch, "writes outside of the range")

	// An aborted transaction must not wake the watcher either.
	txn = db.Txn(true)
	if err := txn.Insert("main", TestObject{ID: "h", Int: 70}); err != nil {
		t.Fatalf("err inserting: %s", err)
	}
	txn.Abort()
	checkWatchQuiet(t, watch, "aborted transaction")

	// A cross-boundary update moving an object into the range must wake the
	// watcher.
	txn = db.Txn(true)
	if err := txn.Insert("main", TestObject{ID: "c", Int: 65}); err != nil {
		t.Fatalf("err inserting: %s", err)
	}
	txn.Commit()
	checkWatchFired(t, watch, "update moving an object into the range")

	// The fired subscription must have been cleaned up.
	if n := db.watchers.count(); n != 0 {
		t.Fatalf("bad: %d", n)
	}

	// The observer must be able to read the change that woke it up.
	txn = db.Txn(false)
	iter, err = txn.WatchRange("main", "score", 60, 80)
	if err != nil {
		t.Fatalf("err watch range: %s", err)
	}
	result := rangeResults(t, iter)
	if len(result) != 1 || result[0].ID != "c" || result[0].Int != 65 {
		t.Fatalf("bad: %#v", result)
	}
	watch = iter.WatchCh()
	txn.Abort()

	// A cross-boundary update moving the object back out of the range must
	// wake the watcher: the old key is in the range even though the new key
	// is not.
	txn = db.Txn(true)
	if err := txn.Insert("main", TestObject{ID: "c", Int: 90}); err != nil {
		t.Fatalf("err inserting: %s", err)
	}
	txn.Commit()
	checkWatchFired(t, watch, "update moving an object out of the range")
}

func TestTxn_WatchRange_WatchBounds(t *testing.T) {
	db := testRangeDB(t)

	subscribe := func() <-chan struct{} {
		t.Helper()
		txn := db.Txn(false)
		iter, err := txn.WatchRange("main", "score", 60, 80)
		if err != nil {
			t.Fatalf("err watch range: %s", err)
		}
		watch := iter.WatchCh()
		txn.Abort()
		return watch
	}

	// The upper bound is exclusive: writing an object with the upper bound
	// value must not wake the watcher.
	watch := subscribe()
	insertScores(t, db, map[string]int{"a": 80})
	checkWatchQuiet(t, watch, "upper bound is exclusive")

	// The lower bound is inclusive: writing an object with the lower bound
	// value must wake the watcher.
	insertScores(t, db, map[string]int{"b": 60})
	checkWatchFired(t, watch, "lower bound is inclusive")

	// Deleting an object inside the range must wake the watcher.
	watch = subscribe()
	txn := db.Txn(true)
	if err := txn.Delete("main", TestObject{ID: "b"}); err != nil {
		t.Fatalf("err deleting: %s", err)
	}
	txn.Commit()
	checkWatchFired(t, watch, "delete inside the range")

	// Deleting an object outside of the range must not wake the watcher.
	watch = subscribe()
	txn = db.Txn(true)
	if err := txn.Delete("main", TestObject{ID: "a"}); err != nil {
		t.Fatalf("err deleting: %s", err)
	}
	txn.Commit()
	checkWatchQuiet(t, watch, "delete outside of the range")
}

func TestTxn_WatchRange_Snapshot(t *testing.T) {
	db := testRangeDB(t)
	insertScores(t, db, map[string]int{"a": 65})

	// Snapshots do not track watch subscriptions, so the watch channel
	// blocks forever.
	snap := db.Snapshot()
	txn := snap.Txn(false)
	defer txn.Abort()
	iter, err := txn.WatchRange("main", "score", 60, 80)
	if err != nil {
		t.Fatalf("err watch range: %s", err)
	}
	if result := rangeResults(t, iter); len(result) != 1 {
		t.Fatalf("bad: %#v", result)
	}
	if iter.WatchCh() != nil {
		t.Fatalf("bad: expected nil watch channel on a snapshot")
	}
}

func TestTxn_WatchRange_Errors(t *testing.T) {
	db := testRangeDB(t)
	txn := db.Txn(false)
	defer txn.Abort()

	if _, err := txn.WatchRange("nope", "score", 60, 80); err == nil {
		t.Fatalf("should fail on invalid table")
	}
	if _, err := txn.WatchRange("main", "nope", 60, 80); err == nil {
		t.Fatalf("should fail on invalid index")
	}
	if _, err := txn.WatchRange("main", "score", "bad", 80); err == nil {
		t.Fatalf("should fail on invalid lower bound")
	}
	if _, err := txn.WatchRange("main", "score", 60, "bad"); err == nil {
		t.Fatalf("should fail on invalid upper bound")
	}
}
