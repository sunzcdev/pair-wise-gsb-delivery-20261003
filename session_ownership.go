package centrifuge

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrSessionOwnershipLost is returned by SessionOwnershipStore methods when the
// caller's lease generation is no longer the current one for the key: another
// connection took the session over, the lease expired, or it was released.
var ErrSessionOwnershipLost = errors.New("session ownership lost")

// SessionKey identifies a session ownership slot. A session is owned per
// user ID + device ID: a new connection from the same user on the same device
// (for example a mobile client that switched networks and reconnected while
// its old socket is still lingering on another node) takes the session over.
type SessionKey struct {
	UserID   string
	DeviceID string
}

// SessionOwner identifies the concrete connection holding a lease.
type SessionOwner struct {
	NodeID   string
	ClientID string
}

// SessionLease is an ownership grant returned by SessionOwnershipStore.Acquire.
// The Generation is a fencing token: it strictly increases with every
// successful Acquire for the same key, so any holder of an older generation
// is known to be superseded.
type SessionLease struct {
	Generation uint64
}

// SessionOwnershipStore stores session ownership leases. It is the source of
// truth for session takeover: which connection currently owns the session of
// a user ID + device ID pair, and which generation that ownership carries.
//
// The interface is deliberately shaped so a real shared store (for example
// Redis, with a Lua script performing the compare-and-update atomically) can
// maintain the guarantees below across nodes:
//
//   - Acquire is an atomic take-over: it increments the generation and
//     installs the caller as the owner. The moment it returns, every older
//     generation is invalid — no broadcast or notification is required to
//     prove the takeover happened.
//   - Renew and Release only succeed for the current generation and owner,
//     so a late disconnect of a previous owner can never disturb the lease
//     of the connection that superseded it.
//   - Validate reports whether a generation is still current; protected
//     operations are fenced on it.
//
// Implementations must be safe for concurrent use.
type SessionOwnershipStore interface {
	// Acquire takes ownership of key for owner and returns the new lease.
	// It always succeeds in taking over (the previous owner is superseded)
	// unless the store itself is unavailable or ctx is done.
	Acquire(ctx context.Context, key SessionKey, owner SessionOwner, ttl time.Duration) (SessionLease, error)
	// Renew extends the lease TTL. It returns ErrSessionOwnershipLost when
	// the lease generation or owner no longer matches the current entry
	// (including the case of an expired lease).
	Renew(ctx context.Context, key SessionKey, lease SessionLease, owner SessionOwner, ttl time.Duration) error
	// Validate returns nil when lease is the current, unexpired lease for
	// key, and ErrSessionOwnershipLost otherwise.
	Validate(ctx context.Context, key SessionKey, lease SessionLease) error
	// Release drops the lease, but only when generation and owner still
	// match the current entry — a release arriving late from a superseded
	// owner is a no-op.
	Release(ctx context.Context, key SessionKey, lease SessionLease, owner SessionOwner) error
}

// sessionOwnershipEntry is the stored state for one key. Released and expired
// entries are kept (with a zero expiresAt) so generations stay monotonic for
// the lifetime of the store.
type sessionOwnershipEntry struct {
	generation uint64
	owner      SessionOwner
	expiresAt  time.Time
}

func (e *sessionOwnershipEntry) valid(now time.Time) bool {
	return !e.expiresAt.IsZero() && now.Before(e.expiresAt)
}

// sessionOwnershipTable is the shared core of the in-process stores.
type sessionOwnershipTable struct {
	now     func() time.Time
	mu      sync.Mutex
	entries map[SessionKey]*sessionOwnershipEntry
}

func newSessionOwnershipTable(now func() time.Time) *sessionOwnershipTable {
	return &sessionOwnershipTable{
		now:     now,
		entries: make(map[SessionKey]*sessionOwnershipEntry),
	}
}

func (t *sessionOwnershipTable) acquire(_ context.Context, key SessionKey, owner SessionOwner, ttl time.Duration) (SessionLease, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[key]
	if !ok {
		e = &sessionOwnershipEntry{}
		t.entries[key] = e
	}
	e.generation++
	e.owner = owner
	e.expiresAt = t.now().Add(ttl)
	return SessionLease{Generation: e.generation}, nil
}

func (t *sessionOwnershipTable) renew(_ context.Context, key SessionKey, lease SessionLease, owner SessionOwner, ttl time.Duration) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[key]
	if !ok || e.generation != lease.Generation || e.owner != owner || !e.valid(t.now()) {
		return ErrSessionOwnershipLost
	}
	e.expiresAt = t.now().Add(ttl)
	return nil
}

func (t *sessionOwnershipTable) validate(_ context.Context, key SessionKey, lease SessionLease) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[key]
	if !ok || e.generation != lease.Generation || !e.valid(t.now()) {
		return ErrSessionOwnershipLost
	}
	return nil
}

