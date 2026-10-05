// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ntp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testEndpoint = "mock.example.com:123"

// fakeClock is a manually controllable Clock. Timers created against it
// fire only when Advance moves the clock to or past their deadline, which
// lets cooldown behavior be tested without any real waiting.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock() *fakeClock {
	return &fakeClock{
		now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{
		clock: c,
		at:    c.now.Add(d),
		ch:    make(chan time.Time, 1),
	}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves the clock forward by d and fires every due, unstopped
// timer exactly once.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	fired := make([]*fakeTimer, 0)
	remaining := c.timers[:0]
	for _, t := range c.timers {
		t.mu.Lock()
		if !t.stopped && !c.now.Before(t.at) {
			t.stopped = true
			fired = append(fired, t)
		} else if !t.stopped {
			remaining = append(remaining, t)
		}
		t.mu.Unlock()
	}
	c.timers = remaining
	c.mu.Unlock()

	for _, t := range fired {
		t.ch <- t.at
	}
}

// pendingTimers returns the number of timers waiting to fire.
func (c *fakeClock) pendingTimers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if c.now.Before(t.at) {
			n++
		}
	}
	return n
}

// hasPendingTimer reports whether any timer is still waiting to fire.
func (c *fakeClock) hasPendingTimer() bool {
	return c.pendingTimers() > 0
}

type fakeTimer struct {
	clock   *fakeClock
	at      time.Time
	ch      chan time.Time
	mu      sync.Mutex
	stopped bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	c := t.clock
	c.mu.Lock()
	defer c.mu.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	wasActive := !t.stopped
	if wasActive {
		t.stopped = true
		c := t.clock
		remaining := c.timers[:0]
		for _, x := range c.timers {
			if x != t {
				remaining = append(remaining, x)
			}
		}
		c.timers = remaining
	}
	return wasActive
}

// mockV4Conn is a local fake connection that answers a client NTPv4 query
// with a scripted response. It honors the random origin timestamp, so the
// request-correlation check passes unless Mismatch is set.
type mockV4Conn struct {
	mu       sync.Mutex
	clock    *fakeClock
	kissCode string // empty means a normal stratum-2 response
	stratum  uint8
	poll     int8
	mismatch bool
	failRead bool

	request  []byte
	dials    int
	closed   bool
	release  chan struct{} // optional: release the response when signaled
	deadline time.Time
}

func newMockV4Conn(clock *fakeClock) *mockV4Conn {
	return &mockV4Conn{clock: clock, stratum: 2, poll: 6}
}

func (c *mockV4Conn) Read(b []byte) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, fmt.Errorf("read from closed connection")
	}
	if c.failRead {
		c.mu.Unlock()
		return 0, errors.New("simulated read failure")
	}
	if len(c.request) < msgSize {
		c.mu.Unlock()
		return 0, ErrInvalidTime
	}
	release := c.release
	c.mu.Unlock()

	if release != nil {
		c.mu.Lock()
		var timerCh <-chan time.Time
		if !c.deadline.IsZero() {
			d := time.Until(c.deadline)
			if d <= 0 {
				c.mu.Unlock()
				return 0, errMockTimeout
			}
			timer := time.NewTimer(d)
			defer timer.Stop()
			timerCh = timer.C
		}
		c.mu.Unlock()

		select {
		case <-release:
		case <-timerCh:
			return 0, errMockTimeout
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	origin := c.request[40:48]
	now := c.clock.Now()

	m := &messageV4{
		Stratum:        c.stratum,
		Poll:           c.poll,
		Precision:      -20,
		RootDelay:      timeShortV4(1 << 15), // 0.5 s
		RootDispersion: timeShortV4(1 << 14), // 0.25 s
		ReceiveTime:    toTimestamp(now),
		TransmitTime:   toTimestamp(now.Add(10 * time.Millisecond)),
	}
	if c.kissCode != "" {
		m.Stratum = 0
		m.ReferenceID = kissCodeID(c.kissCode)
	}
	if c.mismatch {
		// Corrupt the echoed origin timestamp so the correlation check
		// fails and the response cannot update polling state.
		var bogus [8]byte
		for i := range bogus {
			bogus[i] = 0xff
		}
		origin = bogus[:]
	}
	m.setVersion(4)
	m.setMode(serverMode)
	m.setLeap(LeapNoWarning)

	out := make([]byte, msgSize)
	out[0] = m.LiVnMode
	out[1] = m.Stratum
	out[2] = byte(m.Poll)
	out[3] = byte(m.Precision)
	binary.BigEndian.PutUint32(out[4:8], uint32(m.RootDelay))
	binary.BigEndian.PutUint32(out[8:12], uint32(m.RootDispersion))
	binary.BigEndian.PutUint32(out[12:16], m.ReferenceID)
	// Reference time at 16:24 stays zero; origin 24:32, receive
	// 32:40, transmit 40:48.
	copy(out[24:32], origin)
	binary.BigEndian.PutUint64(out[32:40], uint64(m.ReceiveTime))
	binary.BigEndian.PutUint64(out[40:48], uint64(m.TransmitTime))

	copy(b, out)
	return len(out), nil
}

func (c *mockV4Conn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, fmt.Errorf("write to closed connection")
	}
	c.request = append(c.request[:0], b...)
	return len(b), nil
}

