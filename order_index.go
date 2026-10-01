// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

// orderIndex is the immutable, published order-statistics view for a single
// sortable index. It is stored in the root transaction tree under a dedicated
// path and is replaced wholesale on each write commit, just like a radix
// index. Old snapshots keep pointing at the previous orderIndex, so their
// ranks never drift.
//
// root is the persistent treap of index keys to stored objects. rng carries
// the PRNG state used to allocate priorities for new keys; it is advanced only
// on the writable copy so that committed snapshots stay reproducible.
type orderIndex struct {
	root *tNode
	rng  xorshift
}

// orderTxn is a mutable view of an orderIndex scoped to one write
// transaction. It starts from a snapshot root and buffers insert/delete calls
// without publishing anything until the transaction commits.
type orderTxn struct {
	root *tNode

	// rng is a private copy of the published PRNG state. Mutating it never
	// touches a previously committed orderIndex, so transactions on shared
	// snapshots stay isolated.
	rng xorshift
}

// newOrderTxn returns a writable copy seeded from the published index.
func newOrderTxn(oi *orderIndex) *orderTxn {
	if oi == nil {
		oi = &orderIndex{}
	}
	return &orderTxn{
		root: oi.root,
		rng:  oi.rng,
	}
}

// insert adds or replaces an entry in the buffered view.
func (t *orderTxn) insert(key []byte, val interface{}) {
	t.root = tInsert(t.root, key, val, &t.rng)
}

// remove deletes an entry from the buffered view if it is present.
func (t *orderTxn) remove(key []byte) {
	t.root = tDelete(t.root, key)
}

// commit produces the new immutable orderIndex. The PRNG state has been
// mutated in place on the shared index struct, which is safe because the
// writer lock guarantees a single writer and readers never use the PRNG.
func (t *orderTxn) commit() *orderIndex {
	return &orderIndex{
		root: t.root,
		rng:  t.rng,
	}
}

// orderIndexPath returns the root-tree path under which the order-statistics
// structure for an index is stored. It is distinct from indexPath so the two
// never collide.
func orderIndexPath(table, index string) []byte {
	return []byte(table + "." + index + ".order")
}

// orderRoot fetches the published orderIndex for the given sortable index from
// the transaction's snapshot, returning an empty index if none exists yet.
func (txn *Txn) orderRoot(table, index string) *orderIndex {
	raw, _ := txn.rootTxn.Get(orderIndexPath(table, index))
	if raw == nil {
		return &orderIndex{}
	}
	return raw.(*orderIndex)
}

// readableOrder returns the order-statistics root visible to the transaction.
// A write transaction observes its own buffered mutations; a read transaction
// observes the committed snapshot.
func (txn *Txn) readableOrder(table, index string) *tNode {
	if txn.write && txn.orderModified != nil {
		if ot, ok := txn.orderModified[tableIndex{table, index}]; ok {
			return ot.root
		}
	}
	return txn.orderRoot(table, index).root
}

// writableOrder returns a buffered mutable view of an orderIndex for a write
// transaction, creating it on first use.
func (txn *Txn) writableOrder(table, index string) *orderTxn {
	if txn.orderModified == nil {
		txn.orderModified = make(map[tableIndex]*orderTxn)
	}
	key := tableIndex{table, index}
	if ot, ok := txn.orderModified[key]; ok {
		return ot
	}
	ot := newOrderTxn(txn.orderRoot(table, index))
	txn.orderModified[key] = ot
	return ot
}
