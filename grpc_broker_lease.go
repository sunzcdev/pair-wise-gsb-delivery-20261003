// Copyright IBM Corp. 2016, 2026
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/oklog/run"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// ErrBrokerLeaseRevoked is returned when dialing or reusing a broker
// endpoint ID whose lease has been revoked. A revoked ID stays reserved for
// a grace period so that a late connection can never be routed to a
// different service that reuses the same numeric ID.
var ErrBrokerLeaseRevoked = errors.New("grpc broker: endpoint lease revoked")

// ErrBrokerLeaseConflict is returned when registering an endpoint with an
// ID that is already held by an active lease.
var ErrBrokerLeaseConflict = errors.New("grpc broker: endpoint ID already leased")

// brokerLeaseRevokeGrace is the default amount of time a revoked endpoint ID
// stays reserved before it may be reused. It matches the connection info
// timeout used by Dial and timeoutWait, so any connection info that was
// already in flight when the lease was revoked is guaranteed to have been
// discarded before the ID becomes available again.
const brokerLeaseRevokeGrace = 5 * time.Second

// BrokerLeaseOption configures a BrokerLease created by AcceptWithLease or
// AcceptAndServeWithLease.
type BrokerLeaseOption func(*BrokerLease)

// WithRevokeCallback registers a function that is called after the lease has
// been revoked and its resources have been released. The callback runs in
// its own goroutine, so a blocking callback cannot delay resource
// reclamation, but it should not block forever.
func WithRevokeCallback(fn func()) BrokerLeaseOption {
	return func(l *BrokerLease) {
		l.onRevoke = fn
	}
}

// BrokerLease represents a leased broker endpoint created by
// AcceptWithLease or AcceptAndServeWithLease.
//
// The endpoint is revoked when the parent context is done, when its TTL
// expires without a Renew, or when Revoke is called explicitly. Revocation
// closes the listener, fails any in-flight Dial for the endpoint ID on the
// same broker, and reserves the ID for a grace period so it cannot be
// reused by another service while stale connection info may still be in
// flight.
type BrokerLease struct {
	id       uint32
	broker   *GRPCBroker
	listener net.Listener

	ctx    context.Context
	cancel context.CancelFunc

	onRevoke func()

	done chan struct{}
	once sync.Once

	mu      sync.Mutex
	ttl     time.Duration
	expiry  time.Time
	renewCh chan struct{}
}

// ID returns the broker endpoint ID the lease is bound to.
func (l *BrokerLease) ID() uint32 {
	return l.id
}

// Listener returns the listener for the leased endpoint. It is closed when
// the lease is revoked.
func (l *BrokerLease) Listener() net.Listener {
	return l.listener
}

// Done returns a channel that is closed once the lease has been revoked.
func (l *BrokerLease) Done() <-chan struct{} {
	return l.done
}

// Renew extends the lease expiry by its TTL, measured from now. It is a
// no-op for leases without a TTL, which live until their context is done or
// Revoke is called. Renew returns ErrBrokerLeaseRevoked if the lease has
// already been revoked.
func (l *BrokerLease) Renew() error {
	select {
	case <-l.done:
		return ErrBrokerLeaseRevoked
	default:
	}

	l.mu.Lock()
	if l.ttl > 0 {
		l.expiry = time.Now().Add(l.ttl)
	}
	l.mu.Unlock()

	select {
	case l.renewCh <- struct{}{}:
	default:
	}

	return nil
}

// Revoke revokes the lease, releasing the endpoint. It is idempotent and
// safe to call concurrently with Dial.
func (l *BrokerLease) Revoke() error {
	l.once.Do(l.revoke)
	return nil
}

// revoke releases the lease's resources. Cleanup completes before the
// revoke callback is invoked, and the callback runs in its own goroutine so
// that a blocking callback cannot stall reclamation.
func (l *BrokerLease) revoke() {
	b := l.broker
	now := time.Now()

	b.Lock()
	if cur, ok := b.leases[l.id]; ok && cur == l {
		delete(b.leases, l.id)
		b.revokedLeases[l.id] = now.Add(b.leaseRevokeGrace)

		// Sweep expired tombstones while we're here.
		for id, expiry := range b.revokedLeases {
			if !now.Before(expiry) {
				delete(b.revokedLeases, id)
			}
		}
	}

	// Fail any in-flight Dial for this ID on this broker, so that a Dial
	// racing with the revocation resolves to an error instead of blocking
	// until its timeout.
	if p, ok := b.clientStreams[l.id]; ok {
		p.once.Do(func() { close(p.doneCh) })
	}
	b.Unlock()

	if l.listener != nil {
		_ = l.listener.Close()
	}
	l.cancel()
	close(l.done)

	if l.onRevoke != nil {
		go l.onRevoke()
	}
}

