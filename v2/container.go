package gobreaker

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	// ErrContainerFull is returned when the container holds MaxEntries breakers
	// and none of them is idle long enough to be reclaimed. Every entry is still
	// protected by an in-flight request or an outstanding half-open permit, so
	// the container refuses to silently evict and replace one of them.
	ErrContainerFull = errors.New("circuit breaker container is full")
	// ErrNilFactory is returned when the container is configured with a nil
	// BreakerFactory.
	ErrNilFactory = errors.New("breaker factory must not be nil")
)

// BreakerFactory builds a new CircuitBreaker for the given backend key.
// The key is supplied verbatim by the caller; the container never parses it or
// derives a partition from a URL. The factory is called while the container
// lock is held, so it must not call back into the container.
type BreakerFactory[T any] func(key string) *CircuitBreaker[T]

// ContainerSettings configures a Container.
//
// MaxEntries limits how many backend breakers are retained at the same time.
// A value less than or equal to 0 means there is no limit.
//
// IdleTimeout is how long a breaker may stay unused before it becomes eligible
// for reclaim. A breaker is considered idle only when it has no in-flight
// requests and no outstanding half-open permits. If IdleTimeout is less than
// or equal to 0, the container uses DefaultIdleTimeout.
//
// Factory builds a breaker for a key the first time that key is requested.
type ContainerSettings[T any] struct {
	MaxEntries  int
	IdleTimeout time.Duration
	Factory     BreakerFactory[T]
}

// DefaultIdleTimeout is used when ContainerSettings.IdleTimeout is not positive.
const DefaultIdleTimeout = 5 * time.Minute

type breakerEntry[T any] struct {
	breaker  *CircuitBreaker[T]
	lastUsed time.Time
	// active counts in-flight requests and outstanding half-open permits.
	// An entry with active > 0 is pinned in the container and can never be
	// reclaimed until every caller has reported its outcome.
	active int
}

// Container keeps one CircuitBreaker per caller-provided backend key.
//
// Each backend is tracked independently: tripping the breaker of one key never
// affects the breakers of other keys. Keys are opaque strings and the container
// never derives a key or partition from a URL; callers choose the partitioning
// scheme.
//
// The number of retained breakers is bounded by MaxEntries and idle entries are
// reclaimed once they exceed IdleTimeout. A breaker with an in-flight request
// or an unreleased half-open permit is always protected from reclaim.
type Container[T any] struct {
	maxEntries  int
	idleTimeout time.Duration
	factory     BreakerFactory[T]
	now         func() time.Time

	mutex   sync.Mutex
	entries map[string]*breakerEntry[T]
	closed  bool
}

// NewContainer returns a Container configured with the given settings.
// It returns ErrNilFactory if settings.Factory is nil.
func NewContainer[T any](settings ContainerSettings[T]) (*Container[T], error) {
	if settings.Factory == nil {
		return nil, ErrNilFactory
	}

	idleTimeout := settings.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = DefaultIdleTimeout
	}

	return &Container[T]{
		maxEntries:  settings.MaxEntries,
		idleTimeout: idleTimeout,
		factory:     settings.Factory,
		now:         time.Now,
		entries:     make(map[string]*breakerEntry[T]),
	}, nil
}

// Get returns the breaker registered for key, creating it on first use.
//
// If the limit is reached and no idle entry can be reclaimed, Get returns
// ErrContainerFull without disturbing any of the active breakers.
//
// The returned pointer is a snapshot. Go through Execute or Allow so that the
// container can protect the breaker while a request is in flight; calling the
// breaker directly does not pin it against idle reclaim.
func (c *Container[T]) Get(key string) (*CircuitBreaker[T], error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	return c.getLocked(key, true)
}

