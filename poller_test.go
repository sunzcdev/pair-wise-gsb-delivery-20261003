// Copyright © Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ntp

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// fakeClock is a manually controllable implementation of Clock.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*fakeTimer]struct{}
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{
		now:    start,
		timers: make(map[*fakeTimer]struct{}),
	}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	t := &fakeTimer{
		ch:       make(chan time.Time, 1),
		deadline: c.now.Add(d),
		clock:    c,
	}
	c.mu.Lock()
	c.timers[t] = struct{}{}
	c.mu.Unlock()
	return t
}

// Advance moves the fake clock forward and fires any timers whose deadline
// has passed.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeTimer
	for t := range c.timers {
		if !t.stopped && !c.now.Before(t.deadline) {
			due = append(due, t)
		}
	}
	for _, t := range due {
		delete(c.timers, t)
		t.fired = true
		select {
		case t.ch <- t.deadline:
		default:
		}
	}
	c.mu.Unlock()
}

func (c *fakeClock) timerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for t := range c.timers {
		if !t.stopped {
			n++
		}
	}
	return n
}

// waitForTimers blocks (using real time) until n pending fake timers are
// registered.
func (c *fakeClock) waitForTimers(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.timerCount() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d fake timers (have %d)", n, c.timerCount())
}

type fakeTimer struct {
	ch       chan time.Time
	deadline time.Time
	clock    *fakeClock
	stopped  bool
	fired    bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	delete(t.clock.timers, t)
	return true
}

// fakeServer hands out fake connections that answer NTP queries entirely
// in memory, so no real network or real cooldown time is required.
type fakeServer struct {
	mu          sync.Mutex
	respond     func(req []byte) ([]byte, error)
	requests    int
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
}

func (s *fakeServer) dialer(localAddress, remoteAddress string) (net.Conn, error) {
	c := &fakeConn{srv: s}
	c.cond.L = &sync.Mutex{}
	return c, nil
}