// errMockTimeout is a net.Error-style timeout used by the mock
// connection when its read deadline elapses.
var errMockTimeout = &mockTimeoutError{}

type mockTimeoutError struct{}

func (*mockTimeoutError) Error() string   { return "simulated read deadline exceeded" }
func (*mockTimeoutError) Timeout() bool   { return true }
func (*mockTimeoutError) Temporary() bool { return true }

func (c *mockV4Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *mockV4Conn) LocalAddr() net.Addr  { return nil }
func (c *mockV4Conn) RemoteAddr() net.Addr { return nil }
func (c *mockV4Conn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}

func (c *mockV4Conn) SetReadDeadline(t time.Time) error { return c.SetDeadline(t) }
func (c *mockV4Conn) SetWriteDeadline(time.Time) error  { return nil }

func kissCodeID(code string) uint32 {
	if len(code) != 4 {
		return 0
	}
	return uint32(code[0])<<24 | uint32(code[1])<<16 |
		uint32(code[2])<<8 | uint32(code[3])
}

func timestampFromBytes(b []byte) timestamp {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return timestamp(v)
}

// newTestPoller builds a Poller whose queries are served by freshly
// created mock connections produced by nextConn.
func newTestPoller(clock *fakeClock, nextConn func() *mockV4Conn) *Poller {
	return NewPoller(PollerOptions{
		Clock: clock,
		QueryOptions: QueryOptions{
			Version: 4,
			Timeout: 5 * time.Second,
			Dialer: func(_, _ string) (net.Conn, error) {
				c := nextConn()
				c.mu.Lock()
				c.dials++
				c.mu.Unlock()
				return c, nil
			},
		},
	})
}

func TestOfflinePollerSuccess(t *testing.T) {
	clock := newFakeClock()
	var conn *mockV4Conn
	poller := newTestPoller(clock, func() *mockV4Conn {
		conn = newMockV4Conn(clock)
		return conn
	})

	r, err := poller.Query(context.Background(), "mock.example.com", nil)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.Equal(t, uint8(2), r.Stratum)
	assert.False(t, r.IsKissOfDeath())
	assert.True(t, conn.closed)

	snap, ok := poller.State(testEndpoint)
	require.True(t, ok)
	assert.False(t, snap.Blocked)
	assert.False(t, snap.InFlight)
	assert.True(t, snap.CooldownUntil.IsZero())
}

