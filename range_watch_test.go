package memdb

import (
	"testing"
	"time"
)

func watchRangeResults(t *testing.T, iter *RangeIterator) []string {
	t.Helper()
	var ids []string
	for obj := iter.Next(); obj != nil; obj = iter.Next() {
		ids = append(ids, obj.(*TestObject).ID)
	}
	return ids
}

func assertWatchQuiet(t *testing.T, iter *RangeIterator) {
	t.Helper()
	select {
	case <-iter.WatchCh():
		t.Fatalf("watch should not have fired")
	case <-time.After(100 * time.Millisecond):
	}
}

func assertWatchFired(t *testing.T, iter *RangeIterator) {
	t.Helper()
	select {
	case <-iter.WatchCh():
	case <-time.After(5 * time.Second):
		t.Fatalf("watch should have fired")
	}
}

func TestTxn_WatchRange_Bounds(t *testing.T) {
	db := testDB(t)

	txn := db.Txn(true)
	for _, obj := range []*TestObject{
		{ID: "before", Foo: "a", Qux: []string{"q"}},
		{ID: "lower", Foo: "b", Qux: []string{"q"}},
		{ID: "lower-dup", Foo: "b", Qux: []string{"q"}},
		{ID: "inside", Foo: "c", Qux: []string{"q"}},
		{ID: "upper", Foo: "d", Qux: []string{"q"}},
		{ID: "after", Foo: "e", Qux: []string{"q"}},
	} {
		if err := txn.Insert("main", obj); err != nil {
			t.Fatalf("err: %v", err)
		}
	}
	txn.Commit()

	txn = db.Txn(false)
	defer txn.Abort()

	// The range [b, d) includes the lower bound and its duplicates, and
	// excludes the upper bound.
	iter, err := txn.WatchRange("main", "foo", "b", "d")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer iter.Stop()

	got := watchRangeResults(t, iter)
	want := []string{"lower", "lower-dup", "inside"}
	if len(got) != len(want) {
		t.Fatalf("bad: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bad: got %v want %v", got, want)
		}
	}
}