func (s *fakeServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

type fakeConn struct {
	cond sync.Cond
	srv  *fakeServer
	req  []byte
	resp []byte
	err  error
	sent bool
}

func (c *fakeConn) Write(b []byte) (int, error) {
	c.cond.L.Lock()
	defer c.cond.L.Unlock()

	c.srv.mu.Lock()
	c.srv.requests++
	respond := c.srv.respond
	c.srv.mu.Unlock()

	c.req = append(c.req[:0], b...)

	in := c.srv.inFlight.Add(1)
	for {
		max := c.srv.maxInFlight.Load()
		if in <= max || c.srv.maxInFlight.CompareAndSwap(max, in) {
			break
		}
	}

	if respond != nil {
		c.resp, c.err = respond(c.req)
	}
	c.srv.inFlight.Add(-1)

	c.sent = true
	c.cond.Broadcast()
	return len(b), nil
}

func (c *fakeConn) Read(b []byte) (int, error) {
	c.cond.L.Lock()
	for !c.sent {
		c.cond.Wait()
	}
	c.cond.L.Unlock()

	if c.err != nil {
		return 0, c.err
	}
	return copy(b, c.resp), nil
}

func (c *fakeConn) Close() error { return nil }

func (c *fakeConn) LocalAddr() net.Addr  { return &net.UDPAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr { return &net.UDPAddr{} }

func (c *fakeConn) SetDeadline(time.Time) error     { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error {
	return nil
}

// kissID encodes a 4-character kiss code into an NTP reference ID.
func kissID(code string) uint32 {
	return binary.BigEndian.Uint32([]byte(code))
}

// v4Response builds a valid NTPv4 server response. The origin timestamp is
// echoed from the client request so the request-correlation check passes.
// When echo is false a bogus origin is written instead.
func v4Response(req []byte, stratum uint8, refID uint32, poll int8, echo bool) []byte {
	buf := make([]byte, msgSize)
	buf[0] = (4 << 3) | uint8(serverMode)
	buf[1] = stratum
	buf[2] = byte(poll)
	buf[3] = 250 // precision: 2^-6 seconds
	binary.BigEndian.PutUint32(buf[12:], refID)

	// Reference, receive and transmit timestamps must be valid and ordered.
	ref := toTimestamp(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	recv := toTimestamp(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	xmit := toTimestamp(time.Date(2026, 1, 2, 0, 0, 1, 0, time.UTC))
	binary.BigEndian.PutUint64(buf[16:], uint64(ref))
	binary.BigEndian.PutUint64(buf[32:], uint64(recv))
	binary.BigEndian.PutUint64(buf[40:], uint64(xmit))

	if echo && len(req) >= 48 {
		binary.BigEndian.PutUint64(buf[24:], binary.BigEndian.Uint64(req[40:]))
	} else {
		binary.BigEndian.PutUint64(buf[24:], 0xdeadbeefcafef00d)
	}
	return buf
}

func pollerQueryOptions(srv *fakeServer) QueryOptions {
	return QueryOptions{
		Version: 4,
		Timeout: time.Second,
		Dialer:  srv.dialer,
	}
}

func kissErrAs(t *testing.T, err error) *KissError {
	t.Helper()
	var ke *KissError
	if !errors.As(err, &ke) {
		t.Fatalf("expected *KissError, got %v", err)
	}
	return ke
}

const testAddress = "ntp.example"

func TestOfflinePollerRateCooldown(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)

	var calls atomic.Int32
	srv := &fakeServer{}
	srv.respond = func(req []byte) ([]byte, error) {
		switch calls.Add(1) {
		case 1:
			// RATE with poll exponent 6 (64 seconds).
			return v4Response(req, 0, kissID("RATE"), 6, true), nil
		default:
			// Normal stratum-2 responses after the cooldown.
			return v4Response(req, 2, refID, 6, true), nil
		}
	}

	p, err := NewPoller(&PollerOptions{
		Clock:          clock,
		DefaultOptions: pollerQueryOptions(srv),
	})
	assert.NoError(t, err)

	ctx := context.Background()

	// First query receives RATE and arms the cooldown.
	r, err := p.Query(ctx, testAddress, nil)
	var ke *KissError
	assert.True(t, errors.As(err, &ke))
	ke = kissErrAs(t, err)
	assert.Equal(t, "RATE", ke.Code)
	assert.Equal(t, KissRate, ke.Action)
	assert.Equal(t, start.Add(64*time.Second), ke.RetryAfter)
	assert.NotNil(t, r)
	assert.Equal(t, 1, srv.requestCount())

	st, err := p.State(testAddress)
	assert.NoError(t, err)
	assert.False(t, st.Denied)
	assert.Equal(t, start.Add(64*time.Second), st.RetryAfter)

	// Second query blocks on the fake clock, waiting out the cooldown.
	done := make(chan result, 1)
	go func() {
		r, err := p.Query(ctx, testAddress, nil)
		done <- result{r, err}
	}()
	clock.waitForTimers(t, 1)
	assert.Equal(t, 1, srv.requestCount(), "no request while cooling down")

	// Advance past the cooldown; the query now runs and succeeds.
	clock.Advance(64 * time.Second)
	select {
	case res := <-done:
		assert.NoError(t, res.err)
		assert.NotNil(t, res.r)
		assert.Equal(t, uint8(2), res.r.Stratum)
	case <-time.After(2 * time.Second):
		t.Fatal("second query did not complete after cooldown")
	}
	assert.Equal(t, 2, srv.requestCount())

	// Successful response clears the state.
	st, err = p.State(testAddress)
	assert.NoError(t, err)
	assert.False(t, st.Denied)
	assert.True(t, st.RetryAfter.IsZero())

	// The permit was released: a third query proceeds immediately.
	r, err = p.Query(ctx, testAddress, nil)
	assert.NoError(t, err)
	assert.NotNil(t, r)
	assert.Equal(t, 3, srv.requestCount())
}

func TestOfflinePollerDenyRequiresReset(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)

	var calls atomic.Int32
	srv := &fakeServer{}
	srv.respond = func(req []byte) ([]byte, error) {
		calls.Add(1)
		return v4Response(req, 0, kissID("RSTR"), 6, true), nil
	}

	p, err := NewPoller(&PollerOptions{
		Clock:          clock,
		DefaultOptions: pollerQueryOptions(srv),
	})
	assert.NoError(t, err)

	// First query receives the deny-class KoD.
	_, err = p.Query(context.Background(), testAddress, nil)
	ke := kissErrAs(t, err)
	assert.Equal(t, "RSTR", ke.Code)
	assert.Equal(t, KissDeny, ke.Action)
	assert.True(t, errors.Is(err, ErrKissOfDeath))
	assert.True(t, errors.Is(err, ErrEndpointDenied))

	st, err := p.State(testAddress)
	assert.NoError(t, err)
	assert.True(t, st.Denied)
	assert.Equal(t, "RSTR", st.DenyCode)

	// Advancing time does not clear a deny lockout.
	clock.Advance(time.Hour)
	_, err = p.Query(context.Background(), testAddress, nil)
	assert.ErrorIs(t, err, ErrEndpointDenied)
	assert.Equal(t, 1, srv.requestCount(), "denied query must not be sent")

	// DENY behaves the same way.
	srv.mu.Lock()
	srv.respond = func(req []byte) ([]byte, error) {
		calls.Add(1)
		return v4Response(req, 0, kissID("DENY"), 6, true), nil
	}
	srv.mu.Unlock()

	assert.NoError(t, p.Reset(testAddress))
	st, err = p.State(testAddress)
	assert.NoError(t, err)
	assert.False(t, st.Denied)

	_, err = p.Query(context.Background(), testAddress, nil)
	ke = kissErrAs(t, err)
	assert.Equal(t, "DENY", ke.Code)
	assert.Equal(t, KissDeny, ke.Action)
	assert.True(t, errors.Is(err, ErrEndpointDenied))

	// Until explicitly reset.
	assert.NoError(t, p.Reset(testAddress))
	srv.mu.Lock()
	srv.respond = func(req []byte) ([]byte, error) {
		calls.Add(1)
		return v4Response(req, 2, refID, 6, true), nil
	}
	srv.mu.Unlock()
	r, err := p.Query(context.Background(), testAddress, nil)
	assert.NoError(t, err)
	assert.NotNil(t, r)
}

func TestOfflinePollerOrdinaryErrorsDoNotBecomeKoD(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)

	srv := &fakeServer{}

	// Timeout: the connection never answers.
	srv.respond = func(req []byte) ([]byte, error) {
		return nil, errors.New("i/o timeout")
	}

	p, err := NewPoller(&PollerOptions{
		Clock: clock,
		DefaultOptions: QueryOptions{
			Version: 4,
			Timeout: time.Second,
			Dialer:  srv.dialer,
		},
	})
	assert.NoError(t, err)

	_, err = p.Query(context.Background(), testAddress, nil)
	assert.NotNil(t, err)
	assert.False(t, errors.Is(err, ErrKissOfDeath))

	st, err := p.State(testAddress)
	assert.NoError(t, err)
	assert.False(t, st.Denied)
	assert.True(t, st.RetryAfter.IsZero())

	// A spoofed KoD whose origin timestamp does not match fails the
	// correlation check and must not update state.
	srv.mu.Lock()
	srv.respond = func(req []byte) ([]byte, error) {
		return v4Response(req, 0, kissID("DENY"), 6, false), nil
	}
	srv.mu.Unlock()

	r, err := p.Query(context.Background(), testAddress, nil)
	assert.Nil(t, r)
	assert.ErrorIs(t, err, ErrServerResponseMismatch)

	st, err = p.State(testAddress)
	assert.NoError(t, err)
	assert.False(t, st.Denied, "uncorrelated KoD must not deny the endpoint")
	assert.True(t, st.RetryAfter.IsZero())

	// Permit was released after the failed queries: a good query runs.
	srv.mu.Lock()
	srv.respond = func(req []byte) ([]byte, error) {
		return v4Response(req, 2, refID, 6, true), nil
	}
	srv.mu.Unlock()
	r, err = p.Query(context.Background(), testAddress, nil)
	assert.NoError(t, err)
	assert.NotNil(t, r)
}

type result struct {
	r   *Response
	err error
}

func TestOfflinePollerCancelReleasesPermit(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)

	var calls atomic.Int32
	srv := &fakeServer{}
	srv.respond = func(req []byte) ([]byte, error) {
		switch calls.Add(1) {
		case 1:
			return v4Response(req, 0, kissID("RATE"), 6, true), nil
		default:
			return v4Response(req, 2, refID, 6, true), nil
		}
	}

	p, err := NewPoller(&PollerOptions{
		Clock:          clock,
		DefaultOptions: pollerQueryOptions(srv),
	})
	assert.NoError(t, err)

	// Arm a cooldown.
	_, err = p.Query(context.Background(), testAddress, nil)
	kissErrAs(t, err)

	// A second query holds the permit and waits for the cooldown. Cancel
	// it before the request is sent.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan result, 1)
	go func() {
		r, err := p.Query(ctx, testAddress, nil)
		done <- result{r, err}
	}()
	clock.waitForTimers(t, 1)
	cancel()

	select {
	case res := <-done:
		assert.ErrorIs(t, res.err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("canceled query did not return")
	}
	assert.Equal(t, 1, srv.requestCount())

	// The permit was released, even though the cooldown is still pending.
	// Reset clears it and a new query can claim the permit and run.
	assert.NoError(t, p.Reset(testAddress))
	r, err := p.Query(context.Background(), testAddress, nil)
	assert.NoError(t, err)
	assert.NotNil(t, r)
	assert.Equal(t, 2, srv.requestCount())
}

func TestOfflinePollerResetInterruptsCooldown(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)

	var calls atomic.Int32
	srv := &fakeServer{}
	srv.respond = func(req []byte) ([]byte, error) {
		switch calls.Add(1) {
		case 1:
			return v4Response(req, 0, kissID("RATE"), 6, true), nil
		default:
			return v4Response(req, 2, refID, 6, true), nil
		}
	}

	p, err := NewPoller(&PollerOptions{
		Clock:          clock,
		DefaultOptions: pollerQueryOptions(srv),
	})
	assert.NoError(t, err)

	_, err = p.Query(context.Background(), testAddress, nil)
	kissErrAs(t, err)

	done := make(chan result, 1)
	go func() {
		r, err := p.Query(context.Background(), testAddress, nil)
		done <- result{r, err}
	}()
	clock.waitForTimers(t, 1)

	// Reset wakes the waiter immediately; no fake time advancement needed.
	assert.NoError(t, p.Reset(testAddress))
	select {
	case res := <-done:
		assert.NoError(t, res.err)
		assert.NotNil(t, res.r)
	case <-time.After(2 * time.Second):
		t.Fatal("query did not resume after reset")
	}
	assert.Equal(t, 2, srv.requestCount())
}

func TestOfflinePollerSerializesConcurrentQueries(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)

	var calls atomic.Int32
	srv := &fakeServer{}
	srv.respond = func(req []byte) ([]byte, error) {
		calls.Add(1)
		return v4Response(req, 2, refID, 6, true), nil
	}

	p, err := NewPoller(&PollerOptions{
		Clock:          clock,
		DefaultOptions: pollerQueryOptions(srv),
	})
	assert.NoError(t, err)

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			r, err := p.Query(context.Background(), testAddress, nil)
			assert.NoError(t, err)
			assert.NotNil(t, r)
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(n), calls.Load())
	assert.Equal(t, int32(1), srv.maxInFlight.Load(),
		"only one query at a time may touch the endpoint")
}

