// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ntp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Default bounds applied to the delay suggested by a RATE kiss-of-death
// response when no PollerOptions bounds are specified.
const (
	defaultMinRateBackoff = 1 * time.Second
	defaultMaxRateBackoff = 24 * time.Hour
)

// KissClass categorizes a kiss-of-death code by the effect it has on the
// polling state maintained for an endpoint.
type KissClass uint8

const (
	// KissUnknown is the class assigned to a kiss-of-death response whose
	// code is not recognized as a rate-limiting or denial code.
	KissUnknown KissClass = iota

	// KissRate is the class of the RATE code. It asks the client to slow
	// down temporarily.
	KissRate

	// KissDeny is the class of access-denial codes (DENY, RSTR). The
	// endpoint remains blocked until the Poller is explicitly reset.
	KissDeny
)

// Sentinel errors returned in association with kiss-of-death responses.
var (
	// ErrEndpointBlocked is returned when a query is attempted against an
	// endpoint that remains blocked by a prior DENY or RSTR response.
	ErrEndpointBlocked = errors.New("ntp: endpoint is blocked")
)

// EndpointState is a point-in-time snapshot of the polling state maintained
// by a Poller for a single normalized endpoint.
type EndpointState struct {
	// Endpoint is the normalized "host:port" address to which this state
	// pertains.
	Endpoint string

	// InFlight is true if another caller currently holds the endpoint's
	// single polling permit and has an outstanding query.
	InFlight bool

	// Blocked is true if the most recent validated response carried a
	// denial kiss code (DENY or RSTR). Queries are refused while Blocked
	// is true, and the block persists until Reset or ResetAll is called.
	Blocked bool

	// KissCode is the kiss code reported by the most recent validated
	// kiss-of-death response. It is the empty string if no kiss-of-death
	// response has been recorded.
	KissCode string

	// CooldownUntil is the time at which a temporary rate-limit (RATE)
	// imposed on the endpoint expires. It is the zero time if no cooldown
	// is in effect or it has already expired.
	CooldownUntil time.Time
}

// PollerOptions contains configurable options used by a Poller.
type PollerOptions struct {
	// QueryOptions holds the default options used for every query issued
	// by the Poller. Per-query values supplied to Poller.Query override
	// the matching fields.
	QueryOptions

	// MinRateBackoff is the minimum delay applied after a RATE
	// kiss-of-death response. The server-suggested delay is clamped to
	// the range [MinRateBackoff, MaxRateBackoff]. Defaults to 1 second.
	MinRateBackoff time.Duration

	// MaxRateBackoff is the maximum delay applied after a RATE
	// kiss-of-death response. The server-suggested delay is clamped to
	// the range [MinRateBackoff, MaxRateBackoff]. Defaults to 24 hours.
	MaxRateBackoff time.Duration

	// Clock is used to obtain the current time and to schedule the wait
	// for a RATE-imposed cooldown. If nil, the system clock is used.
	// Specifying a custom Clock (such as a controllable fake clock)
	// allows cooldown behavior to be tested without waiting in real
	// time. The same clock is also used to timestamp outgoing queries
	// unless QueryOptions.GetSystemTime is set explicitly.
	Clock Clock
}

// A Poller is an optional, stateful polling client used in place of the
// stateless Query and QueryWithOptions functions. It tracks kiss-of-death
// responses on a per-endpoint basis.
//
// A RATE response temporarily delays the next query to the same endpoint
// by the server-suggested interval, clamped to configured local bounds.
// Denial responses (DENY and RSTR) block the endpoint until it is
// explicitly recovered with Reset or ResetAll.
//
// Only responses that pass the request-correlation check (and any
// configured authentication) update this state; timeouts, network errors,
// and mismatched responses are reported as ordinary errors and never
// masquerade as a kiss of death.
//
// Concurrent calls targeting the same endpoint serialize: the Poller
// atomically hands out one polling permit per endpoint, so only one query
// is in flight at a time. A call canceled before its query is sent (for
// example, while waiting out a cooldown) releases the permit without
// contacting the server.
type Poller struct {
	opt   PollerOptions
	clock Clock

	mu     sync.Mutex
	states map[string]*endpointState
}

// endpointState holds the mutable polling state of one normalized endpoint.
// All fields are guarded by the owning Poller's mutex.
type endpointState struct {
	key           string
	permit        chan struct{}
	inFlight      bool
	blocked       bool
	kissCode      string
	cooldownUntil time.Time
}

// NewPoller creates a Poller configured with the provided options.
func NewPoller(opt PollerOptions) *Poller {
	if opt.MinRateBackoff <= 0 {
		opt.MinRateBackoff = defaultMinRateBackoff
	}
	if opt.MaxRateBackoff <= 0 {
		opt.MaxRateBackoff = defaultMaxRateBackoff
	}
	if opt.MinRateBackoff > opt.MaxRateBackoff {
		opt.MinRateBackoff = opt.MaxRateBackoff
	}

	clock := opt.Clock
	if clock == nil {
		clock = systemClock{}
	}
	if opt.GetSystemTime == nil {
		opt.GetSystemTime = clock.Now
	}

	return &Poller{
		opt:    opt,
		clock:  clock,
		states: make(map[string]*endpointState),
	}
}

