package gobreaker

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrNoSharedStore is returned when there is no shared store.
	ErrNoSharedStore = errors.New("no shared store")
	// ErrNoSharedState is returned when there is no shared state.
	ErrNoSharedState = errors.New("no shared state")
	// ErrStateConflict is returned by VersionedSharedDataStore when a
	// compare-and-swap write fails because the stored version has changed.
	ErrStateConflict = errors.New("shared state version conflict")
	// ErrInvalidSharedState is returned when the data in the shared store
	// cannot be decoded into a SharedState.
	ErrInvalidSharedState = errors.New("invalid shared state")
)

// SharedState represents the shared state of DistributedCircuitBreaker.
type SharedState struct {
	State      State     `json:"state"`
	Generation uint64    `json:"generation"`
	Age        uint64    `json:"age"`
	Counts     Counts    `json:"counts"`
	Buckets    []Counts  `json:"buckets"`
	Start      time.Time `json:"start"`
	Expiry     time.Time `json:"expiry"`
}

// SharedDataStore stores the shared state of DistributedCircuitBreaker.
type SharedDataStore interface {
	Lock(name string) error
	Unlock(name string) error
	GetData(name string) ([]byte, error)
	SetData(name string, data []byte) error
}

// VersionedSharedDataStore stores the shared state of DistributedCircuitBreaker
// with optimistic concurrency control. It allows DistributedCircuitBreaker to
// update the shared state without holding a cross-process lock.
type VersionedSharedDataStore interface {
	// GetDataWithVersion returns the data stored under name and its version.
	// It returns nil data and version 0 when the key does not exist.
	GetDataWithVersion(name string) (data []byte, version uint64, err error)
	// CompareAndSwapData stores data under name only if the current version
	// equals the given version. It returns ErrStateConflict when the
	// versions differ.
	CompareAndSwapData(name string, data []byte, version uint64) error
}

// DistributedCircuitBreaker extends CircuitBreaker with SharedDataStore.
type DistributedCircuitBreaker[T any] struct {
	*CircuitBreaker[T]
	store  SharedDataStore
	vstore VersionedSharedDataStore
}

// NewDistributedCircuitBreaker returns a new DistributedCircuitBreaker.
func NewDistributedCircuitBreaker[T any](store SharedDataStore, settings Settings) (dcb *DistributedCircuitBreaker[T], err error) {
	if store == nil {
		return nil, ErrNoSharedStore
	}

	dcb = &DistributedCircuitBreaker[T]{
		CircuitBreaker: NewCircuitBreaker[T](settings),
		store:          store,
	}

	err = dcb.lock()
	if err != nil {
		return nil, err
	}
	defer func() {
		e := dcb.unlock()
		if err == nil {
			err = e
		}
	}()

	_, err = dcb.getSharedState()
	if err == ErrNoSharedState {
		err = dcb.setSharedState(dcb.extract())
	}
	if err != nil {
		return nil, err
	}

	return dcb, nil
}

// NewDistributedCircuitBreakerWithVersionedStore returns a new
// DistributedCircuitBreaker that updates the shared state with
// compare-and-swap writes instead of cross-process locks.
func NewDistributedCircuitBreakerWithVersionedStore[T any](vstore VersionedSharedDataStore, settings Settings) (*DistributedCircuitBreaker[T], error) {
	if vstore == nil {
		return nil, ErrNoSharedStore
	}

	dcb := &DistributedCircuitBreaker[T]{
		CircuitBreaker: NewCircuitBreaker[T](settings),
		vstore:         vstore,
	}

	err := dcb.initSharedState()
	if err != nil {
		return nil, err
	}

	return dcb, nil
}

// initSharedState loads the shared state, initializing it when absent.
// When a concurrent instance wins the initialization race, its state is
// adopted instead of being overwritten.
func (dcb *DistributedCircuitBreaker[T]) initSharedState() error {
	shared, version, err := dcb.getSharedStateVersioned()
	if err == ErrNoSharedState {
		err = dcb.compareAndSwapSharedState(dcb.extract(), version)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrStateConflict) {
			return err
		}
		shared, _, err = dcb.getSharedStateVersioned()
	}
	if err != nil {
		return err
	}

	dcb.inject(shared)
	return nil
}

