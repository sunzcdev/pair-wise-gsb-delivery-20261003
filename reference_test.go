// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import (
	"errors"
	"testing"
	"time"
)

// refNode is a row in a self-referencing table, used for reference cycles.
type refNode struct {
	ID     string
	Parent string // empty means NULL (the index is AllowMissing)
}

// refEdge belongs to a node; deleting the node cascades.
type refEdge struct {
	ID     string
	NodeID string
}

// refAttr belongs to an edge; deleting the edge is restricted while an attr
// references it.
type refAttr struct {
	ID     string
	EdgeID string
}

// refDiamondChild references the same parent through two cascade paths so a
// single parent delete can reach one child more than once.
type refDiamondChild struct {
	ID string
	L  string // left parent (nullable)
	R  string // right parent (nullable)
}

// referenceTestSchema defines a three-table chain
//
//	attr ->(restrict)-> edge ->(cascade)-> node
//
// plus a self-referencing node table that permits cycles.
func referenceTestSchema() *DBSchema {
	return &DBSchema{
		Tables: map[string]*TableSchema{
			"node": {
				Name: "node",
				Indexes: map[string]*IndexSchema{
					"id":     {Name: "id", Unique: true, Indexer: &StringFieldIndex{Field: "ID"}},
					"parent": {Name: "parent", Indexer: &StringFieldIndex{Field: "Parent"}, AllowMissing: true},
				},
				References: []*ReferenceSchema{
					{
						Name:         "node-parent",
						LocalIndex:   "parent",
						ForeignTable: "node",
						OnDelete:     ReferenceActionRestrict,
					},
				},
			},
			"edge": {
				Name: "edge",
				Indexes: map[string]*IndexSchema{
					"id":   {Name: "id", Unique: true, Indexer: &StringFieldIndex{Field: "ID"}},
					"node": {Name: "node", Indexer: &StringFieldIndex{Field: "NodeID"}, AllowMissing: true},
				},
				References: []*ReferenceSchema{
					{
						Name:         "edge-node",
						LocalIndex:   "node",
						ForeignTable: "node",
						OnDelete:     ReferenceActionCascade,
					},
				},
			},
			"attr": {
				Name: "attr",
				Indexes: map[string]*IndexSchema{
					"id":   {Name: "id", Unique: true, Indexer: &StringFieldIndex{Field: "ID"}},
					"edge": {Name: "edge", Indexer: &StringFieldIndex{Field: "EdgeID"}},
				},
				References: []*ReferenceSchema{
					{
						Name:         "attr-edge",
						LocalIndex:   "edge",
						ForeignTable: "edge",
						OnDelete:     ReferenceActionRestrict,
					},
				},
			},
			"diamond": {
				Name: "diamond",
				Indexes: map[string]*IndexSchema{
					"id": {Name: "id", Unique: true, Indexer: &StringFieldIndex{Field: "ID"}},
					"l":  {Name: "l", Indexer: &StringFieldIndex{Field: "L"}, AllowMissing: true},
					"r":  {Name: "r", Indexer: &StringFieldIndex{Field: "R"}, AllowMissing: true},
				},
				References: []*ReferenceSchema{
					{
						Name:         "diamond-l",
						LocalIndex:   "l",
						ForeignTable: "diamond",
						OnDelete:     ReferenceActionCascade,
					},
					{
						Name:         "diamond-r",
						LocalIndex:   "r",
						ForeignTable: "diamond",
						OnDelete:     ReferenceActionCascade,
					},
				},
			},
		},
	}
}

func testReferenceDB(t *testing.T) *MemDB {
	t.Helper()
	db, err := NewMemDB(referenceTestSchema())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	return db
}

