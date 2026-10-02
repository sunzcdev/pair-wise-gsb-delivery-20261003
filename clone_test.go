// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import (
	"fmt"
	"strings"
	"testing"
)

// testObjCloner deep copies a TestObject, including its slice and map
// fields, so the copy is fully independent of the original.
func testObjCloner(obj interface{}) (interface{}, error) {
	src, ok := obj.(*TestObject)
	if !ok {
		return nil, fmt.Errorf("unexpected object type %T", obj)
	}
	cp := *src
	cp.Qux = append([]string(nil), src.Qux...)
	cp.QuxEmpty = append([]string(nil), src.QuxEmpty...)
	if src.Zod != nil {
		cp.Zod = make(map[string]string, len(src.Zod))
		for k, v := range src.Zod {
			cp.Zod[k] = v
		}
	}
	if src.ZodEmpty != nil {
		cp.ZodEmpty = make(map[string]string, len(src.ZodEmpty))
		for k, v := range src.ZodEmpty {
			cp.ZodEmpty[k] = v
		}
	}
	return &cp, nil
}

func testCloneDB(t *testing.T, cloner ObjectCloneFunc) *MemDB {
	t.Helper()
	schema := testValidSchema()
	schema.Tables["main"].ObjectCloner = cloner
	db, err := NewMemDB(schema)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	return db
}

func TestTxn_Clone_InsertInputBoundary(t *testing.T) {
	db := testCloneDB(t, testObjCloner)

	obj := testObj()
	txn := db.Txn(true)
	if err := txn.Insert("main", obj); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	// Mutate the original object after insert, including its slice and map.
	obj.Foo = "mutated"
	obj.Qux[0] = "mutated"
	obj.Zod["Role"] = "mutated"

	txn = db.Txn(false)
	defer txn.Abort()

	// The stored object must be unaffected.
	raw, err := txn.First("main", "id", "my-cool-obj")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	stored := raw.(*TestObject)
	if stored.Foo != "Testing" {
		t.Fatalf("bad: %#v", stored.Foo)
	}
	if stored.Qux[0] != "Test" {
		t.Fatalf("bad: %#v", stored.Qux)
	}
	if stored.Zod["Role"] != "Server" {
		t.Fatalf("bad: %#v", stored.Zod)
	}

	// The indexes must still reflect the stored values, not the mutated
	// original.
	if raw, err := txn.First("main", "foo", "Testing"); err != nil || raw == nil {
		t.Fatalf("bad: %v %#v", err, raw)
	}
	if raw, err := txn.First("main", "foo", "mutated"); err != nil || raw != nil {
		t.Fatalf("bad: %v %#v", err, raw)
	}
	if raw, err := txn.First("main", "qux", "Test"); err != nil || raw == nil {
		t.Fatalf("bad: %v %#v", err, raw)
	}
	if raw, err := txn.First("main", "qux", "mutated"); err != nil || raw != nil {
		t.Fatalf("bad: %v %#v", err, raw)
	}
}

func TestTxn_Clone_ReadOutputBoundary(t *testing.T) {
	db := testCloneDB(t, testObjCloner)

	txn := db.Txn(true)
	if err := txn.Insert("main", testObj()); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	txn = db.Txn(false)
	defer txn.Abort()

	// Mutate the object returned by the query, including its slice and map.
	raw, err := txn.First("main", "id", "my-cool-obj")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	leaked := raw.(*TestObject)
	leaked.Foo = "mutated"
	leaked.Qux[0] = "mutated"
	leaked.Zod["Role"] = "mutated"

	// The stored object and its indexes must be unaffected.
	raw, err = txn.First("main", "id", "my-cool-obj")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	stored := raw.(*TestObject)
	if stored.Foo != "Testing" {
		t.Fatalf("bad: %#v", stored.Foo)
	}
	if stored.Qux[0] != "Test" {
		t.Fatalf("bad: %#v", stored.Qux)
	}
	if stored.Zod["Role"] != "Server" {
		t.Fatalf("bad: %#v", stored.Zod)
	}
	if raw, err := txn.First("main", "foo", "Testing"); err != nil || raw == nil {
		t.Fatalf("bad: %v %#v", err, raw)
	}
	if raw, err := txn.First("main", "qux", "Test"); err != nil || raw == nil {
		t.Fatalf("bad: %v %#v", err, raw)
	}

	// Successive reads must return distinct objects.
	other, err := txn.First("main", "id", "my-cool-obj")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if other.(*TestObject) == stored {
		t.Fatalf("expected independent copies")
	}
}