func TestTxn_WatchRange_EmptyRange(t *testing.T) {
	db := testDB(t)

	txn := db.Txn(true)
	if err := txn.Insert("main", &TestObject{ID: "obj", Foo: "c", Qux: []string{"q"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	txn = db.Txn(false)
	defer txn.Abort()

	// A lower bound at or above the upper bound is an empty range.
	iter, err := txn.WatchRange("main", "foo", "d", "b")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer iter.Stop()
	if obj := iter.Next(); obj != nil {
		t.Fatalf("bad: %#v", obj)
	}

	// The watch never fires for an empty range.
	wtxn := db.Txn(true)
	if err := wtxn.Insert("main", &TestObject{ID: "obj2", Foo: "c", Qux: []string{"q"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()
	assertWatchQuiet(t, iter)
}

func TestTxn_WatchRange_OutsideRangeQuiet(t *testing.T) {
	db := testDB(t)

	txn := db.Txn(false)
	iter, err := txn.WatchRange("main", "foo", "b", "d")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer iter.Stop()
	txn.Abort()

	// Writes outside the range must not wake the watcher: inserts below and
	// above the range, and an update that stays outside the range.
	wtxn := db.Txn(true)
	if err := wtxn.Insert("main", &TestObject{ID: "low", Foo: "a", Qux: []string{"q"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := wtxn.Insert("main", &TestObject{ID: "high", Foo: "d", Qux: []string{"q"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := wtxn.Insert("main", &TestObject{ID: "mover", Foo: "a", Qux: []string{"q"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()

	wtxn = db.Txn(true)
	if err := wtxn.Insert("main", &TestObject{ID: "mover", Foo: "e", Qux: []string{"q"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()

	assertWatchQuiet(t, iter)
}

func TestTxn_WatchRange_CrossBoundaryUpdate(t *testing.T) {
	db := testDB(t)

	txn := db.Txn(true)
	if err := txn.Insert("main", &TestObject{ID: "enter", Foo: "a", Qux: []string{"q"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := txn.Insert("main", &TestObject{ID: "leave", Foo: "c", Qux: []string{"q"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	// Watch the range and move an object into it: the new key is in range.
	txn = db.Txn(false)
	iter, err := txn.WatchRange("main", "foo", "b", "d")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Abort()

	wtxn := db.Txn(true)
	if err := wtxn.Insert("main", &TestObject{ID: "enter", Foo: "c", Qux: []string{"q"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()

	assertWatchFired(t, iter)
	if n := db.rangeWatcherCount(); n != 0 {
		t.Fatalf("bad: %d subscriptions left after firing", n)
	}

	// Watch again and move an object out of the range: the old key is in
	// range even though the new key is not.
	txn = db.Txn(false)
	iter, err = txn.WatchRange("main", "foo", "b", "d")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Abort()
	defer iter.Stop()

	wtxn = db.Txn(true)
	if err := wtxn.Insert("main", &TestObject{ID: "leave", Foo: "e", Qux: []string{"q"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()

	assertWatchFired(t, iter)
}

func TestTxn_WatchRange_InsertDelete(t *testing.T) {
	db := testDB(t)

	txn := db.Txn(false)
	iter, err := txn.WatchRange("main", "foo", "b", "d")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Abort()

	// An insert inside the range wakes the watcher.
	wtxn := db.Txn(true)
	if err := wtxn.Insert("main", &TestObject{ID: "obj", Foo: "c", Qux: []string{"q"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()
	assertWatchFired(t, iter)

	// A delete of a row inside the range wakes the watcher, even when other
	// rows with the same indexed value remain.
	txn = db.Txn(false)
	iter, err = txn.WatchRange("main", "foo", "b", "d")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Abort()
	defer iter.Stop()

	wtxn = db.Txn(true)
	if err := wtxn.Insert("main", &TestObject{ID: "dup", Foo: "c", Qux: []string{"q"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := wtxn.Delete("main", &TestObject{ID: "obj"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()
	assertWatchFired(t, iter)
}

func TestTxn_WatchRange_WakeSeesChanges(t *testing.T) {
	db := testDB(t)

	txn := db.Txn(false)
	iter, err := txn.WatchRange("main", "foo", "b", "d")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Abort()
	defer iter.Stop()

	wtxn := db.Txn(true)
	if err := wtxn.Insert("main", &TestObject{ID: "obj", Foo: "c", Qux: []string{"q"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()

	// Once the watch fires, a new read transaction must already observe the
	// committed change.
	assertWatchFired(t, iter)
	rtxn := db.Txn(false)
	defer rtxn.Abort()
	raw, err := rtxn.First("main", "foo", "c")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if raw == nil || raw.(*TestObject).ID != "obj" {
		t.Fatalf("bad: %#v", raw)
	}
}

func TestTxn_WatchRange_Stop(t *testing.T) {
	db := testDB(t)

	txn := db.Txn(false)
	iter1, err := txn.WatchRange("main", "foo", "b", "d")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	iter2, err := txn.WatchRange("main", "foo", "b", "d")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Abort()

	if n := db.rangeWatcherCount(); n != 2 {
		t.Fatalf("bad: %d subscriptions", n)
	}

	// Stop releases the subscription; it is idempotent.
	iter1.Stop()
	iter1.Stop()
	if n := db.rangeWatcherCount(); n != 1 {
		t.Fatalf("bad: %d subscriptions", n)
	}

	// A stopped subscription does not fire, the remaining one does and is
	// cleaned up afterwards.
	wtxn := db.Txn(true)
	if err := wtxn.Insert("main", &TestObject{ID: "obj", Foo: "c", Qux: []string{"q"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()

	assertWatchQuiet(t, iter1)
	assertWatchFired(t, iter2)
	if n := db.rangeWatcherCount(); n != 0 {
		t.Fatalf("bad: %d subscriptions left", n)
	}

	// Stopping after firing is a no-op and must not panic on a closed
	// channel.
	iter2.Stop()
}

func TestTxn_WatchRange_Errors(t *testing.T) {
	db := testDB(t)
	txn := db.Txn(false)
	defer txn.Abort()

	if _, err := txn.WatchRange("nope", "foo", "b", "d"); err == nil {
		t.Fatalf("should fail on invalid table")
	}
	if _, err := txn.WatchRange("main", "nope", "b", "d"); err == nil {
		t.Fatalf("should fail on invalid index")
	}
	if n := db.rangeWatcherCount(); n != 0 {
		t.Fatalf("bad: %d subscriptions", n)
	}
}
