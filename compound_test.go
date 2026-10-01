// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

// collectCompound drains a ResultIterator into a slice of objects.
func collectCompound(iter ResultIterator) []interface{} {
	var out []interface{}
	for {
		obj := iter.Next()
		if obj == nil {
			return out
		}
		out = append(out, obj)
	}
}

// compoundIDs drains a ResultIterator and returns the sorted primary IDs of
// the resulting TestObjects.
func compoundIDs(iter ResultIterator) []string {
	var ids []string
	for _, raw := range collectCompound(iter) {
		ids = append(ids, raw.(*TestObject).ID)
	}
	sort.Strings(ids)
	return ids
}

func requireWatchFires(t *testing.T, watchCh <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-watchCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("watch did not fire: %s", msg)
	}
}

func requireWatchSilent(t *testing.T, watchCh <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-watchCh:
		t.Fatalf("watch fired unexpectedly: %s", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestTxn_GetCompound_Intersection(t *testing.T) {
	db := testDB(t)
	txn := db.Txn(true)
	objs := []*TestObject{
		{ID: "1", Foo: "a", Qux: []string{"x", "y"}},
		{ID: "2", Foo: "a", Qux: []string{"y"}},
		{ID: "3", Foo: "b", Qux: []string{"x", "y"}},
		{ID: "4", Foo: "b", Qux: []string{"z"}},
	}
	for _, obj := range objs {
		if err := txn.Insert("main", obj); err != nil {
			t.Fatalf("err: %v", err)
		}
	}
	txn.Commit()

	txn = db.Txn(false)
	defer txn.Abort()

	// Intersection of foo=a ({1,2}) and qux=x ({1,3}) is {1}.
	iter, err := txn.GetCompound("main",
		QueryCondition{Index: "foo", Args: []interface{}{"a"}},
		QueryCondition{Index: "qux", Args: []interface{}{"x"}},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ids := compoundIDs(iter); fmt.Sprint(ids) != "[1]" {
		t.Fatalf("bad: %v", ids)
	}

	// Intersection of foo=a ({1,2}) and qux=y ({1,2,3}) is {1,2}.
	iter, err = txn.GetCompound("main",
		QueryCondition{Index: "foo", Args: []interface{}{"a"}},
		QueryCondition{Index: "qux", Args: []interface{}{"y"}},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ids := compoundIDs(iter); fmt.Sprint(ids) != "[1 2]" {
		t.Fatalf("bad: %v", ids)
	}

	// Same query with the conditions swapped must give the same results.
	iter, err = txn.GetCompound("main",
		QueryCondition{Index: "qux", Args: []interface{}{"y"}},
		QueryCondition{Index: "foo", Args: []interface{}{"a"}},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ids := compoundIDs(iter); fmt.Sprint(ids) != "[1 2]" {
		t.Fatalf("bad: %v", ids)
	}
}

func TestTxn_GetCompound_MultiValueDedup(t *testing.T) {
	db := testDB(t)
	txn := db.Txn(true)

	// Object 1 is indexed twice under the qux_prefix "abc" prefix. Padding
	// objects make the foo candidate set much larger so that the multi-value
	// index drives the iteration and would emit object 1 twice without
	// de-duplication.
	if err := txn.Insert("main", &TestObject{ID: "1", Foo: "a", Qux: []string{"abc1", "abc2"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	for i := 0; i < 10; i++ {
		obj := &TestObject{ID: fmt.Sprintf("pad-%d", i), Foo: "a", Qux: []string{"zzz"}}
		if err := txn.Insert("main", obj); err != nil {
			t.Fatalf("err: %v", err)
		}
	}
	txn.Commit()

	txn = db.Txn(false)
	defer txn.Abort()

	iter, err := txn.GetCompound("main",
		QueryCondition{Index: "foo", Args: []interface{}{"a"}},
		QueryCondition{Index: "qux_prefix", Args: []interface{}{"abc"}},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	ids := compoundIDs(iter)
	if fmt.Sprint(ids) != "[1]" {
		t.Fatalf("bad: %v", ids)
	}
}

func TestTxn_GetCompound_NoResults(t *testing.T) {
	db := testDB(t)
	txn := db.Txn(true)
	if err := txn.Insert("main", &TestObject{ID: "1", Foo: "a", Qux: []string{"x"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	txn = db.Txn(false)
	defer txn.Abort()

	// One condition matches nothing, so the intersection is empty.
	iter, err := txn.GetCompound("main",
		QueryCondition{Index: "foo", Args: []interface{}{"missing"}},
		QueryCondition{Index: "qux", Args: []interface{}{"x"}},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if obj := iter.Next(); obj != nil {
		t.Fatalf("bad: %#v", obj)
	}
	if iter.WatchCh() == nil {
		t.Fatalf("watch channel should be exposed even with no results")
	}

	// Both conditions match, but no object satisfies both.
	iter, err = txn.GetCompound("main",
		QueryCondition{Index: "foo", Args: []interface{}{"a"}},
		QueryCondition{Index: "qux", Args: []interface{}{"y"}},
	)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if obj := iter.Next(); obj != nil {
		t.Fatalf("bad: %#v", obj)
	}
}

func TestTxn_GetCompound_DeleteReinsert(t *testing.T) {
	db := testDB(t)
	txn := db.Txn(true)
	obj := &TestObject{ID: "1", Foo: "a", Qux: []string{"x"}}
	if err := txn.Insert("main", obj); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	query := func() (ResultIterator, *Txn) {
		txn := db.Txn(false)
		iter, err := txn.GetCompound("main",
			QueryCondition{Index: "foo", Args: []interface{}{"a"}},
			QueryCondition{Index: "qux", Args: []interface{}{"x"}},
		)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		return iter, txn
	}

	// The object is present initially.
	iter, rtxn := query()
	if ids := compoundIDs(iter); fmt.Sprint(ids) != "[1]" {
		t.Fatalf("bad: %v", ids)
	}
	rtxn.Abort()

	// Delete the object: the watch established before the delete must fire
	// and a fresh query must be empty.
	iter, rtxn = query()
	watchCh := iter.WatchCh()
	wtxn := db.Txn(true)
	if err := wtxn.Delete("main", obj); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()
	requireWatchFires(t, watchCh, "delete of matching object")
	rtxn.Abort()

	iter, rtxn = query()
	if ids := compoundIDs(iter); len(ids) != 0 {
		t.Fatalf("bad: %v", ids)
	}
	rtxn.Abort()

	// Re-insert the object: the watch on the empty result must fire and a
	// fresh query must see the object again.
	iter, rtxn = query()
	watchCh = iter.WatchCh()
	wtxn = db.Txn(true)
	if err := wtxn.Insert("main", obj); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()
	requireWatchFires(t, watchCh, "re-insert of matching object")
	rtxn.Abort()

	iter, rtxn = query()
	if ids := compoundIDs(iter); fmt.Sprint(ids) != "[1]" {
		t.Fatalf("bad: %v", ids)
	}
	rtxn.Abort()
}

func TestTxn_GetCompound_ValueMigration(t *testing.T) {
	db := testDB(t)
	txn := db.Txn(true)
	obj := &TestObject{ID: "1", Foo: "ready", Qux: []string{"x"}}
	if err := txn.Insert("main", obj); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	query := func() (ResultIterator, *Txn) {
		txn := db.Txn(false)
		iter, err := txn.GetCompound("main",
			QueryCondition{Index: "foo", Args: []interface{}{"ready"}},
			QueryCondition{Index: "qux", Args: []interface{}{"x"}},
		)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		return iter, txn
	}

	iter, rtxn := query()
	if ids := compoundIDs(iter); fmt.Sprint(ids) != "[1]" {
		t.Fatalf("bad: %v", ids)
	}
	rtxn.Abort()

	// Migrate the object from foo=ready to foo=pending. The watch must fire
	// and the object must leave the result set.
	iter, rtxn = query()
	watchCh := iter.WatchCh()
	wtxn := db.Txn(true)
	if err := wtxn.Insert("main", &TestObject{ID: "1", Foo: "pending", Qux: []string{"x"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()
	requireWatchFires(t, watchCh, "object migrating out of the condition value")
	rtxn.Abort()

	iter, rtxn = query()
	if ids := compoundIDs(iter); len(ids) != 0 {
		t.Fatalf("bad: %v", ids)
	}
	rtxn.Abort()

	// Migrate it back to foo=ready.
	iter, rtxn = query()
	watchCh = iter.WatchCh()
	wtxn = db.Txn(true)
	if err := wtxn.Insert("main", &TestObject{ID: "1", Foo: "ready", Qux: []string{"x"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()
	requireWatchFires(t, watchCh, "object migrating into the condition value")
	rtxn.Abort()

	iter, rtxn = query()
	if ids := compoundIDs(iter); fmt.Sprint(ids) != "[1]" {
		t.Fatalf("bad: %v", ids)
	}
	rtxn.Abort()
}

func TestTxn_GetCompound_Watch(t *testing.T) {
	db := testDB(t)
	txn := db.Txn(true)
	if err := txn.Insert("main", &TestObject{ID: "1", Foo: "a", Qux: []string{"x"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	query := func() (ResultIterator, *Txn) {
		txn := db.Txn(false)
		iter, err := txn.GetCompound("main",
			QueryCondition{Index: "foo", Args: []interface{}{"a"}},
			QueryCondition{Index: "qux", Args: []interface{}{"x"}},
		)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		return iter, txn
	}

	// A write committed right after the query is established must not be
	// missed: the watch was registered before the write started.
	iter, rtxn := query()
	watchCh := iter.WatchCh()
	wtxn := db.Txn(true)
	if err := wtxn.Insert("main", &TestObject{ID: "2", Foo: "a", Qux: []string{"x"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()
	requireWatchFires(t, watchCh, "insert immediately after establishing the watch")
	rtxn.Abort()

	// A write touching only one of the conditions still fires the combined
	// watch, even though the intersection is unchanged.
	iter, rtxn = query()
	watchCh = iter.WatchCh()
	wtxn = db.Txn(true)
	if err := wtxn.Insert("main", &TestObject{ID: "3", Foo: "a", Qux: []string{"other"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()
	requireWatchFires(t, watchCh, "insert matching only one condition")
	rtxn.Abort()

	// An unrelated write must not fire the watch.
	iter, rtxn = query()
	watchCh = iter.WatchCh()
	wtxn = db.Txn(true)
	if err := wtxn.Insert("main", &TestObject{ID: "4", Foo: "unrelated", Qux: []string{"other"}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	wtxn.Commit()
	requireWatchSilent(t, watchCh, "unrelated insert")
	rtxn.Abort()
}

func TestTxn_GetCompound_Errors(t *testing.T) {
	db := testDB(t)
	txn := db.Txn(false)
	defer txn.Abort()

	if _, err := txn.GetCompound("main"); err == nil {
		t.Fatalf("expected error for no conditions")
	}
	if _, err := txn.GetCompound("nope", QueryCondition{Index: "foo", Args: []interface{}{"a"}}); err == nil {
		t.Fatalf("expected error for invalid table")
	}
	if _, err := txn.GetCompound("main", QueryCondition{Index: "nope", Args: []interface{}{"a"}}); err == nil {
		t.Fatalf("expected error for invalid index")
	}
}

func BenchmarkTxn_GetCompound(b *testing.B) {
	db, err := NewMemDB(testValidSchema())
	if err != nil {
		b.Fatalf("err: %v", err)
	}
	txn := db.Txn(true)
	// A large candidate set on foo and a tiny one on qux: the query should
	// be driven by the small set.
	for i := 0; i < 10000; i++ {
		obj := &TestObject{ID: fmt.Sprintf("%d", i), Foo: "a", Qux: []string{fmt.Sprintf("q%d", i)}}
		if err := txn.Insert("main", obj); err != nil {
			b.Fatalf("err: %v", err)
		}
	}
	for i := 0; i < 5; i++ {
		obj := &TestObject{ID: fmt.Sprintf("match-%d", i), Foo: "a", Qux: []string{"x"}}
		if err := txn.Insert("main", obj); err != nil {
			b.Fatalf("err: %v", err)
		}
	}
	txn.Commit()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		txn := db.Txn(false)
		iter, err := txn.GetCompound("main",
			QueryCondition{Index: "foo", Args: []interface{}{"a"}},
			QueryCondition{Index: "qux", Args: []interface{}{"x"}},
		)
		if err != nil {
			b.Fatalf("err: %v", err)
		}
		if n := len(collectCompound(iter)); n != 5 {
			b.Fatalf("bad: %d", n)
		}
		txn.Abort()
	}
}