// Query performs a stateful NTP query of the remote server at the
// specified address, which follows the same formatting rules as the
// address parameter of QueryWithOptions.
//
// If the endpoint is rate-limited from a prior RATE response, Query waits
// until the cooldown expires or ctx is canceled. If the endpoint is
// blocked by a prior DENY or RSTR response, Query immediately returns an
// error wrapping ErrEndpointBlocked without contacting the server.
//
// The ctx governs only the wait for a polling permit or cooldown. Once a
// query has actually been sent, its lifetime is bounded by the Timeout in
// the effective QueryOptions, as with QueryWithOptions.
//
// A validated kiss-of-death response is returned as a *KissError wrapping
// ErrKissOfDeath.
func (p *Poller) Query(ctx context.Context, address string, opt *QueryOptions) (*Response, error) {
	effective := p.opt.QueryOptions
	if opt != nil {
		effective = *opt
		if effective.GetSystemTime == nil {
			effective.GetSystemTime = p.opt.GetSystemTime
		}
		if effective.Dialer == nil {
			effective.Dialer = p.opt.Dialer
		}
		if effective.Dial == nil {
			effective.Dial = p.opt.Dial
		}
	}

	key, err := normalizeEndpoint(address, effective.Port)
	if err != nil {
		return nil, err
	}

	// A denial persists independently of the polling permit, so fail fast
	// before claiming the permit or contacting the server.
	if err := p.checkBlocked(key); err != nil {
		return nil, err
	}

	st, err := p.acquire(ctx, key)
	if err != nil {
		return nil, err
	}
	defer p.release(key)

	if err := p.waitForReady(ctx, st); err != nil {
		return nil, err
	}

	r, qerr := QueryWithOptions(key, effective)
	if qerr == nil && r != nil && r.IsKissOfDeath() {
		// A successfully correlated kiss-of-death response is the only
		// kind of error allowed to update the endpoint's polling state.
		qerr = p.recordKiss(st, r)
	} else if qerr == nil && r != nil {
		// A valid time response clears any earlier RATE cooldown, though
		// a DENY/RSTR block is recoverable only via Reset.
		p.clearCooldown(st)
	}
	return r, qerr
}

// clearCooldown removes a RATE cooldown following a successful query.
func (p *Poller) clearCooldown(st *endpointState) {
	p.mu.Lock()
	st.cooldownUntil = time.Time{}
	p.mu.Unlock()
}