func TestTxn_Clone_IteratorOutputBoundary(t *testing.T) {
	db := testCloneDB(t, testObjCloner)

	txn := db.Txn(true)
	for i := 0; i < 3; i++ {
		obj := testObj()
		obj.ID = fmt.Sprintf("obj-%d", i)
		if err := txn.Insert("main", obj); err != nil {
			t.Fatalf("err: %v", err)
		}
	}
	txn.Commit()

	txn = db.Txn(false)
	defer txn.Abort()

	// Mutate every object returned by the iterator.
	iter, err := txn.Get("main", "id")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	count := 0
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		obj := raw.(*TestObject)
		obj.Foo = "mutated"
		obj.Qux[0] = "mutated"
		obj.Zod["Role"] = "mutated"
		count++
	}
	if count != 3 {
		t.Fatalf("bad: %d", count)
	}

	// The stored objects must be unaffected.
	iter, err = txn.Get("main", "id")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	count = 0
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		obj := raw.(*TestObject)
		if obj.Foo != "Testing" || obj.Qux[0] != "Test" || obj.Zod["Role"] != "Server" {
			t.Fatalf("bad: %#v", obj)
		}
		count++
	}
	if count != 3 {
		t.Fatalf("bad: %d", count)
	}

	// LowerBound must follow the same ownership convention.
	iter, err = txn.LowerBound("main", "id", "obj-0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	raw := iter.Next()
	raw.(*TestObject).Foo = "mutated"
	stored, err := txn.First("main", "id", "obj-0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if stored.(*TestObject).Foo != "Testing" {
		t.Fatalf("bad: %#v", stored)
	}
}

func TestTxn_Clone_UpdateSameTxn(t *testing.T) {
	db := testCloneDB(t, testObjCloner)

	txn := db.Txn(true)

	obj := testObj()
	if err := txn.Insert("main", obj); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Mutate the original and re-insert as an update in the same txn.
	obj.Foo = "updated"
	obj.Qux = []string{"new"}
	if err := txn.Insert("main", obj); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Reads inside the same transaction see the updated values and the
	// stale index entries are gone.
	raw, err := txn.First("main", "foo", "updated")
	if err != nil || raw == nil {
		t.Fatalf("bad: %v %#v", err, raw)
	}
	if raw, err := txn.First("main", "qux", "Test"); err != nil || raw != nil {
		t.Fatalf("bad: %v %#v", err, raw)
	}
	if raw, err := txn.First("main", "qux", "new"); err != nil || raw == nil {
		t.Fatalf("bad: %v %#v", err, raw)
	}
	txn.Commit()

	// Mutating the original after commit must not affect the stored object.
	obj.Foo = "mutated"
	obj.Qux[0] = "mutated"

	txn = db.Txn(false)
	defer txn.Abort()
	raw, err = txn.First("main", "id", "my-cool-obj")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	stored := raw.(*TestObject)
	if stored.Foo != "updated" || stored.Qux[0] != "new" {
		t.Fatalf("bad: %#v", stored)
	}
	if raw, err := txn.First("main", "qux", "new"); err != nil || raw == nil {
		t.Fatalf("bad: %v %#v", err, raw)
	}
}

