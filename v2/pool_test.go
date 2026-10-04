package gobreaker

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestPool(maxEntries int, idleTimeout time.Duration, created *int) *Pool[bool] {
	return NewPool[bool](maxEntries, idleTimeout, func(key string) Settings {
		*created++
		return Settings{
			ReadyToTrip: func(counts Counts) bool { return counts.ConsecutiveFailures >= 1 },
			Timeout:     10 * time.Second,
		}
	})
}

func TestPoolSameKeyCreatedOnce(t *testing.T) {
	var created int
	pool := newTestPool(4, time.Minute, &created)

	// Interleave Get and Execute on the same key.
	first, err := pool.Get("backend-a")
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		_, err = pool.Execute("backend-a", func() (bool, error) { return true, nil })
		require.NoError(t, err)

		again, err := pool.Get("backend-a")
		require.NoError(t, err)
		assert.Same(t, first, again)
	}

	assert.Equal(t, 1, created)
	assert.Equal(t, "backend-a", first.Name())
	assert.Equal(t, 1, pool.Len())
}

func TestPoolConcurrentSameKeyCreatedOnce(t *testing.T) {
	var mu sync.Mutex
	var created int
	pool := NewPool[bool](4, time.Minute, func(key string) Settings {
		mu.Lock()
		created++
		mu.Unlock()
		return Settings{}
	})

	const goroutines = 32
	breakers := make([]*CircuitBreaker[bool], goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cb, err := pool.Get("backend-a")
			assert.NoError(t, err)
			breakers[i] = cb
		}(i)
	}
	wg.Wait()

	assert.Equal(t, 1, created)
	for _, cb := range breakers {
		assert.Same(t, breakers[0], cb)
	}
}

func TestPoolIsolatesBackends(t *testing.T) {
	var created int
	pool := newTestPool(4, time.Minute, &created)

	_, err := pool.Execute("backend-a", func() (bool, error) { return false, errFailed })
	assert.True(t, errors.Is(err, errFailed))

	cbA, err := pool.Get("backend-a")
	require.NoError(t, err)
	assert.Equal(t, StateOpen, cbA.State())

	// The failure of backend-a does not affect backend-b.
	result, err := pool.Execute("backend-b", func() (bool, error) { return true, nil })
	require.NoError(t, err)
	assert.True(t, result)

	cbB, err := pool.Get("backend-b")
	require.NoError(t, err)
	assert.Equal(t, StateClosed, cbB.State())
}

func TestPoolFullWithAllEntriesActive(t *testing.T) {
	var created int
	pool := newTestPool(2, time.Hour, &created)
	now := time.Now()
	pool.now = func() time.Time { return now }

	doneA, err := pool.Allow("backend-a")
	require.NoError(t, err)
	doneB, err := pool.Allow("backend-b")
	require.NoError(t, err)

	// Capacity is full and both entries have requests in flight:
	// the pool must report it explicitly instead of evicting one.
	_, err = pool.Get("backend-c")
	assert.True(t, errors.Is(err, ErrPoolFull))
	_, err = pool.Execute("backend-c", func() (bool, error) { return true, nil })
	assert.True(t, errors.Is(err, ErrPoolFull))
	_, err = pool.Allow("backend-c")
	assert.True(t, errors.Is(err, ErrPoolFull))
	assert.Equal(t, 2, pool.Len())
	assert.Equal(t, 2, created)

	// Once the in-flight requests finish and go idle, a new key evicts the
	// oldest idle entry instead of failing.
	doneA(nil)
	doneB(nil)
	now = now.Add(2 * time.Hour)

	cbC, err := pool.Get("backend-c")
	require.NoError(t, err)
	assert.Equal(t, 3, created)
	assert.Equal(t, 2, pool.Len())
	assert.Equal(t, "backend-c", cbC.Name())
}

