package buntdb

import (
	"runtime"
	"sync"

	"github.com/tidwall/rtred"
)

const (
	// indexBuildScanBatch is the maximum number of items scanned per
	// read-lock acquisition while a background index build is scanning.
	indexBuildScanBatch = 1024
	// indexBuildCatchupBatch is the maximum number of pending changes
	// applied per write-lock acquisition during the catch-up phase.
	indexBuildCatchupBatch = 1024
)

// DefaultMaxPendingIndexChanges is the default bound on the number of
// distinct keys with unapplied changes that a background index build will
// tolerate before failing with ErrIndexBuildOverflow.
const DefaultMaxPendingIndexChanges = 1 << 16

// BackgroundIndexOptions represents options for a background index build
// started with CreateIndexAsync or CreateSpatialIndexAsync.
type BackgroundIndexOptions struct {
	// MaxPendingChanges bounds the number of distinct keys with changes
	// that have not yet been applied to the candidate index. While the
	// build is scanning, every key that is inserted, updated, or deleted
	// is tracked so that the change can be replayed before the index is
	// published. When the number of such keys exceeds this bound the
	// build fails with ErrIndexBuildOverflow instead of accumulating
	// changes without limit.
	// A zero value uses DefaultMaxPendingIndexChanges.
	MaxPendingChanges int
}

// IndexBuild represents an in-progress background index build. The index
// being built is not visible to queries until the build completes
// successfully.
type IndexBuild struct {
	db  *DB
	idx *index // candidate index, private to the build until published

	done chan struct{} // closed when the build reaches a terminal state

	mu       sync.Mutex
	scanned  int                 // number of items scanned so far
	total    int                 // keyspace size when the build started
	pending  map[string]struct{} // changed keys not yet applied
	maxPend  int                 // bound on len(pending)
	cancel   bool                // set by Cancel
	failErr  error               // set by fail
	err      error               // terminal error, nil on success
	finished bool                // true after done is closed

	// inserted maps a key to the exact item that was inserted into the
	// candidate index, allowing precise removal. Only accessed by the
	// build goroutine.
	inserted map[string]*dbItem
}

// CreateIndexAsync starts building a new index in the background and
// returns immediately with an IndexBuild handle. Unlike CreateIndex, the
// build does not block write transactions for the duration of the scan.
//
// The new index is not visible to queries (Ascend*, Descend*, Indexes,
// etc.) until the build completes successfully. Writes that occur while
// the build is in progress are tracked and replayed onto the candidate
// index before it is published, so the resulting index is identical to
// one that was built synchronously after the writes stopped.
//
// Use IndexBuild.Wait to wait for completion, IndexBuild.Progress to
// observe progress, and IndexBuild.Cancel to abort the build. A failed or
// canceled build only discards its own candidate index; indexes that are
// already in use are never affected.
//
// An error is returned immediately if an index with the same name already
// exists or is already being built.
func (db *DB) CreateIndexAsync(name, pattern string,
	opts *BackgroundIndexOptions,
	less ...func(a, b string) bool,
) (*IndexBuild, error) {
	return db.createIndexAsync(name, pattern, less, nil, opts)
}

// CreateSpatialIndexAsync is the background equivalent of
// CreateSpatialIndex. See CreateIndexAsync for details.
func (db *DB) CreateSpatialIndexAsync(name, pattern string,
	opts *BackgroundIndexOptions,
	rect func(item string) (min, max []float64),
) (*IndexBuild, error) {
	return db.createIndexAsync(name, pattern, nil, rect, opts)
}

