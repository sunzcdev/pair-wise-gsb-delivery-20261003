// Copyright IBM Corp. 2013, 2026
// SPDX-License-Identifier: MPL-2.0

package memberlist

import (
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// countingStore is an in-memory IncarnationStore that records calls and
// can be programmed to fail.
type countingStore struct {
	mu       sync.Mutex
	value    uint32
	loads    int
	stores   int
	loadErr  error
	storeErr error
}

func (s *countingStore) Load() (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	return s.value, s.loadErr
}

func (s *countingStore) Store(inc uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stores++
	if s.storeErr != nil {
		return s.storeErr
	}
	s.value = inc
	return nil
}

func (s *countingStore) stats() (loads, stores int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads, s.stores
}

func incarnationTestConfig(tb testing.TB, name string, store IncarnationStore) *Config {
	tb.Helper()
	c := testConfig(tb)
	c.Name = name
	c.IncarnationStore = store
	return c
}

func TestFileIncarnationStore_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node1.incarnation")

	store, err := NewFileIncarnationStore(path, "node1")
	require.NoError(t, err)

	// A missing file starts at zero.
	inc, err := store.Load()
	require.NoError(t, err)
	require.Equal(t, uint32(0), inc)

	require.NoError(t, store.Store(42))

	// A new instance over the same file sees the persisted value.
	reopened, err := NewFileIncarnationStore(path, "node1")
	require.NoError(t, err)
	inc, err = reopened.Load()
	require.NoError(t, err)
	require.Equal(t, uint32(42), inc)
}

func TestFileIncarnationStore_RejectsSharedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.incarnation")

	store1, err := NewFileIncarnationStore(path, "node1")
	require.NoError(t, err)
	require.NoError(t, store1.Store(7))

	// Another node must not reuse the same counter file.
	store2, err := NewFileIncarnationStore(path, "node2")
	require.NoError(t, err)
	_, err = store2.Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "its own incarnation file")
}

func TestFileIncarnationStore_RequiresNodeAndPath(t *testing.T) {
	_, err := NewFileIncarnationStore("", "node1")
	require.Error(t, err)

	_, err = NewFileIncarnationStore(filepath.Join(t.TempDir(), "x"), "")
	require.Error(t, err)
}

func TestMemberlist_IncarnationPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "node1.incarnation")
	delegate := &MockDelegate{}
	delegate.setMeta([]byte("meta-v1"))

	newStore := func() IncarnationStore {
		store, err := NewFileIncarnationStore(storePath, "node1")
		require.NoError(t, err)
		return store
	}

	// First process: burn some incarnations and save a batch of its
	// packets, as if they were still gossiping around the network.
	c1 := incarnationTestConfig(t, "node1", newStore())
	c1.Delegate = delegate
	m1, err := Create(c1)
	require.NoError(t, err)

	for i := 0; i < 5; i++ {
		_, err := m1.nextIncarnation()
		require.NoError(t, err)
	}
	oldInc := m1.incarnation.Load()
	oldAlive := alive{
		Incarnation: oldInc,
		Node:        "node1",
		Meta:        []byte("meta-v1"),
		Vsn:         c1.BuildVsnArray(),
	}
	oldSuspect := suspect{Incarnation: oldInc, Node: "node1", From: "peer"}
	oldDead := dead{Incarnation: oldInc, Node: "node1", From: "peer"}
	require.NoError(t, m1.Shutdown())

	// Second process: same name, same store, fresh metadata.
	delegate.setMeta([]byte("meta-v2"))
	c2 := incarnationTestConfig(t, "node1", newStore())
	c2.Delegate = delegate
	m2, err := Create(c2)
	require.NoError(t, err)
	defer m2.Shutdown()

	// The restarted process must not publish a regressed incarnation.
	newInc := m2.incarnation.Load()
	require.Greater(t, newInc, oldInc)

	self := m2.LocalNode()
	require.Equal(t, []byte("meta-v2"), self.Meta)

	// Replay the saved packets out of order, addressed to the node's
	// current identity. None of them may override the fresh state.
	oldAlive.Addr = self.Addr
	oldAlive.Port = self.Port
	m2.deadNode(&oldDead)
	m2.aliveNode(&oldAlive, nil, false)
	m2.suspectNode(&oldSuspect)

	self = m2.LocalNode()
	require.Equal(t, []byte("meta-v2"), self.Meta)
	require.Equal(t, StateAlive, self.State)
	require.Equal(t, newInc, m2.incarnation.Load())

	m2.nodeLock.RLock()
	state := m2.nodeMap["node1"]
	require.Equal(t, StateAlive, state.State)
	require.Equal(t, newInc, state.Incarnation)
	m2.nodeLock.RUnlock()
}

