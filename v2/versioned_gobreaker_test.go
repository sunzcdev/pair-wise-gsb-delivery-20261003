package gobreaker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ VersionedDataStore = (*fakeVersionedStore)(nil)

// fakeVersionedStore is an in-memory VersionedDataStore.
// beforeCAS runs inside the compare-and-set critical section and can mutate
// the stored record, which lets tests inject a precisely timed conflict
// without any real backend or flaky scheduling.
type fakeVersionedStore struct {
	mu        sync.Mutex
	data      map[string][]byte
	versions  map[string]Version
	next      uint64
	beforeCAS func(key string, expect Version, data []byte)
}

func newFakeVersionedStore() *fakeVersionedStore {
	return &fakeVersionedStore{
		data:     map[string][]byte{},
		versions: map[string]Version{},
	}
}

func (s *fakeVersionedStore) Lock(string) error   { return nil }
func (s *fakeVersionedStore) Unlock(string) error { return nil }

func (s *fakeVersionedStore) GetData(key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.data[key]), nil
}

func (s *fakeVersionedStore) SetData(key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = bytes.Clone(data)
	return nil
}

func (s *fakeVersionedStore) GetVersionedData(key string) ([]byte, Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.data[key]
	if !ok {
		return nil, nil, nil
	}
	return bytes.Clone(data), bytes.Clone(s.versions[key]), nil
}

func (s *fakeVersionedStore) CompareAndSetData(key string, expect Version, data []byte) (Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.beforeCAS != nil {
		s.beforeCAS(key, bytes.Clone(expect), bytes.Clone(data))
	}

	current, exists := s.versions[key]
	switch {
	case !exists && expect != nil:
		return nil, ErrVersionConflict
	case exists && !bytes.Equal(current, expect):
		return nil, ErrVersionConflict
	}

	s.next++
	version := Version(fmt.Appendf(nil, "v%d", s.next))
	s.versions[key] = bytes.Clone(version)
	s.data[key] = bytes.Clone(data)
	return bytes.Clone(version), nil
}

// bumpLocked republishes identical bytes under a new version. Callers must
// hold mu (e.g. from beforeCAS).
func (s *fakeVersionedStore) bumpLocked(key string) {
	s.next++
	s.versions[key] = Version(fmt.Appendf(nil, "v%d", s.next))
}

// stateLocked decodes the shared state. Callers must hold mu.
func (s *fakeVersionedStore) stateLocked(key string) SharedState {
	var state SharedState
	if err := json.Unmarshal(s.data[key], &state); err != nil {
		panic(err)
	}
	return state
}

// putStateLocked writes the shared state under a new version. Callers must
// hold mu.
func (s *fakeVersionedStore) putStateLocked(key string, state SharedState) {
	data, err := json.Marshal(state)
	if err != nil {
		panic(err)
	}
	s.next++
	s.versions[key] = Version(fmt.Appendf(nil, "v%d", s.next))
	s.data[key] = data
}

func versionedSettings(name string) Settings {
	return Settings{
		Name:    name,
		Timeout: time.Second,
	}
}

func newVersionedDCB(t *testing.T, store VersionedDataStore, name string) *DistributedCircuitBreaker[any] {
	t.Helper()
	dcb, err := NewDistributedCircuitBreaker[any](store, versionedSettings(name))
	require.NoError(t, err)
	return dcb
}

func newDCBOn(t *testing.T, store SharedDataStore, name string) *DistributedCircuitBreaker[any] {
	t.Helper()
	dcb, err := NewDistributedCircuitBreaker[any](store, versionedSettings(name))
	require.NoError(t, err)
	return dcb
}

// TestVersionedStoreCASSemantics checks the fake store's compare-and-set
// contract directly.
func TestVersionedStoreCASSemantics(t *testing.T) {
	store := newFakeVersionedStore()
	key := "key"

	v1, err := store.CompareAndSetData(key, nil, []byte("a"))
	require.NoError(t, err)
	assert.NotEmpty(t, v1)

	_, err = store.CompareAndSetData(key, nil, []byte("b"))
	assert.ErrorIs(t, err, ErrVersionConflict)

	_, err = store.CompareAndSetData(key, Version("stale"), []byte("b"))
	assert.ErrorIs(t, err, ErrVersionConflict)

	v2, err := store.CompareAndSetData(key, v1, []byte("b"))
	require.NoError(t, err)
	assert.NotEqual(t, string(v1), string(v2))

	data, version, err := store.GetVersionedData(key)
	require.NoError(t, err)
	assert.Equal(t, []byte("b"), data)
	assert.True(t, bytes.Equal(v2, version))

	missing, missingVersion, err := store.GetVersionedData("missing")
	require.NoError(t, err)
	assert.Empty(t, missing)
	assert.Nil(t, missingVersion)
}

