// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import (
	"context"
	"fmt"
	"math"
	"sync"

	iradix "github.com/hashicorp/go-immutable-radix"
)

// IndexQuery describes a single condition against one index of a table. A set
// of IndexQuery values can be evaluated together in a single read transaction
// using Txn.Intersect.
type IndexQuery struct {
	// Index is the name of the index to query. As with Txn.Get, a "_prefix"
	// suffix may be used to perform a prefix match if the index supports it.
	Index string

	// Args are passed to the indexer to build the lookup value.
	Args []interface{}
}

// Intersect is used to construct a ResultIterator over the rows of a table
// that match ALL of the given index queries. Each query is an exact match on
// an index (or a prefix match when the "_prefix" suffix is used), and the
// results are the intersection of the individual query results. Every
// matching object is returned exactly once, even if it matches a query
// through multiple values of a multi-value index.
//
// All queries are evaluated against the same consistent snapshot of the
// transaction, so callers no longer need to intersect the results of
// multiple Get calls themselves.
//
// The candidate sets of the queries are counted first, and only the smallest
// set is materialized; the remaining sets are streamed through it. This
// keeps memory proportional to the smallest candidate set rather than the
// largest, which matters when the selectivity of the conditions differs
// greatly.
//
// The watch channel of the returned iterator fires when a subsequent write
// transaction may have changed the result. The watches are registered on the
// same snapshot that produced the results, so a write committed at any point
// after this transaction started is guaranteed to trigger the watch, even if
// it lands while the watches are being established.
//
// See the documentation for ResultIterator to understand the behaviour of
// the returned ResultIterator.
func (txn *Txn) Intersect(table string, queries ...IndexQuery) (ResultIterator, error) {
	if len(queries) == 0 {
		return nil, fmt.Errorf("intersect requires at least one query")
	}

	// A single query needs no intersection; defer to Get.
	if len(queries) == 1 {
		return txn.Get(table, queries[0].Index, queries[0].Args...)
	}

	// Resolve every query against the same consistent snapshot and register
	// a watch for each. The radix root is kept so that each pass over a
	// condition can use a fresh iterator.
	type condition struct {
		root  *iradix.Node
		val   []byte
		watch <-chan struct{}
	}
	conds := make([]condition, 0, len(queries))
	for _, q := range queries {
		indexSchema, val, err := txn.getIndexValue(table, q.Index, q.Args...)
		if err != nil {
			return nil, err
		}
		indexTxn := txn.readableIndex(table, indexSchema.Name)
		root := indexTxn.Root()
		indexIter := root.Iterator()
		watchCh := indexIter.SeekPrefixWatch(val)
		conds = append(conds, condition{root: root, val: val, watch: watchCh})
	}

	// Count the candidates of each condition to find the smallest set. The
	// walk stops as soon as a condition exceeds the best count seen so far,
	// so this never walks much more than the smallest set overall.
	driver := 0
	driverCount := math.MaxInt
	for i := range conds {
		count := 0
		iter := conds[i].root.Iterator()
		iter.SeekPrefix(conds[i].val)
		for {
			if _, _, ok := iter.Next(); !ok {
				break
			}
			count++
			if count > driverCount {
				break
			}
		}
		if count < driverCount {
			driver, driverCount = i, count
		}
	}

	// Build the primary-key extractor used to de-duplicate and intersect.
	tableSchema := txn.db.schema.Tables[table]
	idIndexer := tableSchema.Indexes[id].Indexer.(SingleIndexer)
	keyOf := func(obj interface{}) (string, error) {
		ok, idVal, err := idIndexer.FromObject(obj)
		if err != nil {
			return "", fmt.Errorf("failed to build primary index: %v", err)
		}
		if !ok {
			return "", fmt.Errorf("object missing primary index")
		}
		return string(idVal), nil
	}

	// Materialize only the smallest candidate set. Objects are keyed by
	// primary ID, which also de-duplicates objects that a multi-value index
	// returns more than once.
	objs := make([]interface{}, 0, driverCount)
	inSet := make(map[string]struct{}, driverCount)
	driverIter := conds[driver].root.Iterator()
	driverIter.SeekPrefix(conds[driver].val)
	for {
		_, obj, ok := driverIter.Next()
		if !ok {
			break
		}
		key, err := keyOf(obj)
		if err != nil {
			return nil, err
		}
		if _, dup := inSet[key]; dup {
			continue
		}
		inSet[key] = struct{}{}
		objs = append(objs, obj)
	}

	// Stream each remaining condition and keep only the candidates that are
	// still in the running set. The larger sets are never materialized.
	for i := range conds {
		if i == driver || len(objs) == 0 {
			continue
		}
		matched := make(map[string]struct{}, len(objs))
		iter := conds[i].root.Iterator()
		iter.SeekPrefix(conds[i].val)
		for {
			_, obj, ok := iter.Next()
			if !ok {
				break
			}
			key, err := keyOf(obj)
			if err != nil {
				return nil, err
			}
			if _, ok := inSet[key]; ok {
				matched[key] = struct{}{}
			}
		}
		kept := make([]interface{}, 0, len(matched))
		for _, obj := range objs {
			key, err := keyOf(obj)
			if err != nil {
				return nil, err
			}
			if _, ok := matched[key]; ok {
				kept = append(kept, obj)
			}
		}
		objs, inSet = kept, matched
	}

	// Collect the per-condition watches. They are merged into a single
	// channel lazily in WatchCh so that callers that never watch don't pay
	// for a goroutine.
	watches := NewWatchSet()
	for _, c := range conds {
		watches.Add(c.watch)
	}
	return &intersectIterator{
		objs:    objs,
		watches: watches,
		driver:  driver,
	}, nil
}

// intersectIterator is a ResultIterator over the materialized intersection
// of multiple index queries.
type intersectIterator struct {
	objs    []interface{}
	watches WatchSet

	// watchOnce and watchCh implement the lazy merged watch channel.
	watchOnce sync.Once
	watchCh   chan struct{}

	// driver records which query supplied the candidate set. It is
	// unexported and only used by tests to verify the smallest set is used.
	driver int
}

func (i *intersectIterator) WatchCh() <-chan struct{} {
	i.watchOnce.Do(func() {
		i.watchCh = make(chan struct{})
		go func() {
			i.watches.WatchCtx(context.Background())
			close(i.watchCh)
		}()
	})
	return i.watchCh
}

func (i *intersectIterator) Next() interface{} {
	if len(i.objs) == 0 {
		return nil
	}
	obj := i.objs[0]
	i.objs = i.objs[1:]
	return obj
}