const (
	mutexTimeout  = 5 * time.Second
	mutexWaitTime = 500 * time.Millisecond
)

func (dcb *DistributedCircuitBreaker[T]) mutexKey() string {
	return "gobreaker:mutex:" + dcb.name
}

func (dcb *DistributedCircuitBreaker[T]) lock() error {
	if dcb.store == nil {
		return ErrNoSharedStore
	}

	var err error
	expiry := time.Now().Add(mutexTimeout)
	for time.Now().Before(expiry) {
		err = dcb.store.Lock(dcb.mutexKey())
		if err == nil {
			return nil
		}

		time.Sleep(mutexWaitTime)
	}
	return err
}

func (dcb *DistributedCircuitBreaker[T]) unlock() error {
	if dcb.store == nil {
		return ErrNoSharedStore
	}

	return dcb.store.Unlock(dcb.mutexKey())
}

func (dcb *DistributedCircuitBreaker[T]) sharedStateKey() string {
	return "gobreaker:state:" + dcb.name
}

func (dcb *DistributedCircuitBreaker[T]) getSharedState() (SharedState, error) {
	var state SharedState
	if dcb.store == nil {
		return state, ErrNoSharedStore
	}

	data, err := dcb.store.GetData(dcb.sharedStateKey())
	if len(data) == 0 {
		return state, ErrNoSharedState
	} else if err != nil {
		return state, err
	}

	err = json.Unmarshal(data, &state)
	return state, err
}

func (dcb *DistributedCircuitBreaker[T]) setSharedState(state SharedState) error {
	if dcb.store == nil {
		return ErrNoSharedStore
	}

	data, err := json.Marshal(state)
	if err != nil {
		return err
	}

	return dcb.store.SetData(dcb.sharedStateKey(), data)
}

func (dcb *DistributedCircuitBreaker[T]) getSharedStateVersioned() (SharedState, uint64, error) {
	var state SharedState
	if dcb.vstore == nil {
		return state, 0, ErrNoSharedStore
	}

	data, version, err := dcb.vstore.GetDataWithVersion(dcb.sharedStateKey())
	if err != nil {
		return state, 0, err
	}
	if len(data) == 0 {
		return state, version, ErrNoSharedState
	}

	err = json.Unmarshal(data, &state)
	if err != nil {
		return state, version, fmt.Errorf("%w: %v", ErrInvalidSharedState, err)
	}
	return state, version, nil
}

func (dcb *DistributedCircuitBreaker[T]) compareAndSwapSharedState(state SharedState, version uint64) error {
	if dcb.vstore == nil {
		return ErrNoSharedStore
	}

	data, err := json.Marshal(state)
	if err != nil {
		return err
	}

	return dcb.vstore.CompareAndSwapData(dcb.sharedStateKey(), data, version)
}

func (dcb *DistributedCircuitBreaker[T]) inject(shared SharedState) {
	dcb.mutex.Lock()
	defer dcb.mutex.Unlock()

	dcb.state = shared.State
	dcb.generation = shared.Generation
	dcb.counts.Counts = shared.Counts
	dcb.counts.age = shared.Age
	dcb.counts.buckets = copyBuckets(shared.Buckets)
	dcb.start = shared.Start
	dcb.expiry = shared.Expiry
}

func copyBuckets(buckets []Counts) []Counts {
	if buckets == nil {
		return []Counts{}
	}

	counts := make([]Counts, len(buckets))
	copy(counts, buckets)
	return counts
}

func (dcb *DistributedCircuitBreaker[T]) extract() SharedState {
	dcb.mutex.Lock()
	defer dcb.mutex.Unlock()

	state := SharedState{
		State:      dcb.state,
		Generation: dcb.generation,
		Age:        dcb.counts.age,
		Counts:     dcb.counts.Counts,
		Buckets:    copyBuckets(dcb.counts.buckets),
		Start:      dcb.start,
		Expiry:     dcb.expiry,
	}

	return state
}