// createIndexAsync registers a new background index build and starts the
// build goroutine.
func (db *DB) createIndexAsync(name, pattern string,
	lessers []func(a, b string) bool,
	rect func(item string) (min, max []float64),
	opts *BackgroundIndexOptions,
) (*IndexBuild, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil, ErrDatabaseClosed
	}
	if name == "" {
		// cannot create an index without a name.
		return nil, ErrIndexExists
	}
	if _, ok := db.idxs[name]; ok {
		return nil, ErrIndexExists
	}
	if _, ok := db.builds[name]; ok {
		// an index with this name is already being built.
		return nil, ErrIndexExists
	}
	// generate a less function, same rules as Tx.createIndex.
	var less func(a, b string) bool
	switch len(lessers) {
	default:
		less = func(a, b string) bool {
			for i := 0; i < len(lessers)-1; i++ {
				if lessers[i](a, b) {
					return true
				}
				if lessers[i](b, a) {
					return false
				}
			}
			return lessers[len(lessers)-1](a, b)
		}
	case 0:
		// no less function
	case 1:
		less = lessers[0]
	}
	idx := &index{
		name:    name,
		pattern: pattern,
		less:    less,
		rect:    rect,
		db:      db,
	}
	if less != nil {
		idx.btr = btreeNew(lessCtx(idx))
	}
	if rect != nil {
		idx.rtr = rtred.New(idx)
	}
	maxPend := DefaultMaxPendingIndexChanges
	if opts != nil && opts.MaxPendingChanges > 0 {
		maxPend = opts.MaxPendingChanges
	}
	b := &IndexBuild{
		db:       db,
		idx:      idx,
		done:     make(chan struct{}),
		pending:  make(map[string]struct{}),
		maxPend:  maxPend,
		inserted: make(map[string]*dbItem),
		total:    db.keys.Len(),
	}
	db.builds[name] = b
	go b.run()
	return b, nil
}

// Progress returns the number of items scanned so far and the size of the
// keyspace when the build started. The total is a snapshot taken at the
// start of the build and may differ from the current database size.
func (b *IndexBuild) Progress() (scanned, total int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.scanned, b.total
}

// Done returns a channel that is closed when the build reaches a terminal
// state, whether by success, failure, or cancellation.
func (b *IndexBuild) Done() <-chan struct{} {
	return b.done
}

// Wait blocks until the build reaches a terminal state and returns the
// result. A nil return value means the index was published and is now
// visible to queries. Otherwise the returned error describes why the
// build did not complete: ErrIndexBuildCanceled, ErrIndexBuildOverflow,
// ErrIndexBuildReset, ErrIndexExists (an index with the same name was
// created before this build could publish), or ErrDatabaseClosed.
func (b *IndexBuild) Wait() error {
	<-b.done
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

// Cancel aborts the build. The candidate index is discarded and the index
// name is released; indexes that are already in use are not affected.
// Cancel returns immediately; use Wait to observe the completion of the
// cancellation, which reports ErrIndexBuildCanceled. Calling Cancel on a
// finished build returns that build's result.
func (b *IndexBuild) Cancel() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.finished {
		return b.err
	}
	b.cancel = true
	return nil
}

// fail marks the build as failed. The build goroutine observes the
// failure, discards the candidate, and unregisters the build.
func (b *IndexBuild) fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.finished && b.failErr == nil {
		b.failErr = err
	}
}

// addPending records a changed key for the catch-up phase. It is called
// from insertIntoDatabase and deleteFromDatabase while the database write
// lock is held.
func (b *IndexBuild) addPending(key string) {
	if !b.idx.match(key) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.finished || b.failErr != nil || b.cancel {
		return
	}
	b.pending[key] = struct{}{}
	if len(b.pending) > b.maxPend {
		// the catch-up phase cannot keep up within the configured
		// capacity bound. fail explicitly rather than accumulating
		// changes without limit.
		b.failErr = ErrIndexBuildOverflow
	}
}

// checkAbort returns the reason the build should abort, or nil.
func (b *IndexBuild) checkAbort() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failErr != nil {
		return b.failErr
	}
	if b.cancel {
		return ErrIndexBuildCanceled
	}
	return nil
}

// run executes the build: scan phase, then catch-up and publish.
func (b *IndexBuild) run() {
	if b.scanPhase() {
		b.catchupPhase()
	}
}

