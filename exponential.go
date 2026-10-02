package backoff

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

/*
ExponentialBackOff is a backoff implementation that increases the backoff
period for each retry attempt using a randomization function that grows exponentially.

NextBackOff() is calculated using the following formula:

	randomized interval =
	    RetryInterval * (random value in range [1 - RandomizationFactor, 1 + RandomizationFactor])

In other words NextBackOff() will range between the randomization factor
percentage below and above the retry interval.

For example, given the following parameters:

	RetryInterval = 2
	RandomizationFactor = 0.5
	Multiplier = 2

the actual backoff period used in the next retry attempt will range between 1 and 3 seconds,
multiplied by the exponential, that is, between 2 and 6 seconds.

Note: MaxInterval caps the RetryInterval and not the randomized interval.

Example: Given the following default arguments, for 9 tries the sequence will be:

	Request #  RetryInterval (seconds)  Randomized Interval (seconds)

	 1          0.5                     [0.25,   0.75]
	 2          0.75                    [0.375,  1.125]
	 3          1.125                   [0.562,  1.687]
	 4          1.687                   [0.8435, 2.53]
	 5          2.53                    [1.265,  3.795]
	 6          3.795                   [1.897,  5.692]
	 7          5.692                   [2.846,  8.538]
	 8          8.538                   [4.269, 12.807]
	 9         12.807                   [6.403, 19.210]

Note: Implementation is not thread-safe.
*/
type ExponentialBackOff struct {
	InitialInterval     time.Duration
	RandomizationFactor float64
	Multiplier          float64
	MaxInterval         time.Duration

	currentInterval time.Duration
	random          *rand.PCG
}

// Default values for ExponentialBackOff.
const (
	DefaultInitialInterval     = 500 * time.Millisecond
	DefaultRandomizationFactor = 0.5
	DefaultMultiplier          = 1.5
	DefaultMaxInterval         = 60 * time.Second
)

// NewExponentialBackOff creates an instance of ExponentialBackOff using default values.
func NewExponentialBackOff() *ExponentialBackOff {
	return &ExponentialBackOff{
		InitialInterval:     DefaultInitialInterval,
		RandomizationFactor: DefaultRandomizationFactor,
		Multiplier:          DefaultMultiplier,
		MaxInterval:         DefaultMaxInterval,
	}
}

// Reset the interval back to the initial retry interval and restarts the timer.
// Reset must be called before using b.
//
// Reset does not touch the random source, so a policy restored from a snapshot
// keeps its random sequence but restarts the interval growth; see Restore and
// WithBackOffReset for how to continue a snapshot through Retry.
func (b *ExponentialBackOff) Reset() {
	b.currentInterval = b.InitialInterval
}

// NextBackOff calculates the next backoff interval using the formula:
//
//	Randomized interval = RetryInterval * (1 ± RandomizationFactor)
func (b *ExponentialBackOff) NextBackOff() time.Duration {
	if b.currentInterval == 0 {
		b.currentInterval = b.InitialInterval
	}

	next := getRandomValueFromInterval(b.RandomizationFactor, b.nextRandom(), b.currentInterval)
	b.incrementCurrentInterval()
	return next
}

// nextRandom returns the next value in [0, 1) from b's own random source,
// seeding it from the global source on first use. Keeping the source per
// instance is what allows Snapshot to capture the future random sequence.
func (b *ExponentialBackOff) nextRandom() float64 {
	if b.random == nil {
		b.random = rand.NewPCG(rand.Uint64(), rand.Uint64())
	}
	return rand.New(b.random).Float64()
}

// ExponentialBackOffStateVersion identifies the snapshot format produced by
// Snapshot and accepted by Restore.
const ExponentialBackOffStateVersion = 1

// ErrInvalidState is returned by Restore when a snapshot is not a valid
// ExponentialBackOffState: an unsupported version, out-of-range values, or a
// corrupt random state.
var ErrInvalidState = errors.New("backoff: invalid exponential backoff state")

// ExponentialBackOffState is a portable snapshot of an ExponentialBackOff: its
// configured parameters, its position in the interval sequence, and the state
// of its random source. Restoring it makes another policy produce the exact
// sequence of intervals the snapshotted one would have produced next.
//
// The state is plain serializable data — no operations, errors, or timers — so
// it can be encoded (e.g. with encoding/json) and stored to survive a pause or
// a process restart.
type ExponentialBackOffState struct {
	Version             int           `json:"version"`
	InitialInterval     time.Duration `json:"initial_interval"`
	RandomizationFactor float64       `json:"randomization_factor"`
	Multiplier          float64       `json:"multiplier"`
	MaxInterval         time.Duration `json:"max_interval"`
	CurrentInterval     time.Duration `json:"current_interval"`
	RandomState         []byte        `json:"random_state"`
}

