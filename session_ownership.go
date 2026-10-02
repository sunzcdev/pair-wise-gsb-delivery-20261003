package centrifuge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrSessionOwnershipLost is returned by SessionOwnershipStore.Renew when the
// store no longer shows the caller as the owner of the key: either a newer
// generation took the session over, or the lease expired before it was renewed.
var ErrSessionOwnershipLost = errors.New("centrifuge: session ownership lost")

// SessionOwnershipStore coordinates session ownership between connections of
// the same user and device, possibly living on different Centrifuge nodes.
//
// The store is the single source of truth for who may act for a
// (user ID, device ID) pair: a connection is eligible for protected operations
// (subscribe, publish) only while the store shows its generation as the current
// one. Broadcasting a "kick this user" message between nodes is NOT a
// substitute: a message can be lost or delayed, while a generation check
// against the shared record proves whether the takeover happened.
//
// Implementations must provide these guarantees:
//
//   - Generations are monotonic per key: Acquire returns a generation greater
//     than any generation previously issued for the key, even after the
//     previous owner released its lease or the lease expired. A real shared
//     store can implement this with an atomic increment (e.g. Redis INCR)
//     which doubles as a fencing token.
//   - Acquire is a takeover: it always succeeds for a live store, even if the
//     previous owner's lease has not expired yet. This is what lets a mobile
//     client reconnecting after a network switch take its session back from a
//     half-open connection still hanging on another node.
//   - Renew and Release are guarded by (owner ID, generation): a stale owner
//     can neither extend nor remove the record of a newer owner. In
//     particular a late Release of an old generation must be a no-op.
//   - Validate reports whether (owner ID, generation) is the current,
//     unexpired owner of the key.
type SessionOwnershipStore interface {
	// Acquire takes ownership of the key for ownerID and returns the new
	// generation. The lease must be renewed with Renew before ttl passes,
	// otherwise it expires and Validate starts reporting the owner out.
	Acquire(ctx context.Context, key string, ownerID string, ttl time.Duration) (uint64, error)
	// Renew extends the lease of (ownerID, generation). It returns
	// ErrSessionOwnershipLost when the record no longer shows this exact
	// owner and generation as the current, unexpired owner.
	Renew(ctx context.Context, key string, ownerID string, generation uint64, ttl time.Duration) error
	// Release gives the lease up. It is a no-op unless (ownerID, generation)
	// matches the current record, so a late release from a previous
	// generation can never remove a newer owner's record.
	Release(ctx context.Context, key string, ownerID string, generation uint64) error
	// Validate reports whether (ownerID, generation) is the current,
	// unexpired owner of the key.
	Validate(ctx context.Context, key string, ownerID string, generation uint64) (bool, error)
}

// sessionOwnershipKey builds the store key for a (user ID, device ID) pair.
// The user ID is length-prefixed so no pair of distinct pairs can collide.
func sessionOwnershipKey(userID string, deviceID string) string {
	return fmt.Sprintf("centrifuge:session:%d:%s%s", len(userID), userID, deviceID)
}

// SessionOwnershipConfig configures session takeover by user ID and device ID.
// When Store is set and a connection authenticates with Credentials.DeviceID
// set, the connection acquires a session ownership lease during connect and
// keeps renewing it. A newer connection with the same user ID and device ID
// takes the session over with a higher generation; the previous connection
// then fails protected operations (subscribe, publish) with
// ErrorSessionTakenOver and is disconnected with DisconnectSessionTakenOver
// once its next renewal reaches the store.
type SessionOwnershipConfig struct {
	// Store is the ownership store. Nil Store disables the feature.
	Store SessionOwnershipStore
	// AcquireTimeout bounds the ownership acquisition during connect. When it
	// is exceeded the connect fails with DisconnectSessionOwnershipUnavailable.
	// Defaults to 5 seconds.
	AcquireTimeout time.Duration
	// LeaseTTL is the ownership lease duration. If the owning node crashes and
	// stops renewing, the lease becomes invalid after this much time.
	// Defaults to 15 seconds.
	LeaseTTL time.Duration
	// RenewInterval is how often the owning connection renews its lease.
	// Defaults to LeaseTTL / 3.
	RenewInterval time.Duration
}

func (c SessionOwnershipConfig) acquireTimeout() time.Duration {
	if c.AcquireTimeout > 0 {
		return c.AcquireTimeout
	}
	return 5 * time.Second
}

func (c SessionOwnershipConfig) leaseTTL() time.Duration {
	if c.LeaseTTL > 0 {
		return c.LeaseTTL
	}
	return 15 * time.Second
}

func (c SessionOwnershipConfig) renewInterval() time.Duration {
	if c.RenewInterval > 0 {
		return c.RenewInterval
	}
	return c.leaseTTL() / 3
}

// sessionOwnershipRecord is the single record a store keeps per key. A record
// is never deleted: a released or expired record stays as a tombstone so that
// generations remain monotonic per key.
type sessionOwnershipRecord struct {
	ownerID    string
	generation uint64
	expiresAt  time.Time
}

func (r sessionOwnershipRecord) live(now time.Time) bool {
	return !r.expiresAt.IsZero() && now.Before(r.expiresAt)
}

// MemorySessionOwnershipStore is a SessionOwnershipStore which keeps records
// in process memory. It provides the full takeover guarantees for a single
// node, but several nodes each using their own MemorySessionOwnershipStore do
// not see each other: use a shared implementation (or a real external store)
// for multi-node deployments.
type MemorySessionOwnershipStore struct {
	mu      sync.Mutex
	records map[string]sessionOwnershipRecord
	now     func() time.Time
}