// scanPhase iterates the keyspace in batches, releasing the read lock
// between batches so that write transactions are not blocked for the
// duration of the scan. Returns false if the build was aborted.
func (b *IndexBuild) scanPhase() bool {
	var lastKey string
	started := false
	for {
		if err := b.checkAbort(); err != nil {
			b.abort(err)
			return false
		}
		b.db.mu.RLock()
		if b.db.closed {
			b.db.mu.RUnlock()
			b.abort(ErrDatabaseClosed)
			return false
		}
		count := 0
		finished := true
		iter := func(item interface{}) bool {
			dbi := item.(*dbItem)
			if started && dbi.key == lastKey {
				// skip the pivot item itself
				return true
			}
			if count == indexBuildScanBatch {
				// batch limit reached, continue in the next batch
				finished = false
				return false
			}
			if b.idx.match(dbi.key) {
				b.candidateSet(dbi)
			}
			lastKey = dbi.key
			started = true
			count++
			return true
		}
		if !started {
			btreeAscend(b.db.keys, iter)
		} else {
			btreeAscendGreaterOrEqual(b.db.keys, &dbItem{key: lastKey}, iter)
		}
		b.db.mu.RUnlock()
		b.mu.Lock()
		b.scanned += count
		b.mu.Unlock()
		if finished {
			return true
		}
		// allow pending write transactions to proceed between batches
		runtime.Gosched()
	}
}

// catchupPhase applies pending changes to the candidate index in batches.
// Once the pending set is drained while holding the write lock, no further
// changes can arrive and the candidate is published atomically.
func (b *IndexBuild) catchupPhase() {
	for {
		b.db.mu.Lock()
		if b.db.closed {
			b.db.mu.Unlock()
			b.abort(ErrDatabaseClosed)
			return
		}
		if err := b.checkAbort(); err != nil {
			b.db.mu.Unlock()
			b.abort(err)
			return
		}
		// apply up to a batch of pending changes
		n := 0
		b.mu.Lock()
		for key := range b.pending {
			delete(b.pending, key)
			b.applyChange(key)
			n++
			if n == indexBuildCatchupBatch {
				break
			}
		}
		empty := len(b.pending) == 0
		b.mu.Unlock()
		if empty {
			// the pending set is empty and the write lock is held, so
			// no new changes can arrive. publish the candidate index.
			var err error
			if _, ok := b.db.idxs[b.idx.name]; ok {
				// an index with the same name was created while this
				// build was in progress.
				err = ErrIndexExists
			} else {
				b.db.idxs[b.idx.name] = b.idx
			}
			delete(b.db.builds, b.idx.name)
			b.db.mu.Unlock()
			b.finish(err)
			return
		}
		b.db.mu.Unlock()
		// more changes to apply, let writers proceed between batches
		runtime.Gosched()
	}
}

// applyChange brings the candidate index up to date for a single key by
// re-reading the current value from the database. Must be called with the
// database write lock held.
func (b *IndexBuild) applyChange(key string) {
	if old, ok := b.inserted[key]; ok {
		b.candidateDelete(old)
	}
	if item := b.db.get(key); item != nil && b.idx.match(item.key) {
		b.candidateSet(item)
	}
}

// candidateSet inserts an item into the candidate index, replacing any
// previously inserted item with the same key. Only called by the build
// goroutine.
func (b *IndexBuild) candidateSet(dbi *dbItem) {
	if old, ok := b.inserted[dbi.key]; ok {
		b.candidateDelete(old)
	}
	if b.idx.btr != nil {
		b.idx.btr.Set(dbi)
	}
	if b.idx.rtr != nil {
		b.idx.rtr.Insert(dbi)
	}
	b.inserted[dbi.key] = dbi
}

// candidateDelete removes an item from the candidate index. The item must
// be the exact one that was previously inserted. Only called by the build
// goroutine.
func (b *IndexBuild) candidateDelete(dbi *dbItem) {
	if b.idx.btr != nil {
		b.idx.btr.Delete(dbi)
	}
	if b.idx.rtr != nil {
		b.idx.rtr.Remove(dbi)
	}
	delete(b.inserted, dbi.key)
}

// abort discards the candidate index and unregisters the build. Only this
// build's own registration is removed; indexes that are in use, including
// any index that was created with the same name after this build started,
// are left untouched.
func (b *IndexBuild) abort(err error) {
	b.db.mu.Lock()
	if cur, ok := b.db.builds[b.idx.name]; ok && cur == b {
		delete(b.db.builds, b.idx.name)
	}
	b.db.mu.Unlock()
	b.finish(err)
}

// finish sets the terminal result and closes the done channel.
func (b *IndexBuild) finish(err error) {
	b.mu.Lock()
	if b.finished {
		b.mu.Unlock()
		return
	}
	b.err = err
	b.finished = true
	b.mu.Unlock()
	close(b.done)
}
