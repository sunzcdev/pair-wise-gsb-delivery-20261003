package memdb

import (
	"bytes"
	"fmt"

	iradix "github.com/hashicorp/go-immutable-radix"
)

// rangeWatcher is a single WatchRange subscription registered on a MemDB.
// The channel is closed, and the subscription removed, when a committed
// write transaction mutates any index key within [lower, upper).
type rangeWatcher struct {
	lower []byte // inclusive lower bound, encoded index key
	upper []byte // exclusive upper bound, encoded index key
	ch    chan struct{}
}

// matches reports whether the given encoded index key falls within the
// watched range [lower, upper).
//
// For non-unique indexes the stored keys carry the primary key as a suffix,
// while the bounds do not. This preserves the half-open semantics on the
// indexed value itself: a key whose value equals the lower bound sorts at or
// after lower (included), and a key whose value equals the upper bound sorts
// strictly after upper because of the appended primary key (excluded).
func (w *rangeWatcher) matches(key []byte) bool {
	return bytes.Compare(key, w.lower) >= 0 && bytes.Compare(key, w.upper) < 0
}

// indexMutation records the set of encoded keys inserted into or deleted
// from a single table index during a write transaction. dirty is set when
// the index was mutated in bulk (DeletePrefix) and the exact key set is not
// tracked, in which case all watchers on the index are notified.
type indexMutation struct {
	keys  [][]byte
	dirty bool
}

// addRangeWatcher registers a WatchRange subscription on the given index.
func (db *MemDB) addRangeWatcher(ti tableIndex, w *rangeWatcher) {
	db.watchMu.Lock()
	defer db.watchMu.Unlock()
	if db.rangeWatchers == nil {
		db.rangeWatchers = make(map[tableIndex]map[*rangeWatcher]struct{})
	}
	watchers := db.rangeWatchers[ti]
	if watchers == nil {
		watchers = make(map[*rangeWatcher]struct{})
		db.rangeWatchers[ti] = watchers
	}
	watchers[w] = struct{}{}
}

// removeRangeWatcher unregisters a WatchRange subscription without closing
// its channel. It is a no-op if the subscription already fired or was
// already removed.
func (db *MemDB) removeRangeWatcher(ti tableIndex, w *rangeWatcher) {
	db.watchMu.Lock()
	defer db.watchMu.Unlock()
	watchers := db.rangeWatchers[ti]
	if _, ok := watchers[w]; !ok {
		return
	}
	delete(watchers, w)
	if len(watchers) == 0 {
		delete(db.rangeWatchers, ti)
	}
}

// rangeWatcherCount returns the number of active WatchRange subscriptions.
func (db *MemDB) rangeWatcherCount() int {
	db.watchMu.Lock()
	defer db.watchMu.Unlock()
	n := 0
	for _, watchers := range db.rangeWatchers {
		n += len(watchers)
	}
	return n
}

// notifyRangeWatchers fires every WatchRange subscription whose range
// intersects the given committed mutations. Fired subscriptions are removed
// and their channels closed. This must be called only after the new root of
// the DB has been published, so that woken observers reading through a new
// transaction can see the changes that triggered the notification.
func (db *MemDB) notifyRangeWatchers(muts map[tableIndex]*indexMutation) {
	if len(muts) == 0 {
		return
	}
	db.watchMu.Lock()
	defer db.watchMu.Unlock()
	for ti, mut := range muts {
		watchers := db.rangeWatchers[ti]
		for w := range watchers {
			fired := mut.dirty
			if !fired {
				for _, key := range mut.keys {
					if w.matches(key) {
						fired = true
						break
					}
				}
			}
			if fired {
				delete(watchers, w)
				close(w.ch)
			}
		}
		if len(watchers) == 0 {
			delete(db.rangeWatchers, ti)
		}
	}
}

// recordIndexKeys notes that the given encoded keys were inserted into or
// deleted from an index during this transaction, so that WatchRange
// subscriptions can be evaluated at commit time.
func (txn *Txn) recordIndexKeys(table, index string, keys ...[]byte) {
	mut := txn.indexMutation(table, index)
	mut.keys = append(mut.keys, keys...)
}

// markIndexDirty notes that an index was mutated in bulk without tracking
// the exact keys, so all WatchRange subscriptions on it must fire at commit.
func (txn *Txn) markIndexDirty(table, index string) {
	txn.indexMutation(table, index).dirty = true
}