// TestVersionedExecuteMergesAfterConflict is the core guarantee: when two
// instances each complete one request and the second write loses a
// compare-and-set, only the state merge is retried. The shared counters must
// reflect both requests (2), not just the last writer's (1), and each business
// function must run exactly once.
func TestVersionedExecuteMergesAfterConflict(t *testing.T) {
	store := newFakeVersionedStore()
	dcb1 := newVersionedDCB(t, store, "merge-conflict")
	dcb2 := newVersionedDCB(t, store, "merge-conflict")

	var runs1, runs2 int32
	_, err := dcb1.Execute(func() (any, error) {
		atomic.AddInt32(&runs1, 1)
		return "one", nil
	})
	require.NoError(t, err)

	// Force exactly one conflict for dcb2's commit: a concurrent writer
	// republishes identical bytes under a newer version (pure version bump,
	// no counters), which invalidates dcb2's expected version.
	conflicted := false
	store.beforeCAS = func(key string, _ Version, _ []byte) {
		if key != dcb2.sharedStateKey() || conflicted {
			return
		}
		conflicted = true
		store.bumpLocked(key)
	}

	result, err := dcb2.Execute(func() (any, error) {
		atomic.AddInt32(&runs2, 1)
		return "two", nil
	})
	require.NoError(t, err)
	assert.Equal(t, "two", result)

	assert.Equal(t, int32(1), atomic.LoadInt32(&runs1))
	assert.Equal(t, int32(1), atomic.LoadInt32(&runs2),
		"business function must not be re-run after a conflict")
	assert.True(t, conflicted, "test must actually inject a conflict")

	shared, err := dcb1.getSharedState()
	require.NoError(t, err)
	assert.Equal(t, uint32(2), shared.Counts.Requests,
		"both requests must survive the conflict, not just one")
	assert.Equal(t, uint32(2), shared.Counts.TotalSuccesses)
	assert.Equal(t, uint32(2), shared.Counts.ConsecutiveSuccesses)
}

// TestVersionedExecuteMergesConcurrentOutcome injects a conflict that also
// carries another instance's completed request: nothing is lost and the local
// business function still runs once.
func TestVersionedExecuteMergesConcurrentOutcome(t *testing.T) {
	store := newFakeVersionedStore()
	dcb1 := newVersionedDCB(t, store, "merge-event")
	dcb2 := newVersionedDCB(t, store, "merge-event")

	var runs int32
	conflicted := false
	store.beforeCAS = func(key string, _ Version, _ []byte) {
		if key != dcb2.sharedStateKey() || conflicted {
			return
		}
		conflicted = true

		// Another instance completes a successful request concurrently.
		state := store.stateLocked(key)
		state.Counts.Requests++
		state.Counts.TotalSuccesses++
		state.Counts.ConsecutiveSuccesses++
		state.Buckets[0].Requests++
		state.Buckets[0].TotalSuccesses++
		state.Buckets[0].ConsecutiveSuccesses++
		store.putStateLocked(key, state)
	}

	_, err := dcb2.Execute(func() (any, error) {
		atomic.AddInt32(&runs, 1)
		return nil, nil
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&runs))

	shared, err := dcb1.getSharedState()
	require.NoError(t, err)
	expected := Counts{
		Requests:             2,
		TotalSuccesses:       2,
		ConsecutiveSuccesses: 2,
	}
	assert.Equal(t, expected, shared.Counts)
	assert.Equal(t, expected, shared.Buckets[0])
}

// TestVersionedInitializationRace: two instances creating the state at the
// same time must not both succeed. The compare-and-set loser adopts the
// winner's state instead of returning an error or overwriting it.
func TestVersionedInitializationRace(t *testing.T) {
	store := newFakeVersionedStore()
	key := "gobreaker:state:init-race"

	// Build the winner's initial state on a separate store so the hook itself
	// never re-enters store.mu.
	winnerState := newVersionedDCB(t, newFakeVersionedStore(), "init-race").extract()
	winnerState.Generation = 777

	injected := false
	store.beforeCAS = func(gotKey string, expect Version, _ []byte) {
		if gotKey != key || injected || expect != nil {
			return
		}
		injected = true

		// The peer instance wins the race and publishes its own state.
		store.putStateLocked(key, winnerState)
	}

	dcb, err := NewDistributedCircuitBreaker[any](store, versionedSettings("init-race"))
	require.NoError(t, err, "initialization loser must adopt the winner state")
	assert.True(t, injected, "test must actually inject the race")

	shared, err := dcb.getSharedState()
	require.NoError(t, err)
	assert.Equal(t, uint64(777), shared.Generation,
		"the winner's state must be adopted, not the local one")

	_, version, err := store.GetVersionedData(key)
	require.NoError(t, err)
	assert.Equal(t, "v1", string(version))
}

