package goproxy

import (
	"container/list"
	"sync"
)

// memoryStore is the default bounded CacheStore. It keeps resources in an
// LRU list ordered by Get/Put access and evicts the least recently used
// entries once capacity is exceeded.
type memoryStore struct {
	mu       sync.Mutex
	capacity int
	ll       *list.List
	items    map[string]*list.Element
}

type memoryEntry struct {
	key      string
	resource *CachedResource
}

func newMemoryStore(capacity int) *memoryStore {
	return &memoryStore{
		capacity: capacity,
		ll:       list.New(),
		items:    make(map[string]*list.Element),
	}
}

// NewMemoryStore returns the default bounded, LRU-ordered in-memory
// CacheStore with room for capacity URL resources. It is useful when
// constructing a CacheConfig while only overriding other settings.
func NewMemoryStore(capacity int) CacheStore {
	if capacity <= 0 {
		capacity = defaultCacheMaxResources
	}
	return newMemoryStore(capacity)
}

func (s *memoryStore) Get(key string) *CachedResource {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[key]; ok {
		s.ll.MoveToFront(el)
		return el.Value.(*memoryEntry).resource
	}
	return nil
}

func (s *memoryStore) Put(key string, resource *CachedResource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[key]; ok {
		s.ll.MoveToFront(el)
		el.Value.(*memoryEntry).resource = resource
		return
	}
	el := s.ll.PushFront(&memoryEntry{key: key, resource: resource})
	s.items[key] = el
	for s.ll.Len() > s.capacity {
		oldest := s.ll.Back()
		if oldest == nil {
			break
		}
		s.ll.Remove(oldest)
		delete(s.items, oldest.Value.(*memoryEntry).key)
	}
}

func (s *memoryStore) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[key]; ok {
		s.ll.Remove(el)
		delete(s.items, key)
	}
}

// Len reports the number of cached resources (for tests/diagnostics).
func (s *memoryStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ll.Len()
}