func (txn *Txn) indexMutation(table, index string) *indexMutation {
	if txn.indexMutations == nil {
		txn.indexMutations = make(map[tableIndex]*indexMutation)
	}
	ti := tableIndex{table, index}
	mut, ok := txn.indexMutations[ti]
	if !ok {
		mut = &indexMutation{}
		txn.indexMutations[ti] = mut
	}
	return mut
}

// RangeIterator is a ResultIterator over the rows of an index whose encoded
// keys fall within a half-open range, returned by WatchRange. In addition to
// the ResultIterator behaviour it allows the watch subscription to be
// released explicitly via Stop.
type RangeIterator struct {
	db      *MemDB
	ti      tableIndex
	watcher *rangeWatcher
	iter    *iradix.Iterator
	upper   []byte
}

// WatchCh returns a channel that is closed when a write transaction commits
// a mutation to any index key within the watched range. The subscription is
// consumed when it fires; create a new WatchRange iterator to keep watching.
func (r *RangeIterator) WatchCh() <-chan struct{} {
	return r.watcher.ch
}

// Next returns the next row within the range, or nil once the upper bound
// is reached or the index is exhausted.
func (r *RangeIterator) Next() interface{} {
	key, value, ok := r.iter.Next()
	if !ok || bytes.Compare(key, r.upper) >= 0 {
		return nil
	}
	return value
}

// Stop unregisters the watch subscription so it no longer fires and no
// longer consumes resources. It does not close the watch channel, and it is
// a no-op if the subscription already fired or was stopped.
func (r *RangeIterator) Stop() {
	r.db.removeRangeWatcher(r.ti, r.watcher)
}

// WatchRange is used to construct a RangeIterator over all the rows of an
// index whose value falls within the half-open range [lower, upper): the
// lower bound is inclusive and the upper bound is exclusive. The bounds are
// encoded with the index's indexer, exactly like the args of Get. If lower
// encodes to a key greater than or equal to upper, the range is empty: the
// iterator yields no rows and the watch never fires.
//
// For non-unique indexes, where multiple rows may share the same indexed
// value, the half-open semantics apply to the indexed value itself: rows
// whose value equals lower are included and rows whose value equals upper
// are excluded, no matter how many duplicates exist.
//
// Unlike LowerBound, the returned iterator can be watched. The watch channel
// is closed when a subsequently committed write transaction inserts or
// deletes any index key within the range. Updates are evaluated against both
// the old and the new index keys, so moving a row into or out of the range
// wakes the watcher. The watch fires on any key mutation intersecting the
// range, even if the membership of the result set is ultimately unchanged.
//
// The watch channel is closed only after the committing transaction has
// published the new state of the DB, so an observer woken by the channel can
// start a new read transaction and immediately see the changes.
//
// A subscription is consumed when it fires. If the watch is no longer
// needed before that, call Stop to release it. Subscriptions only fire for
// transactions committed on the same MemDB they were created from; watchers
// created on a Snapshot never fire.
//
// Note that all values read in the transaction form a consistent snapshot
// from the time when the transaction was created.
func (txn *Txn) WatchRange(table, index string, lower, upper interface{}) (*RangeIterator, error) {
	// Get the table schema
	tableSchema, ok := txn.db.schema.Tables[table]
	if !ok {
		return nil, fmt.Errorf("invalid table '%s'", table)
	}

	// Get the index schema
	indexSchema, ok := tableSchema.Indexes[index]
	if !ok {
		return nil, fmt.Errorf("invalid index '%s'", index)
	}

	// Encode the bounds with the index's indexer
	lowerVal, err := indexSchema.Indexer.FromArgs(lower)
	if err != nil {
		return nil, fmt.Errorf("index error: %v", err)
	}
	upperVal, err := indexSchema.Indexer.FromArgs(upper)
	if err != nil {
		return nil, fmt.Errorf("index error: %v", err)
	}

	// Get an iterator over the index and seek it to the lower bound
	indexTxn := txn.readableIndex(table, indexSchema.Name)
	indexIter := indexTxn.Root().Iterator()
	indexIter.SeekLowerBound(lowerVal)

	// Register the watch subscription
	ti := tableIndex{table, indexSchema.Name}
	watcher := &rangeWatcher{
		lower: lowerVal,
		upper: upperVal,
		ch:    make(chan struct{}),
	}
	txn.db.addRangeWatcher(ti, watcher)

	iter := &RangeIterator{
		db:      txn.db,
		ti:      ti,
		watcher: watcher,
		iter:    indexIter,
		upper:   upperVal,
	}
	return iter, nil
}
