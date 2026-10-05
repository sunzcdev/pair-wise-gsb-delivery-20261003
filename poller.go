// Copyright © Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ntp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Default polling bounds used when a Poller is created with a zero-valued
// PollerOptions. They clamp the server-suggested wait after a RATE kiss of
// death, protecting against both impractically short and excessively long
// delays.
const (
	defaultMinPollInterval = 1 * time.Second
	defaultMaxPollInterval = 1 * time.Hour
)

// kissRate is the RATE kiss code, the only kiss code treated as a
// temporary throttle rather than a deny-class lockout.
const kissRate = "RATE"

var (
	// ErrEndpointDenied is returned by Poller.Query when a previous query
	// received a "deny"-class kiss-of-death response (such as DENY or
	// RSTR). The endpoint remains unavailable until Poller.Reset is
	// called.
	ErrEndpointDenied = errors.New("endpoint denied until explicitly reset")

	// ErrInvalidPollInterval is returned by NewPoller when the configured
	// minimum or maximum poll interval is invalid.
	ErrInvalidPollInterval = errors.New("invalid poll interval")
)

// KissAction classifies how the Poller reacts to a kiss-of-death response.
type KissAction int

const (
	// KissRate indicates a temporary rate-limit kiss of death (the RATE
	// kiss code). The next query to the endpoint is deferred until the
	// server-suggested wait, clamped to local bounds, has elapsed.
	KissRate KissAction = iota + 1

	// KissDeny indicates a deny-class kiss of death (such as DENY or
	// RSTR). The endpoint is denied until Poller.Reset is called.
	KissDeny
)

// KissError wraps a kiss-of-death response returned by a Poller. Use
// errors.Is to test for ErrKissOfDeath or ErrEndpointDenied, and inspect
// the Code and Action fields for the kiss code and the resulting state
// transition. The Response is also returned, so it remains available for
// inspection.
type KissError struct {
	// Code is the 4-character kiss code reported by the server (e.g.
	// "RATE", "DENY", "RSTR"). It may be empty if the server sent
	// non-printable reference ID bytes.
	Code string

	// Action describes how the Poller reacted to the kiss of death.
	Action KissAction

	// RetryAfter is the time at which the endpoint may be queried again.
	// It is non-zero only for the KissRate action.
	RetryAfter time.Time
}

// Error implements the error interface.
func (e *KissError) Error() string {
	code := e.Code
	if code == "" {
		code = "????"
	}
	switch e.Action {
	case KissRate:
		return fmt.Sprintf("kiss of death %q: rate limited, retry after %s",
			code, e.RetryAfter.UTC().Format(time.RFC3339Nano))
	case KissDeny:
		return fmt.Sprintf("kiss of death %q: endpoint denied until reset", code)
	default:
		return fmt.Sprintf("kiss of death %q", code)
	}
}

// Unwrap allows errors.Is to match the underlying sentinel errors.
func (e *KissError) Unwrap() error {
	if e.Action == KissDeny {
		return errors.Join(ErrKissOfDeath, ErrEndpointDenied)
	}
	return ErrKissOfDeath
}

// Clock provides access to the current time and to timers. It is used by
// the Poller to enforce rate-limit cooldowns. In production the default
// system clock is used; tests may provide a controllable fake clock so
// that cooldown behavior can be exercised without waiting real time.
type Clock interface {
	// Now returns the current time.
	Now() time.Time

	// NewTimer creates a new Timer that sends the current time on its
	// channel after at least d has elapsed.
	NewTimer(d time.Duration) Timer
}

// Timer is a cancellable timer returned by Clock.NewTimer.
type Timer interface {
	// C returns the channel on which the timer's firing time is
	// delivered.
	C() <-chan time.Time

	// Stop prevents the timer from firing. It reports whether the call
	// stopped the timer before it was scheduled to fire.
	Stop() bool
}

// systemClock is the default Clock implementation backed by the package's
// platform-specific system time.
type systemClock struct{}

func (systemClock) Now() time.Time { return getSystemTime() }

func (systemClock) NewTimer(d time.Duration) Timer {
	return &systemTimer{t: time.NewTimer(d)}
}

type systemTimer struct{ t *time.Timer }

func (t *systemTimer) C() <-chan time.Time { return t.t.C }
func (t *systemTimer) Stop() bool          { return t.t.Stop() }