// getLocked returns the breaker for key, creating and registering it when
// necessary. When touch is true, a successful lookup refreshes lastUsed.
func (c *Container[T]) getLocked(key string, touch bool) (*CircuitBreaker[T], error) {
	now := c.now()

	if entry, ok := c.entries[key]; ok {
		if touch {
			entry.lastUsed = now
		}
		return entry.breaker, nil
	}

	if c.maxEntries > 0 && len(c.entries) >= c.maxEntries {
		if !c.reclaimLocked(now) {
			return nil, ErrContainerFull
		}
	}

	breaker := c.factory(key)
	c.entries[key] = &breakerEntry[T]{
		breaker:  breaker,
		lastUsed: now,
	}
	return breaker, nil
}

// reclaimLocked removes one idle entry that has been unused for at least
// idleTimeout. Entries with in-flight requests or outstanding half-open
// permits are never touched. When several entries are reclaimable the one that
// has been idle for the longest time is removed. It reports whether an entry
// was reclaimed.
func (c *Container[T]) reclaimLocked(now time.Time) bool {
	var oldestKey string
	var oldestUsed time.Time

	for key, entry := range c.entries {
		if entry.active != 0 {
			continue
		}
		if now.Sub(entry.lastUsed) < c.idleTimeout {
			continue
		}
		if oldestKey == "" || entry.lastUsed.Before(oldestUsed) {
			oldestKey = key
			oldestUsed = entry.lastUsed
		}
	}

	if oldestKey == "" {
		return false
	}

	delete(c.entries, oldestKey)
	return true
}

// Cleanup reclaims every idle entry that has been unused for at least
// IdleTimeout and returns the number of removed entries. Active entries are
// left in place. Call it periodically if eager reclaim is wanted; otherwise
// idle entries are reclaimed lazily when a new key needs room.
func (c *Container[T]) Cleanup() int {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	now := c.now()
	removed := 0
	for key, entry := range c.entries {
		if entry.active == 0 && now.Sub(entry.lastUsed) >= c.idleTimeout {
			delete(c.entries, key)
			removed++
		}
	}
	return removed
}

// Len returns the number of breakers currently retained.
func (c *Container[T]) Len() int {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	return len(c.entries)
}

// Allow checks whether a request for key can proceed.
//
// On success it returns the breaker for key and a done callback that the caller
// must invoke once with the request outcome. Holding the callback pins the
// breaker in the container, so an outstanding half-open permit is protected
// until done is called. If the breaker rejects the request (for example because
// it is open or its half-open permits are exhausted) the breaker is not pinned
// and the original rejection error is returned.
//
// As with Get, ErrContainerFull is returned when no new breaker can be created
// and no idle entry can be reclaimed. Calling done more than once has no
// effect.
func (c *Container[T]) Allow(key string) (*CircuitBreaker[T], func(error), error) {
	c.mutex.Lock()

	breaker, err := c.getLocked(key, false)
	if err != nil {
		c.mutex.Unlock()
		return nil, nil, err
	}

	entry := c.entries[key]
	generation, age, err := breaker.beforeRequest()
	if err != nil {
		c.mutex.Unlock()
		return nil, nil, err
	}

	entry.active++
	entry.lastUsed = c.now()
	c.mutex.Unlock()

	var once sync.Once
	done := func(reqErr error) {
		breaker.afterRequest(generation, age, reqErr)

		c.mutex.Lock()
		entry.active--
		entry.lastUsed = c.now()
		c.mutex.Unlock()
	}

	return breaker, func(err error) {
		once.Do(func() {
			done(err)
		})
	}, nil
}

// Execute runs req through the breaker registered for key, creating it on first
// use. The breaker is pinned for the whole execution, so an in-flight request
// always protects its own breaker from reclaim.
//
// It returns ErrContainerFull when the container cannot make room for key, the
// breaker's rejection error when it refuses the request, or the result of req
// otherwise. A panic in req is handled as an error and re-panicked.
func (c *Container[T]) Execute(key string, req func() (T, error)) (T, error) {
	_, done, err := c.Allow(key)
	if err != nil {
		var defaultValue T
		return defaultValue, err
	}

	defer func() {
		if e := recover(); e != nil {
			done(fmt.Errorf("%v", e))
			panic(e)
		}
	}()

	result, err := req()
	done(err)
	return result, err
}