func TestOfflineNormalizeEndpoint(t *testing.T) {
	cases := []struct {
		address string
		key     string
		err     bool
	}{
		{"example.com", "example.com:123", false},
		{"Example.COM:123", "example.com:123", false},
		{"10.0.0.1:9999", "10.0.0.1:9999", false},
		{"[2001:DB8::1]", "[2001:db8::1]:123", false},
		{"[2001:db8::1]:9999", "[2001:db8::1]:9999", false},
		{"[fe80::1%eth0]:123", "[fe80::1%eth0]:123", false},
		{"", "", true},
		{"[fe80::", "", true},
	}

	for _, c := range cases {
		key, err := normalizeEndpoint(c.address)
		if c.err {
			assert.NotNil(t, err, c.address)
			continue
		}
		assert.NoError(t, err, c.address)
		assert.Equal(t, c.key, key, c.address)
	}
}

func TestOfflineNormalizedEndpointsShareState(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)

	var calls atomic.Int32
	srv := &fakeServer{}
	srv.respond = func(req []byte) ([]byte, error) {
		calls.Add(1)
		return v4Response(req, 0, kissID("DENY"), 6, true), nil
	}

	p, err := NewPoller(&PollerOptions{
		Clock:          clock,
		DefaultOptions: pollerQueryOptions(srv),
	})
	assert.NoError(t, err)

	_, err = p.Query(context.Background(), "ntp.example", nil)
	kissErrAs(t, err)

	// An equivalent spelling of the same endpoint shares the deny state.
	_, err = p.Query(context.Background(), "NTP.EXAMPLE:123", nil)
	assert.ErrorIs(t, err, ErrEndpointDenied)
	assert.Equal(t, int32(1), calls.Load())

	// A different endpoint is unaffected.
	_, err = p.Query(context.Background(), "other.example", nil)
	kissErrAs(t, err)
	assert.Equal(t, int32(2), calls.Load())
}