// NewMemorySessionOwnershipStore creates a new MemorySessionOwnershipStore.
func NewMemorySessionOwnershipStore() *MemorySessionOwnershipStore {
	return &MemorySessionOwnershipStore{
		records: make(map[string]sessionOwnershipRecord),
		now:     time.Now,
	}
}

// Acquire implements SessionOwnershipStore.Acquire.
func (s *MemorySessionOwnershipStore) Acquire(_ context.Context, key string, ownerID string, ttl time.Duration) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.records[key]
	record.ownerID = ownerID
	record.generation++
	record.expiresAt = s.now().Add(ttl)
	s.records[key] = record
	return record.generation, nil
}

// Renew implements SessionOwnershipStore.Renew.
func (s *MemorySessionOwnershipStore) Renew(_ context.Context, key string, ownerID string, generation uint64, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	if !ok || record.ownerID != ownerID || record.generation != generation || !record.live(s.now()) {
		return ErrSessionOwnershipLost
	}
	record.expiresAt = s.now().Add(ttl)
	s.records[key] = record
	return nil
}

// Release implements SessionOwnershipStore.Release.
func (s *MemorySessionOwnershipStore) Release(_ context.Context, key string, ownerID string, generation uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	if !ok || record.ownerID != ownerID || record.generation != generation {
		// Stale or foreign release: never touch a newer owner's record.
		return nil
	}
	// Keep the record as a tombstone so the next Acquire still gets a higher
	// generation.
	record.expiresAt = time.Time{}
	s.records[key] = record
	return nil
}

// Validate implements SessionOwnershipStore.Validate.
func (s *MemorySessionOwnershipStore) Validate(_ context.Context, key string, ownerID string, generation uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	if !ok {
		return false, nil
	}
	return record.ownerID == ownerID && record.generation == generation && record.live(s.now()), nil
}

// SharedSessionOwnershipStore is a SessionOwnershipStore implementation meant
// to be shared by several Node instances, standing in for a real external
// store (like Redis) in multi-node tests. All nodes configured with the same
// instance observe one consistent ownership record per key, so the takeover
// guarantees hold across nodes.
//
// It additionally provides hooks to make distributed scenarios deterministic
// in tests: synchronizing concurrent acquisitions, failing acquisitions and
// renewals, and controlling time.
type SharedSessionOwnershipStore struct {
	inner *MemorySessionOwnershipStore

	hooksMu     sync.Mutex
	acquireHook func(ctx context.Context, key string) error
	renewErr    error
	validateErr error
}

// NewSharedSessionOwnershipStore creates a new SharedSessionOwnershipStore.
func NewSharedSessionOwnershipStore() *SharedSessionOwnershipStore {
	return &SharedSessionOwnershipStore{
		inner: NewMemorySessionOwnershipStore(),
	}
}

// SetAcquireHook installs a hook called at the start of every Acquire, before
// the record is touched. A non-nil error returned by the hook fails the
// acquisition. Pass nil to remove the hook. Tests use it to synchronize
// concurrent acquisitions from several nodes or to simulate store timeouts
// (e.g. block on ctx.Done() and return ctx.Err()).
func (s *SharedSessionOwnershipStore) SetAcquireHook(hook func(ctx context.Context, key string) error) {
	s.hooksMu.Lock()
	defer s.hooksMu.Unlock()
	s.acquireHook = hook
}

// SetRenewError makes every subsequent Renew call fail with the given error,
// simulating an ownership store outage. Pass nil to restore normal behavior.
func (s *SharedSessionOwnershipStore) SetRenewError(err error) {
	s.hooksMu.Lock()
	defer s.hooksMu.Unlock()
	s.renewErr = err
}

// SetValidateError makes every subsequent Validate call fail with the given
// error. Pass nil to restore normal behavior.
func (s *SharedSessionOwnershipStore) SetValidateError(err error) {
	s.hooksMu.Lock()
	defer s.hooksMu.Unlock()
	s.validateErr = err
}

// SetClock replaces the clock the store uses for lease expiry. It allows
// deterministic tests of lease expiration (e.g. the owner crash scenario)
// without sleeping. The clock must be set before the store is used
// concurrently.
func (s *SharedSessionOwnershipStore) SetClock(now func() time.Time) {
	s.inner.mu.Lock()
	defer s.inner.mu.Unlock()
	s.inner.now = now
}

// Acquire implements SessionOwnershipStore.Acquire.
func (s *SharedSessionOwnershipStore) Acquire(ctx context.Context, key string, ownerID string, ttl time.Duration) (uint64, error) {
	s.hooksMu.Lock()
	hook := s.acquireHook
	s.hooksMu.Unlock()
	if hook != nil {
		if err := hook(ctx, key); err != nil {
			return 0, err
		}
	}
	return s.inner.Acquire(ctx, key, ownerID, ttl)
}

// Renew implements SessionOwnershipStore.Renew.
func (s *SharedSessionOwnershipStore) Renew(ctx context.Context, key string, ownerID string, generation uint64, ttl time.Duration) error {
	s.hooksMu.Lock()
	err := s.renewErr
	s.hooksMu.Unlock()
	if err != nil {
		return err
	}
	return s.inner.Renew(ctx, key, ownerID, generation, ttl)
}

// Release implements SessionOwnershipStore.Release.
func (s *SharedSessionOwnershipStore) Release(ctx context.Context, key string, ownerID string, generation uint64) error {
	return s.inner.Release(ctx, key, ownerID, generation)
}

// Validate implements SessionOwnershipStore.Validate.
func (s *SharedSessionOwnershipStore) Validate(ctx context.Context, key string, ownerID string, generation uint64) (bool, error) {
	s.hooksMu.Lock()
	err := s.validateErr
	s.hooksMu.Unlock()
	if err != nil {
		return false, err
	}
	return s.inner.Validate(ctx, key, ownerID, generation)
}
