package gobreaker

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// FakeVersionedStore implements VersionedSharedDataStore in memory and allows
// tests to inject version conflicts and competing writes precisely.
type FakeVersionedStore struct {
	mu        sync.Mutex
	data      map[string][]byte
	versions  map[string]uint64
	conflicts map[string]int
	rivals    map[string][][]byte
}

func NewFakeVersionedStore() *FakeVersionedStore {
	return &FakeVersionedStore{
		data:      map[string][]byte{},
		versions:  map[string]uint64{},
		conflicts: map[string]int{},
		rivals:    map[string][][]byte{},
	}
}

func (s *FakeVersionedStore) GetDataWithVersion(name string) ([]byte, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.data[name], s.versions[name], nil
}

func (s *FakeVersionedStore) CompareAndSwapData(name string, data []byte, version uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conflicts[name] > 0 {
		s.conflicts[name]--
		return ErrStateConflict
	}
	if queue := s.rivals[name]; len(queue) > 0 {
		s.versions[name]++
		s.data[name] = queue[0]
		s.rivals[name] = queue[1:]
		return ErrStateConflict
	}
	if s.versions[name] != version {
		return ErrStateConflict
	}

	s.versions[name]++
	s.data[name] = append([]byte(nil), data...)
	return nil
}

// InjectConflicts makes the next n CompareAndSwapData calls on name fail
// with ErrStateConflict without modifying the stored data.
func (s *FakeVersionedStore) InjectConflicts(name string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.conflicts[name] += n
}

// InjectRivalWrite makes the next CompareAndSwapData call on name fail with
// ErrStateConflict after storing data, simulating a competing instance that
// won a concurrent write.
func (s *FakeVersionedStore) InjectRivalWrite(name string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.rivals[name] = append(s.rivals[name], data)
}

// SetRaw stores raw data under name, bypassing serialization.
func (s *FakeVersionedStore) SetRaw(name string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.versions[name]++
	s.data[name] = data
}

func (s *FakeVersionedStore) sharedState(t *testing.T, name string) SharedState {
	t.Helper()

	data, _, err := s.GetDataWithVersion("gobreaker:state:" + name)
	assert.NoError(t, err)

	var state SharedState
	assert.NoError(t, json.Unmarshal(data, &state))
	return state
}

func versionedTestSettings(name string) Settings {
	return Settings{
		Name:        name,
		MaxRequests: 3,
		Interval:    time.Second,
		Timeout:     2 * time.Second,
		ReadyToTrip: func(counts Counts) bool {
			return counts.ConsecutiveFailures > 5
		},
	}
}

func TestDistributedCircuitBreakerVersionedStoreMergesConcurrentCounts(t *testing.T) {
	store := NewFakeVersionedStore()
	settings := versionedTestSettings("MergeTest")

	dcb1, err := NewDistributedCircuitBreakerWithVersionedStore[any](store, settings)
	assert.NoError(t, err)
	dcb2, err := NewDistributedCircuitBreakerWithVersionedStore[any](store, settings)
	assert.NoError(t, err)

	runs1, runs2 := 0, 0
	_, err = dcb1.Execute(func() (any, error) {
		runs1++
		// dcb2 completes a full request while dcb1's request is in flight,
		// so dcb1's compare-and-swap write conflicts and must merge.
		_, err2 := dcb2.Execute(func() (any, error) {
			runs2++
			return nil, nil
		})
		assert.NoError(t, err2)
		return nil, nil
	})
	assert.NoError(t, err)
	assert.Equal(t, 1, runs1, "the request must run exactly once")
	assert.Equal(t, 1, runs2, "the request must run exactly once")

	shared := store.sharedState(t, "MergeTest")
	assert.Equal(t, uint32(2), shared.Counts.Requests)
	assert.Equal(t, uint32(2), shared.Counts.TotalSuccesses)
}

func TestDistributedCircuitBreakerVersionedStoreConflictRetriesMergeOnly(t *testing.T) {
	store := NewFakeVersionedStore()
	dcb, err := NewDistributedCircuitBreakerWithVersionedStore[any](store, versionedTestSettings("RetryTest"))
	assert.NoError(t, err)

	store.InjectConflicts("gobreaker:state:RetryTest", 3)

	runs := 0
	_, err = dcb.Execute(func() (any, error) {
		runs++
		return nil, nil
	})
	assert.NoError(t, err)
	assert.Equal(t, 1, runs, "conflicts must not re-run the business function")
	assert.Equal(t, 0, store.conflicts["gobreaker:state:RetryTest"], "all injected conflicts should be consumed")

	shared := store.sharedState(t, "RetryTest")
	assert.Equal(t, uint32(1), shared.Counts.Requests)
	assert.Equal(t, uint32(1), shared.Counts.TotalSuccesses)
}