func (t *sessionOwnershipTable) release(_ context.Context, key SessionKey, lease SessionLease, owner SessionOwner) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[key]
	if !ok || e.generation != lease.Generation || e.owner != owner {
		// Late release from a superseded owner: must not disturb the
		// current lease.
		return nil
	}
	e.expiresAt = time.Time{}
	return nil
}

// MemorySessionOwnershipStore is a SessionOwnershipStore keeping leases in the
// memory of a single process. It is the zero-dependency default for a
// single-node deployment: every node uses its own instance, so it provides no
// cross-node guarantees.
type MemorySessionOwnershipStore struct {
	table *sessionOwnershipTable
}

// NewMemorySessionOwnershipStore creates a MemorySessionOwnershipStore.
func NewMemorySessionOwnershipStore() *MemorySessionOwnershipStore {
	return &MemorySessionOwnershipStore{table: newSessionOwnershipTable(time.Now)}
}

// Acquire implements SessionOwnershipStore.
func (s *MemorySessionOwnershipStore) Acquire(ctx context.Context, key SessionKey, owner SessionOwner, ttl time.Duration) (SessionLease, error) {
	return s.table.acquire(ctx, key, owner, ttl)
}

// Renew implements SessionOwnershipStore.
func (s *MemorySessionOwnershipStore) Renew(ctx context.Context, key SessionKey, lease SessionLease, owner SessionOwner, ttl time.Duration) error {
	return s.table.renew(ctx, key, lease, owner, ttl)
}

// Validate implements SessionOwnershipStore.
func (s *MemorySessionOwnershipStore) Validate(ctx context.Context, key SessionKey, lease SessionLease) error {
	return s.table.validate(ctx, key, lease)
}

// Release implements SessionOwnershipStore.
func (s *MemorySessionOwnershipStore) Release(ctx context.Context, key SessionKey, lease SessionLease, owner SessionOwner) error {
	return s.table.release(ctx, key, lease, owner)
}

// SharedSessionOwnershipStore is an in-process SessionOwnershipStore meant to
// be shared by several Node instances: pass the same instance to every node.
// It provides the full atomic takeover semantics, which makes it suitable for
// multi-node tests and single-process multi-node deployments. A production
// deployment spanning several processes should instead provide a
// SessionOwnershipStore over a real shared store (e.g. Redis) with the same
// compare-and-update semantics.
type SharedSessionOwnershipStore struct {
	table *sessionOwnershipTable
}

// NewSharedSessionOwnershipStore creates a SharedSessionOwnershipStore.
func NewSharedSessionOwnershipStore() *SharedSessionOwnershipStore {
	return &SharedSessionOwnershipStore{table: newSessionOwnershipTable(time.Now)}
}

// Acquire implements SessionOwnershipStore.
func (s *SharedSessionOwnershipStore) Acquire(ctx context.Context, key SessionKey, owner SessionOwner, ttl time.Duration) (SessionLease, error) {
	return s.table.acquire(ctx, key, owner, ttl)
}

// Renew implements SessionOwnershipStore.
func (s *SharedSessionOwnershipStore) Renew(ctx context.Context, key SessionKey, lease SessionLease, owner SessionOwner, ttl time.Duration) error {
	return s.table.renew(ctx, key, lease, owner, ttl)
}

// Validate implements SessionOwnershipStore.
func (s *SharedSessionOwnershipStore) Validate(ctx context.Context, key SessionKey, lease SessionLease) error {
	return s.table.validate(ctx, key, lease)
}

// Release implements SessionOwnershipStore.
func (s *SharedSessionOwnershipStore) Release(ctx context.Context, key SessionKey, lease SessionLease, owner SessionOwner) error {
	return s.table.release(ctx, key, lease, owner)
}

// sessionOwnershipState is the per-connection lease state. It is installed
// once during connectCmd (before the client is registered in the hub) and
// never replaced afterwards.
type sessionOwnershipState struct {
	key     SessionKey
	owner   SessionOwner
	lease   SessionLease
	revoked atomic.Bool
	stopCh  chan struct{}
	once    sync.Once
}