// Snapshot returns the current state of b: its parameters, its position in the
// interval sequence, and its random source state. See ExponentialBackOffState.
func (b *ExponentialBackOff) Snapshot() ExponentialBackOffState {
	if b.random == nil {
		b.random = rand.NewPCG(rand.Uint64(), rand.Uint64())
	}
	randomState, _ := b.random.MarshalBinary() // never fails for *rand.PCG
	return ExponentialBackOffState{
		Version:             ExponentialBackOffStateVersion,
		InitialInterval:     b.InitialInterval,
		RandomizationFactor: b.RandomizationFactor,
		Multiplier:          b.Multiplier,
		MaxInterval:         b.MaxInterval,
		CurrentInterval:     b.currentInterval,
		RandomState:         randomState,
	}
}

// Restore replaces b's parameters, position, and random source with a snapshot
// previously obtained from Snapshot, so that subsequent NextBackOff calls
// continue the exact interval sequence the snapshotted policy would have
// produced. It returns an error wrapping ErrInvalidState — and leaves b
// unchanged — if the snapshot has an unsupported version or invalid values.
//
// Note that Retry resets its backoff policy before the first attempt by
// default, which would discard a restored position. Pass
// WithBackOffReset(false) together with WithBackOff(b) to make Retry continue
// a restored policy instead of restarting it.
func (b *ExponentialBackOff) Restore(state ExponentialBackOffState) error {
	if state.Version != ExponentialBackOffStateVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrInvalidState, state.Version)
	}
	if state.InitialInterval <= 0 {
		return fmt.Errorf("%w: initial interval must be positive, got %v", ErrInvalidState, state.InitialInterval)
	}
	if state.MaxInterval <= 0 {
		return fmt.Errorf("%w: max interval must be positive, got %v", ErrInvalidState, state.MaxInterval)
	}
	if state.CurrentInterval < 0 {
		return fmt.Errorf("%w: current interval must not be negative, got %v", ErrInvalidState, state.CurrentInterval)
	}
	if math.IsNaN(state.RandomizationFactor) || state.RandomizationFactor < 0 || state.RandomizationFactor > 1 {
		return fmt.Errorf("%w: randomization factor must be in [0, 1], got %v", ErrInvalidState, state.RandomizationFactor)
	}
	if math.IsNaN(state.Multiplier) || math.IsInf(state.Multiplier, 0) || state.Multiplier < 1 {
		return fmt.Errorf("%w: multiplier must be a finite value >= 1, got %v", ErrInvalidState, state.Multiplier)
	}
	pcg := &rand.PCG{}
	if err := pcg.UnmarshalBinary(state.RandomState); err != nil {
		return fmt.Errorf("%w: random state: %v", ErrInvalidState, err)
	}

	b.InitialInterval = state.InitialInterval
	b.RandomizationFactor = state.RandomizationFactor
	b.Multiplier = state.Multiplier
	b.MaxInterval = state.MaxInterval
	b.currentInterval = state.CurrentInterval
	b.random = pcg
	return nil
}

// Increments the current interval by multiplying it with the multiplier.
func (b *ExponentialBackOff) incrementCurrentInterval() {
	// Check for overflow, if overflow is detected set the current interval to the max interval.
	if float64(b.currentInterval) >= float64(b.MaxInterval)/b.Multiplier {
		b.currentInterval = b.MaxInterval
	} else {
		b.currentInterval = time.Duration(float64(b.currentInterval) * b.Multiplier)
	}
}

// Returns a random value from the following interval:
//
//	[currentInterval - randomizationFactor * currentInterval, currentInterval + randomizationFactor * currentInterval].
func getRandomValueFromInterval(randomizationFactor, random float64, currentInterval time.Duration) time.Duration {
	if randomizationFactor == 0 {
		return currentInterval // make sure no randomness is used when randomizationFactor is 0.
	}
	var delta = randomizationFactor * float64(currentInterval)
	var minInterval = float64(currentInterval) - delta
	var maxInterval = float64(currentInterval) + delta

	// Get a random value from the range [minInterval, maxInterval].
	// The formula used below has a +1 because if the minInterval is 1 and the maxInterval is 3 then
	// we want a 33% chance for selecting either 1, 2 or 3.
	value := minInterval + (random * (maxInterval - minInterval + 1))
	// float64(math.MaxInt64) rounds up to 2^63, which does not fit in a Duration.
	if value >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return time.Duration(value)
}