func TestOfflinePollerRateCooldown(t *testing.T) {
	clock := newFakeClock()
	var conns []*mockV4Conn
	first := newMockV4Conn(clock)
	first.kissCode = "RATE"
	first.poll = 6
	second := newMockV4Conn(clock)
	poller := newTestPoller(clock, func() *mockV4Conn {
		next := first
		if len(conns) == 1 {
			next = second
		}
		conns = append(conns, next)
		return next
	})
	poller.opt.MinRateBackoff = 2 * time.Second
	poller.opt.MaxRateBackoff = 10 * time.Minute

	// First query gets a RATE suggesting a 64-second poll interval.
	r, err := poller.Query(context.Background(), testEndpoint, nil)
	require.Error(t, err)
	require.NotNil(t, r)
	assert.True(t, r.IsKissOfDeath())

	var ke *KissError
	require.ErrorAs(t, err, &ke)
	assert.Equal(t, "RATE", ke.Code)
	assert.True(t, ke.IsRate())
	assert.True(t, errors.Is(err, ErrKissOfDeath))

	snap, ok := poller.State(testEndpoint)
	require.True(t, ok)
	assert.False(t, snap.Blocked)
	wantUntil := clock.Now().Add(64 * time.Second)
	assert.True(t, snap.CooldownUntil.Equal(wantUntil))

	// A second immediate query must wait and must not dial early.
	done := make(chan error, 1)
	go func() {
		_, err := poller.Query(context.Background(), testEndpoint, nil)
		done <- err
	}()

	require.Eventually(t, clock.hasPendingTimer, time.Second, time.Millisecond)
	assert.Equal(t, 1, first.dials)
	assert.Equal(t, 0, second.dials)

	// Advancing short of the deadline keeps the query waiting.
	clock.Advance(63 * time.Second)
	select {
	case <-done:
		t.Fatal("query completed before cooldown expired")
	case <-time.After(20 * time.Millisecond):
	}
	assert.Equal(t, 0, second.dials)

	// Crossing the deadline unblocks the query.
	clock.Advance(1 * time.Second)
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("query did not complete after cooldown expired")
	}
	assert.Equal(t, 1, second.dials)

	// The successful query cleared the cooldown.
	snap, _ = poller.State(testEndpoint)
	assert.True(t, snap.CooldownUntil.IsZero())
	assert.False(t, snap.InFlight)
}

func TestOfflinePollerRateClampedToBounds(t *testing.T) {
	clock := newFakeClock()
	c := newMockV4Conn(clock)
	poller := newTestPoller(clock, func() *mockV4Conn { return c })
	poller.opt.MinRateBackoff = 30 * time.Second
	poller.opt.MaxRateBackoff = 10 * time.Minute

	c.kissCode = "RATE"
	c.poll = 2 // server suggests only 4 seconds
	_, err := poller.Query(context.Background(), testEndpoint, nil)
	require.Error(t, err)
	snap, _ := poller.State(testEndpoint)
	assert.True(t, snap.CooldownUntil.Equal(clock.Now().Add(30*time.Second)))

	require.NoError(t, poller.Reset(testEndpoint))

	c2 := newMockV4Conn(clock)
	c2.kissCode = "RATE"
	c2.poll = 20 // server suggests ~12 days
	poller.opt.QueryOptions.Dialer = func(_, _ string) (net.Conn, error) { return c2, nil }
	_, err = poller.Query(context.Background(), testEndpoint, nil)
	require.Error(t, err)
	snap, _ = poller.State(testEndpoint)
	assert.True(t, snap.CooldownUntil.Equal(clock.Now().Add(10*time.Minute)))
}

func TestOfflinePollerDenyCodesBlock(t *testing.T) {
	for _, code := range []string{"DENY", "RSTR"} {
		t.Run(code, func(t *testing.T) {
			clock := newFakeClock()
			c := newMockV4Conn(clock)
			c.kissCode = code
			dials := 0
			poller := newTestPoller(clock, func() *mockV4Conn {
				dials++
				return c
			})

			r, err := poller.Query(context.Background(), testEndpoint, nil)
			require.Error(t, err)
			require.NotNil(t, r)

			var ke *KissError
			require.ErrorAs(t, err, &ke)
			assert.True(t, ke.IsDeny())
			assert.False(t, ke.Blocked())
			assert.True(t, errors.Is(err, ErrKissOfDeath))

			snap, _ := poller.State(testEndpoint)
			assert.True(t, snap.Blocked)
			assert.Equal(t, code, snap.KissCode)

			// Even after advancing the fake clock, the endpoint remains
			// blocked and is never dialed again.
			clock.Advance(24 * time.Hour)
			_, err = poller.Query(context.Background(), testEndpoint, nil)
			require.Error(t, err)
			require.ErrorAs(t, err, &ke)
			assert.True(t, ke.Blocked())
			assert.True(t, errors.Is(err, ErrEndpointBlocked))
			assert.False(t, errors.Is(err, ErrKissOfDeath))
			assert.Equal(t, 1, dials)

			// Recovery requires an explicit reset.
			require.NoError(t, poller.Reset(testEndpoint))

			ok := newMockV4Conn(clock)
			poller.opt.QueryOptions.Dialer = func(_, _ string) (net.Conn, error) {
				return ok, nil
			}
			r2, err := poller.Query(context.Background(), testEndpoint, nil)
			require.NoError(t, err)
			assert.Equal(t, uint8(2), r2.Stratum)

			snap, _ = poller.State(testEndpoint)
			assert.False(t, snap.Blocked)
			assert.Equal(t, "", snap.KissCode)
		})
	}
}