// State returns the State of DistributedCircuitBreaker.
func (dcb *DistributedCircuitBreaker[T]) State() (state State, err error) {
	if dcb.vstore != nil {
		return dcb.stateVersioned()
	}

	shared, err := dcb.getSharedState()
	if err != nil {
		return shared.State, err
	}

	err = dcb.lock()
	if err != nil {
		return state, err
	}
	defer func() {
		e := dcb.unlock()
		if err == nil {
			err = e
		}
	}()

	dcb.inject(shared)
	state = dcb.CircuitBreaker.State()
	shared = dcb.extract()

	err = dcb.setSharedState(shared)
	return state, err
}

// stateVersioned returns the State of DistributedCircuitBreaker using
// compare-and-swap writes, retrying the state merge on version conflicts.
func (dcb *DistributedCircuitBreaker[T]) stateVersioned() (State, error) {
	for {
		shared, version, err := dcb.getSharedStateVersioned()
		if err != nil {
			return shared.State, err
		}

		dcb.inject(shared)
		state := dcb.CircuitBreaker.State()

		err = dcb.compareAndSwapSharedState(dcb.extract(), version)
		if err == nil {
			return state, nil
		}
		if !errors.Is(err, ErrStateConflict) {
			return state, err
		}
	}
}

// Execute runs the given request if the DistributedCircuitBreaker accepts it.
func (dcb *DistributedCircuitBreaker[T]) Execute(req func() (T, error)) (t T, err error) {
	if dcb.vstore != nil {
		return dcb.executeVersioned(req)
	}

	shared, err := dcb.getSharedState()
	if err != nil {
		return t, err
	}

	err = dcb.lock()
	if err != nil {
		return t, err
	}
	defer func() {
		e := dcb.unlock()
		if err == nil {
			err = e
		}
	}()

	dcb.inject(shared)
	t, err = dcb.CircuitBreaker.Execute(req)
	shared = dcb.extract()

	e := dcb.setSharedState(shared)
	if e != nil {
		return t, e
	}

	return t, err
}

// executeVersioned runs the given request exactly once and merges its outcome
// into the shared state with compare-and-swap writes. On a version conflict
// only the state merge is retried; the request is never run again.
func (dcb *DistributedCircuitBreaker[T]) executeVersioned(req func() (T, error)) (t T, err error) {
	shared, version, err := dcb.getSharedStateVersioned()
	if err != nil {
		return t, err
	}

	dcb.inject(shared)
	generation, age, err := dcb.beforeRequest()
	if err != nil {
		// Best effort: persist time-based state changes such as
		// open to half-open transitions.
		_ = dcb.compareAndSwapSharedState(dcb.extract(), version)
		return t, err
	}

	defer func() {
		if e := recover(); e != nil {
			_ = dcb.mergeOutcome(generation, age, version, fmt.Errorf("%v", e))
			panic(e)
		}
	}()

	t, err = req()
	if e := dcb.mergeOutcome(generation, age, version, err); e != nil && err == nil {
		err = e
	}
	return t, err
}

// mergeOutcome records the outcome of an already executed request into the
// shared state. When the write conflicts, the shared state is reloaded and
// the same outcome is merged again; the request itself is not re-executed.
func (dcb *DistributedCircuitBreaker[T]) mergeOutcome(generation, age, version uint64, reqErr error) error {
	for {
		dcb.afterRequest(generation, age, reqErr)

		err := dcb.compareAndSwapSharedState(dcb.extract(), version)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrStateConflict) {
			return err
		}

		shared, v, err := dcb.getSharedStateVersioned()
		if err != nil {
			return err
		}
		version = v
		dcb.inject(shared)
		generation, age = dcb.countRequest()
	}
}

// countRequest registers an already admitted request against the freshly
// loaded local state and returns the generation and age its outcome
// belongs to. Outcomes tagged with a stale generation are dropped by
// afterRequest instead of corrupting the counts of a newer generation.
func (dcb *DistributedCircuitBreaker[T]) countRequest() (uint64, uint64) {
	dcb.mutex.Lock()
	defer dcb.mutex.Unlock()

	_, generation, age := dcb.currentState(time.Now())
	dcb.counts.onRequest()
	return generation, age
}
