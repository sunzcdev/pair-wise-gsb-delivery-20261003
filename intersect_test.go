// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import (
	"sort"
	"testing"
	"time"
)

type intersectInstance struct {
	ID     string
	DC     string
	Status string
	Tags   []string
}

func intersectTestSchema() *DBSchema {
	return &DBSchema{
		Tables: map[string]*TableSchema{
			"instance": {
				Name: "instance",
				Indexes: map[string]*IndexSchema{
					"id": {
						Name:    "id",
						Unique:  true,
						Indexer: &StringFieldIndex{Field: "ID"},
					},
					"dc": {
						Name:    "dc",
						Indexer: &StringFieldIndex{Field: "DC"},
					},
					"status": {
						Name:    "status",
						Indexer: &StringFieldIndex{Field: "Status"},
					},
					"tags": {
						Name:         "tags",
						AllowMissing: true,
						Indexer:      &StringSliceFieldIndex{Field: "Tags"},
					},
				},
			},
		},
	}
}

func intersectTestDB(t *testing.T) *MemDB {
	t.Helper()
	db, err := NewMemDB(intersectTestSchema())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	return db
}

func seedInstances(t *testing.T, db *MemDB) {
	t.Helper()
	txn := db.Txn(true)
	instances := []*intersectInstance{
		{ID: "a", DC: "dc1", Status: "ready", Tags: []string{"web", "v1"}},
		{ID: "b", DC: "dc1", Status: "ready", Tags: []string{"web"}},
		{ID: "c", DC: "dc1", Status: "busy", Tags: []string{"db"}},
		{ID: "d", DC: "dc2", Status: "ready", Tags: []string{"web", "v1"}},
		{ID: "e", DC: "dc2", Status: "busy", Tags: []string{"v1"}},
	}
	for _, inst := range instances {
		if err := txn.Insert("instance", inst); err != nil {
			t.Fatalf("err: %v", err)
		}
	}
	txn.Commit()
}

func intersectIDs(t *testing.T, iter ResultIterator) []string {
	t.Helper()
	var ids []string
	for obj := iter.Next(); obj != nil; obj = iter.Next() {
		ids = append(ids, obj.(*intersectInstance).ID)
	}
	sort.Strings(ids)
	return ids
}

func expectIDs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func expectWatchFires(t *testing.T, watchCh <-chan struct{}) {
	t.Helper()
	select {
	case <-watchCh:
	case <-time.After(time.Second):
		t.Fatalf("watch did not fire")
	}
}

