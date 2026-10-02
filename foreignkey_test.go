// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import (
	"strings"
	"testing"
)

type fkParent struct {
	ID string
}

type fkChild struct {
	ID       string
	ParentID string
}

type fkGrandchild struct {
	ID       string
	ChildID  string
	Child2ID string
}

type fkNode struct {
	ID     string
	PeerID string
}

// testFKSchema builds a schema with a three table reference chain
// (grandchild -> child -> parent) plus a self-referencing table (node)
// that allows reference cycles.
func testFKSchema(childAction, grandchildAction RefAction) *DBSchema {
	return &DBSchema{
		Tables: map[string]*TableSchema{
			"parent": &TableSchema{
				Name: "parent",
				Indexes: map[string]*IndexSchema{
					"id": &IndexSchema{
						Name:    "id",
						Unique:  true,
						Indexer: &StringFieldIndex{Field: "ID"},
					},
				},
			},
			"child": &TableSchema{
				Name: "child",
				Indexes: map[string]*IndexSchema{
					"id": &IndexSchema{
						Name:    "id",
						Unique:  true,
						Indexer: &StringFieldIndex{Field: "ID"},
					},
					"parent_id": &IndexSchema{
						Name:    "parent_id",
						Indexer: &StringFieldIndex{Field: "ParentID"},
					},
				},
				ForeignKeys: []*ForeignKeySchema{
					&ForeignKeySchema{Table: "parent", Index: "parent_id", OnDelete: childAction},
				},
			},
			"grandchild": &TableSchema{
				Name: "grandchild",
				Indexes: map[string]*IndexSchema{
					"id": &IndexSchema{
						Name:    "id",
						Unique:  true,
						Indexer: &StringFieldIndex{Field: "ID"},
					},
					"child_id": &IndexSchema{
						Name:    "child_id",
						Indexer: &StringFieldIndex{Field: "ChildID"},
					},
					"child2_id": &IndexSchema{
						Name:         "child2_id",
						AllowMissing: true,
						Indexer:      &StringFieldIndex{Field: "Child2ID"},
					},
				},
				ForeignKeys: []*ForeignKeySchema{
					&ForeignKeySchema{Table: "child", Index: "child_id", OnDelete: grandchildAction},
					&ForeignKeySchema{Table: "child", Index: "child2_id", OnDelete: grandchildAction},
				},
			},
			"node": &TableSchema{
				Name: "node",
				Indexes: map[string]*IndexSchema{
					"id": &IndexSchema{
						Name:    "id",
						Unique:  true,
						Indexer: &StringFieldIndex{Field: "ID"},
					},
					"peer": &IndexSchema{
						Name:         "peer",
						AllowMissing: true,
						Indexer:      &StringFieldIndex{Field: "PeerID"},
					},
				},
				ForeignKeys: []*ForeignKeySchema{
					&ForeignKeySchema{Table: "node", Index: "peer", OnDelete: Cascade},
				},
			},
		},
	}
}

func testFKDB(t *testing.T, childAction, grandchildAction RefAction) *MemDB {
	db, err := NewMemDB(testFKSchema(childAction, grandchildAction))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	return db
}

