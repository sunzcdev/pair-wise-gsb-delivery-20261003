package gobreaker

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

var errBackend = errors.New("backend failed")

// fakeClock is a goroutine-safe controllable time source for deterministic
// interleaving of acquisition, execution and idle reclaim without real sleeps.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(0, 0)}
}

func (fc *fakeClock) get() time.Time {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.now
}

func (fc *fakeClock) advance(d time.Duration) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.now = fc.now.Add(d)
}

// newTestContainer wires a container to a fake clock. The factory builds
// breakers that trip after a single failure and move to half-open quickly, so
// half-open permits can be exercised deterministically.
func newTestContainer[T any](t *testing.T, maxEntries int, idleTimeout time.Duration) (*Container[T], *fakeClock, *atomic.Int64) {
	t.Helper()

	clock := newFakeClock()
	var created atomic.Int64

	container, err := NewContainer(ContainerSettings[T]{
		MaxEntries:  maxEntries,
		IdleTimeout: idleTimeout,
		Factory: func(key string) *CircuitBreaker[T] {
			created.Add(1)
			return NewCircuitBreaker[T](Settings{
				Name:        key,
				MaxRequests: 1,
				Timeout:     time.Millisecond,
				ReadyToTrip: func(Counts) bool { return true },
			})
		},
	})
	assert.NoError(t, err)
	container.now = clock.get

	return container, clock, &created
}

func TestContainerNilFactory(t *testing.T) {
	_, err := NewContainer[bool](ContainerSettings[bool]{})
	assert.ErrorIs(t, err, ErrNilFactory)
}

func TestContainerIsolatesFailuresByKey(t *testing.T) {
	container, _, _ := newTestContainer[bool](t, 0, time.Minute)

	// A failing backend trips only its own breaker.
	_, err := container.Execute("a", func() (bool, error) { return false, errBackend })
	assert.ErrorIs(t, err, errBackend)

	_, err = container.Execute("a", func() (bool, error) { return true, nil })
	assert.ErrorIs(t, err, ErrOpenState)

	// A different backend key is unaffected by a's failure.
	ok, err := container.Execute("b", func() (bool, error) { return true, nil })
	assert.NoError(t, err)
	assert.True(t, ok)

	assert.Equal(t, StateOpen, container.entries["a"].breaker.State())
	assert.Equal(t, StateClosed, container.entries["b"].breaker.State())
	assert.Equal(t, 2, container.Len())
}

func TestContainerSameKeyCreatedOnce(t *testing.T) {
	container, _, created := newTestContainer[bool](t, 0, time.Minute)

	first, err := container.Get("a")
	assert.NoError(t, err)

	// Concurrent gets of the same key must all observe the single instance.
	const goroutines = 32
	var wg sync.WaitGroup
	same := make(chan *CircuitBreaker[bool], goroutines)
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cb, getErr := container.Get("a")
			assert.NoError(t, getErr)
			same <- cb
		}()
	}
	wg.Wait()
	close(same)

	for cb := range same {
		assert.Same(t, first, cb)
	}
	assert.Equal(t, int64(1), created.Load())
}

func TestContainerCapacityAndActiveProtection(t *testing.T) {
	container, clock, _ := newTestContainer[bool](t, 2, time.Minute)

	_, doneA, err := container.Allow("a")
	assert.NoError(t, err)
	defer doneA(nil)

	_, doneB, err := container.Allow("b")
	assert.NoError(t, err)
	defer doneB(nil)

	// Both slots are occupied and busy; a third key gets an explicit result
	// instead of silently evicting one of the active breakers.
	_, err = container.Get("c")
	assert.ErrorIs(t, err, ErrContainerFull)
	assert.Equal(t, 2, container.Len())

	breakerA := container.entries["a"].breaker
	breakerB := container.entries["b"].breaker

	// Advancing past the idle timeout while the requests are in flight does not
	// reclaim anything: active breakers are protected.
	clock.advance(2 * time.Minute)
	_, err = container.Get("c")
	assert.ErrorIs(t, err, ErrContainerFull)
	assert.Same(t, breakerA, container.entries["a"].breaker)
	assert.Same(t, breakerB, container.entries["b"].breaker)

	// One request finishes. It is still fresh, so it cannot be replaced either.
	doneA(nil)
	_, err = container.Get("c")
	assert.ErrorIs(t, err, ErrContainerFull)

	// After the released breaker becomes idle it is reclaimed to make room.
	clock.advance(time.Minute)
	cbC, err := container.Get("c")
	assert.NoError(t, err)
	assert.Same(t, breakerB, container.entries["b"].breaker)
	assert.Same(t, cbC, container.entries["c"].breaker)
	assert.NotContains(t, container.entries, "a")
	assert.Equal(t, 2, container.Len())
}

