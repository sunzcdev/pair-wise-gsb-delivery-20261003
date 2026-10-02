// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// copyTestObject contains a slice and a map so that tests can verify that
// copies made at the database boundaries are fully independent.
type copyTestObject struct {
	ID    string
	Tags  []string
	Attrs map[string]string
}

// copyTestObjectFunc deep copies a copyTestObject.
func copyTestObjectFunc(obj interface{}) (interface{}, error) {
	o, ok := obj.(*copyTestObject)
	if !ok {
		return nil, fmt.Errorf("unexpected object type %T", obj)
	}
	copied := &copyTestObject{
		ID:    o.ID,
		Tags:  append([]string(nil), o.Tags...),
		Attrs: make(map[string]string, len(o.Attrs)),
	}
	for k, v := range o.Attrs {
		copied.Attrs[k] = v
	}
	return copied, nil
}

func testCopySchema(copyFn ObjectCopyFunc) *DBSchema {
	return &DBSchema{
		Tables: map[string]*TableSchema{
			"main": {
				Name:       "main",
				ObjectCopy: copyFn,
				Indexes: map[string]*IndexSchema{
					"id": {
						Name:    "id",
						Unique:  true,
						Indexer: &StringFieldIndex{Field: "ID"},
					},
					"tags": {
						Name:    "tags",
						Indexer: &StringSliceFieldIndex{Field: "Tags"},
					},
					"attrs": {
						Name:    "attrs",
						Indexer: &StringMapFieldIndex{Field: "Attrs"},
					},
				},
			},
		},
	}
}

func testCopyObj() *copyTestObject {
	return &copyTestObject{
		ID:   "obj-1",
		Tags: []string{"alpha", "beta"},
		Attrs: map[string]string{
			"role": "server",
			"zone": "us-east-1",
		},
	}
}