func TestPoolReclaimIdleExpiresEntries(t *testing.T) {
	var created int
	pool := newTestPool(4, time.Minute, &created)
	now := time.Now()
	pool.now = func() time.Time { return now }

	oldA, err := pool.Get("backend-a")
	require.NoError(t, err)
	_, err = pool.Get("backend-b")
	require.NoError(t, err)

	// Not yet idle long enough.
	now = now.Add(30 * time.Second)
	assert.Equal(t, 0, pool.ReclaimIdle())
	assert.Equal(t, 2, pool.Len())

	// Past the idle timeout both entries are reclaimed.
	now = now.Add(time.Minute)
	assert.Equal(t, 2, pool.ReclaimIdle())
	assert.Equal(t, 0, pool.Len())

	// The same key is recreated as a fresh instance.
	newA, err := pool.Get("backend-a")
	require.NoError(t, err)
	assert.NotSame(t, oldA, newA)
	assert.Equal(t, 3, created)
	assert.Equal(t, StateClosed, newA.State())
	assert.Equal(t, Counts{}, newA.Counts())
}

func TestPoolActiveRequestProtectsInstance(t *testing.T) {
	var created int
	pool := newTestPool(4, time.Minute, &created)
	now := time.Now()
	pool.now = func() time.Time { return now }

	oldA, err := pool.Get("backend-a")
	require.NoError(t, err)

	// Start a request and let the entry sit past the idle timeout while the
	// request is still in flight.
	done, err := pool.Allow("backend-a")
	require.NoError(t, err)
	now = now.Add(10 * time.Minute)
	assert.Equal(t, 0, pool.ReclaimIdle())
	assert.Equal(t, 1, pool.Len())

	// The in-flight request fails and trips the old instance.
	done(errFailed)
	assert.Equal(t, StateOpen, oldA.State())

	// Once idle again the entry is reclaimed, and the same key is recreated
	// as a fresh instance that the old instance's failure cannot pollute.
	now = now.Add(10 * time.Minute)
	assert.Equal(t, 1, pool.ReclaimIdle())

	newA, err := pool.Get("backend-a")
	require.NoError(t, err)
	assert.NotSame(t, oldA, newA)
	assert.Equal(t, StateClosed, newA.State())
	assert.Equal(t, Counts{}, newA.Counts())

	// Late activity on the evicted instance never reaches the new one.
	pseudoSleep(oldA, 11*time.Second)
	_, err = oldA.Execute(func() (bool, error) { return false, errFailed })
	assert.True(t, errors.Is(err, errFailed))
	assert.Equal(t, Counts{}, newA.Counts())
	assert.Equal(t, StateClosed, newA.State())
}

func TestPoolHalfOpenPermitProtectsInstance(t *testing.T) {
	var created int
	pool := newTestPool(4, time.Minute, &created)
	now := time.Now()
	pool.now = func() time.Time { return now }

	cb, err := pool.Get("backend-a")
	require.NoError(t, err)

	// Trip the breaker and force it into the half-open state.
	_, err = pool.Execute("backend-a", func() (bool, error) { return false, errFailed })
	require.True(t, errors.Is(err, errFailed))
	require.Equal(t, StateOpen, cb.State())
	pseudoSleep(cb, 11*time.Second)
	require.Equal(t, StateHalfOpen, cb.State())

	// Take the half-open permit and let the entry go past the idle timeout
	// while the permit is still outstanding.
	done, err := pool.Allow("backend-a")
	require.NoError(t, err)
	now = now.Add(10 * time.Minute)
	assert.Equal(t, 0, pool.ReclaimIdle())
	assert.Equal(t, 1, pool.Len())

	// The outstanding permit still completes against the same instance.
	done(nil)
	assert.Equal(t, StateClosed, cb.State())
}

func TestPoolDoneCalledTwiceHasNoEffect(t *testing.T) {
	var created int
	pool := newTestPool(4, time.Minute, &created)
	now := time.Now()
	pool.now = func() time.Time { return now }

	done, err := pool.Allow("backend-a")
	require.NoError(t, err)
	done(nil)
	done(errFailed) // second call must be a no-op

	cb, err := pool.Get("backend-a")
	require.NoError(t, err)
	assert.Equal(t, StateClosed, cb.State())
	assert.Equal(t, uint32(1), cb.Counts().TotalSuccesses)

	// The entry was unpinned exactly once, so it is reclaimable.
	now = now.Add(10 * time.Minute)
	assert.Equal(t, 1, pool.ReclaimIdle())
}
