// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import (
	"bytes"
	"context"
	"fmt"

	iradix "github.com/hashicorp/go-immutable-radix"
)

// QueryCondition is a single constraint used by GetCompound. It behaves
// exactly like the constraints of Get: Index names the index to use (a
// "_prefix" suffix performs a prefix match if the index implements
// PrefixIndexer) and Args are passed to the indexer to build the lookup
// value.
type QueryCondition struct {
	Index string
	Args  []interface{}
}

// GetCompound is used to construct a ResultIterator over all the rows that
// match every one of the given conditions. Each condition constrains a
// (possibly different) index of the same table, and the result is the
// intersection of the individual matches. Every matching object is returned
// exactly once, even if it is indexed multiple times within a single
// condition (for example by a MultiIndexer under a prefix match).
//
// All conditions are evaluated within the calling transaction, so the
// results form a consistent snapshot, exactly as if each condition had been
// queried separately with Get and intersected by the caller.
//
// The condition with the smallest candidate set is used to drive the
// iteration, and candidates are checked against the remaining conditions by
// computing their index values directly. This keeps the work and memory
// proportional to the smallest candidate set rather than the largest, which
// matters when the candidate set sizes differ greatly.
//
// The watch channel of the returned iterator fires when a subsequent write
// transaction modifies any of the candidate sets of any condition, so it may
// fire for changes that do not affect the intersection. All watches are
// established before GetCompound returns, so a write committed after this
// call cannot be missed.
//
// See the documentation for ResultIterator to understand the behaviour of
// the returned ResultIterator.
func (txn *Txn) GetCompound(table string, conditions ...QueryCondition) (ResultIterator, error) {
	if len(conditions) == 0 {
		return nil, fmt.Errorf("at least one condition is required")
	}

	// Get the table schema
	tableSchema, ok := txn.db.schema.Tables[table]
	if !ok {
		return nil, fmt.Errorf("invalid table '%s'", table)
	}
	idIndexer := tableSchema.Indexes[id].Indexer.(SingleIndexer)

	// Resolve each condition to its index schema and lookup value, and count
	// the size of each candidate set. Counting walks the index without
	// materializing anything, so memory stays bounded by the smallest set
	// rather than the largest.
	type resolvedCondition struct {
		schema *IndexSchema
		val    []byte
		count  int
	}
	resolved := make([]resolvedCondition, len(conditions))
	for i, cond := range conditions {
		indexSchema, val, err := txn.getIndexValue(table, cond.Index, cond.Args...)
		if err != nil {
			return nil, err
		}

		indexTxn := txn.readableIndex(table, indexSchema.Name)
		iter := indexTxn.Root().Iterator()
		iter.SeekPrefix(val)
		count := 0
		for {
			if _, _, ok := iter.Next(); !ok {
				break
			}
			count++
		}
		resolved[i] = resolvedCondition{schema: indexSchema, val: val, count: count}
	}

	// Pick the condition with the smallest candidate set to drive the
	// iteration. If any condition matches nothing the intersection is empty,
	// and this naturally selects that condition so Next returns nil
	// immediately.
	driver := 0
	for i := range resolved {
		if resolved[i].count < resolved[driver].count {
			driver = i
		}
	}

	// Establish a watch on every condition's candidate set before returning,
	// and build the filters applied to each driver candidate.
	watchChans := make([]<-chan struct{}, 0, len(resolved))
	filters := make([]compoundFilter, 0, len(resolved)-1)
	var driverIter *iradix.Iterator
	for i := range resolved {
		rc := &resolved[i]
		indexTxn := txn.readableIndex(table, rc.schema.Name)
		iter := indexTxn.Root().Iterator()
		watchCh := iter.SeekPrefixWatch(rc.val)
		watchChans = append(watchChans, watchCh)
		if i == driver {
			driverIter = iter
		} else {
			filters = append(filters, compoundFilter{schema: rc.schema, val: rc.val})
		}
	}

	return &compoundIterator{
		iter:      driverIter,
		idIndexer: idIndexer,
		filters:   filters,
		watchCh:   mergeWatchChannels(watchChans),
	}, nil
}

// compoundFilter checks a single resolved condition against a candidate
// object by recomputing the object's index values, avoiding the need to
// materialize the condition's candidate set.
type compoundFilter struct {
	schema *IndexSchema
	val    []byte
}

// matches returns whether the object satisfies the condition. A nil
// condition value matches everything, mirroring Get with no arguments.
func (f *compoundFilter) matches(obj interface{}) (bool, error) {
	if f.val == nil {
		return true, nil
	}
	switch indexer := f.schema.Indexer.(type) {
	case SingleIndexer:
		ok, val, err := indexer.FromObject(obj)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		return bytes.HasPrefix(val, f.val), nil
	case MultiIndexer:
		ok, vals, err := indexer.FromObject(obj)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		for _, val := range vals {
			if bytes.HasPrefix(val, f.val) {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("indexer for '%s' must be a SingleIndexer or MultiIndexer", f.schema.Name)
	}
}

// compoundIterator is a ResultIterator that walks the smallest candidate set
// and filters each candidate against the remaining conditions, de-duplicating
// objects by their primary ID.
type compoundIterator struct {
	iter      *iradix.Iterator
	idIndexer SingleIndexer
	filters   []compoundFilter
	seen      map[string]struct{}
	watchCh   <-chan struct{}
}

func (c *compoundIterator) WatchCh() <-chan struct{} {
	return c.watchCh
}

func (c *compoundIterator) Next() interface{} {
	for {
		_, obj, ok := c.iter.Next()
		if !ok {
			return nil
		}

		// De-duplicate by primary ID. An object can appear multiple times in
		// the driving candidate set, for example when a MultiIndexer maps it
		// to several values matched by a prefix condition.
		okID, idVal, err := c.idIndexer.FromObject(obj)
		if err != nil || !okID {
			continue
		}
		if c.seen == nil {
			c.seen = make(map[string]struct{})
		}
		key := string(idVal)
		if _, dup := c.seen[key]; dup {
			continue
		}
		c.seen[key] = struct{}{}

		// A candidate that fails a filter now would fail it again, so it is
		// safe to have marked it seen above.
		match := true
		for i := range c.filters {
			ok, err := c.filters[i].matches(obj)
			if err != nil || !ok {
				match = false
				break
			}
		}
		if match {
			return obj
		}
	}
}

// mergeWatchChannels combines multiple watch channels into a single channel
// that is closed when any of the inputs is closed. For more than one input
// this spawns a goroutine that lives until one of the inputs fires, similar
// to WatchSet.WatchCh.
func mergeWatchChannels(chans []<-chan struct{}) <-chan struct{} {
	switch len(chans) {
	case 0:
		return nil
	case 1:
		return chans[0]
	}
	ws := NewWatchSet()
	for _, ch := range chans {
		ws.Add(ch)
	}
	merged := make(chan struct{})
	go func() {
		ws.WatchCtx(context.Background())
		close(merged)
	}()
	return merged
}