func mustInsert(t *testing.T, txn *Txn, table string, obj interface{}) {
	t.Helper()
	if err := txn.Insert(table, obj); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func mustGet(t *testing.T, txn *Txn, table, id string) interface{} {
	t.Helper()
	obj, err := txn.First(table, "id", id)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if obj == nil {
		t.Fatalf("expected %s/%s to exist", table, id)
	}
	return obj
}

func mustMissing(t *testing.T, txn *Txn, table, id string) {
	t.Helper()
	obj, err := txn.First(table, "id", id)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if obj != nil {
		t.Fatalf("expected %s/%s to be missing, got %#v", table, id, obj)
	}
}

func TestForeignKey_SchemaValidate(t *testing.T) {
	valid := testFKSchema(Restrict, Cascade)
	if err := valid.Validate(); err != nil {
		t.Fatalf("should validate: %v", err)
	}

	// Unknown referenced table
	s := testFKSchema(Restrict, Cascade)
	s.Tables["child"].ForeignKeys[0].Table = "nope"
	if err := s.Validate(); err == nil {
		t.Fatalf("should not validate, unknown referenced table")
	}

	// Unknown referenced index
	s = testFKSchema(Restrict, Cascade)
	s.Tables["child"].ForeignKeys[0].RefIndex = "nope"
	if err := s.Validate(); err == nil {
		t.Fatalf("should not validate, unknown referenced index")
	}

	// Referenced index must be unique
	s = testFKSchema(Restrict, Cascade)
	s.Tables["parent"].Indexes["name"] = &IndexSchema{
		Name:    "name",
		Indexer: &StringFieldIndex{Field: "ID"},
	}
	s.Tables["child"].ForeignKeys[0].RefIndex = "name"
	if err := s.Validate(); err == nil {
		t.Fatalf("should not validate, referenced index not unique")
	}

	// Unknown child index
	s = testFKSchema(Restrict, Cascade)
	s.Tables["child"].ForeignKeys[0].Index = "nope"
	if err := s.Validate(); err == nil {
		t.Fatalf("should not validate, unknown child index")
	}

	// Missing referenced table
	s = testFKSchema(Restrict, Cascade)
	s.Tables["child"].ForeignKeys[0].Table = ""
	if err := s.Validate(); err == nil {
		t.Fatalf("should not validate, missing referenced table")
	}

	// Invalid delete action
	s = testFKSchema(Restrict, Cascade)
	s.Tables["child"].ForeignKeys[0].OnDelete = RefAction(42)
	if err := s.Validate(); err == nil {
		t.Fatalf("should not validate, invalid delete action")
	}
}

func TestForeignKey_InsertChildBeforeParent(t *testing.T) {
	db := testFKDB(t, Restrict, Cascade)

	// Seed an initial committed object.
	txn := db.Txn(true)
	mustInsert(t, txn, "parent", &fkParent{ID: "p0"})
	if err := txn.CommitWithError(); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Open a read transaction before the write commits.
	before := db.Txn(false)
	defer before.Abort()

	// Insert the child (and grandchild) before the parent in one txn.
	wtxn := db.Txn(true)
	mustInsert(t, wtxn, "grandchild", &fkGrandchild{ID: "g1", ChildID: "c1"})
	mustInsert(t, wtxn, "child", &fkChild{ID: "c1", ParentID: "p1"})
	mustInsert(t, wtxn, "parent", &fkParent{ID: "p1"})
	if err := wtxn.CommitWithError(); err != nil {
		t.Fatalf("err: %v", err)
	}

	// The read transaction from before the commit keeps its old view.
	mustGet(t, before, "parent", "p0")
	mustMissing(t, before, "parent", "p1")
	mustMissing(t, before, "child", "c1")
	mustMissing(t, before, "grandchild", "g1")

	// A new read transaction sees the committed state.
	after := db.Txn(false)
	defer after.Abort()
	mustGet(t, after, "parent", "p0")
	mustGet(t, after, "parent", "p1")
	mustGet(t, after, "child", "c1")
	mustGet(t, after, "grandchild", "g1")
}

func TestForeignKey_CommitViolation(t *testing.T) {
	db := testFKDB(t, Restrict, Cascade)

	// Watch for the child that the failed transaction tries to insert.
	rtxn := db.Txn(false)
	watchCh, _, err := rtxn.FirstWatch("child", "id", "c1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer rtxn.Abort()

	// Insert a child referencing a missing parent.
	wtxn := db.Txn(true)
	mustInsert(t, wtxn, "child", &fkChild{ID: "c1", ParentID: "missing"})
	if err := wtxn.CommitWithError(); err == nil {
		t.Fatalf("should fail, dangling reference")
	} else if !strings.Contains(err.Error(), "foreign key violation") {
		t.Fatalf("unexpected error: %v", err)
	}

	// The failed commit must not leave partial state behind.
	check := db.Txn(false)
	mustMissing(t, check, "child", "c1")
	check.Abort()

	// The failed commit must not trigger watch notifications.
	select {
	case <-watchCh:
		t.Fatalf("watch should not fire on failed commit")
	default:
	}

	// The writer lock must be released and the DB still usable.
	wtxn = db.Txn(true)
	mustInsert(t, wtxn, "parent", &fkParent{ID: "p1"})
	mustInsert(t, wtxn, "child", &fkChild{ID: "c1", ParentID: "p1"})
	if err := wtxn.CommitWithError(); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Now the watch fires and the old read transaction still sees nothing.
	select {
	case <-watchCh:
	default:
		t.Fatalf("watch should fire on successful commit")
	}
	mustMissing(t, rtxn, "child", "c1")
}

func TestForeignKey_Restrict(t *testing.T) {
	db := testFKDB(t, Restrict, Cascade)

	txn := db.Txn(true)
	mustInsert(t, txn, "parent", &fkParent{ID: "p1"})
	mustInsert(t, txn, "child", &fkChild{ID: "c1", ParentID: "p1"})
	if err := txn.CommitWithError(); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Watch the parent that the failed transaction tries to delete.
	rtxn := db.Txn(false)
	watchCh, _, err := rtxn.FirstWatch("parent", "id", "p1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer rtxn.Abort()

	// Deleting a referenced parent is rejected at commit time.
	wtxn := db.Txn(true)
	if err := wtxn.Delete("parent", &fkParent{ID: "p1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := wtxn.CommitWithError(); err == nil {
		t.Fatalf("should fail, restricted delete")
	}

	// No partial state: the parent and child are still fully indexed.
	check := db.Txn(false)
	defer check.Abort()
	mustGet(t, check, "parent", "p1")
	mustGet(t, check, "child", "c1")
	iter, err := check.Get("child", "parent_id", "p1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if obj := iter.Next(); obj == nil {
		t.Fatalf("child index entry for parent_id should survive failed commit")
	}

	// No notifications from the failed commit.
	select {
	case <-watchCh:
		t.Fatalf("watch should not fire on failed commit")
	default:
	}
}

func TestForeignKey_CascadeChain(t *testing.T) {
	db := testFKDB(t, Cascade, Cascade)

	// Insert the chain child-first across all three tables.
	txn := db.Txn(true)
	mustInsert(t, txn, "grandchild", &fkGrandchild{ID: "g1", ChildID: "c1"})
	mustInsert(t, txn, "child", &fkChild{ID: "c1", ParentID: "p1"})
	mustInsert(t, txn, "parent", &fkParent{ID: "p1"})
	if err := txn.CommitWithError(); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Deleting the parent cascades through the whole chain.
	wtxn := db.Txn(true)
	if err := wtxn.Delete("parent", &fkParent{ID: "p1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := wtxn.CommitWithError(); err != nil {
		t.Fatalf("err: %v", err)
	}

	check := db.Txn(false)
	defer check.Abort()
	mustMissing(t, check, "parent", "p1")
	mustMissing(t, check, "child", "c1")
	mustMissing(t, check, "grandchild", "g1")
}

func TestForeignKey_CascadeDiamond(t *testing.T) {
	db := testFKDB(t, Cascade, Cascade)

	// Diamond: p1 <- {c1, c2} <- g1, so g1 is reachable via two paths.
	txn := db.Txn(true)
	mustInsert(t, txn, "parent", &fkParent{ID: "p1"})
	mustInsert(t, txn, "child", &fkChild{ID: "c1", ParentID: "p1"})
	mustInsert(t, txn, "child", &fkChild{ID: "c2", ParentID: "p1"})
	mustInsert(t, txn, "grandchild", &fkGrandchild{ID: "g1", ChildID: "c1", Child2ID: "c2"})
	if err := txn.CommitWithError(); err != nil {
		t.Fatalf("err: %v", err)
	}

	wtxn := db.Txn(true)
	wtxn.TrackChanges()
	if err := wtxn.Delete("parent", &fkParent{ID: "p1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := wtxn.CommitWithError(); err != nil {
		t.Fatalf("err: %v", err)
	}

	// The shared grandchild must be processed exactly once.
	changes := wtxn.Changes()
	if len(changes) != 4 {
		t.Fatalf("bad: %#v", changes)
	}
	deleted := make(map[string]int)
	for _, c := range changes {
		if c.Deleted() {
			deleted[c.Table]++
		}
	}
	if deleted["parent"] != 1 || deleted["child"] != 2 || deleted["grandchild"] != 1 {
		t.Fatalf("bad: %#v", changes)
	}

	check := db.Txn(false)
	defer check.Abort()
	mustMissing(t, check, "parent", "p1")
	mustMissing(t, check, "child", "c1")
	mustMissing(t, check, "child", "c2")
	mustMissing(t, check, "grandchild", "g1")
}

func TestForeignKey_CascadeCycle(t *testing.T) {
	db := testFKDB(t, Restrict, Cascade)

	// A reference cycle: a <-> b, inserted peer-first.
	txn := db.Txn(true)
	mustInsert(t, txn, "node", &fkNode{ID: "a", PeerID: "b"})
	mustInsert(t, txn, "node", &fkNode{ID: "b", PeerID: "a"})
	if err := txn.CommitWithError(); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Deleting one side of the cycle cascades without looping forever.
	wtxn := db.Txn(true)
	if err := wtxn.Delete("node", &fkNode{ID: "a"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := wtxn.CommitWithError(); err != nil {
		t.Fatalf("err: %v", err)
	}

	check := db.Txn(false)
	defer check.Abort()
	mustMissing(t, check, "node", "a")
	mustMissing(t, check, "node", "b")
}

func TestForeignKey_DanglingSelfReference(t *testing.T) {
	db := testFKDB(t, Restrict, Cascade)

	txn := db.Txn(true)
	mustInsert(t, txn, "node", &fkNode{ID: "a", PeerID: "missing"})
	if err := txn.CommitWithError(); err == nil {
		t.Fatalf("should fail, dangling self reference")
	}

	check := db.Txn(false)
	defer check.Abort()
	mustMissing(t, check, "node", "a")
}

func TestForeignKey_NoForeignKeysUnaffected(t *testing.T) {
	db := testDB(t)

	txn := db.Txn(true)
	obj := testObj()
	mustInsert(t, txn, "main", obj)
	if err := txn.CommitWithError(); err != nil {
		t.Fatalf("err: %v", err)
	}

	txn = db.Txn(true)
	if err := txn.Delete("main", obj); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := txn.CommitWithError(); err != nil {
		t.Fatalf("err: %v", err)
	}

	check := db.Txn(false)
	defer check.Abort()
	mustMissing(t, check, "main", obj.ID)
}