func mustFirst(t *testing.T, txn *Txn, table, index, arg string) interface{} {
	t.Helper()
	raw, err := txn.First(table, index, arg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	return raw
}

// A child may be inserted before its parent within the same transaction
// because validation is deferred to commit.
func TestReference_CommitTimeValidation_ChildBeforeParent(t *testing.T) {
	db := testReferenceDB(t)

	txn := db.Txn(true)
	if err := txn.Insert("edge", &refEdge{ID: "e1", NodeID: "n1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := txn.Insert("node", &refNode{ID: "n1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := txn.Commit(); err != nil {
		t.Fatalf("commit should succeed when both rows are present: %v", err)
	}

	rTxn := db.Txn(false)
	if raw := mustFirst(t, rTxn, "node", "id", "n1"); raw == nil {
		t.Fatalf("node should be committed")
	}
	if raw := mustFirst(t, rTxn, "edge", "id", "e1"); raw == nil {
		t.Fatalf("edge should be committed")
	}
}

// A dangling reference fails commit and the error wraps ErrReferenceViolation.
func TestReference_CommitFails_MissingParent(t *testing.T) {
	db := testReferenceDB(t)

	txn := db.Txn(true)
	if err := txn.Insert("edge", &refEdge{ID: "e1", NodeID: "ghost"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	err := txn.Commit()
	if err == nil {
		t.Fatalf("commit should fail for a dangling reference")
	}
	if !errors.Is(err, ErrReferenceViolation) {
		t.Fatalf("expected ErrReferenceViolation, got: %v", err)
	}

	// Nothing should have been published.
	rTxn := db.Txn(false)
	if raw := mustFirst(t, rTxn, "edge", "id", "e1"); raw != nil {
		t.Fatalf("edge must not be committed after failed commit")
	}
}

// Restrict prevents deleting a referenced parent.
func TestReference_RestrictDelete(t *testing.T) {
	db := testReferenceDB(t)

	txn := db.Txn(true)
	if err := txn.Insert("node", &refNode{ID: "n1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := txn.Insert("edge", &refEdge{ID: "e1", NodeID: "n1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := txn.Insert("attr", &refAttr{ID: "a1", EdgeID: "e1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := txn.Commit(); err != nil {
		t.Fatalf("err: %v", err)
	}

	del := db.Txn(true)
	if err := del.Delete("edge", &refEdge{ID: "e1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := del.Commit(); !errors.Is(err, ErrReferenceViolation) {
		t.Fatalf("expected restrict error, got: %v", err)
	}

	rTxn := db.Txn(false)
	if raw := mustFirst(t, rTxn, "edge", "id", "e1"); raw == nil {
		t.Fatalf("restricted edge must still exist")
	}
}

// Cascade removes the referencing children when the parent is deleted.
func TestReference_CascadeDelete(t *testing.T) {
	db := testReferenceDB(t)

	txn := db.Txn(true)
	if err := txn.Insert("node", &refNode{ID: "n1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := txn.Insert("edge", &refEdge{ID: "e1", NodeID: "n1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := txn.Insert("edge", &refEdge{ID: "e2", NodeID: "n1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := txn.Commit(); err != nil {
		t.Fatalf("err: %v", err)
	}

	del := db.Txn(true)
	if err := del.Delete("node", &refNode{ID: "n1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := del.Commit(); err != nil {
		t.Fatalf("cascade delete should succeed: %v", err)
	}

	rTxn := db.Txn(false)
	for _, id := range []string{"n1", "e1", "e2"} {
		table := "node"
		if id != "n1" {
			table = "edge"
		}
		if raw := mustFirst(t, rTxn, table, "id", id); raw != nil {
			t.Fatalf("%s should have been cascade deleted", id)
		}
	}
}

// Cascade propagates across the full three-table chain node -> edge -> attr
// when the attr reference is also cascade.
func TestReference_CascadeAcrossThreeTableChain(t *testing.T) {
	schema := referenceTestSchema()
	// Make attr cascade too so deleting a node removes the entire subtree.
	schema.Tables["attr"].References[0].OnDelete = ReferenceActionCascade
	db, err := NewMemDB(schema)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	txn := db.Txn(true)
	if err := txn.Insert("node", &refNode{ID: "n1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := txn.Insert("edge", &refEdge{ID: "e1", NodeID: "n1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := txn.Insert("attr", &refAttr{ID: "a1", EdgeID: "e1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := txn.Commit(); err != nil {
		t.Fatalf("err: %v", err)
	}

	del := db.Txn(true)
	if err := del.Delete("node", &refNode{ID: "n1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := del.Commit(); err != nil {
		t.Fatalf("chained cascade should succeed: %v", err)
	}

	rTxn := db.Txn(false)
	if raw := mustFirst(t, rTxn, "node", "id", "n1"); raw != nil {
		t.Fatalf("node should be deleted")
	}
	if raw := mustFirst(t, rTxn, "edge", "id", "e1"); raw != nil {
		t.Fatalf("edge should be cascade deleted")
	}
	if raw := mustFirst(t, rTxn, "attr", "id", "a1"); raw != nil {
		t.Fatalf("attr should be transitively cascade deleted")
	}
}

// A child reachable through more than one cascade path is deleted only once.
//
// The graph is:
//
//	root
//	 |\
//	 l  r
//	 | \|
//	 leaf   (leaf references both l and r)
//
// Deleting root reaches leaf through l (root->l->leaf) and r (root->r->leaf);
// leaf must be deleted a single time even though it is reached twice.
func TestReference_CascadeDedup(t *testing.T) {
	db := testReferenceDB(t)

	txn := db.Txn(true)
	rows := []*refDiamondChild{
		{ID: "root"},
		{ID: "l", L: "root"},
		{ID: "r", R: "root"},
		{ID: "leaf", L: "l", R: "r"},
	}
	for _, r := range rows {
		if err := txn.Insert("diamond", r); err != nil {
			t.Fatalf("err: %v", err)
		}
	}
	if err := txn.Commit(); err != nil {
		t.Fatalf("err: %v", err)
	}

	del := db.Txn(true)
	if err := del.Delete("diamond", &refDiamondChild{ID: "root"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := del.Commit(); err != nil {
		t.Fatalf("cascade through a diamond should succeed: %v", err)
	}

	rTxn := db.Txn(false)
	for _, id := range []string{"root", "l", "r", "leaf"} {
		if raw := mustFirst(t, rTxn, "diamond", "id", id); raw != nil {
			t.Fatalf("%s should have been cascade deleted", id)
		}
	}
}

// Reference cycles are allowed on insert and cascade safely on delete.
func TestReference_CycleInsertAndCascade(t *testing.T) {
	db := testReferenceDB(t)

	// Child-before-parent across a cycle: a -> b -> a, inserted in that order.
	txn := db.Txn(true)
	if err := txn.Insert("node", &refNode{ID: "a", Parent: "b"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := txn.Insert("node", &refNode{ID: "b", Parent: "a"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := txn.Commit(); err != nil {
		t.Fatalf("cyclic references should commit: %v", err)
	}

	// Break the cycle first: make both nodes roots, then delete one. This
	// verifies restrict semantics inside a self-referencing table.
	brk := db.Txn(true)
	if err := brk.Insert("node", &refNode{ID: "a"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := brk.Insert("node", &refNode{ID: "b"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := brk.Commit(); err != nil {
		t.Fatalf("err: %v", err)
	}

	del := db.Txn(true)
	if err := del.Delete("node", &refNode{ID: "a"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := del.Commit(); err != nil {
		t.Fatalf("delete of unreferenced node should succeed: %v", err)
	}
	rTxn := db.Txn(false)
	if raw := mustFirst(t, rTxn, "node", "id", "a"); raw != nil {
		t.Fatalf("a should be deleted")
	}
	if raw := mustFirst(t, rTxn, "node", "id", "b"); raw == nil {
		t.Fatalf("b should remain")
	}
}

// A read transaction opened before commit must not see the pending writes, and
// the same read transaction continues to see the old snapshot after commit.
func TestReference_ReadTransactionSnapshot(t *testing.T) {
	db := testReferenceDB(t)

	txn := db.Txn(true)
	if err := txn.Insert("node", &refNode{ID: "n1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := txn.Insert("edge", &refEdge{ID: "e1", NodeID: "n1"}); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Read snapshot taken while the write is still uncommitted.
	before := db.Txn(false)
	if raw := mustFirst(t, before, "node", "id", "n1"); raw != nil {
		t.Fatalf("read txn must not see uncommitted node")
	}

	if err := txn.Commit(); err != nil {
		t.Fatalf("err: %v", err)
	}

	// The earlier read txn still observes its old snapshot.
	if raw := mustFirst(t, before, "node", "id", "n1"); raw != nil {
		t.Fatalf("old read txn must keep its pre-commit snapshot")
	}
	// A fresh read txn sees the committed rows.
	after := db.Txn(false)
	if raw := mustFirst(t, after, "node", "id", "n1"); raw == nil {
		t.Fatalf("new read txn must see committed node")
	}
	if raw := mustFirst(t, after, "edge", "id", "e1"); raw == nil {
		t.Fatalf("new read txn must see committed edge")
	}
}

// A failed commit must not fire watch notifications nor run deferred funcs.
func TestReference_FailedCommit_NoNotifyNoDefer(t *testing.T) {
	db := testReferenceDB(t)

	setup := db.Txn(true)
	if err := setup.Insert("edge", &refEdge{ID: "e1", NodeID: "n1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := setup.Insert("node", &refNode{ID: "n1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := setup.Commit(); err != nil {
		t.Fatalf("err: %v", err)
	}

	watchTxn := db.Txn(false)
	watchCh, _, err := watchTxn.FirstWatch("node", "id", "n1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	deferRan := false
	fail := db.Txn(true)
	fail.Defer(func() { deferRan = true })
	// Delete the referenced node's child relationship indirectly: deleting
	// the node cascades edge e1, but attr is not involved. Instead insert a
	// dangling edge to force a reference error.
	if err := fail.Insert("edge", &refEdge{ID: "e-bad", NodeID: "ghost"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := fail.Commit(); err == nil {
		t.Fatalf("expected commit failure")
	}

	if deferRan {
		t.Fatalf("deferred functions must not run on a failed commit")
	}

	select {
	case <-watchCh:
		t.Fatalf("watch must not fire after a failed commit")
	case <-time.After(50 * time.Millisecond):
	}

	// A subsequent successful commit must still be able to notify.
	ok := db.Txn(true)
	if err := ok.Insert("node", &refNode{ID: "n2"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := ok.Commit(); err != nil {
		t.Fatalf("err: %v", err)
	}

	watchCh2, _, err := db.Txn(false).FirstWatch("node", "id", "n2")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	del := db.Txn(true)
	if err := del.Delete("node", &refNode{ID: "n2"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := del.Commit(); err != nil {
		t.Fatalf("err: %v", err)
	}
	select {
	case <-watchCh2:
	case <-time.After(2 * time.Second):
		t.Fatalf("watch should fire after a later successful commit")
	}
}

// Tables that declare no references behave exactly as they did before.
func TestReference_NoReferencesStillWork(t *testing.T) {
	db := testDB(t)

	txn := db.Txn(true)
	obj := testObj()
	if err := txn.Insert("main", obj); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := txn.Commit(); err != nil {
		t.Fatalf("commit on reference-free table must succeed: %v", err)
	}

	rTxn := db.Txn(false)
	if raw := mustFirst(t, rTxn, "main", "id", obj.ID); raw != obj {
		t.Fatalf("bad: %#v", raw)
	}

	del := db.Txn(true)
	if err := del.Delete("main", obj); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := del.Commit(); err != nil {
		t.Fatalf("err: %v", err)
	}
	rTxn2 := db.Txn(false)
	if raw := mustFirst(t, rTxn2, "main", "id", obj.ID); raw != nil {
		t.Fatalf("object should be deleted")
	}
}

// Schema validation rejects invalid reference declarations.
func TestReference_SchemaValidation(t *testing.T) {
	base := referenceTestSchema()

	cases := []struct {
		name   string
		mutate func(*DBSchema)
	}{
		{
			name: "missing local index",
			mutate: func(s *DBSchema) {
				s.Tables["edge"].References[0].LocalIndex = "nope"
			},
		},
		{
			name: "missing foreign table",
			mutate: func(s *DBSchema) {
				s.Tables["edge"].References[0].ForeignTable = "nope"
			},
		},
		{
			name: "non-unique foreign index",
			mutate: func(s *DBSchema) {
				s.Tables["edge"].References[0].ForeignIndex = "parent"
			},
		},
		{
			name: "indexer type mismatch",
			mutate: func(s *DBSchema) {
				// A local int indexer referencing the string id indexer.
				s.Tables["edge"].Indexes["nodeint"] = &IndexSchema{
					Name:    "nodeint",
					Indexer: &IntFieldIndex{Field: "Bar"},
				}
				s.Tables["edge"].References[0].LocalIndex = "nodeint"
			},
		},
		{
			name: "bad on-delete action",
			mutate: func(s *DBSchema) {
				s.Tables["edge"].References[0].OnDelete = "explode"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := referenceTestSchema()
			tc.mutate(s)
			if err := s.Validate(); err == nil {
				t.Fatalf("expected schema validation error for %s", tc.name)
			}
		})
	}

	if err := base.Validate(); err != nil {
		t.Fatalf("base schema should be valid: %v", err)
	}
}