// acquireSessionOwnership takes over the session for the connection's
// user ID + device ID. It is a no-op when the feature is not configured or
// the connection has no device ID. On store failure (including the acquire
// timeout) the connect is rejected with a temporary ErrorInternal.
func (c *Client) acquireSessionOwnership() error {
	cfg := c.node.config.SessionOwnership
	if cfg.Store == nil {
		return nil
	}
	c.mu.RLock()
	user := c.user
	deviceID := c.deviceID
	clientCtx := c.ctx
	c.mu.RUnlock()
	if user == "" || deviceID == "" {
		return nil
	}
	key := SessionKey{UserID: user, DeviceID: deviceID}
	owner := SessionOwner{NodeID: c.node.ID(), ClientID: c.uid}
	ctx, cancel := context.WithTimeout(clientCtx, cfg.acquireTimeout())
	lease, err := cfg.Store.Acquire(ctx, key, owner, cfg.leaseTTL())
	cancel()
	if err != nil {
		c.node.logger.log(newErrorLogEntry(err, "error acquiring session ownership", map[string]any{"client": c.uid, "user": user, "device": deviceID}))
		return ErrorInternal
	}
	so := &sessionOwnershipState{
		key:    key,
		owner:  owner,
		lease:  lease,
		stopCh: make(chan struct{}),
	}
	c.mu.Lock()
	c.sessionOwnership = so
	c.mu.Unlock()
	go c.sessionRenewLoop(so)
	// Best-effort local fast path: close superseded connections of the same
	// user + device living on this node. The fencing never depends on this —
	// it is enforced by the store — and remote nodes are covered by their own
	// renew/validate paths, so the old socket may be closed with a delay.
	c.kickSessionPredecessors(so)
	return nil
}

// kickSessionPredecessors disconnects other connections of this node which
// hold a session for the same user ID + device ID.
func (c *Client) kickSessionPredecessors(so *sessionOwnershipState) {
	for uid, other := range c.node.hub.UserConnections(so.key.UserID) {
		if uid == c.uid {
			continue
		}
		other.mu.RLock()
		otherSO := other.sessionOwnership
		other.mu.RUnlock()
		if otherSO != nil && otherSO.key.DeviceID == so.key.DeviceID {
			other.Disconnect(DisconnectSessionTakenOver)
		}
	}
}

// sessionRenewLoop extends the lease until the connection closes or a renew
// fails. A renew failure has a defined fail-closed behavior: the connection
// is revoked immediately (protected operations start failing) and then
// disconnected — with DisconnectSessionTakenOver when the store reports the
// ownership as lost, and with DisconnectForceReconnect on a store error, so
// the client reconnects and acquires a fresh lease.
func (c *Client) sessionRenewLoop(so *sessionOwnershipState) {
	cfg := c.node.config.SessionOwnership
	ticker := time.NewTicker(cfg.renewInterval())
	defer ticker.Stop()
	for {
		select {
		case <-so.stopCh:
			return
		case <-ticker.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), cfg.acquireTimeout())
		err := cfg.Store.Renew(ctx, so.key, so.lease, so.owner, cfg.leaseTTL())
		cancel()
		if err == nil {
			continue
		}
		so.revoked.Store(true)
		if errors.Is(err, ErrSessionOwnershipLost) {
			c.Disconnect(DisconnectSessionTakenOver)
		} else {
			c.node.logger.log(newErrorLogEntry(err, "error renewing session ownership", map[string]any{"client": c.uid, "user": so.key.UserID, "device": so.key.DeviceID}))
			c.Disconnect(DisconnectForceReconnect)
		}
		return
	}
}

// stopSessionOwnership stops the renew loop and releases the lease. The
// release is generation- and owner-matched by the store, so a close arriving
// late — after another connection already took the session over — is a no-op
// there. Called from close().
func (c *Client) stopSessionOwnership() {
	c.mu.RLock()
	so := c.sessionOwnership
	c.mu.RUnlock()
	if so == nil {
		return
	}
	so.once.Do(func() { close(so.stopCh) })
	cfg := c.node.config.SessionOwnership
	ctx, cancel := context.WithTimeout(context.Background(), cfg.acquireTimeout())
	_ = cfg.Store.Release(ctx, so.key, so.lease, so.owner)
	cancel()
}

// checkSessionOwnership fences protected operations (subscribe, publish) on
// the session generation. It is a no-op for connections without a session
// lease. A connection whose generation was superseded gets ErrorSessionRevoked
// and is disconnected; a store failure fails closed with a temporary
// ErrorInternal.
func (c *Client) checkSessionOwnership() error {
	c.mu.RLock()
	so := c.sessionOwnership
	clientCtx := c.ctx
	c.mu.RUnlock()
	if so == nil {
		return nil
	}
	if so.revoked.Load() {
		return ErrorSessionRevoked
	}
	cfg := c.node.config.SessionOwnership
	ctx, cancel := context.WithTimeout(clientCtx, cfg.acquireTimeout())
	err := cfg.Store.Validate(ctx, so.key, so.lease)
	cancel()
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrSessionOwnershipLost) {
		so.revoked.Store(true)
		c.Disconnect(DisconnectSessionTakenOver)
		return ErrorSessionRevoked
	}
	c.node.logger.log(newErrorLogEntry(err, "error validating session ownership", map[string]any{"client": c.uid, "user": so.key.UserID, "device": so.key.DeviceID}))
	return ErrorInternal
}
