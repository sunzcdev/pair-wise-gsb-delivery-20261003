package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// DefaultMaxBytes is the default capacity for NewMemoryStore (64 MiB).
const DefaultMaxBytes = 64 << 20

type storedBody struct {
	data []byte
	refs int
}

type storedEntry struct {
	entry *Entry
	elem  *list.Element
}

// MemoryStore is a bounded in-memory Store.
//
// Bodies are stored content-addressed and reference counted: identical
// representations share one backing array, and bodies are reclaimed once no
// variant references them. Entries are evicted least-recently-used first
// once the total resident size exceeds MaxBytes.
type MemoryStore struct {
	// MaxBytes bounds the combined size of stored bodies and entry
	// metadata. A non-positive value means DefaultMaxBytes.
	MaxBytes int

	entries map[string]*storedEntry
	lru     *list.List // front = most recently used
	bodies  map[string]*storedBody

	size int
	mu   sync.Mutex
}

// NewMemoryStore returns a MemoryStore with the given byte budget. A
// non-positive maxBytes selects DefaultMaxBytes.
func NewMemoryStore(maxBytes int) *MemoryStore {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &MemoryStore{
		MaxBytes: maxBytes,
		entries:  make(map[string]*storedEntry),
		lru:      list.New(),
		bodies:   make(map[string]*storedBody),
	}
}

// BodyID returns the content-addressed identifier for a body.
func BodyID(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// GetEntry implements Store.
func (s *MemoryStore) GetEntry(key string) (*Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	se, ok := s.entries[key]
	if !ok {
		return nil, false
	}
	s.lru.MoveToFront(se.elem)
	return se.entry, true
}

// PutEntry implements Store. Replacing an entry reconciles body reference
// counts and evicts until the store is back within budget.
func (s *MemoryStore) PutEntry(entry *Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.entries[entry.PrimaryKey]

	// Acquire references for the new body set first, so bodies still used
	// after the replacement are never dropped to zero and deleted.
	newIDs := entryBodyIDs(entry)
	for id := range newIDs {
		if b, ok := s.bodies[id]; ok {
			b.refs++
		}
	}
	if old != nil {
		s.removeEntryLocked(old)
	}

	elem := s.lru.PushFront(entry)
	s.entries[entry.PrimaryKey] = &storedEntry{entry: entry, elem: elem}
	s.size += entrySize(entry)

	s.evictLocked()
}

// DeleteEntry implements Store.
func (s *MemoryStore) DeleteEntry(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	se, ok := s.entries[key]
	if !ok {
		return
	}
	s.removeEntryLocked(se)
}

// PutBody implements Store. The body is reference counted lazily; writing
// the same ID twice reuses the existing backing array.
func (s *MemoryStore) PutBody(id string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.bodies[id]; ok {
		if !bytesEqual(b.data, body) {
			// Content-addressed IDs make a collision effectively
			// impossible; replacing would corrupt sharers, so ignore.
			return
		}
		return
	}
	s.bodies[id] = &storedBody{data: body, refs: 0}
	s.size += len(body)
}

// GetBody implements Store.
func (s *MemoryStore) GetBody(id string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bodies[id]
	if !ok {
		return nil, false
	}
	return b.data, true
}

// DeleteBody implements Store. Bodies are normally reclaimed via entry
// reference counting; an explicit delete is honored when unreferenced.
func (s *MemoryStore) DeleteBody(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bodies[id]
	if !ok || b.refs > 0 {
		return
	}
	s.size -= len(b.data)
	delete(s.bodies, id)
}

func (s *MemoryStore) removeEntryLocked(se *storedEntry) {
	s.size -= entrySize(se.entry)
	s.releaseEntryBodiesLocked(se.entry)
	s.lru.Remove(se.elem)
	delete(s.entries, se.entry.PrimaryKey)
}

func (s *MemoryStore) releaseEntryBodiesLocked(entry *Entry) {
	for id := range entryBodyIDs(entry) {
		b, ok := s.bodies[id]
		if !ok {
			continue
		}
		b.refs--
		if b.refs <= 0 {
			s.size -= len(b.data)
			delete(s.bodies, id)
		}
	}
}

// entryBodyIDs returns the set of distinct body IDs an entry references.
func entryBodyIDs(entry *Entry) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, v := range entry.Variants {
		if v.BodyID != "" {
			ids[v.BodyID] = struct{}{}
		}
	}
	return ids
}

func (s *MemoryStore) evictLocked() {
	for s.size > s.MaxBytes {
		elem := s.lru.Back()
		if elem == nil {
			return
		}
		entry := elem.Value.(*Entry)
		s.removeEntryLocked(s.entries[entry.PrimaryKey])
	}
}

// Len reports the number of cached entries (test/diagnostics helper).
func (s *MemoryStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Size reports the current resident size in bytes (test/diagnostics).
func (s *MemoryStore) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

// BodyRefs reports the reference count of a stored body (test helper).
func (s *MemoryStore) BodyRefs(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.bodies[id]; ok {
		return b.refs
	}
	return 0
}

func entrySize(entry *Entry) int {
	n := len(entry.PrimaryKey)
	for _, v := range entry.Variants {
		n += len(v.VaryKey)
		n += len(v.VaryNames) * 16
		for k, vs := range v.Header {
			n += len(k)
			for _, val := range vs {
				n += len(val)
			}
		}
	}
	return n
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
