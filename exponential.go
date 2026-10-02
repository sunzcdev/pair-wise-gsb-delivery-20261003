package backoff

import (
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
	randomSource    *rand.PCG
	random          *rand.Rand
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
// Reset does not reseed the random source: the randomization sequence
// continues where it left off. To also capture the random state, use Snapshot.
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

// nextRandom returns the next random value in [0.0, 1.0) from the backoff's
// own random source, lazily seeded on first use. A dedicated source (instead
// of the global one) is what allows the random sequence to be captured and
// restored via Snapshot and RestoreExponentialBackOff.
func (b *ExponentialBackOff) nextRandom() float64 {
	if b.random == nil {
		b.randomSource = rand.NewPCG(rand.Uint64(), rand.Uint64())
		b.random = rand.New(b.randomSource)
	}
	return b.random.Float64()
}

// ExponentialBackOffStateVersion is the version of the ExponentialBackOffState
// snapshot format produced by Snapshot and accepted by
// RestoreExponentialBackOff.
const ExponentialBackOffStateVersion = 1

// ExponentialBackOffState is a serializable snapshot of an ExponentialBackOff:
// its parameters, its position in the exponential sequence, and the random
// source state needed to reproduce the subsequent randomized intervals.
//
// The snapshot holds plain data only — no operation, error, or timer — so it
// can be encoded with encoding/json, encoding/gob, or similar and stored
// while a task processor is paused.
type ExponentialBackOffState struct {
	Version             int           `json:"version"`
	InitialInterval     time.Duration `json:"initial_interval"`
	RandomizationFactor float64       `json:"randomization_factor"`
	Multiplier          float64       `json:"multiplier"`
	MaxInterval         time.Duration `json:"max_interval"`
	CurrentInterval     time.Duration `json:"current_interval"`
	RandState           []byte        `json:"rand_state"`
}

// Snapshot captures the current state of b so that a paused retry loop can
// later resume the exact same exponential and random sequence with
// RestoreExponentialBackOff.
func (b *ExponentialBackOff) Snapshot() ExponentialBackOffState {
	if b.random == nil {
		b.nextRandom() // seed the random source so its state can be captured
	}
	// rand.PCG.MarshalBinary never fails.
	randState, _ := b.randomSource.MarshalBinary()

	currentInterval := b.currentInterval
	if currentInterval == 0 {
		// Not started yet: NextBackOff would fall back to InitialInterval.
		currentInterval = b.InitialInterval
	}

	return ExponentialBackOffState{
		Version:             ExponentialBackOffStateVersion,
		InitialInterval:     b.InitialInterval,
		RandomizationFactor: b.RandomizationFactor,
		Multiplier:          b.Multiplier,
		MaxInterval:         b.MaxInterval,
		CurrentInterval:     currentInterval,
		RandState:           randState,
	}
}

// RestoreExponentialBackOff rebuilds an ExponentialBackOff from a snapshot
// previously taken with Snapshot, resuming the exponential sequence and the
// randomized intervals exactly where they were captured. It returns an error
// if the snapshot has an unsupported version or holds invalid values.
//
// Note that Retry calls Reset on its backoff policy before the first attempt,
// which would discard the restored position. Pass WithBackOffReset(false) to
// Retry when resuming from a restored snapshot.
func RestoreExponentialBackOff(state ExponentialBackOffState) (*ExponentialBackOff, error) {
	if state.Version != ExponentialBackOffStateVersion {
		return nil, fmt.Errorf("backoff: unsupported state version %d", state.Version)
	}
	if state.InitialInterval <= 0 {
		return nil, fmt.Errorf("backoff: invalid initial interval in state: %d", state.InitialInterval)
	}
	if state.MaxInterval <= 0 {
		return nil, fmt.Errorf("backoff: invalid max interval in state: %d", state.MaxInterval)
	}
	if state.CurrentInterval <= 0 {
		return nil, fmt.Errorf("backoff: invalid current interval in state: %d", state.CurrentInterval)
	}
	if math.IsNaN(state.RandomizationFactor) || state.RandomizationFactor < 0 || state.RandomizationFactor > 1 {
		return nil, fmt.Errorf("backoff: invalid randomization factor in state: %v", state.RandomizationFactor)
	}
	if math.IsNaN(state.Multiplier) || math.IsInf(state.Multiplier, 0) || state.Multiplier < 1 {
		return nil, fmt.Errorf("backoff: invalid multiplier in state: %v", state.Multiplier)
	}

	randomSource := &rand.PCG{}
	if err := randomSource.UnmarshalBinary(state.RandState); err != nil {
		return nil, fmt.Errorf("backoff: invalid random state: %w", err)
	}

	return &ExponentialBackOff{
		InitialInterval:     state.InitialInterval,
		RandomizationFactor: state.RandomizationFactor,
		Multiplier:          state.Multiplier,
		MaxInterval:         state.MaxInterval,
		currentInterval:     state.CurrentInterval,
		randomSource:        randomSource,
		random:              rand.New(randomSource),
	}, nil
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