// acquire atomically claims the endpoint's single polling permit, creating
// the endpoint state on first use. If ctx is canceled before the permit is
// claimed, no query is sent and no permit is retained.
func (p *Poller) acquire(ctx context.Context, key string) (*endpointState, error) {
	p.mu.Lock()
	st, ok := p.states[key]
	if !ok {
		st = &endpointState{
			key:    key,
			permit: make(chan struct{}, 1),
		}
		p.states[key] = st
	}
	permit := st.permit
	p.mu.Unlock()

	select {
	case permit <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	p.mu.Lock()
	st.inFlight = true
	p.mu.Unlock()
	return st, nil
}

// release returns the endpoint's polling permit.
func (p *Poller) release(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[key]
	if st == nil {
		return
	}
	st.inFlight = false
	select {
	case <-st.permit:
	default:
	}
}

// waitForReady reports an error if the endpoint is blocked, otherwise it
// waits for any RATE-imposed cooldown to elapse.
func (p *Poller) waitForReady(ctx context.Context, st *endpointState) error {
	for {
		p.mu.Lock()
		blocked := st.blocked
		until := st.cooldownUntil
		p.mu.Unlock()
		if blocked {
			return ErrEndpointBlocked
		}

		now := p.clock.Now()
		if !until.After(now) {
			return nil
		}

		t := p.clock.NewTimer(until.Sub(now))
		select {
		case <-t.C():
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
	}
}

// checkBlocked returns a blocked KissError if the endpoint is currently
// denied, without claiming its polling permit.
func (p *Poller) checkBlocked(key string) error {
	p.mu.Lock()
	st := p.states[key]
	blocked := st != nil && st.blocked
	kiss := ""
	if st != nil {
		kiss = st.kissCode
	}
	p.mu.Unlock()
	if !blocked {
		return nil
	}
	return &KissError{
		Code:    kiss,
		class:   KissDeny,
		blocked: true,
	}
}

// recordKiss updates the endpoint state to reflect a validated
// kiss-of-death response and returns the error surfaced to the caller.
func (p *Poller) recordKiss(st *endpointState, r *Response) error {
	code := r.KissCode
	class := classifyKiss(code)

	p.mu.Lock()
	st.kissCode = code
	switch class {
	case KissRate:
		st.blocked = false
		st.cooldownUntil = p.clock.Now().Add(p.rateBackoff(r.Poll))
	case KissDeny:
		st.blocked = true
		st.cooldownUntil = time.Time{}
	}
	p.mu.Unlock()

	return &KissError{Code: code, class: class}
}

// rateBackoff clamps the server-suggested RATE delay to the configured
// local bounds.
func (p *Poller) rateBackoff(suggested time.Duration) time.Duration {
	d := suggested
	if d < p.opt.MinRateBackoff {
		d = p.opt.MinRateBackoff
	}
	if d > p.opt.MaxRateBackoff {
		d = p.opt.MaxRateBackoff
	}
	return d
}

// State returns a snapshot of the polling state for the specified endpoint.
// The address is normalized the same way as in Query. The boolean result is
// false if the endpoint has never been queried through the Poller.
func (p *Poller) State(address string) (EndpointState, bool) {
	key, err := normalizeEndpoint(address, p.opt.Port)
	if err != nil {
		return EndpointState{Endpoint: address}, false
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[key]
	if st == nil {
		return EndpointState{Endpoint: key}, false
	}

	snap := EndpointState{
		Endpoint: st.key,
		InFlight: st.inFlight,
		Blocked:  st.blocked,
		KissCode: st.kissCode,
	}
	if st.cooldownUntil.After(p.clock.Now()) {
		snap.CooldownUntil = st.cooldownUntil
	}
	return snap, true
}

// Reset clears the polling state of the specified endpoint, removing any
// RATE cooldown or DENY/RSTR block. The address is normalized the same way
// as in Query. Reset does not affect a query currently in flight, but the
// endpoint is queryable again once that query finishes.
func (p *Poller) Reset(address string) error {
	key, err := normalizeEndpoint(address, p.opt.Port)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if st := p.states[key]; st != nil {
		st.blocked = false
		st.kissCode = ""
		st.cooldownUntil = time.Time{}
	}
	return nil
}

// ResetAll clears the polling state of every endpoint. It does not affect
// queries currently in flight.
func (p *Poller) ResetAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, st := range p.states {
		st.blocked = false
		st.kissCode = ""
		st.cooldownUntil = time.Time{}
	}
}

// normalizeEndpoint returns the canonical "host:port" form used to key
// polling state, so addresses differing only in port or bracket notation
// share state.
func normalizeEndpoint(address string, defaultPort int) (string, error) {
	if defaultPort == 0 {
		defaultPort = defaultPortValue
	}
	return fixHostPort(address, defaultPort)
}

// classifyKiss maps a kiss code to its polling-state effect.
func classifyKiss(code string) KissClass {
	switch strings.ToUpper(code) {
	case "RATE":
		return KissRate
	case "DENY", "RSTR":
		return KissDeny
	default:
		return KissUnknown
	}
}

// KissError is returned by Poller.Query when a validated response carries a
// kiss of death. It wraps ErrKissOfDeath and reports the server's kiss
// code and its effect on polling state. A KissError for which Blocked
// reports true is also returned when querying an endpoint already blocked
// by a prior DENY or RSTR response, in which case it additionally wraps
// ErrEndpointBlocked.
type KissError struct {
	// Code is the 4-character kiss code reported by the server. It may be
	// empty if the server did not include a printable code.
	Code string

	class   KissClass
	blocked bool
}

// Class reports the kind of kiss code received.
func (e *KissError) Class() KissClass {
	return e.class
}

// IsRate reports whether the kiss code asks the client to slow down.
func (e *KissError) IsRate() bool {
	return e.class == KissRate
}

// IsDeny reports whether the kiss code denies access to the endpoint.
func (e *KissError) IsDeny() bool {
	return e.class == KissDeny
}

// Blocked reports whether the error reflects an endpoint that remains
// blocked pending an explicit Poller reset, either because the most recent
// response carried a DENY or RSTR kiss code or because such a response
// was received earlier.
func (e *KissError) Blocked() bool {
	return e.blocked
}

func (e *KissError) Error() string {
	switch {
	case e.blocked:
		return fmt.Sprintf("ntp: endpoint is blocked by %s kiss of death: %v",
			e.Code, ErrEndpointBlocked)
	case e.class == KissRate:
		return fmt.Sprintf("ntp: RATE kiss of death received (%s): %v",
			e.Code, ErrKissOfDeath)
	case e.class == KissDeny:
		return fmt.Sprintf("ntp: %s kiss of death received: %v",
			e.Code, ErrKissOfDeath)
	case e.Code == "":
		return fmt.Sprintf("ntp: kiss of death received: %v", ErrKissOfDeath)
	default:
		return fmt.Sprintf("ntp: %s kiss of death received: %v",
			e.Code, ErrKissOfDeath)
	}
}

// Unwrap allows errors.Is to recognize the underlying condition. A
// blocked endpoint wraps ErrEndpointBlocked; any other kiss of death wraps
// ErrKissOfDeath.
func (e *KissError) Unwrap() error {
	if e.blocked {
		return ErrEndpointBlocked
	}
	return ErrKissOfDeath
}