func TestTxn_Clone_Snapshot(t *testing.T) {
	db := testCloneDB(t, testObjCloner)

	txn := db.Txn(true)
	if err := txn.Insert("main", testObj()); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	snap := db.Snapshot()

	// Objects read from the snapshot are independent copies.
	snapTxn := snap.Txn(false)
	defer snapTxn.Abort()
	raw, err := snapTxn.First("main", "id", "my-cool-obj")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	leaked := raw.(*TestObject)
	leaked.Foo = "mutated"
	leaked.Qux[0] = "mutated"
	leaked.Zod["Role"] = "mutated"

	// Mutating the snapshot read result affects neither the snapshot nor
	// the primary DB.
	raw, err = snapTxn.First("main", "id", "my-cool-obj")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := raw.(*TestObject).Foo; got != "Testing" {
		t.Fatalf("bad: %#v", got)
	}
	readTxn := db.Txn(false)
	defer readTxn.Abort()
	raw, err = readTxn.First("main", "id", "my-cool-obj")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := raw.(*TestObject).Foo; got != "Testing" {
		t.Fatalf("bad: %#v", got)
	}

	// Writes to the primary DB do not affect the snapshot's view.
	txn = db.Txn(true)
	updated := testObj()
	updated.Foo = "updated"
	if err := txn.Insert("main", updated); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	raw, err = snapTxn.First("main", "id", "my-cool-obj")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := raw.(*TestObject).Foo; got != "Testing" {
		t.Fatalf("bad: %#v", got)
	}
}

func TestTxn_Clone_InsertClonerError(t *testing.T) {
	db := testCloneDB(t, func(obj interface{}) (interface{}, error) {
		return nil, fmt.Errorf("boom")
	})

	txn := db.Txn(true)
	defer txn.Abort()
	err := txn.Insert("main", testObj())
	if err == nil {
		t.Fatalf("should fail")
	}
	if !strings.Contains(err.Error(), "main") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("bad: %v", err)
	}
}

func TestTxn_Clone_InsertClonerNil(t *testing.T) {
	db := testCloneDB(t, func(obj interface{}) (interface{}, error) {
		return nil, nil
	})

	txn := db.Txn(true)
	defer txn.Abort()
	err := txn.Insert("main", testObj())
	if err == nil {
		t.Fatalf("should fail")
	}
	if !strings.Contains(err.Error(), "main") || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("bad: %v", err)
	}
}

func TestTxn_Clone_InsertClonerMissingPrimaryIndex(t *testing.T) {
	db := testCloneDB(t, func(obj interface{}) (interface{}, error) {
		return &TestObject{}, nil
	})

	txn := db.Txn(true)
	defer txn.Abort()
	err := txn.Insert("main", testObj())
	if err == nil {
		t.Fatalf("should fail")
	}
	if !strings.Contains(err.Error(), "main") || !strings.Contains(err.Error(), "missing primary index") {
		t.Fatalf("bad: %v", err)
	}
}

func TestTxn_Clone_InsertClonerMismatchedPrimaryIndex(t *testing.T) {
	db := testCloneDB(t, func(obj interface{}) (interface{}, error) {
		cp := *(obj.(*TestObject))
		cp.ID = "somebody-else"
		return &cp, nil
	})

	txn := db.Txn(true)
	defer txn.Abort()
	err := txn.Insert("main", testObj())
	if err == nil {
		t.Fatalf("should fail")
	}
	if !strings.Contains(err.Error(), "main") || !strings.Contains(err.Error(), "mismatched primary index") {
		t.Fatalf("bad: %v", err)
	}
}

func TestTxn_Clone_ReadClonerError(t *testing.T) {
	// Fail every clone after the first one so inserts succeed but reads
	// fail.
	calls := 0
	db := testCloneDB(t, func(obj interface{}) (interface{}, error) {
		calls++
		if calls > 1 {
			return nil, fmt.Errorf("read boom")
		}
		return testObjCloner(obj)
	})

	txn := db.Txn(true)
	if err := txn.Insert("main", testObj()); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	txn = db.Txn(false)
	defer txn.Abort()

	_, err := txn.First("main", "id", "my-cool-obj")
	if err == nil {
		t.Fatalf("should fail")
	}
	if !strings.Contains(err.Error(), "main") || !strings.Contains(err.Error(), "read boom") {
		t.Fatalf("bad: %v", err)
	}

	_, err = txn.Get("main", "id")
	if err == nil {
		t.Fatalf("should fail")
	}
	if !strings.Contains(err.Error(), "main") || !strings.Contains(err.Error(), "read boom") {
		t.Fatalf("bad: %v", err)
	}
}