func TestIntersect_Basic(t *testing.T) {
	db := intersectTestDB(t)
	seedInstances(t, db)

	txn := db.Txn(false)
	defer txn.Abort()

	// The motivating case: ready instances in a datacenter.
	iter, err := txn.Intersect("instance",
		IndexQuery{Index: "dc", Args: []interface{}{"dc1"}},
		IndexQuery{Index: "status", Args: []interface{}{"ready"}},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	expectIDs(t, intersectIDs(t, iter), []string{"a", "b"})

	// The order of the queries must not matter.
	iter, err = txn.Intersect("instance",
		IndexQuery{Index: "status", Args: []interface{}{"ready"}},
		IndexQuery{Index: "dc", Args: []interface{}{"dc1"}},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	expectIDs(t, intersectIDs(t, iter), []string{"a", "b"})
}

func TestIntersect_SingleQuery(t *testing.T) {
	db := intersectTestDB(t)
	seedInstances(t, db)

	txn := db.Txn(false)
	defer txn.Abort()

	iter, err := txn.Intersect("instance",
		IndexQuery{Index: "status", Args: []interface{}{"ready"}},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	expectIDs(t, intersectIDs(t, iter), []string{"a", "b", "d"})
}

func TestIntersect_MultiValueIndex(t *testing.T) {
	db := intersectTestDB(t)
	seedInstances(t, db)

	txn := db.Txn(false)
	defer txn.Abort()

	// Two conditions on the same multi-value index: objects carrying both
	// tags match, and must appear exactly once.
	iter, err := txn.Intersect("instance",
		IndexQuery{Index: "tags", Args: []interface{}{"web"}},
		IndexQuery{Index: "tags", Args: []interface{}{"v1"}},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	expectIDs(t, intersectIDs(t, iter), []string{"a", "d"})

	// A multi-value condition combined with a single-value condition.
	iter, err = txn.Intersect("instance",
		IndexQuery{Index: "tags", Args: []interface{}{"web"}},
		IndexQuery{Index: "dc", Args: []interface{}{"dc1"}},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	expectIDs(t, intersectIDs(t, iter), []string{"a", "b"})

	// Repeating the identical condition must not duplicate results.
	iter, err = txn.Intersect("instance",
		IndexQuery{Index: "tags", Args: []interface{}{"web"}},
		IndexQuery{Index: "tags", Args: []interface{}{"web"}},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	expectIDs(t, intersectIDs(t, iter), []string{"a", "b", "d"})
}

func TestIntersect_NoResults(t *testing.T) {
	db := intersectTestDB(t)
	seedInstances(t, db)

	txn := db.Txn(false)
	defer txn.Abort()

	// One of the conditions matches nothing, so the intersection is empty
	// but the watch must still be usable.
	iter, err := txn.Intersect("instance",
		IndexQuery{Index: "dc", Args: []interface{}{"dc9"}},
		IndexQuery{Index: "status", Args: []interface{}{"ready"}},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	expectIDs(t, intersectIDs(t, iter), nil)

	watchCh := iter.WatchCh()
	if watchCh == nil {
		t.Fatalf("watch channel must not be nil")
	}

	wtxn := db.Txn(true)
	if err := wtxn.Insert("instance", &intersectInstance{ID: "z", DC: "dc9", Status: "ready"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()

	expectWatchFires(t, watchCh)
}

func TestIntersect_DeleteReinsert(t *testing.T) {
	db := intersectTestDB(t)
	seedInstances(t, db)

	query := func() ([]string, <-chan struct{}) {
		txn := db.Txn(false)
		defer txn.Abort()
		iter, err := txn.Intersect("instance",
			IndexQuery{Index: "dc", Args: []interface{}{"dc1"}},
			IndexQuery{Index: "tags", Args: []interface{}{"web"}},
		)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		return intersectIDs(t, iter), iter.WatchCh()
	}

	ids, watchCh := query()
	expectIDs(t, ids, []string{"a", "b"})

	// Delete "a"; the watch must fire.
	wtxn := db.Txn(true)
	if err := wtxn.Delete("instance", &intersectInstance{ID: "a"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()
	expectWatchFires(t, watchCh)

	ids, _ = query()
	expectIDs(t, ids, []string{"b"})

	// Re-insert "a" under the same primary key; it must show up exactly once.
	wtxn = db.Txn(true)
	if err := wtxn.Insert("instance", &intersectInstance{ID: "a", DC: "dc1", Status: "ready", Tags: []string{"web", "v1"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()

	ids, _ = query()
	expectIDs(t, ids, []string{"a", "b"})
}

func TestIntersect_ConditionMigration(t *testing.T) {
	db := intersectTestDB(t)
	seedInstances(t, db)

	query := func(status string) ([]string, <-chan struct{}) {
		txn := db.Txn(false)
		defer txn.Abort()
		iter, err := txn.Intersect("instance",
			IndexQuery{Index: "dc", Args: []interface{}{"dc1"}},
			IndexQuery{Index: "status", Args: []interface{}{status}},
		)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		return intersectIDs(t, iter), iter.WatchCh()
	}

	readyIDs, readyWatch := query("ready")
	expectIDs(t, readyIDs, []string{"a", "b"})

	// Migrate "b" from ready to busy.
	wtxn := db.Txn(true)
	if err := wtxn.Insert("instance", &intersectInstance{ID: "b", DC: "dc1", Status: "busy", Tags: []string{"web"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()

	// The watch established before the migration must fire.
	expectWatchFires(t, readyWatch)

	readyIDs, _ = query("ready")
	expectIDs(t, readyIDs, []string{"a"})

	busyIDs, _ := query("busy")
	expectIDs(t, busyIDs, []string{"b", "c"})
}

func TestIntersect_WatchWriteNearSubscribe(t *testing.T) {
	db := intersectTestDB(t)
	seedInstances(t, db)

	// Open the read transaction, then commit a write before the query (and
	// its watches) are established. The results reflect the old snapshot,
	// but the watch must still fire so the caller re-queries.
	txn := db.Txn(false)
	defer txn.Abort()

	wtxn := db.Txn(true)
	if err := wtxn.Insert("instance", &intersectInstance{ID: "f", DC: "dc1", Status: "ready"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()

	iter, err := txn.Intersect("instance",
		IndexQuery{Index: "dc", Args: []interface{}{"dc1"}},
		IndexQuery{Index: "status", Args: []interface{}{"ready"}},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Snapshot semantics: "f" is not visible to this transaction.
	expectIDs(t, intersectIDs(t, iter), []string{"a", "b"})

	// The wakeup must not be missed even though the write landed before the
	// watches were registered.
	expectWatchFires(t, iter.WatchCh())
}

func TestIntersect_WatchOnMatchingWrite(t *testing.T) {
	db := intersectTestDB(t)
	seedInstances(t, db)

	txn := db.Txn(false)
	defer txn.Abort()

	iter, err := txn.Intersect("instance",
		IndexQuery{Index: "dc", Args: []interface{}{"dc1"}},
		IndexQuery{Index: "status", Args: []interface{}{"ready"}},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	expectIDs(t, intersectIDs(t, iter), []string{"a", "b"})
	watchCh := iter.WatchCh()

	wtxn := db.Txn(true)
	if err := wtxn.Insert("instance", &intersectInstance{ID: "g", DC: "dc1", Status: "ready"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()

	expectWatchFires(t, watchCh)
}

func TestIntersect_UsesSmallestCandidateSet(t *testing.T) {
	db := intersectTestDB(t)

	// One datacenter holds many instances, but only one of them is busy, so
	// the status condition is by far the smaller candidate set.
	wtxn := db.Txn(true)
	for i := 0; i < 100; i++ {
		inst := &intersectInstance{
			ID:     string(rune(i)) + "-inst",
			DC:     "dc1",
			Status: "ready",
		}
		if err := wtxn.Insert("instance", inst); err != nil {
			t.Fatalf("err: %v", err)
		}
	}
	if err := wtxn.Insert("instance", &intersectInstance{ID: "lonely", DC: "dc1", Status: "busy"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()

	txn := db.Txn(false)
	defer txn.Abort()

	// The large condition is listed first; the small one must still drive.
	iter, err := txn.Intersect("instance",
		IndexQuery{Index: "dc", Args: []interface{}{"dc1"}},
		IndexQuery{Index: "status", Args: []interface{}{"busy"}},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	inner, ok := iter.(*intersectIterator)
	if !ok {
		t.Fatalf("expected *intersectIterator, got %T", iter)
	}
	if inner.driver != 1 {
		t.Fatalf("expected query 1 to drive the intersection, got %d", inner.driver)
	}
	expectIDs(t, intersectIDs(t, iter), []string{"lonely"})

	// Reversed order must pick the other query as the driver.
	iter, err = txn.Intersect("instance",
		IndexQuery{Index: "status", Args: []interface{}{"busy"}},
		IndexQuery{Index: "dc", Args: []interface{}{"dc1"}},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	inner = iter.(*intersectIterator)
	if inner.driver != 0 {
		t.Fatalf("expected query 0 to drive the intersection, got %d", inner.driver)
	}
	expectIDs(t, intersectIDs(t, iter), []string{"lonely"})
}

func TestIntersect_Errors(t *testing.T) {
	db := intersectTestDB(t)

	txn := db.Txn(false)
	defer txn.Abort()

	if _, err := txn.Intersect("instance"); err == nil {
		t.Fatalf("expected an error with no queries")
	}
	if _, err := txn.Intersect("nope",
		IndexQuery{Index: "dc", Args: []interface{}{"dc1"}},
		IndexQuery{Index: "status", Args: []interface{}{"ready"}},
	); err == nil {
		t.Fatalf("expected an error for an invalid table")
	}
	if _, err := txn.Intersect("instance",
		IndexQuery{Index: "nope", Args: []interface{}{"dc1"}},
		IndexQuery{Index: "status", Args: []interface{}{"ready"}},
	); err == nil {
		t.Fatalf("expected an error for an invalid index")
	}
}