func TestOfflinePollerUnknownKissDoesNotBlock(t *testing.T) {
	clock := newFakeClock()
	c := newMockV4Conn(clock)
	c.kissCode = "INIT"
	poller := newTestPoller(clock, func() *mockV4Conn { return c })

	r, err := poller.Query(context.Background(), testEndpoint, nil)
	require.Error(t, err)
	require.NotNil(t, r)
	var ke *KissError
	require.ErrorAs(t, err, &ke)
	assert.Equal(t, KissUnknown, ke.Class())

	snap, _ := poller.State(testEndpoint)
	assert.False(t, snap.Blocked)
	assert.True(t, snap.CooldownUntil.IsZero())

	// The next query is allowed immediately.
	ok := newMockV4Conn(clock)
	poller.opt.QueryOptions.Dialer = func(_, _ string) (net.Conn, error) {
		return ok, nil
	}
	_, err = poller.Query(context.Background(), testEndpoint, nil)
	assert.NoError(t, err)
}

func TestOfflinePollerMismatchedResponseIgnored(t *testing.T) {
	clock := newFakeClock()
	c := newMockV4Conn(clock)
	c.kissCode = "RATE"
	c.mismatch = true
	poller := newTestPoller(clock, func() *mockV4Conn { return c })

	r, err := poller.Query(context.Background(), testEndpoint, nil)
	assert.Nil(t, r)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrServerResponseMismatch)

	// The failed correlation check must not install a cooldown.
	var ke *KissError
	assert.False(t, errors.As(err, &ke))
	snap, _ := poller.State(testEndpoint)
	assert.False(t, snap.Blocked)
	assert.True(t, snap.CooldownUntil.IsZero())

	ok := newMockV4Conn(clock)
	poller.opt.QueryOptions.Dialer = func(_, _ string) (net.Conn, error) {
		return ok, nil
	}
	r2, err := poller.Query(context.Background(), testEndpoint, nil)
	require.NoError(t, err)
	assert.Equal(t, uint8(2), r2.Stratum)
}

func TestOfflinePollerOrdinaryErrorsNotKiss(t *testing.T) {
	clock := newFakeClock()
	c := newMockV4Conn(clock)
	c.failRead = true
	poller := newTestPoller(clock, func() *mockV4Conn { return c })

	r, err := poller.Query(context.Background(), testEndpoint, nil)
	assert.Nil(t, r)
	require.Error(t, err)

	var ke *KissError
	assert.False(t, errors.As(err, &ke))
	assert.False(t, errors.Is(err, ErrKissOfDeath))

	snap, _ := poller.State(testEndpoint)
	assert.False(t, snap.Blocked)
	assert.True(t, snap.CooldownUntil.IsZero())
	assert.False(t, snap.InFlight)
}

func TestOfflinePollerTimeoutIsOrdinaryError(t *testing.T) {
	clock := newFakeClock()
	c := newMockV4Conn(clock)
	release := make(chan struct{})
	c.release = release

	timeout := 50 * time.Millisecond
	poller := NewPoller(PollerOptions{
		Clock: clock,
		QueryOptions: QueryOptions{
			Version: 4,
			Timeout: timeout,
			Dialer:  func(_, _ string) (net.Conn, error) { return c, nil },
		},
	})

	start := time.Now()
	r, err := poller.Query(context.Background(), testEndpoint, nil)
	elapsed := time.Since(start)
	close(release)

	assert.Nil(t, r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deadline")
	var ke *KissError
	assert.False(t, errors.As(err, &ke))
	assert.Less(t, elapsed, 2*time.Second)

	snap, _ := poller.State(testEndpoint)
	assert.False(t, snap.Blocked)
	assert.False(t, snap.InFlight)
}

func TestOfflinePollerConcurrentPermit(t *testing.T) {
	clock := newFakeClock()

	var mu sync.Mutex
	inFlight := 0
	maxInFlight := 0
	var conns []*mockV4Conn

	poller := NewPoller(PollerOptions{
		Clock: clock,
		QueryOptions: QueryOptions{
			Version: 4,
			Timeout: 5 * time.Second,
			Dialer: func(_, _ string) (net.Conn, error) {
				mu.Lock()
				inFlight++
				if inFlight > maxInFlight {
					maxInFlight = inFlight
				}
				c := newMockV4Conn(clock)
				conns = append(conns, c)
				mu.Unlock()

				time.Sleep(20 * time.Millisecond)

				mu.Lock()
				inFlight--
				mu.Unlock()
				return c, nil
			},
		},
	})

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = poller.Query(context.Background(), testEndpoint, nil)
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		assert.NoError(t, err)
	}
	assert.Equal(t, n, len(conns))
	assert.Equal(t, 1, maxInFlight, "only one query may be in flight per endpoint")

	snap, _ := poller.State(testEndpoint)
	assert.False(t, snap.InFlight)
}