func TestDistributedCircuitBreakerVersionedStoreInitializationRace(t *testing.T) {
	settings := versionedTestSettings("InitRace")

	// Build a valid competing initial state with another instance.
	rivalStore := NewFakeVersionedStore()
	rival, err := NewDistributedCircuitBreakerWithVersionedStore[any](rivalStore, settings)
	assert.NoError(t, err)
	rivalData, err := json.Marshal(rival.extract())
	assert.NoError(t, err)

	store := NewFakeVersionedStore()
	store.InjectRivalWrite("gobreaker:state:InitRace", rivalData)

	dcb, err := NewDistributedCircuitBreakerWithVersionedStore[any](store, settings)
	assert.NoError(t, err, "losing the initialization race must not fail")

	// The competing state is adopted instead of being overwritten.
	assert.Equal(t, rival.generation, dcb.generation)
	assert.True(t, rival.start.Equal(dcb.start), "start time should be adopted from the rival state")

	// The adopted state is usable.
	_, err = dcb.Execute(func() (any, error) { return nil, nil })
	assert.NoError(t, err)
	shared := store.sharedState(t, "InitRace")
	assert.Equal(t, uint32(1), shared.Counts.Requests)
}

func TestDistributedCircuitBreakerVersionedStoreCorruptedData(t *testing.T) {
	t.Run("Constructor", func(t *testing.T) {
		store := NewFakeVersionedStore()
		store.SetRaw("gobreaker:state:Corrupt", []byte("{broken"))

		_, err := NewDistributedCircuitBreakerWithVersionedStore[any](store, versionedTestSettings("Corrupt"))
		assert.ErrorIs(t, err, ErrInvalidSharedState)
	})

	t.Run("ExecuteAndState", func(t *testing.T) {
		store := NewFakeVersionedStore()
		dcb, err := NewDistributedCircuitBreakerWithVersionedStore[any](store, versionedTestSettings("Corrupt2"))
		assert.NoError(t, err)

		store.SetRaw("gobreaker:state:Corrupt2", []byte("{broken"))

		runs := 0
		_, err = dcb.Execute(func() (any, error) {
			runs++
			return nil, nil
		})
		assert.ErrorIs(t, err, ErrInvalidSharedState)
		assert.Equal(t, 0, runs, "corrupted state must reject before running the request")

		_, err = dcb.State()
		assert.ErrorIs(t, err, ErrInvalidSharedState)
	})
}

func TestDistributedCircuitBreakerVersionedStoreStaleGenerationOutcome(t *testing.T) {
	store := NewFakeVersionedStore()
	settings := versionedTestSettings("StaleGen")
	settings.ReadyToTrip = func(counts Counts) bool {
		return counts.ConsecutiveFailures >= 2
	}

	dcb1, err := NewDistributedCircuitBreakerWithVersionedStore[any](store, settings)
	assert.NoError(t, err)
	dcb2, err := NewDistributedCircuitBreakerWithVersionedStore[any](store, settings)
	assert.NoError(t, err)

	_, err = dcb1.Execute(func() (any, error) {
		// Trip the circuit from the other instance while dcb1's request is
		// in flight, moving the shared state to a new generation.
		for i := 0; i < 2; i++ {
			_, _ = dcb2.Execute(func() (any, error) { return nil, errors.New("fail") })
		}
		return nil, nil
	})
	assert.NoError(t, err)

	shared := store.sharedState(t, "StaleGen")
	assert.Equal(t, StateOpen, shared.State)
	// dcb1's success completed against the old generation and must not be
	// counted as a success in the tripped generation.
	assert.Equal(t, uint32(0), shared.Counts.TotalSuccesses)
	assert.Equal(t, uint32(0), shared.Counts.ConsecutiveSuccesses)
}

func TestDistributedCircuitBreakerVersionedStoreStateTransitions(t *testing.T) {
	store := NewFakeVersionedStore()
	settings := versionedTestSettings("Transitions")
	settings.ReadyToTrip = func(counts Counts) bool {
		return counts.ConsecutiveFailures >= 2
	}

	dcb, err := NewDistributedCircuitBreakerWithVersionedStore[any](store, settings)
	assert.NoError(t, err)

	state, err := dcb.State()
	assert.NoError(t, err)
	assert.Equal(t, StateClosed, state)

	for i := 0; i < 2; i++ {
		_, _ = dcb.Execute(func() (any, error) { return nil, errors.New("fail") })
	}
	state, err = dcb.State()
	assert.NoError(t, err)
	assert.Equal(t, StateOpen, state)

	_, err = dcb.Execute(func() (any, error) { return nil, nil })
	assert.Equal(t, ErrOpenState, err)
}
