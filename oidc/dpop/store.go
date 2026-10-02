package dpop

import (
	"context"
	"sync"
	"time"
)

// ReplayStore records proof identifiers so that a DPoP proof cannot be
// presented twice.
//
// Add MUST perform an atomic check-and-set of jti: it inserts jti with the
// given time to live only if it is not already present, and returns
// [ErrReplay] (or an error wrapping it) when jti was already recorded. The
// atomicity requirement applies across all service instances, so
// implementations backed by a shared system should use a conditional write
// such as Redis "SET key value NX PX ttl", a unique insert, or a database
// transaction rather than a separate read-then-write.
//
// Expired entries may be discarded at any time; they must never cause Add to
// return [ErrReplay]. Cancelling ctx must abort the operation.
type ReplayStore interface {
	// Add atomically records jti for ttl and reports whether it was newly
	// inserted. It returns false together with [ErrReplay] when jti is
	// already live in the store.
	Add(ctx context.Context, jti string, ttl time.Duration) (bool, error)
}

// MemoryStore is a process-local [ReplayStore] backed by a map. It is safe
// for concurrent use and is suitable for a single service instance or for
// tests. Multiple service instances do not share replay state with a
// MemoryStore; deploy them behind a shared [ReplayStore] implementation.
type MemoryStore struct {
	mu      sync.Mutex
	entries map[string]time.Time
	now     func() time.Time
}

// NewMemoryStore returns an empty process-local [ReplayStore].
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		entries: map[string]time.Time{},
		now:     time.Now,
	}
}

// Add implements [ReplayStore].
func (s *MemoryStore) Add(ctx context.Context, jti string, ttl time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()

	// Opportunistically drop expired entries so the map cannot grow without
	// bound. The check below is what enforces replay protection.
	for id, expires := range s.entries {
		if !expires.After(now) {
			delete(s.entries, id)
		}
	}

	if expires, ok := s.entries[jti]; ok && expires.After(now) {
		return false, ErrReplay
	}
	s.entries[jti] = now.Add(ttl)
	return true, nil
}
