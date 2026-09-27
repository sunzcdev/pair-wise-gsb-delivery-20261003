// Copyright IBM Corp. 2013, 2026
// SPDX-License-Identifier: MPL-2.0

package memberlist

import (
	"errors"
	"math"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFileIncarnationStore_LoadReserve(t *testing.T) {
	dir := t.TempDir()

	s, err := NewFileIncarnationStore(dir, "node-a")
	require.NoError(t, err)

	high, err := s.Load()
	require.NoError(t, err)
	require.Equal(t, uint32(0), high)

	require.NoError(t, s.Reserve(50))
	// Reservations are monotonic; a lower value is a no-op.
	require.NoError(t, s.Reserve(40))

	// A new process opening the same node file sees the reservation.
	reopened, err := NewFileIncarnationStore(dir, "node-a")
	require.NoError(t, err)
	high, err = reopened.Load()
	require.NoError(t, err)
	require.Equal(t, uint32(50), high)
}

func TestFileIncarnationStore_PerNodeFiles(t *testing.T) {
	dir := t.TempDir()

	a, err := NewFileIncarnationStore(dir, "node-a")
	require.NoError(t, err)
	b, err := NewFileIncarnationStore(dir, "node-b")
	require.NoError(t, err)

	require.NoError(t, a.Reserve(100))

	// Each node has its own counter; nodes never share a single file.
	high, err := b.Load()
	require.NoError(t, err)
	require.Equal(t, uint32(0), high)

	require.NoError(t, b.Reserve(7))
	high, err = a.Load()
	require.NoError(t, err)
	require.Equal(t, uint32(100), high)
}

func TestFileIncarnationStore_InvalidNodeName(t *testing.T) {
	dir := t.TempDir()

	_, err := NewFileIncarnationStore(dir, "")
	require.Error(t, err)

	_, err = NewFileIncarnationStore(dir, filepath.Join("other", "node"))
	require.Error(t, err)
}

func TestFileIncarnationStore_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node-a.incarnation"), []byte("garbage"), 0o644))

	_, err := NewFileIncarnationStore(dir, "node-a")
	require.Error(t, err)
}

// failingStore reports explicit errors from Load and Reserve.
type failingStore struct {
	loadErr    error
	reserveErr error
}

func (s *failingStore) Load() (uint32, error) { return 0, s.loadErr }
func (s *failingStore) Reserve(uint32) error  { return s.reserveErr }

// countingStore records reservations in memory and counts writes.
type countingStore struct {
	mu       sync.Mutex
	high     uint32
	reserves int
}

func (s *countingStore) Load() (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.high, nil
}

func (s *countingStore) Reserve(high uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.high = high
	s.reserves++
	return nil
}

func (s *countingStore) reserveCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reserves
}

func TestIncarnation_InMemoryDefaultUnchanged(t *testing.T) {
	m := GetMemberlist(t, nil)
	defer func() { _ = m.Shutdown() }()

	for want := uint32(1); want <= 3; want++ {
		inc, err := m.nextIncarnation()
		require.NoError(t, err)
		require.Equal(t, want, inc)
	}

	// The historical in-memory behavior wraps around instead of erroring.
	m.incarnation.Store(math.MaxUint32)
	inc, err := m.nextIncarnation()
	require.NoError(t, err)
	require.Equal(t, uint32(0), inc)
}

func TestIncarnation_LoadFailure(t *testing.T) {
	loadErr := errors.New("disk gone")
	c := testConfig(t)
	c.IncarnationStore = &failingStore{loadErr: loadErr}

	_, err := Create(c)
	require.ErrorIs(t, err, loadErr)
}

func TestIncarnation_ReserveFailure(t *testing.T) {
	reserveErr := errors.New("disk full")
	c := testConfig(t)
	c.IncarnationStore = &failingStore{reserveErr: reserveErr}

	// The very first incarnation must be reserved before it is published,
	// so a reservation failure fails node creation explicitly.
	_, err := Create(c)
	require.ErrorIs(t, err, reserveErr)
}

