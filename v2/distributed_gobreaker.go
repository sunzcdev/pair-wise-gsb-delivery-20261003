package gobreaker

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"time"
)

var (
	// ErrNoSharedStore is returned when there is no shared store.
	ErrNoSharedStore = errors.New("no shared store")
	// ErrNoSharedState is returned when there is no shared state.
	ErrNoSharedState = errors.New("no shared state")
	// ErrVersionConflict is returned when a compare-and-set write loses to a
	// concurrent write and cannot be retried any more.
	ErrVersionConflict = errors.New("shared state version conflict")
	// ErrCorruptSharedState is returned when the shared state in the store
	// cannot be decoded.
	ErrCorruptSharedState = errors.New("corrupt shared state")
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

// Version is an opaque token identifying one revision of a stored record.
// A nil Version means that the record does not exist.
type Version []byte

// VersionedDataStore stores shared state using optimistic concurrency control
// instead of a cross-process lock. Implementations update shared state with
// CompareAndSetData, which only succeeds when the stored record still has the
// Version returned by GetVersionedData.
//
// A DistributedCircuitBreaker backed by a VersionedDataStore never holds a
// cross-process lock while running a business request: the request is admitted
// and executed against a local snapshot, and its outcome is merged into the
// latest stored state afterwards. A lost compare-and-set only retries the
// merge; the business function is never executed again.
type VersionedDataStore interface {
	SharedDataStore

	// GetVersionedData returns the record and its current Version.
	// When the record does not exist, it returns a nil Version,
	// nil or empty data and no error.
	GetVersionedData(name string) (data []byte, version Version, err error)

	// CompareAndSetData writes data unconditionally when the stored Version
	// equals expect. A nil expect means that the record must not exist.
	// It returns the new Version on success, or ErrVersionConflict when the
	// stored Version no longer matches.
	CompareAndSetData(name string, expect Version, data []byte) (Version, error)
}

// maxCASAttempts bounds how many merge retries one optimistic update performs
// before giving up with ErrVersionConflict.
const maxCASAttempts = 10

// DistributedCircuitBreaker extends CircuitBreaker with SharedDataStore.
type DistributedCircuitBreaker[T any] struct {
	*CircuitBreaker[T]
	store  SharedDataStore
	vstore VersionedDataStore
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

	if vstore, ok := store.(VersionedDataStore); ok {
		dcb.vstore = vstore
		return dcb, dcb.initVersioned()
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

// initVersioned creates the shared state without taking a cross-process lock.
// Two instances racing to initialize both attempt a create with a nil
// expected Version; the loser gets ErrVersionConflict and adopts the state the
// winner created instead of failing or overwriting it.
func (dcb *DistributedCircuitBreaker[T]) initVersioned() error {
	_, _, err := dcb.getVersionedSharedState()
	if err == nil {
		return nil
	}
	if err != ErrNoSharedState {
		return err
	}

	data, err := json.Marshal(dcb.extract())
	if err != nil {
		return err
	}

	_, err = dcb.vstore.CompareAndSetData(dcb.sharedStateKey(), nil, data)
	if err == nil {
		return nil
	}
	if err != ErrVersionConflict {
		return err
	}

	// Another instance won the initialization race. Adopt its state. A record
	// that appeared but cannot be decoded is corrupt rather than missing.
	_, _, err = dcb.getVersionedSharedState()
	return err
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

	return decodeSharedState(data)
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

func decodeSharedState(data []byte) (SharedState, error) {
	var state SharedState
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("%w: %v", ErrCorruptSharedState, err)
	}
	return state, nil
}

func (dcb *DistributedCircuitBreaker[T]) getVersionedSharedState() (SharedState, Version, error) {
	var version Version
	if dcb.vstore == nil {
		return SharedState{}, nil, ErrNoSharedStore
	}

	data, version, err := dcb.vstore.GetVersionedData(dcb.sharedStateKey())
	if err != nil {
		return SharedState{}, version, err
	}
	if len(data) == 0 {
		return SharedState{}, version, ErrNoSharedState
	}

	state, err := decodeSharedState(data)
	return state, version, err
}

func (dcb *DistributedCircuitBreaker[T]) inject(shared SharedState) {
	dcb.injectInto(dcb.CircuitBreaker, shared)
}

func (dcb *DistributedCircuitBreaker[T]) injectInto(cb *CircuitBreaker[T], shared SharedState) {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	cb.state = shared.State
	cb.generation = shared.Generation
	cb.counts.Counts = shared.Counts
	cb.counts.age = shared.Age
	cb.counts.buckets = copyBuckets(shared.Buckets)
	cb.start = shared.Start
	cb.expiry = shared.Expiry
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
	return dcb.extractFrom(dcb.CircuitBreaker)
}

func (dcb *DistributedCircuitBreaker[T]) extractFrom(cb *CircuitBreaker[T]) SharedState {
	cb.mutex.Lock()
	defer cb.mutex.Unlock()

	return SharedState{
		State:      cb.state,
		Generation: cb.generation,
		Age:        cb.counts.age,
		Counts:     cb.counts.Counts,
		Buckets:    copyBuckets(cb.counts.buckets),
		Start:      cb.start,
		Expiry:     cb.expiry,
	}
}

// State returns the State of DistributedCircuitBreaker.
func (dcb *DistributedCircuitBreaker[T]) State() (state State, err error) {
	if dcb.vstore != nil {
		return dcb.stateVersioned()
	}
	return dcb.stateLocked()
}

func (dcb *DistributedCircuitBreaker[T]) stateLocked() (state State, err error) {
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

// stateVersioned advances the shared state according to the current time
// (rolling buckets or moving Open to HalfOpen) using compare-and-set writes.
// It only writes when the roll changes something, so concurrent State calls
// that have nothing to advance do not churn the version.
func (dcb *DistributedCircuitBreaker[T]) stateVersioned() (State, error) {
	shared, version, err := dcb.getVersionedSharedState()
	if err != nil {
		return shared.State, err
	}

	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		scratch := dcb.scratchBreaker()
		dcb.injectInto(scratch, shared)
		state := scratch.State()
		next := dcb.extractFrom(scratch)

		if reflect.DeepEqual(shared, next) {
			return state, nil
		}

		data, err := json.Marshal(next)
		if err != nil {
			return state, err
		}

		newVersion, err := dcb.vstore.CompareAndSetData(dcb.sharedStateKey(), version, data)
		if err == nil {
			version = newVersion
			return state, nil
		}
		if err != ErrVersionConflict {
			return state, err
		}

		shared, version, err = dcb.getVersionedSharedState()
		if err != nil {
			return shared.State, err
		}

		casBackoff(attempt)
	}

	return StateClosed, ErrVersionConflict
}

// Execute runs the given request if the DistributedCircuitBreaker accepts it.
func (dcb *DistributedCircuitBreaker[T]) Execute(req func() (T, error)) (t T, err error) {
	if dcb.vstore != nil {
		return dcb.executeVersioned(req)
	}
	return dcb.executeLocked(req)
}

func (dcb *DistributedCircuitBreaker[T]) executeLocked(req func() (T, error)) (t T, err error) {
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

// requestOutcome captures one executed request so its effect can be merged into
// a shared state that may have moved while the request was running.
type requestOutcome struct {
	generation uint64
	age        uint64
	admittedAt time.Time
	finishedAt time.Time
	err        error
}

func (dcb *DistributedCircuitBreaker[T]) executeVersioned(req func() (T, error)) (T, error) {
	shared, _, err := dcb.getVersionedSharedState()
	if err != nil {
		var defaultValue T
		return defaultValue, err
	}

	scratch := dcb.scratchBreaker()
	dcb.injectInto(scratch, shared)

	admittedAt := time.Now()
	generation, age, err := scratch.beforeRequestAt(admittedAt)
	if err != nil {
		// The request was rejected (e.g. circuit open) and never ran. Reading
		// the shared state changed nothing, so there is nothing to write.
		var defaultValue T
		return defaultValue, err
	}

	outcome := requestOutcome{generation: generation, age: age, admittedAt: admittedAt}

	defer func() {
		if p := recover(); p != nil {
			outcome.finishedAt = time.Now()
			outcome.err = fmt.Errorf("%v", p)
			_ = dcb.commitOutcome(outcome)
			panic(p)
		}
	}()

	result, reqErr := req()
	outcome.finishedAt = time.Now()
	outcome.err = reqErr

	if commitErr := dcb.commitOutcome(outcome); commitErr != nil {
		return result, commitErr
	}

	return result, reqErr
}

// commitOutcome merges an already executed request into the stored state.
// On a version conflict it re-reads the state and replays only the merge;
// the business function has already finished and is never invoked again.
func (dcb *DistributedCircuitBreaker[T]) commitOutcome(outcome requestOutcome) error {
	shared, version, err := dcb.getVersionedSharedState()
	if err != nil {
		return err
	}

	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		scratch := dcb.scratchBreaker()
		dcb.injectInto(scratch, shared)
		scratch.replayRequest(outcome.generation, outcome.age,
			outcome.admittedAt, outcome.finishedAt, outcome.err)
		next := dcb.extractFrom(scratch)

		data, err := json.Marshal(next)
		if err != nil {
			return err
		}

		_, err = dcb.vstore.CompareAndSetData(dcb.sharedStateKey(), version, data)
		if err == nil {
			return nil
		}
		if err != ErrVersionConflict {
			return err
		}

		shared, version, err = dcb.getVersionedSharedState()
		if err != nil {
			return err
		}

		casBackoff(attempt)
	}

	return ErrVersionConflict
}

// casBackoff sleeps for a short randomized delay after a lost
// compare-and-set so concurrent committers do not keep colliding. The delay
// grows exponentially across retries within one update.
func casBackoff(attempt int) {
	base := 200 * time.Microsecond
	delay := base << attempt
	if delay > 5*time.Millisecond {
		delay = 5 * time.Millisecond
	}
	jitter := time.Duration(rand.Int64N(int64(delay/2 + 1)))
	time.Sleep(delay/2 + jitter)
}
