// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2022 mochi-mqtt, mochi-co
// SPDX-FileContributor: mochi-co

package scram

import "sync"

// CredentialStore is the backend interface used by the hook to retrieve the
// salt, iteration count, and verification credentials for a user. The backend
// never provides a plaintext password.
type CredentialStore interface {
	// GetCredentials returns the SCRAM credentials for a username, or false
	// if the user is unknown.
	GetCredentials(username string) (Credentials, bool)
}

// MapStore is a simple in-memory CredentialStore.
type MapStore struct {
	users map[string]Credentials
	mu    sync.RWMutex
}

// NewMapStore returns an empty in-memory credential store.
func NewMapStore() *MapStore {
	return &MapStore{
		users: make(map[string]Credentials),
	}
}

// Add inserts or replaces the credentials for a username.
func (s *MapStore) Add(username string, creds Credentials) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[username] = creds
}

// Delete removes a username from the store.
func (s *MapStore) Delete(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.users, username)
}

// GetCredentials returns the SCRAM credentials for a username, or false if
// the user is unknown.
func (s *MapStore) GetCredentials(username string) (Credentials, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	creds, ok := s.users[username]
	return creds, ok
}
