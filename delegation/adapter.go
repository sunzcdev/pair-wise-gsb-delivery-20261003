package delegation

import "sync"

// Adapter persists delegation grants. Implementations are expected to provide
// durable storage; the built-in MemoryAdapter keeps grants in process memory.
//
// SaveGrant is called for every created grant. RevokeGrant is called when a
// grant is withdrawn. LoadGrants is called once when the manager is built and
// may return grants previously saved by any manager instance.
type Adapter interface {
	SaveGrant(grant Grant) error
	RevokeGrant(id string) error
	LoadGrants() ([]Grant, error)
}

// MemoryAdapter keeps grants in memory. It is safe for concurrent use and is
// primarily intended for tests and ephemeral deployments.
type MemoryAdapter struct {
	mu     sync.RWMutex
	grants map[string]Grant
}

// NewMemoryAdapter creates an empty in-memory adapter.
func NewMemoryAdapter() *MemoryAdapter {
	return &MemoryAdapter{grants: map[string]Grant{}}
}

// SaveGrant stores a copy of the grant.
func (a *MemoryAdapter) SaveGrant(grant Grant) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.grants[grant.ID] = grant.clone()
	return nil
}

// RevokeGrant marks the stored grant as revoked. It returns ErrGrantNotFound
// when no grant with the id exists.
func (a *MemoryAdapter) RevokeGrant(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	grant, ok := a.grants[id]
	if !ok {
		return ErrGrantNotFound
	}
	grant.Revoked = true
	a.grants[id] = grant
	return nil
}

// LoadGrants returns copies of all stored grants.
func (a *MemoryAdapter) LoadGrants() ([]Grant, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	grants := make([]Grant, 0, len(a.grants))
	for _, grant := range a.grants {
		grants = append(grants, grant.clone())
	}
	return grants, nil
}
