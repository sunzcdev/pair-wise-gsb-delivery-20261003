// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import "fmt"

// Order identifies the traversal direction used by Rank and Select.
type Order int

const (
	// Ascending ranks objects from the smallest index value to the largest,
	// breaking ties by primary key. This is the zero value.
	Ascending Order = iota

	// Descending ranks objects from the largest index value to the smallest,
	// breaking ties by primary key.
	Descending
)

// ErrNotSortable is returned by Rank/Select when the target index was not
// declared with Sortable: true in the schema.
var ErrNotSortable = fmt.Errorf("index does not support rank/select; set Sortable on the schema")

// sortableSchema resolves and validates an index for order-statistics use.
func (txn *Txn) sortableSchema(table, index string) (*IndexSchema, error) {
	tableSchema, ok := txn.db.schema.Tables[table]
	if !ok {
		return nil, fmt.Errorf("invalid table '%s'", table)
	}
	indexSchema, ok := tableSchema.Indexes[index]
	if !ok {
		return nil, fmt.Errorf("invalid index '%s'", index)
	}
	if !indexSchema.Sortable {
		return nil, ErrNotSortable
	}
	return indexSchema, nil
}

// orderKeyFor extracts the composite sortable key (index value plus primary
// key for non-unique indexes) for an already-stored object.
func orderKeyFor(tableSchema *TableSchema, indexSchema *IndexSchema, obj interface{}) ([]byte, error) {
	okId, idVal, err := tableSchema.Indexes[id].Indexer.(SingleIndexer).FromObject(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to build primary index: %v", err)
	}
	if !okId {
		return nil, fmt.Errorf("object missing primary index")
	}

	ok, val, err := indexSchema.Indexer.(SingleIndexer).FromObject(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to build index '%s': %v", indexSchema.Name, err)
	}
	if !ok {
		if indexSchema.AllowMissing {
			return nil, nil
		}
		return nil, nil
	}
	if !indexSchema.Unique {
		val = append(val, idVal...)
	}
	return val, nil
}

// Select returns the object at the given 1-based position on a sortable index.
// rank is 1-based: 1 selects the first object in the requested order.
//
// Ordering is by encoded index value; equal values on a non-unique index are
// ordered by primary key, producing a stable total order. The result reflects
// the transaction's MVCC snapshot, so a long-lived read transaction keeps
// returning the same object at a given rank even while writers insert, delete
// and update rows.
//
// nil (and no error) is returned when rank is out of range. This runs in
// expected O(log n); it does not iterate over and discard earlier entries.
func (txn *Txn) Select(table, index string, order Order, rank int) (interface{}, error) {
	if rank < 1 {
		return nil, nil
	}
	indexSchema, err := txn.sortableSchema(table, index)
	if err != nil {
		return nil, err
	}

	root := txn.readableOrder(table, indexSchema.Name)
	total := tSize(root)
	if rank > total {
		return nil, nil
	}

	k := rank
	if order == Descending {
		k = total - rank + 1
	}
	node := tKth(root, k)
	if node == nil {
		return nil, nil
	}
	return node.val, nil
}

// RankObject returns the 1-based position of obj on a sortable index, and
// whether the object is present in the index at all. It is the inverse of
// Select: Select(table, index, order, RankObject(...)) returns obj.
//
// The object's position is derived from its index value and primary key, so
// equal index values are stably ordered by primary key. The lookup is
// performed against the transaction snapshot.
//
// When found is false the returned rank is 0. Expected time is O(log n).
func (txn *Txn) RankObject(table, index string, order Order, obj interface{}) (rank int, found bool, err error) {
	indexSchema, err := txn.sortableSchema(table, index)
	if err != nil {
		return 0, false, err
	}
	tableSchema := txn.db.schema.Tables[table]

	key, err := orderKeyFor(tableSchema, indexSchema, obj)
	if err != nil {
		return 0, false, err
	}
	if key == nil {
		return 0, false, nil
	}

	return txn.rankByKey(table, indexSchema, order, key)
}

// Rank returns the 1-based position of the object matching the exact index
// value on a unique sortable index, and whether such an object exists. Use
// RankObject for a non-unique sortable index (where the primary key is needed
// to disambiguate ties). Rank is the inverse of Select.
func (txn *Txn) Rank(table, index string, order Order, args ...interface{}) (rank int, found bool, err error) {
	indexSchema, err := txn.sortableSchema(table, index)
	if err != nil {
		return 0, false, err
	}
	if !indexSchema.Unique {
		return 0, false, fmt.Errorf("Rank requires a unique index; use RankObject for non-unique index '%s'", index)
	}

	key, err := indexSchema.Indexer.FromArgs(args...)
	if err != nil {
		return 0, false, err
	}
	return txn.rankByKey(table, indexSchema, order, key)
}

// rankByKey computes the rank of a composite key, verifying presence.
func (txn *Txn) rankByKey(table string, indexSchema *IndexSchema, order Order, key []byte) (int, bool, error) {
	root := txn.readableOrder(table, indexSchema.Name)
	if _, ok := tGet(root, key); !ok {
		return 0, false, nil
	}

	asc := tCountLess(root, key) + 1
	if order == Descending {
		return tSize(root) - asc + 1, true, nil
	}
	return asc, true, nil
}

// Count returns the number of entries in a sortable index. This reflects the
// transaction snapshot and runs in constant time (the size is cached at the
// root of the order-statistics tree).
func (txn *Txn) Count(table, index string) (int, error) {
	indexSchema, err := txn.sortableSchema(table, index)
	if err != nil {
		return 0, err
	}
	return tSize(txn.readableOrder(table, indexSchema.Name)), nil
}

// FirstN returns a ResultIterator over the first n objects in the requested
// order on a sortable index. This is a convenience for leaderboard-style
// "top N" queries; the iterator walks the order-statistics tree directly.
func (txn *Txn) FirstN(table, index string, order Order, n int) (ResultIterator, error) {
	indexSchema, err := txn.sortableSchema(table, index)
	if err != nil {
		return nil, err
	}
	if n < 0 {
		n = 0
	}
	root := txn.readableOrder(table, indexSchema.Name)
	return &orderIterator{
		root:  root,
		order: order,
		start: 1,
		limit: n,
	}, nil
}

// orderIterator traverses a persistent order tree rank window.
type orderIterator struct {
	root  *tNode
	order Order
	start int
	limit int
	seen  int
}

func (it *orderIterator) WatchCh() <-chan struct{} { return nil }

func (it *orderIterator) Next() interface{} {
	if it.seen >= it.limit {
		return nil
	}
	total := tSize(it.root)
	asc := it.start + it.seen
	if asc > total {
		return nil
	}
	it.seen++

	k := asc
	if it.order == Descending {
		k = total - asc + 1
	}
	node := tKth(it.root, k)
	if node == nil {
		return nil
	}
	return node.val
}