func TestIncarnation_BatchReservation(t *testing.T) {
	store := &countingStore{}
	m := GetMemberlist(t, func(c *Config) {
		c.IncarnationStore = store
		c.IncarnationReserveBatch = 10
	})
	defer func() { _ = m.Shutdown() }()

	// The first bump reserves a whole batch in a single write.
	inc, err := m.nextIncarnation()
	require.NoError(t, err)
	require.Equal(t, uint32(1), inc)
	require.Equal(t, 1, store.reserveCalls())

	// Bumps within the reserved batch do not write again.
	for i := uint32(2); i <= 11; i++ {
		inc, err := m.nextIncarnation()
		require.NoError(t, err)
		require.Equal(t, i, inc)
	}
	require.Equal(t, 1, store.reserveCalls())

	// Crossing the reserved high-water mark triggers the next batch.
	inc, err = m.nextIncarnation()
	require.NoError(t, err)
	require.Equal(t, uint32(12), inc)
	require.Equal(t, 2, store.reserveCalls())
}

func TestIncarnation_Exhaustion(t *testing.T) {
	m := GetMemberlist(t, func(c *Config) {
		c.IncarnationStore = &countingStore{}
		c.IncarnationReserveBatch = 10
	})
	defer func() { _ = m.Shutdown() }()

	// The counter itself cannot advance past MaxUint32.
	m.incarnation.Store(math.MaxUint32)
	m.incarnationReserved.Store(math.MaxUint32)
	_, err := m.nextIncarnation()
	require.ErrorIs(t, err, ErrIncarnationExhausted)

	// There is headroom for the counter, but not for another batch
	// reservation, so the bump fails explicitly instead of publishing an
	// unreserved incarnation.
	m.incarnation.Store(math.MaxUint32 - 5)
	m.incarnationReserved.Store(math.MaxUint32 - 5)
	_, err = m.nextIncarnation()
	require.ErrorIs(t, err, ErrIncarnationExhausted)
}

func TestIncarnation_RestartIgnoresStalePackets(t *testing.T) {
	dir := t.TempDir()
	name := "restart-node"

	newConfig := func(meta []byte) *Config {
		c := testConfig(t)
		c.Name = name
		store, err := NewFileIncarnationStore(dir, name)
		require.NoError(t, err)
		c.IncarnationStore = store
		c.Delegate = &MockDelegate{meta: meta}
		return c
	}

	// First process publishes a few incarnations, then dies without
	// leaving (a fast, unclean restart).
	m1, err := Create(newConfig([]byte("old-meta")))
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		_, err := m1.nextIncarnation()
		require.NoError(t, err)
	}
	oldInc := m1.incarnation.Load()

	// Stash the packets the old process may still have circulating.
	oldAlive := &alive{
		Incarnation: oldInc,
		Node:        name,
		Meta:        []byte("old-meta"),
		Vsn:         m1.config.BuildVsnArray(),
	}
	oldSuspect := &suspect{Incarnation: oldInc, Node: name, From: "peer"}
	oldDead := &dead{Incarnation: oldInc, Node: name, From: "peer"}
	require.NoError(t, m1.Shutdown())

	// The node restarts with the same name and the same store directory.
	m2, err := Create(newConfig([]byte("new-meta")))
	require.NoError(t, err)
	defer func() { _ = m2.Shutdown() }()

	m2.nodeLock.RLock()
	restarted := *m2.nodeMap[name]
	m2.nodeLock.RUnlock()
	require.Greater(t, restarted.Incarnation, oldInc)
	require.Equal(t, []byte("new-meta"), restarted.Meta)
	require.Equal(t, StateAlive, restarted.State)

	// Address the stale alive packet at the restarted process, as if the
	// node came back on the same address.
	oldAlive.Addr = net.IP(restarted.Addr)
	oldAlive.Port = restarted.Port

	// Replay the stale packets out of order: dead, suspect, then alive.
	m2.deadNode(oldDead)
	m2.suspectNode(oldSuspect)
	m2.aliveNode(oldAlive, nil, false)

	// The old packets must not override the new metadata or liveness.
	m2.nodeLock.RLock()
	final := *m2.nodeMap[name]
	m2.nodeLock.RUnlock()
	require.Equal(t, StateAlive, final.State)
	require.Equal(t, []byte("new-meta"), final.Meta)
	require.Equal(t, restarted.Incarnation, final.Incarnation)
}
