// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import (
	"bytes"
	"sync"
)

// rangeWatcher is a single subscription for changes to index keys falling
// within the range [lower, upper). A nil lower bound matches all keys below
// upper, and a nil upper bound matches all keys above lower. The channel is
// closed at most once, after which the subscription is removed from the
// registry.
type rangeWatcher struct {
	lower []byte
	upper []byte
	ch    chan struct{}
}

// contains reports whether the given index key falls within the watched
// range. The lower bound is inclusive and the upper bound is exclusive.
func (w *rangeWatcher) contains(key []byte) bool {
	if w.lower != nil && bytes.Compare(key, w.lower) < 0 {
		return false
	}
	if w.upper != nil && bytes.Compare(key, w.upper) >= 0 {
		return false
	}
	return true
}

// overlapsPrefix reports whether any key carrying the given prefix may fall
// within the watched range. Keys with the prefix occupy the range
// [prefix, prefixSuccessor(prefix)).
func (w *rangeWatcher) overlapsPrefix(prefix []byte) bool {
	if succ := prefixSuccessor(prefix); succ != nil && w.lower != nil &&
		bytes.Compare(w.lower, succ) >= 0 {
		return false
	}
	if w.upper != nil && bytes.Compare(prefix, w.upper) >= 0 {
		return false
	}
	return true
}

// prefixSuccessor returns the smallest key that is strictly greater than all
// keys carrying the given prefix, or nil if there is no such key (the prefix
// is empty or consists only of 0xff bytes).
func prefixSuccessor(prefix []byte) []byte {
	succ := bytes.Clone(prefix)
	for i := len(succ) - 1; i >= 0; i-- {
		if succ[i] != 0xff {
			succ[i]++
			return succ[:i+1]
		}
	}
	return nil
}

// rangeWatcherRegistry tracks the live range watch subscriptions of a DB,
// grouped by the table index they apply to. Subscriptions are one-shot: a
// watcher is removed from the registry before its channel is closed, so the
// number of subscriptions drops back down as watchers are notified.
type rangeWatcherRegistry struct {
	mu   sync.Mutex
	subs map[tableIndex]map[*rangeWatcher]struct{}
}

func newRangeWatcherRegistry() *rangeWatcherRegistry {
	return &rangeWatcherRegistry{
		subs: make(map[tableIndex]map[*rangeWatcher]struct{}),
	}
}

// add subscribes the given watcher and returns the channel that will be
// closed when a matching mutation is committed.
func (r *rangeWatcherRegistry) add(idx tableIndex, w *rangeWatcher) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	set, ok := r.subs[idx]
	if !ok {
		set = make(map[*rangeWatcher]struct{})
		r.subs[idx] = set
	}
	set[w] = struct{}{}
	return w.ch
}

// notifyKey fires and removes all watchers on the given index whose range
// contains the mutated key.
func (r *rangeWatcherRegistry) notifyKey(idx tableIndex, key []byte) {
	r.notifyIf(idx, func(w *rangeWatcher) bool { return w.contains(key) })
}

// notifyPrefix fires and removes all watchers on the given index whose range
// overlaps the deleted prefix.
func (r *rangeWatcherRegistry) notifyPrefix(idx tableIndex, prefix []byte) {
	r.notifyIf(idx, func(w *rangeWatcher) bool { return w.overlapsPrefix(prefix) })
}

func (r *rangeWatcherRegistry) notifyIf(idx tableIndex, match func(*rangeWatcher) bool) {
	r.mu.Lock()
	set := r.subs[idx]
	var fire []chan struct{}
	for w := range set {
		if match(w) {
			fire = append(fire, w.ch)
			delete(set, w)
		}
	}
	if len(set) == 0 {
		delete(r.subs, idx)
	}
	r.mu.Unlock()

	// Close the channels outside of the lock so that woken observers are
	// free to subscribe again without risking deadlock.
	for _, ch := range fire {
		close(ch)
	}
}

// count returns the number of live subscriptions.
func (r *rangeWatcherRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, set := range r.subs {
		n += len(set)
	}
	return n
}