// watch revokes the lease when its context is done or its TTL expires.
func (l *BrokerLease) watch() {
	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	resetTimer := func() {
		l.mu.Lock()
		ttl := l.ttl
		expiry := l.expiry
		l.mu.Unlock()

		if ttl <= 0 {
			timerC = nil
			return
		}
		d := time.Until(expiry)
		if d <= 0 {
			d = time.Nanosecond
		}
		if timer == nil {
			timer = time.NewTimer(d)
		} else {
			timer.Reset(d)
		}
		timerC = timer.C
	}

	resetTimer()
	for {
		select {
		case <-l.ctx.Done():
			_ = l.Revoke()
			return
		case <-timerC:
			timerC = nil
			// A Renew that landed just as the timer fired wins; only
			// revoke if the expiry has really passed.
			l.mu.Lock()
			expired := l.ttl > 0 && !time.Now().Before(l.expiry)
			l.mu.Unlock()
			if expired {
				_ = l.Revoke()
				return
			}
			resetTimer()
		case <-l.renewCh:
			resetTimer()
		case <-l.done:
			return
		}
	}
}

// endpointAvailableLocked reports whether an endpoint ID may be registered
// or dialed. A revoked ID stays reserved for the lease revoke grace period
// so that a late connection cannot find a different service through the
// reused numeric ID.
func (b *GRPCBroker) endpointAvailableLocked(id uint32) error {
	if expiry, ok := b.revokedLeases[id]; ok {
		if time.Now().Before(expiry) {
			return fmt.Errorf("grpc broker: service ID %d was revoked and is reserved until in-flight connections drain: %w", id, ErrBrokerLeaseRevoked)
		}
		delete(b.revokedLeases, id)
	}
	return nil
}

// AcceptWithLease accepts a connection by ID and binds the endpoint's
// lifetime to a lease. The endpoint is revoked when ctx is done, when ttl
// expires without a call to Renew, or when Revoke is called. A ttl of zero
// (or less) disables expiry, tying the endpoint purely to the request
// lifecycle described by ctx.
//
// Unlike Accept, the endpoint ID is reserved for a grace period after
// revocation, so callers must not reuse the ID until the grace period has
// elapsed.
func (b *GRPCBroker) AcceptWithLease(ctx context.Context, id uint32, ttl time.Duration, opts ...BrokerLeaseOption) (*BrokerLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)

	l := &BrokerLease{
		id:      id,
		broker:  b,
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
		renewCh: make(chan struct{}, 1),
	}
	l.ttl = ttl
	if ttl > 0 {
		l.expiry = time.Now().Add(ttl)
	}
	for _, opt := range opts {
		opt(l)
	}

	ln, err := b.accept(id, l)
	if err != nil {
		b.Lock()
		if b.leases[id] == l {
			delete(b.leases, id)
		}
		b.Unlock()
		cancel()
		return nil, err
	}
	l.listener = ln

	go l.watch()
	return l, nil
}

// AcceptAndServeWithLease is like AcceptAndServe, but the endpoint's
// lifetime is bound to a lease as described by AcceptWithLease. It returns
// the lease immediately and serves in the background; the gRPC server is
// stopped when the lease is revoked or the broker is shut down.
func (b *GRPCBroker) AcceptAndServeWithLease(ctx context.Context, id uint32, ttl time.Duration, newGRPCServer func([]grpc.ServerOption) *grpc.Server, opts ...BrokerLeaseOption) (*BrokerLease, error) {
	lease, err := b.AcceptWithLease(ctx, id, ttl, opts...)
	if err != nil {
		return nil, err
	}

	var serverOpts []grpc.ServerOption
	if b.tls != nil {
		serverOpts = []grpc.ServerOption{grpc.Creds(credentials.NewTLS(b.tls))}
	}
	server := newGRPCServer(serverOpts)

	go func() {
		var g run.Group
		{
			// Serve on the lease's listener; revocation closes the
			// listener, which ends Serve and stops the group.
			g.Add(func() error {
				return server.Serve(lease.listener)
			}, func(err error) {
				server.GracefulStop()
			})
		}
		{
			closeCh := make(chan struct{})
			g.Add(func() error {
				select {
				case <-b.doneCh:
				case <-lease.done:
				case <-closeCh:
				}
				return nil
			}, func(err error) {
				close(closeCh)
			})
		}

		_ = g.Run()
	}()

	return lease, nil
}
