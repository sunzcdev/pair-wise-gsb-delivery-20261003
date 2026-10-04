package gobreaker

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// ErrPoolFull is returned by a Pool when a new backend key arrives while the
// pool already retains the maximum number of circuit breakers and none of the
// retained entries can be reclaimed because they all have requests in flight.
// The pool never evicts an entry that is still in use to make room for another.
var ErrPoolFull = errors.New("gobreaker: pool is full and every retained entry is in use")

// Pool retains a bounded number of CircuitBreakers, one per caller-provided
// backend key. Each key gets an isolated breaker: failures observed for one
// backend never affect the breakers of other keys. The pool does not derive
// keys from URLs or any other request attribute; the caller is responsible
// for choosing how backends are partitioned into keys.
//
// Entries that have no requests in flight and have been idle for at least the
// pool's idle timeout are reclaimed, either explicitly via ReclaimIdle or
// lazily when a new key needs room. Entries with requests in flight or with
// outstanding half-open permits obtained via Allow are pinned to their
// circuit breaker instance and are never reclaimed.
type Pool[T any] struct {
	maxEntries  int
	idleTimeout time.Duration
	settings    func(key string) Settings

	mu      sync.Mutex
	entries map[string]*poolEntry[T]
	now     func() time.Time
}

type poolEntry[T any] struct {
	cb     *CircuitBreaker[T]
	active int
	idleAt time.Time
}

// NewPool returns a Pool that retains at most maxEntries circuit breakers.
// An entry becomes reclaimable once it has had no requests in flight for
// idleTimeout; an idleTimeout less than or equal to 0 makes idle entries
// reclaimable immediately. The settings function is called once per new key
// to configure its circuit breaker; if nil, a default function that only sets
// the breaker name to the key is used. If the returned Settings has an empty
// Name, the key is used as the name.
func NewPool[T any](maxEntries int, idleTimeout time.Duration, settings func(key string) Settings) *Pool[T] {
	if maxEntries <= 0 {
		maxEntries = 1
	}
	if settings == nil {
		settings = func(key string) Settings { return Settings{Name: key} }
	}
	return &Pool[T]{
		maxEntries:  maxEntries,
		idleTimeout: idleTimeout,
		settings:    settings,
		entries:     make(map[string]*poolEntry[T]),
		now:         time.Now,
	}
}

// Get returns the CircuitBreaker retained for key, creating it on first use.
// The same instance is returned for every call with the same key until the
// entry is reclaimed. Get returns ErrPoolFull if a new entry is needed while
// the pool is full and no idle entry can be reclaimed.
//
// A breaker obtained via Get is not pinned against reclamation; use Execute
// or Allow to run requests so the entry is protected while in use.
func (p *Pool[T]) Get(key string) (*CircuitBreaker[T], error) {
	e, err := p.entry(key)
	if err != nil {
		return nil, err
	}
	return e.cb, nil
}

// Execute runs the given request through the CircuitBreaker retained for key,
// creating the breaker on first use. The entry is pinned while the request is
// in flight so it cannot be reclaimed. Execute returns ErrPoolFull if a new
// entry is needed while the pool is full and no idle entry can be reclaimed.
func (p *Pool[T]) Execute(key string, req func() (T, error)) (T, error) {
	e, err := p.entry(key)
	if err != nil {
		var defaultValue T
		return defaultValue, err
	}

	p.pin(e)
	defer p.unpin(e)

	return e.cb.Execute(req)
}

// Allow is the two-step variant of Execute, mirroring TwoStepCircuitBreaker.
// If the request may proceed, Allow returns a done callback that must be
// called exactly once with the request's error. The entry stays pinned to its
// circuit breaker instance until done is called, so an outstanding permit -
// including a half-open permit - protects the instance from reclamation.
// Calling done more than once has no effect.
func (p *Pool[T]) Allow(key string) (done func(err error), err error) {
	e, err := p.entry(key)
	if err != nil {
		return nil, err
	}

	p.pin(e)
	generation, age, err := e.cb.beforeRequest()
	if err != nil {
		p.unpin(e)
		return nil, err
	}

	var once sync.Once
	return func(err error) {
		once.Do(func() {
			e.cb.afterRequest(generation, age, err)
			p.unpin(e)
		})
	}, nil
}

// ReclaimIdle removes every entry that has no requests in flight and has been
// idle for at least the pool's idle timeout, and returns the number of
// removed entries. Entries with requests in flight or outstanding permits
// are kept.
func (p *Pool[T]) ReclaimIdle() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.reclaimIdleLocked(p.now(), 0)
}

// Len returns the number of circuit breakers currently retained by the pool.
func (p *Pool[T]) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return len(p.entries)
}

func (p *Pool[T]) entry(key string) (*poolEntry[T], error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if e, ok := p.entries[key]; ok {
		return e, nil
	}

	if len(p.entries) >= p.maxEntries {
		p.reclaimIdleLocked(p.now(), len(p.entries)-p.maxEntries+1)
		if len(p.entries) >= p.maxEntries {
			return nil, ErrPoolFull
		}
	}

	st := p.settings(key)
	if st.Name == "" {
		st.Name = key
	}
	e := &poolEntry[T]{cb: NewCircuitBreaker[T](st), idleAt: p.now()}
	p.entries[key] = e
	return e, nil
}

// reclaimIdleLocked removes reclaimable entries, oldest idle first, until at
// least target entries have been removed or no more entries are reclaimable.
// A target of 0 removes every reclaimable entry. It returns the number of
// removed entries. The caller must hold p.mu.
func (p *Pool[T]) reclaimIdleLocked(now time.Time, target int) int {
	candidates := make([]*poolEntry[T], 0, len(p.entries))
	keys := make(map[*poolEntry[T]]string, len(p.entries))
	for key, e := range p.entries {
		if e.active > 0 {
			continue
		}
		if p.idleTimeout > 0 && now.Sub(e.idleAt) < p.idleTimeout {
			continue
		}
		candidates = append(candidates, e)
		keys[e] = key
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].idleAt.Before(candidates[j].idleAt)
	})

	removed := 0
	for _, e := range candidates {
		if target > 0 && removed >= target {
			break
		}
		delete(p.entries, keys[e])
		removed++
	}
	return removed
}

func (p *Pool[T]) pin(e *poolEntry[T]) {
	p.mu.Lock()
	defer p.mu.Unlock()

	e.active++
}

func (p *Pool[T]) unpin(e *poolEntry[T]) {
	p.mu.Lock()
	defer p.mu.Unlock()

	e.active--
	if e.active == 0 {
		e.idleAt = p.now()
	}
}