func TestContainerIdleReclaimDoesNotPolluteNewInstance(t *testing.T) {
	container, clock, created := newTestContainer[bool](t, 1, time.Minute)

	old, err := container.Get("a")
	assert.NoError(t, err)

	// Fail on the old instance so it would reject the next request.
	_, err = container.Execute("a", func() (bool, error) { return false, errBackend })
	assert.ErrorIs(t, err, errBackend)

	// Let the old breaker become idle and reclaim it, then create a fresh one
	// under the same key.
	clock.advance(2 * time.Minute)
	assert.Equal(t, 1, container.Cleanup())

	fresh, err := container.Get("a")
	assert.NoError(t, err)
	assert.NotSame(t, old, fresh)
	assert.Equal(t, int64(2), created.Load())
	assert.Equal(t, StateClosed, fresh.State())

	// The fresh breaker accepts traffic; it was not polluted by the old one.
	ok, err := container.Execute("a", func() (bool, error) { return true, nil })
	assert.NoError(t, err)
	assert.True(t, ok)

	// The retired breaker still rejects independently and never touches the
	// fresh instance, so a late caller holding the old pointer cannot pollute
	// the new one.
	_, err = old.Execute(func() (bool, error) { return true, nil })
	assert.ErrorIs(t, err, ErrOpenState)

	counts := fresh.Counts()
	assert.Equal(t, uint32(1), counts.Requests)
	assert.Equal(t, uint32(1), counts.TotalSuccesses)
	assert.Zero(t, counts.TotalFailures)
}

func TestContainerHalfOpenPermitPinsEntry(t *testing.T) {
	container, clock, _ := newTestContainer[bool](t, 1, time.Minute)

	// Trip the breaker.
	_, err := container.Execute("a", func() (bool, error) { return false, errBackend })
	assert.ErrorIs(t, err, errBackend)

	// Expire the open state using the breaker's own clock, then grab the single
	// half-open trial permit.
	breaker, err := container.Get("a")
	assert.NoError(t, err)
	pseudoSleep(breaker, 2*time.Millisecond)

	var done func(error)
	breaker, done, err = container.Allow("a")
	assert.NoError(t, err)
	assert.Equal(t, StateHalfOpen, breaker.State())

	// With the half-open permit outstanding, no other request can pass and the
	// entry is pinned even past the idle timeout.
	clock.advance(2 * time.Minute)
	_, _, err = container.Allow("a")
	assert.ErrorIs(t, err, ErrTooManyRequests)
	assert.Equal(t, 0, container.Cleanup())
	assert.Equal(t, 1, container.Len())
	assert.Same(t, breaker, container.entries["a"].breaker)

	// Reporting a successful trial closes the breaker and unpins the entry.
	done(nil)
	assert.Equal(t, StateClosed, breaker.State())

	clock.advance(time.Minute)
	assert.Equal(t, 1, container.Cleanup())
	assert.Equal(t, 0, container.Len())
}

func TestContainerInterleavedAcquireExecuteReclaim(t *testing.T) {
	container, clock, created := newTestContainer[bool](t, 3, time.Minute)

	// Interleave acquires and long-running executions across keys.
	_, doneA, err := container.Allow("a")
	assert.NoError(t, err)

	_, err = container.Execute("b", func() (bool, error) { return true, nil })
	assert.NoError(t, err)

	_, doneC, err := container.Allow("c")
	assert.NoError(t, err)

	breakerA := container.entries["a"].breaker
	breakerB := container.entries["b"].breaker
	breakerC := container.entries["c"].breaker
	assert.Equal(t, 3, container.Len())

	// a and c are busy; b finished and goes idle. Full container rejects d.
	clock.advance(30 * time.Second)
	_, err = container.Get("d")
	assert.ErrorIs(t, err, ErrContainerFull)

	// b ages out and is reclaimed for d while a and c keep running.
	clock.advance(31 * time.Second)
	breakerD, err := container.Get("d")
	assert.NoError(t, err)

	assert.Same(t, breakerA, container.entries["a"].breaker)
	assert.NotContains(t, container.entries, "b")
	assert.Same(t, breakerC, container.entries["c"].breaker)
	assert.Same(t, breakerD, container.entries["d"].breaker)

	// Finishing the in-flight a and c does not create new instances; the same
	// breakers keep serving their keys.
	doneA(nil)
	doneC(nil)

	cbA, err := container.Get("a")
	assert.NoError(t, err)
	assert.Same(t, breakerA, cbA)
	cbC, err := container.Get("c")
	assert.NoError(t, err)
	assert.Same(t, breakerC, cbC)

	// Four distinct keys were requested over time but the container never held
	// more than MaxEntries, and each key was built exactly once while present.
	assert.Equal(t, int64(4), created.Load())
	assert.Equal(t, 3, container.Len())

	// breakerB was retired; a request through its old pointer stays isolated and
	// never resurrects or mutates a current entry.
	_, err = breakerB.Execute(func() (bool, error) { return true, nil })
	assert.NoError(t, err)
	assert.NotContains(t, container.entries, "b")
}