func TestOfflineNewPollerValidation(t *testing.T) {
	_, err := NewPoller(&PollerOptions{MinPollInterval: 0, MaxPollInterval: 0})
	assert.NoError(t, err, "zero values fall back to defaults")

	_, err = NewPoller(&PollerOptions{MinPollInterval: -time.Second})
	assert.ErrorIs(t, err, ErrInvalidPollInterval)

	_, err = NewPoller(&PollerOptions{
		MinPollInterval: 10 * time.Second,
		MaxPollInterval: time.Second,
	})
	assert.ErrorIs(t, err, ErrInvalidPollInterval)
}

func TestOfflineClampRateDelay(t *testing.T) {
	min := 2 * time.Second
	max := 32 * time.Second
	cases := []struct {
		in, out time.Duration
	}{
		{0, min},
		{-time.Second, min},
		{time.Second, min},
		{5 * time.Second, 5 * time.Second},
		{32 * time.Second, 32 * time.Second},
		{time.Hour, max},
	}
	for _, c := range cases {
		assert.Equal(t, c.out, clampRateDelay(c.in, min, max))
	}
}

func TestOfflinePollerClampsServerSuggestion(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)

	var calls atomic.Int32
	srv := &fakeServer{}
	srv.respond = func(req []byte) ([]byte, error) {
		if calls.Add(1) == 1 {
			// Poll exponent 17 = ~36 hours, far above the 32s max.
			return v4Response(req, 0, kissID("RATE"), 17, true), nil
		}
		return v4Response(req, 2, refID, 6, true), nil
	}

	p, err := NewPoller(&PollerOptions{
		Clock:           clock,
		MinPollInterval: 2 * time.Second,
		MaxPollInterval: 32 * time.Second,
		DefaultOptions:  pollerQueryOptions(srv),
	})
	assert.NoError(t, err)

	_, err = p.Query(context.Background(), testAddress, nil)
	ke := kissErrAs(t, err)
	assert.Equal(t, start.Add(32*time.Second), ke.RetryAfter)

	done := make(chan result, 1)
	go func() {
		r, err := p.Query(context.Background(), testAddress, nil)
		done <- result{r, err}
	}()
	clock.waitForTimers(t, 1)

	// Advancing only to the minimum must not release the query.
	clock.Advance(2 * time.Second)
	assert.Equal(t, int32(1), calls.Load())

	clock.Advance(30 * time.Second)
	select {
	case res := <-done:
		assert.NoError(t, res.err)
	case <-time.After(2 * time.Second):
		t.Fatal("query did not run after clamped cooldown")
	}
	assert.Equal(t, int32(2), calls.Load())
}

func TestOfflineClassifyKissCode(t *testing.T) {
	assert.Equal(t, KissRate, classifyKissCode("RATE"))
	assert.Equal(t, KissDeny, classifyKissCode("DENY"))
	assert.Equal(t, KissDeny, classifyKissCode("RSTR"))
	assert.Equal(t, KissDeny, classifyKissCode("STEP"))
	assert.Equal(t, KissDeny, classifyKissCode(""))
}