func TestOfflinePollerCancelWhileCoolingDownReleasesPermit(t *testing.T) {
	clock := newFakeClock()
	var mu sync.Mutex
	rateConn := newMockV4Conn(clock)
	rateConn.kissCode = "RATE"
	rateConn.poll = 7 // 128 seconds, within the 1m..1h local bounds
	nextConn := rateConn
	poller := newTestPoller(clock, func() *mockV4Conn {
		mu.Lock()
		defer mu.Unlock()
		c := nextConn
		return c
	})
	poller.opt.MinRateBackoff = time.Minute
	poller.opt.MaxRateBackoff = time.Hour

	_, err := poller.Query(context.Background(), testEndpoint, nil)
	require.Error(t, err)

	canceledConn := newMockV4Conn(clock)
	mu.Lock()
	nextConn = canceledConn
	mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, e := poller.Query(ctx, testEndpoint, nil)
		done <- e
	}()

	require.Eventually(t, clock.hasPendingTimer, time.Second, time.Millisecond)

	// Cancel before the query is ever sent.
	cancel()
	select {
	case e := <-done:
		assert.ErrorIs(t, e, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceled query did not return")
	}

	// The canceled, unsent query must not have dialed.
	assert.Equal(t, 0, canceledConn.dials)

	// The permit is released: another caller can acquire it immediately.
	// It will itself wait for the cooldown, so advance the fake clock.
	recoveryConn := newMockV4Conn(clock)
	mu.Lock()
	nextConn = recoveryConn
	mu.Unlock()

	proceeded := make(chan struct{})
	go func() {
		_, _ = poller.Query(context.Background(), testEndpoint, nil)
		close(proceeded)
	}()
	require.Eventually(t, func() bool { return clock.pendingTimers() == 1 },
		time.Second, time.Millisecond)
	clock.Advance(3 * time.Minute)
	select {
	case <-proceeded:
	case <-time.After(time.Second):
		t.Fatal("permit was not released after cancellation")
	}
	assert.Equal(t, 1, recoveryConn.dials)
}

func TestOfflinePollerEndpointNormalization(t *testing.T) {
	clock := newFakeClock()
	c := newMockV4Conn(clock)
	c.kissCode = "RATE"
	c.poll = 8
	poller := newTestPoller(clock, func() *mockV4Conn { return c })

	_, err := poller.Query(context.Background(), "mock.example.com", nil)
	require.Error(t, err)

	for _, addr := range []string{"mock.example.com:123", "mock.example.com"} {
		snap, ok := poller.State(addr)
		require.True(t, ok, addr)
		assert.Equal(t, testEndpoint, snap.Endpoint)
		assert.False(t, snap.CooldownUntil.IsZero(), addr)
	}
}

func TestOfflinePollerResetAllAndBlockedFastFail(t *testing.T) {
	clock := newFakeClock()
	c := newMockV4Conn(clock)
	c.kissCode = "DENY"
	poller := newTestPoller(clock, func() *mockV4Conn { return c })

	_, err := poller.Query(context.Background(), testEndpoint, nil)
	require.Error(t, err)

	// A blocked endpoint fails before a permit wait even when the context
	// is already canceled; it never dials.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = poller.Query(ctx, testEndpoint, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEndpointBlocked)

	poller.ResetAll()
	snap, _ := poller.State(testEndpoint)
	assert.False(t, snap.Blocked)
	assert.True(t, snap.CooldownUntil.IsZero())

	ok := newMockV4Conn(clock)
	poller.opt.QueryOptions.Dialer = func(_, _ string) (net.Conn, error) {
		return ok, nil
	}
	r, err := poller.Query(context.Background(), testEndpoint, nil)
	require.NoError(t, err)
	assert.Equal(t, uint8(2), r.Stratum)
}