func TestMemberlist_IncarnationReserveBatch(t *testing.T) {
	store := &countingStore{}
	c := incarnationTestConfig(t, "node1", store)
	c.IncarnationReserveBatch = 10
	m, err := Create(c)
	require.NoError(t, err)
	defer m.Shutdown()

	// Create reserves the first batch once; setAlive stays within it.
	_, stores := store.stats()
	require.Equal(t, 1, stores)
	require.Equal(t, uint32(1), m.incarnation.Load())

	// The rest of the batch is served from memory without disk writes.
	for i := uint32(2); i <= 10; i++ {
		inc, err := m.nextIncarnation()
		require.NoError(t, err)
		require.Equal(t, i, inc)
	}
	_, stores = store.stats()
	require.Equal(t, 1, stores)

	// Crossing the reservation persists the next batch.
	inc, err := m.nextIncarnation()
	require.NoError(t, err)
	require.Equal(t, uint32(11), inc)
	_, stores = store.stats()
	require.Equal(t, 2, stores)
	require.Equal(t, uint32(20), store.value)
}

func TestMemberlist_IncarnationReserveFailure(t *testing.T) {
	store := &countingStore{storeErr: errors.New("disk on fire")}
	c := incarnationTestConfig(t, "node1", store)
	_, err := Create(c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to persist incarnation reservation")
}

func TestMemberlist_IncarnationLoadFailure(t *testing.T) {
	store := &countingStore{loadErr: errors.New("corrupt file")}
	c := incarnationTestConfig(t, "node1", store)
	_, err := Create(c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to load persisted incarnation")
}

func TestMemberlist_IncarnationExhaustedAtStartup(t *testing.T) {
	store := &countingStore{value: math.MaxUint32 - 1}
	c := incarnationTestConfig(t, "node1", store)
	_, err := Create(c)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrIncarnationExhausted)
}

func TestMemberlist_IncarnationExhaustedAtRuntime(t *testing.T) {
	store := &countingStore{value: math.MaxUint32 - 2}
	c := incarnationTestConfig(t, "node1", store)
	c.IncarnationReserveBatch = 2
	m, err := Create(c)
	require.NoError(t, err)
	defer m.Shutdown()

	// setAlive consumed one incarnation; one more fits the reservation.
	inc, err := m.nextIncarnation()
	require.NoError(t, err)
	require.Equal(t, uint32(math.MaxUint32), inc)

	// The uint32 space is now exhausted and must report so explicitly.
	_, err = m.nextIncarnation()
	require.ErrorIs(t, err, ErrIncarnationExhausted)

	_, err = m.skipIncarnation(1)
	require.ErrorIs(t, err, ErrIncarnationExhausted)
}

func TestMemberlist_IncarnationMemoryOnlyUnchanged(t *testing.T) {
	c := testConfig(t)
	m, err := Create(c)
	require.NoError(t, err)
	defer m.Shutdown()

	// Without a store the incarnation starts from scratch as before.
	require.Equal(t, uint32(1), m.incarnation.Load())

	inc, err := m.nextIncarnation()
	require.NoError(t, err)
	require.Equal(t, uint32(2), inc)

	inc, err = m.skipIncarnation(10)
	require.NoError(t, err)
	require.Equal(t, uint32(12), inc)
}
