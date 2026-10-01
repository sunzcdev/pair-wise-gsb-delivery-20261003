package memstore

import (
	"context"
	"sync"
	"time"
)

type item struct {
	object     []byte
	expiration int64
}

// MemStore represents the session store.
type MemStore struct {
	items       map[string]item
	mu          sync.RWMutex
	stopCleanup chan bool
}

// New returns a new MemStore instance, with a background cleanup goroutine that
// runs every minute to remove expired session data.
func New() *MemStore {
	return NewWithCleanupInterval(time.Minute)
}

// NewWithCleanupInterval returns a new MemStore instance. The cleanupInterval
// parameter controls how frequently expired session data is removed by the
// background cleanup goroutine. Setting it to 0 prevents the cleanup goroutine
// from running (i.e. expired sessions will not be removed).
func NewWithCleanupInterval(cleanupInterval time.Duration) *MemStore {
	m := &MemStore{
		items: make(map[string]item),
	}

	if cleanupInterval > 0 {
		m.stopCleanup = make(chan bool)
		go m.startCleanup(cleanupInterval)
	}

	return m
}

// Find returns the data for a given session token from the MemStore instance.
// If the session token is not found or is expired, the returned exists flag will
// be set to false.
func (m *MemStore) Find(token string) ([]byte, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	item, found := m.items[token]
	if !found {
		return nil, false, nil
	}

	if time.Now().UnixNano() > item.expiration {
		return nil, false, nil
	}

	return item.object, true, nil
}

// Commit adds a session token and data to the MemStore instance with the given
// expiry time. If the session token already exists, then the data and expiry
// time are updated.
func (m *MemStore) Commit(token string, b []byte, expiry time.Time) error {
	m.mu.Lock()
	m.items[token] = item{
		object:     b,
		expiration: expiry.UnixNano(),
	}
	m.mu.Unlock()

	return nil
}

// Flash atomically retrieves the data for the given session token, invokes fn
// with it, and stores the data returned by fn while preserving the existing
// expiry. The entire read-modify-write is performed while holding the write
// lock, so concurrent Flash, Find and Commit calls for the same token are
// serialized and a one-time value can only ever be handed out once.
func (m *MemStore) Flash(token string, fn func(b []byte, found bool) (newB []byte, err error)) error {
	return m.flash(context.Background(), token, fn)
}

// FlashCtx is the same as Flash, except that it takes a context.Context.
func (m *MemStore) FlashCtx(ctx context.Context, token string, fn func(b []byte, found bool) (newB []byte, err error)) error {
	return m.flash(ctx, token, fn)
}

func (m *MemStore) flash(_ context.Context, token string, fn func(b []byte, found bool) (newB []byte, err error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	current, found := m.items[token]
	if !found || time.Now().UnixNano() > current.expiration {
		_, err := fn(nil, false)
		return err
	}

	newB, err := fn(current.object, true)
	if err != nil {
		return err
	}

	current.object = newB
	m.items[token] = current
	return nil
}

// Delete removes a session token and corresponding data from the MemStore
// instance.
func (m *MemStore) Delete(token string) error {
	m.mu.Lock()
	delete(m.items, token)
	m.mu.Unlock()

	return nil
}

// All returns a map containing the token and data for all active (i.e.
// not expired) sessions.
func (m *MemStore) All() (map[string][]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var mm = make(map[string][]byte)

	for token, item := range m.items {
		if item.expiration > time.Now().UnixNano() {
			mm[token] = item.object
		}
	}

	return mm, nil
}

func (m *MemStore) startCleanup(interval time.Duration) {
	ticker := time.NewTicker(interval)
	for {
		select {
		case <-ticker.C:
			m.deleteExpired()
		case <-m.stopCleanup:
			ticker.Stop()
			return
		}
	}
}

// StopCleanup terminates the background cleanup goroutine for the MemStore
// instance. It's rare to terminate this; generally MemStore instances and
// their cleanup goroutines are intended to be long-lived and run for the lifetime
// of your application.
//
// There may be occasions though when your use of the MemStore is transient.
// An example is creating a new MemStore instance in a test function. In this
// scenario, the cleanup goroutine (which will run forever) will prevent the
// MemStore object from being garbage collected even after the test function
// has finished. You can prevent this by manually calling StopCleanup.
func (m *MemStore) StopCleanup() {
	if m.stopCleanup != nil {
		m.stopCleanup <- true
	}
}

func (m *MemStore) deleteExpired() {
	now := time.Now().UnixNano()
	m.mu.Lock()
	for token, item := range m.items {
		if now > item.expiration {
			delete(m.items, token)
		}
	}
	m.mu.Unlock()
}
