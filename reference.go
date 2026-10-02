// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import (
	"bytes"
	"fmt"
)

// deletedRow identifies a parent row that has been removed from the pending
// view, carrying the value it had so its foreign-key columns can be inspected.
type deletedRow struct {
	table string
	value interface{}
}

// applyReferences enforces every reference declared in the schema against the
// transaction's pending view. It applies cascade deletes transitively and then
// verifies that no surviving child references a missing parent and that no
// restrict-referenced parent is deleted. All reads and writes use the in-flight
// transaction, so nothing becomes visible (and nothing is lost) until the
// caller proceeds to publish the indexes.
func (txn *Txn) applyReferences() error {
	schema := txn.db.schema

	// Fast path: no table declares references.
	hasReferences := false
	for _, table := range schema.Tables {
		if len(table.References) > 0 {
			hasReferences = true
			break
		}
	}
	if !hasReferences || len(txn.mutated) == 0 {
		return nil
	}

	// Build the inverse index: parent table -> inbound references from child
	// tables that point at it.
	inbound := make(map[string][]inboundRef)
	for childName, child := range schema.Tables {
		for _, ref := range child.References {
			inbound[ref.ForeignTable] = append(inbound[ref.ForeignTable], inboundRef{
				childTable: childName,
				ref:        ref,
			})
		}
	}

	// Seed the cascade with rows explicitly deleted by the transaction.
	// Rows inserted and deleted within the same transaction never existed in
	// any committed view and therefore cannot have pre-existing children.
	visited := make(map[objectID]struct{})
	var queue []deletedRow
	for oid, state := range txn.mutated {
		if state.existed && !state.present {
			visited[oid] = struct{}{}
			queue = append(queue, deletedRow{
				table: oid.Table,
				value: state.original,
			})
		}
	}

	// Process cascade deletes breadth-first. The shared visited set ensures a
	// child reached more than once (diamond or a reference cycle) is deleted
	// at most once.
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]

		for _, in := range inbound[parent.table] {
			matchVal, err := parentMatchValue(schema, in.ref, parent.value)
			if err != nil {
				return err
			}

			children, err := txn.referencingRows(in.childTable, in.ref, matchVal)
			if err != nil {
				return err
			}

			for _, child := range children {
				childID, err := rowPrimaryKey(schema, in.childTable, child)
				if err != nil {
					return err
				}
				childOID := objectID{Table: in.childTable, IndexVal: string(childID)}
				if _, seen := visited[childOID]; seen {
					continue
				}
				visited[childOID] = struct{}{}

				switch in.ref.action() {
				case ReferenceActionRestrict:
					return fmt.Errorf("%w: cannot delete %s row still referenced by %s via reference %q",
						ErrReferenceViolation, parent.table, in.childTable, in.ref.Name)

				case ReferenceActionCascade:
					if err := txn.Delete(in.childTable, child); err != nil {
						return err
					}
					queue = append(queue, deletedRow{
						table: in.childTable,
						value: child,
					})
				}
			}
		}
	}

	// Verify the foreign keys of every row that still exists in the pending
	// view but differs from (or is new relative to) the starting snapshot.
	for oid, state := range txn.mutated {
		if !state.present {
			continue
		}
		table := schema.Tables[oid.Table]
		for _, ref := range table.References {
			local := table.Indexes[ref.LocalIndex]
			indexer := local.Indexer.(SingleIndexer)

			ok, val, err := indexer.FromObject(state.latest)
			if err != nil {
				return fmt.Errorf("failed to build reference %q: %w", ref.Name, err)
			}
			if !ok {
				// A NULL foreign key (AllowMissing index) references nothing.
				continue
			}

			found, err := txn.parentExists(ref, val)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("%w: %s row references missing %s row via reference %q",
					ErrReferenceViolation, oid.Table, ref.ForeignTable, ref.Name)
			}
		}
	}

	return nil
}

// inboundRef pairs a reference declaration with the child table that owns it.
type inboundRef struct {
	childTable string
	ref        *ReferenceSchema
}

// rowPrimaryKey extracts the raw primary-index key for a row.
func rowPrimaryKey(schema *DBSchema, table string, obj interface{}) ([]byte, error) {
	idIndexer := schema.Tables[table].Indexes[id].Indexer.(SingleIndexer)
	ok, idVal, err := idIndexer.FromObject(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to build primary index: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("object missing primary index")
	}
	return idVal, nil
}

// parentMatchValue encodes the value that child rows must match for the given
// reference. For the default "id" target this is the parent's primary key;
// otherwise it is extracted from the parent using the foreign index's indexer.
func parentMatchValue(schema *DBSchema, ref *ReferenceSchema, parent interface{}) ([]byte, error) {
	fName := ref.foreignIndexName()
	if fName == id {
		return rowPrimaryKey(schema, ref.ForeignTable, parent)
	}
	indexer := schema.Tables[ref.ForeignTable].Indexes[fName].Indexer.(SingleIndexer)
	ok, val, err := indexer.FromObject(parent)
	if err != nil {
		return nil, fmt.Errorf("failed to build foreign index '%s': %w", fName, err)
	}
	if !ok {
		return nil, nil
	}
	return val, nil
}

// referencingRows returns the child rows currently present in the pending view
// whose reference column matches the given encoded parent value. Both unique
// and non-unique local indexes are supported via a prefix scan.
func (txn *Txn) referencingRows(childTable string, ref *ReferenceSchema, val []byte) ([]interface{}, error) {
	if val == nil {
		return nil, nil
	}
	indexTxn := txn.readableIndex(childTable, ref.LocalIndex)
	iter := indexTxn.Root().Iterator()
	iter.SeekPrefix(val)

	var rows []interface{}
	for {
		key, value, ok := iter.Next()
		if !ok {
			break
		}
		if !bytes.HasPrefix(key, val) {
			break
		}
		rows = append(rows, value)
	}
	return rows, nil
}

// parentExists reports whether the referenced parent row exists in the pending
// view for the given encoded foreign-key value.
func (txn *Txn) parentExists(ref *ReferenceSchema, val []byte) (bool, error) {
	indexTxn := txn.readableIndex(ref.ForeignTable, ref.foreignIndexName())
	_, ok := indexTxn.Get(val)
	return ok, nil
}