// PollerOptions contains configurable options used by a Poller.
type PollerOptions struct {
	// MinPollInterval clamps the lower bound of a server-suggested wait
	// following a RATE kiss of death. If the server does not suggest a
	// wait, MinPollInterval is used. Defaults to 1 second.
	MinPollInterval time.Duration

	// MaxPollInterval clamps the upper bound of a server-suggested wait
	// following a RATE kiss of death. Defaults to 1 hour.
	MaxPollInterval time.Duration

	// Clock is used to measure cooldowns. If nil, the platform system
	// clock is used.
	Clock Clock

	// DefaultOptions holds the default QueryOptions applied to every query,
	// such as the protocol version, dialer and authentication settings.
	// Per-query options passed to Query fully replace these options when
	// non-nil, except that a nil GetSystemTime always falls back to the
	// Poller's Clock so that cooldowns and measurements share the same time
	// source.
	DefaultOptions QueryOptions
}

// Poller is an optional stateful NTP polling client. In addition to
// performing one-off queries like QueryWithOptions, it tracks per-endpoint
// state across calls:
//
//   - On a RATE kiss of death, subsequent queries to the endpoint are
//     deferred until the server-suggested wait (clamped to local bounds)
//     has elapsed.
//   - On a deny-class kiss of death (such as DENY or RSTR), the endpoint
//     is denied until Reset is called explicitly.
//
// State is keyed by the normalized endpoint address ("host:port"), so
// equivalent spellings of the same endpoint share state.
//
// Concurrent queries against the same endpoint are serialized: only one
// query at a time may be in flight, and callers requesting a query while
// one is already pending wait their turn. If a waiting query is canceled
// via its context before its request has been sent, its polling permit is
// released for another caller.
//
// Timeouts, dial failures and other ordinary errors do not alter endpoint
// state and are never reported as kisses of death. State is updated only
// by a response that passes the package's request-correlation checks.
//
// A Poller is safe for concurrent use.
type Poller struct {
	clock     Clock
	minPoll   time.Duration
	maxPoll   time.Duration
	defOpt    QueryOptions
	mu        sync.Mutex
	endpoints map[string]*epState
}

// NewPoller creates a new Poller. If opt is nil, default values are used.
func NewPoller(opt *PollerOptions) (*Poller, error) {
	p := &Poller{
		clock:     systemClock{},
		minPoll:   defaultMinPollInterval,
		maxPoll:   defaultMaxPollInterval,
		endpoints: make(map[string]*epState),
	}

	if opt != nil {
		p.defOpt = opt.DefaultOptions
		if opt.Clock != nil {
			p.clock = opt.Clock
		}
		if opt.MinPollInterval != 0 {
			p.minPoll = opt.MinPollInterval
		}
		if opt.MaxPollInterval != 0 {
			p.maxPoll = opt.MaxPollInterval
		}
	}

	if p.minPoll <= 0 {
		return nil, fmt.Errorf("%w: minimum must be positive", ErrInvalidPollInterval)
	}
	if p.maxPoll < p.minPoll {
		return nil, fmt.Errorf("%w: maximum is less than minimum", ErrInvalidPollInterval)
	}
	return p, nil
}

// Query performs an NTP query against the endpoint, applying any pending
// per-endpoint state. The address accepts the same forms documented on
// Query. The ctx is used to cancel a wait for a polling permit or a RATE
// cooldown before the request is sent; once the request is in flight the
// query follows its own configured Timeout.
//
// If opt is non-nil it is used for the query; otherwise the Poller's
// DefaultOptions are used.
//
// A kiss-of-death response is returned as a *KissError while its
// *Response is also returned. Other errors are returned unmodified and do
// not affect endpoint state.
func (p *Poller) Query(ctx context.Context, address string, opt *QueryOptions) (*Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	key, err := normalizeEndpoint(address)
	if err != nil {
		return nil, err
	}

	state := p.stateFor(key)

	// Atomically claim the single polling permit for this endpoint and
	// wait out any pending RATE cooldown. Any failure here happens before
	// any request is sent, so the permit must be released.
	if err := state.acquire(ctx, p.clock); err != nil {
		state.release()
		return nil, err
	}

	queryOpt := p.mergeOptions(opt)
	r, qerr := QueryWithOptions(address, queryOpt)

	// Only a fully correlated response is allowed to update state. A
	// timeout, dial error, parse error or request/response mismatch leaves
	// state untouched, as does an authentication failure (r != nil with a
	// non-nil error).
	if qerr == nil && r != nil && r.IsKissOfDeath() {
		switch classifyKissCode(r.KissCode) {
		case KissRate:
			delay := clampRateDelay(r.Poll, p.minPoll, p.maxPoll)
			retryAfter := p.clock.Now().Add(delay)
			state.enterCooldown(retryAfter)
			state.release()
			return r, &KissError{Code: r.KissCode, Action: KissRate, RetryAfter: retryAfter}
		case KissDeny:
			state.deny(r.KissCode)
			state.release()
			return r, &KissError{Code: r.KissCode, Action: KissDeny}
		}
	}

	// The permit is released as soon as the query finishes. A successful
	// non-KoD response also clears any stale state.
	state.release()
	if qerr == nil && r != nil {
		state.clear()
	}
	return r, qerr
}