// TestMemDB_ObjectCopy_InsertIsolation verifies that mutating an object
// after Insert does not affect the stored object or its index entries.
func TestMemDB_ObjectCopy_InsertIsolation(t *testing.T) {
	db, err := NewMemDB(testCopySchema(copyTestObjectFunc))
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	obj := testCopyObj()
	txn := db.Txn(true)
	if err := txn.Insert("main", obj); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	// Mutate the original object in-place.
	obj.Tags[0] = "mutated"
	obj.Attrs["role"] = "mutated"
	delete(obj.Attrs, "zone")

	txn = db.Txn(false)
	defer txn.Abort()

	// The stored object must be unaffected.
	out, err := txn.First("main", "id", "obj-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	stored := out.(*copyTestObject)
	if stored.Tags[0] != "alpha" {
		t.Fatalf("stored slice was mutated: %v", stored.Tags)
	}
	if stored.Attrs["role"] != "server" || stored.Attrs["zone"] != "us-east-1" {
		t.Fatalf("stored map was mutated: %v", stored.Attrs)
	}

	// The indexes must still match the stored values.
	iter, err := txn.Get("main", "tags", "alpha")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if iter.Next() == nil {
		t.Fatalf("tag index lost entry for original value")
	}
	iter, err = txn.Get("main", "tags", "mutated")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if iter.Next() != nil {
		t.Fatalf("tag index gained entry for mutated value")
	}
	iter, err = txn.Get("main", "attrs", "role", "server")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if iter.Next() == nil {
		t.Fatalf("attr index lost entry for original value")
	}
}

// TestMemDB_ObjectCopy_ReadIsolation verifies that mutating an object
// returned by a query does not affect the stored object or its indexes.
func TestMemDB_ObjectCopy_ReadIsolation(t *testing.T) {
	db, err := NewMemDB(testCopySchema(copyTestObjectFunc))
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	txn := db.Txn(true)
	if err := txn.Insert("main", testCopyObj()); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	txn = db.Txn(false)
	defer txn.Abort()

	// Mutate the object handed out by the query.
	out, err := txn.First("main", "id", "obj-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	leaked := out.(*copyTestObject)
	leaked.Tags[0] = "mutated"
	leaked.Attrs["role"] = "mutated"

	// The stored object must be unaffected.
	out, err = txn.First("main", "id", "obj-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	stored := out.(*copyTestObject)
	if stored.Tags[0] != "alpha" {
		t.Fatalf("stored slice was mutated: %v", stored.Tags)
	}
	if stored.Attrs["role"] != "server" {
		t.Fatalf("stored map was mutated: %v", stored.Attrs)
	}

	// The indexes must still match the stored values.
	iter, err := txn.Get("main", "tags", "alpha")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if iter.Next() == nil {
		t.Fatalf("tag index lost entry for original value")
	}
}

// TestMemDB_ObjectCopy_UpdateSameTxn verifies that updates within a single
// transaction keep the indexes consistent with the stored copies.
func TestMemDB_ObjectCopy_UpdateSameTxn(t *testing.T) {
	db, err := NewMemDB(testCopySchema(copyTestObjectFunc))
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	txn := db.Txn(true)
	if err := txn.Insert("main", testCopyObj()); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Update the object within the same transaction.
	updated := testCopyObj()
	updated.Tags = []string{"gamma"}
	updated.Attrs = map[string]string{"role": "client"}
	if err := txn.Insert("main", updated); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Mutating the update input must not affect the in-flight transaction.
	updated.Tags[0] = "mutated"
	updated.Attrs["role"] = "mutated"

	// Reads within the same transaction must see the update.
	out, err := txn.First("main", "id", "obj-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	stored := out.(*copyTestObject)
	if len(stored.Tags) != 1 || stored.Tags[0] != "gamma" {
		t.Fatalf("update not visible in transaction: %v", stored.Tags)
	}
	if stored.Attrs["role"] != "client" {
		t.Fatalf("update not visible in transaction: %v", stored.Attrs)
	}
	txn.Commit()

	// After commit the old index entries must be gone and the new ones
	// must resolve to the updated object.
	txn = db.Txn(false)
	defer txn.Abort()

	iter, err := txn.Get("main", "tags", "alpha")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if iter.Next() != nil {
		t.Fatalf("stale tag index entry survived update")
	}
	iter, err = txn.Get("main", "tags", "gamma")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	got := iter.Next()
	if got == nil {
		t.Fatalf("updated tag index entry missing")
	}
	if got.(*copyTestObject).Attrs["role"] != "client" {
		t.Fatalf("index entry diverged from stored object: %v", got)
	}
	iter, err = txn.Get("main", "tags", "mutated")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if iter.Next() != nil {
		t.Fatalf("mutated input object leaked into index")
	}
}

// TestMemDB_ObjectCopy_Iterator verifies that iterators hand out
// independent copies.
func TestMemDB_ObjectCopy_Iterator(t *testing.T) {
	db, err := NewMemDB(testCopySchema(copyTestObjectFunc))
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	txn := db.Txn(true)
	for i := 0; i < 3; i++ {
		obj := testCopyObj()
		obj.ID = fmt.Sprintf("obj-%d", i)
		if err := txn.Insert("main", obj); err != nil {
			t.Fatalf("err: %v", err)
		}
	}
	txn.Commit()

	txn = db.Txn(false)
	defer txn.Abort()

	// Iterate and mutate every object handed out.
	iter, err := txn.Get("main", "id")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	count := 0
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		obj := raw.(*copyTestObject)
		obj.Tags[0] = "mutated"
		obj.Attrs["role"] = "mutated"
		count++
	}
	if count != 3 {
		t.Fatalf("expected 3 objects, got %d", count)
	}

	// The stored objects must be unaffected.
	iter, err = txn.Get("main", "id")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		obj := raw.(*copyTestObject)
		if obj.Tags[0] != "alpha" || obj.Attrs["role"] != "server" {
			t.Fatalf("stored object was mutated through iterator: %#v", obj)
		}
	}
}

// TestMemDB_ObjectCopy_Snapshot verifies that reads from snapshots hand
// out independent copies as well.
func TestMemDB_ObjectCopy_Snapshot(t *testing.T) {
	db, err := NewMemDB(testCopySchema(copyTestObjectFunc))
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	txn := db.Txn(true)
	if err := txn.Insert("main", testCopyObj()); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	snap := db.Snapshot()

	// Mutate an object read from the snapshot.
	txn = snap.Txn(false)
	defer txn.Abort()
	out, err := txn.First("main", "id", "obj-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	leaked := out.(*copyTestObject)
	leaked.Tags[0] = "mutated"
	leaked.Attrs["role"] = "mutated"

	// Both the snapshot and the primary DB must be unaffected.
	out, err = txn.First("main", "id", "obj-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := out.(*copyTestObject); got.Tags[0] != "alpha" || got.Attrs["role"] != "server" {
		t.Fatalf("snapshot object was mutated: %#v", got)
	}

	txn2 := db.Txn(false)
	defer txn2.Abort()
	out, err = txn2.First("main", "id", "obj-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := out.(*copyTestObject); got.Tags[0] != "alpha" || got.Attrs["role"] != "server" {
		t.Fatalf("primary object was mutated through snapshot: %#v", got)
	}

	// A transaction snapshot must behave the same way.
	writeTxn := db.Txn(true)
	defer writeTxn.Abort()
	txnSnap := writeTxn.Snapshot()
	out, err = txnSnap.First("main", "id", "obj-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	leaked = out.(*copyTestObject)
	leaked.Tags[0] = "mutated"
	out, err = txnSnap.First("main", "id", "obj-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := out.(*copyTestObject); got.Tags[0] != "alpha" {
		t.Fatalf("txn snapshot object was mutated: %#v", got)
	}
}

// TestMemDB_ObjectCopy_Errors verifies that copy failures can be traced
// back to the table and the boundary at which they occurred.
func TestMemDB_ObjectCopy_Errors(t *testing.T) {
	copyErr := errors.New("boom")

	// Insert with a failing copy function.
	db, err := NewMemDB(testCopySchema(func(obj interface{}) (interface{}, error) {
		return nil, copyErr
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	txn := db.Txn(true)
	err = txn.Insert("main", testCopyObj())
	txn.Abort()
	if !errors.Is(err, copyErr) {
		t.Fatalf("expected wrapped copy error, got: %v", err)
	}
	if !strings.Contains(err.Error(), `"main"`) || !strings.Contains(err.Error(), "insert") {
		t.Fatalf("error does not identify table and boundary: %v", err)
	}

	// Insert with a copy function returning nil.
	db, err = NewMemDB(testCopySchema(func(obj interface{}) (interface{}, error) {
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	txn = db.Txn(true)
	err = txn.Insert("main", testCopyObj())
	txn.Abort()
	if err == nil || !strings.Contains(err.Error(), `"main"`) || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("expected nil copy error identifying table, got: %v", err)
	}

	// Read with a failing copy function: insert succeeds, reads fail.
	failReads := false
	db, err = NewMemDB(testCopySchema(func(obj interface{}) (interface{}, error) {
		if failReads {
			return nil, copyErr
		}
		return copyTestObjectFunc(obj)
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	txn = db.Txn(true)
	if err := txn.Insert("main", testCopyObj()); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	failReads = true
	txn = db.Txn(false)
	defer txn.Abort()
	_, err = txn.First("main", "id", "obj-1")
	if !errors.Is(err, copyErr) {
		t.Fatalf("expected wrapped copy error, got: %v", err)
	}
	if !strings.Contains(err.Error(), `"main"`) || !strings.Contains(err.Error(), "read") {
		t.Fatalf("error does not identify table and boundary: %v", err)
	}

	// Iterators cannot return an error from Next, so a copy failure
	// surfaces as a panic carrying the same identifying error.
	iter, err := txn.Get("main", "id")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	func() {
		defer func() {
			r := recover()
			panicErr, ok := r.(error)
			if !ok || !errors.Is(panicErr, copyErr) {
				t.Fatalf("expected panic with copy error, got: %v", r)
			}
			if !strings.Contains(panicErr.Error(), `"main"`) {
				t.Fatalf("panic does not identify table: %v", panicErr)
			}
		}()
		iter.Next()
	}()
}

// TestMemDB_ObjectCopy_Nil verifies that tables without a copy function
// keep the historical pass-by-reference behavior.
func TestMemDB_ObjectCopy_Nil(t *testing.T) {
	db, err := NewMemDB(testCopySchema(nil))
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	obj := testCopyObj()
	txn := db.Txn(true)
	if err := txn.Insert("main", obj); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	txn = db.Txn(false)
	defer txn.Abort()
	out, err := txn.First("main", "id", "obj-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.(*copyTestObject) != obj {
		t.Fatalf("expected the same object to be returned without a copy function")
	}
}