// TestVersionedStaleGenerationCompletion is dropped: a request admitted in an
// old generation (here, closed generation 1) must not be counted after the
// shared state has already advanced to a newer generation (open generation 5).
func TestVersionedStaleGenerationCompletion(t *testing.T) {
	store := newFakeVersionedStore()
	dcb := newVersionedDCB(t, store, "stale-gen")

	started := make(chan struct{})
	proceed := make(chan struct{})
	done := make(chan error, 1)
	var runs int32

	go func() {
		_, err := dcb.Execute(func() (any, error) {
			close(started)
			<-proceed
			atomic.AddInt32(&runs, 1)
			return nil, nil
		})
		done <- err
	}()

	<-started

	// While the request is in flight, another instance advances the shared
	// state to a newer generation.
	key := dcb.sharedStateKey()
	store.mu.Lock()
	store.putStateLocked(key, SharedState{
		State:      StateOpen,
		Generation: 5,
		Buckets:    []Counts{{}},
		Start:      time.Now(),
		Expiry:     time.Now().Add(time.Hour),
	})
	store.mu.Unlock()

	close(proceed)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("execute did not return")
	}

	assert.Equal(t, int32(1), atomic.LoadInt32(&runs))
	shared, err := dcb.getSharedState()
	require.NoError(t, err)
	assert.Equal(t, uint64(5), shared.Generation)
	assert.Equal(t, StateOpen, shared.State)
	assert.Equal(t, Counts{}, shared.Counts,
		"a completion from an old generation must be dropped")
}

// TestVersionedCorruptSharedState treats undecodable stored data distinctly
// from a missing record: construction and execute surface ErrCorruptSharedState.
func TestVersionedCorruptSharedState(t *testing.T) {
	store := newFakeVersionedStore()
	key := "gobreaker:state:corrupt"
	require.NoError(t, store.SetData(key, []byte("{not json")))

	_, err := NewDistributedCircuitBreaker[any](store, versionedSettings("corrupt"))
	assert.ErrorIs(t, err, ErrCorruptSharedState)

	// Corruption appearing after construction is reported as well.
	store2 := newFakeVersionedStore()
	dcb := newVersionedDCB(t, store2, "corrupt-later")
	require.NoError(t, store2.SetData(dcb.sharedStateKey(), []byte("{not json")))
	_, err = dcb.Execute(func() (any, error) {
		t.Fatal("business function must not run with corrupt shared state")
		return nil, nil
	})
	assert.ErrorIs(t, err, ErrCorruptSharedState)
}

// TestVersionedStateRollsTime advances open state to half-open through the
// State method using a compare-and-set write.
func TestVersionedStateRollsTime(t *testing.T) {
	store := newFakeVersionedStore()
	dcb := newVersionedDCB(t, store, "state-roll")

	key := dcb.sharedStateKey()
	store.mu.Lock()
	shared := store.stateLocked(key)
	shared.State = StateOpen
	shared.Expiry = time.Now().Add(-time.Nanosecond)
	store.putStateLocked(key, shared)
	store.mu.Unlock()

	got, err := dcb.State()
	require.NoError(t, err)
	assert.Equal(t, StateHalfOpen, got)
}

// TestLockedStorePathStillWorks proves a plain SharedDataStore keeps using the
// legacy lock/get/set path and still aggregates two instances' requests.
func TestLockedStorePathStillWorks(t *testing.T) {
	store := NewMockStore()
	dcb1 := newDCBOn(t, store, "legacy-path")
	dcb2 := newDCBOn(t, store, "legacy-path")

	require.NoError(t, successRequest(dcb1))
	require.NoError(t, successRequest(dcb2))

	shared, err := dcb1.getSharedState()
	require.NoError(t, err)
	assert.Equal(t, uint32(2), shared.Counts.Requests)
	assert.Equal(t, uint32(2), shared.Counts.TotalSuccesses)
}

// TestVersionedConcurrentRequestsNeverLoseCounts drives several instances
// concurrently so compare-and-set commits genuinely contend. No request
// outcome may be lost and business functions run exactly once each.
func TestVersionedConcurrentRequestsNeverLoseCounts(t *testing.T) {
	store := newFakeVersionedStore()

	const instances = 6
	const requestsPerInstance = 10

	dcbs := make([]*DistributedCircuitBreaker[any], instances)
	for i := range dcbs {
		dcbs[i] = newVersionedDCB(t, store, "concurrent")
	}

	var wg sync.WaitGroup
	var totalRuns int32
	for i := 0; i < instances; i++ {
		wg.Add(1)
		go func(dcb *DistributedCircuitBreaker[any]) {
			defer wg.Done()
			for j := 0; j < requestsPerInstance; j++ {
				_, err := dcb.Execute(func() (any, error) {
					atomic.AddInt32(&totalRuns, 1)
					return nil, nil
				})
				assert.NoError(t, err)
			}
		}(dcbs[i])
	}
	wg.Wait()

	expected := uint32(instances * requestsPerInstance)
	assert.Equal(t, int32(expected), atomic.LoadInt32(&totalRuns))

	shared, err := dcbs[0].getSharedState()
	require.NoError(t, err)
	assert.Equal(t, expected, shared.Counts.Requests)
	assert.Equal(t, expected, shared.Counts.TotalSuccesses)
	assert.Equal(t, expected, shared.Counts.ConsecutiveSuccesses)
}
