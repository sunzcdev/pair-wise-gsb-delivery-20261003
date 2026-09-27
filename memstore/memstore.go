package memstore

import (
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
	versions    map[string]uint64
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
		items:    make(map[string]item),
		versions: make(map[string]uint64),
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
	m.versions[token]++
	m.mu.Unlock()

	return nil
}

// FindVersioned returns the data and version for a given session token from
// the MemStore instance. If the session token is not found or is expired, the
// returned exists flag will be set to false and the version will be 0.
func (m *MemStore) FindVersioned(token string) ([]byte, uint64, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	item, found := m.items[token]
	if !found {
		return nil, 0, false, nil
	}

	if time.Now().UnixNano() > item.expiration {
		return nil, 0, false, nil
	}

	return item.object, m.versions[token], true, nil
}

// CommitVersioned adds a session token and data to the MemStore instance with
// the given expiry time, but only if the version of the data currently stored
// for the token equals expectedVersion (0 means the token must not currently
// exist). The returned committed flag reports whether the commit was applied.
func (m *MemStore) CommitVersioned(token string, b []byte, expiry time.Time, expectedVersion uint64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var currentVersion uint64
	if it, found := m.items[token]; found && time.Now().UnixNano() <= it.expiration {
		currentVersion = m.versions[token]
	}

	if currentVersion != expectedVersion {
		return false, nil
	}

	m.items[token] = item{
		object:     b,
		expiration: expiry.UnixNano(),
	}
	m.versions[token] = currentVersion + 1

	return true, nil
}

// Delete removes a session token and corresponding data from the MemStore
// instance.
func (m *MemStore) Delete(token string) error {
	m.mu.Lock()
	delete(m.items, token)
	delete(m.versions, token)
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
			delete(m.versions, token)
		}
	}
	m.mu.Unlock()
}