// Reset clears all state recorded for the endpoint, including any RATE
// cooldown and deny-class lockout, allowing it to be queried again
// immediately. The address accepts the same forms documented on Query.
func (p *Poller) Reset(address string) error {
	key, err := normalizeEndpoint(address)
	if err != nil {
		return err
	}
	p.mu.Lock()
	state := p.endpoints[key]
	p.mu.Unlock()
	if state != nil {
		state.reset()
	}
	return nil
}

// EndpointState is a point-in-time snapshot of a Poller's state for one
// normalized endpoint.
type EndpointState struct {
	// Denied is true if a deny-class kiss of death has locked the
	// endpoint out until Reset is called.
	Denied bool

	// DenyCode is the kiss code that caused the lockout (e.g. "DENY" or
	// "RSTR"). It is empty when Denied is false.
	DenyCode string

	// RetryAfter is the earliest time at which another query may be sent
	// following a RATE kiss of death. It is the zero time when no
	// cooldown is pending.
	RetryAfter time.Time
}

// State returns the current state recorded for the endpoint. An endpoint
// that has never been queried, or whose last successful query cleared its
// state, returns a zero-valued EndpointState.
func (p *Poller) State(address string) (EndpointState, error) {
	key, err := normalizeEndpoint(address)
	if err != nil {
		return EndpointState{}, err
	}
	p.mu.Lock()
	state := p.endpoints[key]
	p.mu.Unlock()
	if state == nil {
		return EndpointState{}, nil
	}
	return state.snapshot(p.clock.Now()), nil
}

// stateFor returns the lazily-created epState keyed by the normalized
// endpoint.
func (p *Poller) stateFor(key string) *epState {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, ok := p.endpoints[key]
	if !ok {
		state = newEPState()
		p.endpoints[key] = state
	}
	return state
}

// mergeOptions builds the QueryOptions for a query. Explicit per-query
// options replace the Poller defaults, but GetSystemTime always falls back
// to the Poller's clock so cooldowns share the query's time source.
func (p *Poller) mergeOptions(opt *QueryOptions) QueryOptions {
	if opt != nil {
		merged := *opt
		if merged.GetSystemTime == nil {
			merged.GetSystemTime = p.clock.Now
		}
		return merged
	}
	merged := p.defOpt
	if merged.GetSystemTime == nil {
		merged.GetSystemTime = p.clock.Now
	}
	return merged
}

// normalizeEndpoint canonicalizes an address accepted by Query into a
// lower-cased "host:port" key so that equivalent spellings share state.
func normalizeEndpoint(address string) (string, error) {
	fixed, err := fixHostPort(address, defaultPort)
	if err != nil {
		return "", err
	}
	host, portStr, err := net.SplitHostPort(fixed)
	if err != nil {
		return "", err
	}
	port := defaultPort
	if v, err := strconv.Atoi(portStr); err == nil {
		port = v
	}
	host = strings.ToLower(host)
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// classifyKissCode maps a kiss code to the Poller action it triggers. RATE
// is a temporary throttle; every other code is treated as a deny-class
// lockout that requires explicit recovery.
func classifyKissCode(code string) KissAction {
	if code == kissRate {
		return KissRate
	}
	return KissDeny
}

// clampRateDelay bounds the wait suggested by a RATE response to the local
// minimum and maximum. A non-positive suggestion (including a zero Poll
// field) is replaced by the minimum.
func clampRateDelay(suggested, min, max time.Duration) time.Duration {
	switch {
	case suggested <= 0:
		return min
	case suggested < min:
		return min
	case suggested > max:
		return max
	default:
		return suggested
	}
}
