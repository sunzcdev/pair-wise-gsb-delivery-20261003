package lua

import (
	"math/bits"
	"math/rand"
)

// newStateRNG builds the private random source for a newly created main
// state. It is intentionally backed by a dedicated *rand.Rand rather than
// the process-global math/rand source so that independent LStates never
// interfere with each other.
//
// The default seed is derived from the current time; hosts that require
// reproducibility should call LState.SetRandomSeed (or use
// math.randomseed from Lua) after creating the state.
func newStateRNG(seed int64) *rand.Rand {
	return rand.New(rand.NewSource(seed))
}

// SetRandomSeed reseeds the private random source backing math.random.
//
// Every coroutine created from this main state shares the same source, so
// seeding the main state fixes a single shared sequence. Independent
// LStates keep independent sources and sequences. The process-global
// math/rand source is never touched.
//
// This is the Go-side equivalent of calling math.randomseed from Lua.
func (ls *LState) SetRandomSeed(seed int64) {
	ls.G.rng = newStateRNG(seed)
}

// SetRandomSource replaces the random source backing math.random with a
// host-provided one. This allows injecting a deterministic or custom
// source for replay and testing. r must not be nil.
//
// The injected source is shared with every coroutine of this main state.
// It is used through *rand.Rand, so a concurrency-safe source (such as
// one created by rand.New with a mutex-guarded Source) can be used from
// multiple goroutines.
func (ls *LState) SetRandomSource(r *rand.Rand) {
	if r == nil {
		panic("lua: SetRandomSource requires a non-nil *rand.Rand")
	}
	ls.G.rng = r
}

// randomInt64n returns a uniformly distributed pseudo-random int64 in
// the half-open interval [lo, hi).
//
// It never panics: a span that overflows int64 (the full int64 domain) is
// handled separately, and the bounded draw uses unbiased rejection
// sampling instead of the modulo operator, so the result cannot fall
// outside the requested interval. Callers must guarantee lo < hi.
func randomInt64n(r *rand.Rand, lo, hi int64) int64 {
	// Compute the span as uint64 so that the full int64 domain
	// ([MinInt64, MaxInt64+1)) does not overflow to zero.
	span := uint64(hi) - uint64(lo)
	if span == 0 {
		// hi-lo wrapped around, i.e. the entire int64 domain.
		return int64(r.Uint64())
	}
	return lo + int64(randUint64n(r, span))
}

// randUint64n returns a uniformly distributed pseudo-random uint64 in
// [0, n) without modulo bias. n must be positive. It mirrors the
// Lemire-style rejection algorithm used by math/rand itself.
func randUint64n(r *rand.Rand, n uint64) uint64 {
	// n only needs rejection when it does not divide 2^64 evenly.
	if n&(n-1) == 0 {
		return r.Uint64() & (n - 1)
	}
	hi, lo := bits.Mul64(r.Uint64(), n)
	if lo < n {
		threshold := (-n) % n
		for lo < threshold {
			hi, lo = bits.Mul64(r.Uint64(), n)
		}
	}
	return hi
}
