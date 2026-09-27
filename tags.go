/*
 * SPDX-FileCopyrightText: © 2017-2025 Istari Digital, Inc.
 * SPDX-License-Identifier: Apache-2.0
 */

package ristretto

import "sync"

// tagEntry holds the tags registered for a single key. gen is a monotonically
// increasing generation assigned at registration time; it lets asynchronous
// consumers (the processItems goroutine) tell whether the index entry still
// corresponds to the Item they are handling, or a newer Set has replaced it.
// applied reports whether the registered value has been applied to the store.
type tagEntry struct {
	tags    map[string]struct{}
	gen     uint64
	applied bool
}

// tagIndex is a bidirectional index between tags and the keys registered
// under them. A key may belong to any number of tags. It is safe for
// concurrent use.
type tagIndex struct {
	mu    sync.Mutex
	byTag map[string]map[uint64]struct{}
	byKey map[uint64]*tagEntry
	gen   uint64
}

func newTagIndex() *tagIndex {
	return &tagIndex{
		byTag: make(map[string]map[uint64]struct{}),
		byKey: make(map[uint64]*tagEntry),
	}
}

// set replaces the tag set of key with the given tags and returns the
// generation assigned to this registration. Replacing a key's tags removes
// it from the key sets of its previous tags.
func (t *tagIndex) set(key uint64, tags []string) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gen++
	gen := t.gen
	t.removeKeyLocked(key)
	if len(tags) == 0 {
		return gen
	}
	entry := &tagEntry{tags: make(map[string]struct{}, len(tags)), gen: gen}
	for _, tag := range tags {
		entry.tags[tag] = struct{}{}
		keys := t.byTag[tag]
		if keys == nil {
			keys = make(map[uint64]struct{})
			t.byTag[tag] = keys
		}
		keys[key] = struct{}{}
	}
	t.byKey[key] = entry
	return gen
}

func (t *tagIndex) removeKeyLocked(key uint64) {
	entry, ok := t.byKey[key]
	if !ok {
		return
	}
	for tag := range entry.tags {
		keys := t.byTag[tag]
		delete(keys, key)
		if len(keys) == 0 {
			delete(t.byTag, tag)
		}
	}
	delete(t.byKey, key)
}

// del removes key from the index unconditionally.
func (t *tagIndex) del(key uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.removeKeyLocked(key)
}

// delIfGen removes key's entry only if it still carries the given generation,
// i.e. no newer Set has re-registered the key in the meantime. It is used
// when a queued write is rejected or dropped so that a newer registration
// for the same key is not removed by accident.
func (t *tagIndex) delIfGen(key, gen uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if entry, ok := t.byKey[key]; ok && entry.gen == gen {
		t.removeKeyLocked(key)
	}
}

// delIfApplied removes key's entry only if the registered value has been
// applied to the store. It is used when the stored value is evicted or
// expires: a newer Set that is still queued must keep its registration.
func (t *tagIndex) delIfApplied(key uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if entry, ok := t.byKey[key]; ok && entry.applied {
		t.removeKeyLocked(key)
	}
}

// markApplied marks the registration with the given generation as applied to
// the store. Registrations superseded by a newer Set are left untouched.
func (t *tagIndex) markApplied(key, gen uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if entry, ok := t.byKey[key]; ok && entry.gen == gen {
		entry.applied = true
	}
}

// take removes the given tags and every key registered under them from the
// index and returns the affected keys.
func (t *tagIndex) take(tags ...string) map[uint64]struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	keys := make(map[uint64]struct{})
	for _, tag := range tags {
		for key := range t.byTag[tag] {
			keys[key] = struct{}{}
		}
	}
	for key := range keys {
		t.removeKeyLocked(key)
	}
	return keys
}

// clear empties the whole index.
func (t *tagIndex) clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byTag = make(map[string]map[uint64]struct{})
	t.byKey = make(map[uint64]*tagEntry)
}
