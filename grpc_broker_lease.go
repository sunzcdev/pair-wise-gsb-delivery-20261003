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

// BrokerLease is a broker endpoint whose lifetime is bound to a lease.
//
// A lease is reclaimed when any of the following happens:
//
//   - the context passed to LeaseEndpoint or LeaseAcceptAndServe is done
//     (for example when the RPC handler that created the lease returns),
//   - the lease TTL elapses without a call to Extend,
//   - Revoke is called explicitly,
//   - the broker is closed.
//
// Lease IDs are allocated from the same monotonically increasing sequence
// as GRPCBroker.NextId and are never reused for the lifetime of the broker,
// so a late connection attempt for a reclaimed lease can never be routed to
// a different endpoint that happens to share the old numeric ID.
type BrokerLease struct {
	id     uint32
	broker *GRPCBroker
	ln     net.Listener

	doneCh chan struct{}
	once   sync.Once

	mu    sync.Mutex
	timer *time.Timer
}

// LeaseEndpoint reserves a broker endpoint and binds its lifetime to a
// lease. The endpoint is identified by the returned lease's ID, which the
// caller hands to the peer so it can connect with GRPCBroker.Dial.
//
// The lease is reclaimed when ctx is done, when ttl elapses without a call
// to BrokerLease.Extend, or when BrokerLease.Revoke is called. Pass
// context.Background() (or nil) to manage the lease purely through its TTL
// and explicit Extend/Revoke calls, for example for asynchronous tasks that
// outlive the request that created them.
//
// The endpoint listener is available through BrokerLease.Listener. Use
// LeaseAcceptAndServe instead to serve a gRPC server on the endpoint
// directly.
func (b *GRPCBroker) LeaseEndpoint(ctx context.Context, ttl time.Duration) (*BrokerLease, error) {
	if ttl <= 0 {
		return nil, errors.New("plugin: lease ttl must be greater than zero")
	}
	select {
	case <-b.doneCh:
		return nil, errors.New("plugin: broker is closed")
	default:
	}

	l := &BrokerLease{
		id:     b.NextId(),
		broker: b,
		doneCh: make(chan struct{}),
	}

	b.Lock()
	b.leases[l.id] = l
	b.Unlock()

	ln, err := b.Accept(l.id)
	if err != nil {
		b.Lock()
		delete(b.leases, l.id)
		b.Unlock()
		return nil, err
	}
	l.ln = ln

	l.timer = time.AfterFunc(ttl, func() { _ = l.Revoke() })
	go l.watch(ctx)

	return l, nil
}

// LeaseAcceptAndServe reserves a leased broker endpoint (see LeaseEndpoint)
// and immediately serves a gRPC server on it in a background goroutine,
// mirroring AcceptAndServe. The server is gracefully stopped when the lease
// is reclaimed. The returned lease can be used to extend or revoke the
// endpoint, and its ID can be handed to the peer for dialling.
func (b *GRPCBroker) LeaseAcceptAndServe(ctx context.Context, ttl time.Duration, newGRPCServer func([]grpc.ServerOption) *grpc.Server) (*BrokerLease, error) {
	l, err := b.LeaseEndpoint(ctx, ttl)
	if err != nil {
		return nil, err
	}

	go l.serve(newGRPCServer)

	return l, nil
}

// serve runs the gRPC server for a leased endpoint until the server stops,
// the lease is reclaimed, or the broker is closed.
func (l *BrokerLease) serve(newGRPCServer func([]grpc.ServerOption) *grpc.Server) {
	defer func() { _ = l.Revoke() }()

	var opts []grpc.ServerOption
	if l.broker.tls != nil {
		opts = []grpc.ServerOption{grpc.Creds(credentials.NewTLS(l.broker.tls))}
	}

	server := newGRPCServer(opts)

	// Use a run group to stop the server if the lease is reclaimed or the
	// broker is shut down.
	var g run.Group
	{
		// Serve on the lease listener, if shutting down call GracefulStop.
		g.Add(func() error {
			return server.Serve(l.ln)
		}, func(err error) {
			server.GracefulStop()
		})
	}
	{
		// Block on the lease, the broker, or the closeCh. If we are
		// shutting down close the closeCh.
		closeCh := make(chan struct{})
		g.Add(func() error {
			select {
			case <-l.broker.doneCh:
			case <-l.doneCh:
			case <-closeCh:
			}
			return nil
		}, func(err error) {
			close(closeCh)
		})
	}

	// Block until we are done
	_ = g.Run()
}

// watch reclaims the lease when its context is done or the broker closes.
func (l *BrokerLease) watch(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		_ = l.Revoke()
	case <-l.doneCh:
	case <-l.broker.doneCh:
		_ = l.Revoke()
	}
}

// ID returns the broker endpoint ID for this lease. The peer uses this ID
// with GRPCBroker.Dial to connect to the endpoint.
func (l *BrokerLease) ID() uint32 {
	return l.id
}

// Listener returns the network listener for the leased endpoint. The
// listener is closed when the lease is reclaimed.
func (l *BrokerLease) Listener() net.Listener {
	return l.ln
}

// Done returns a channel that is closed when the lease is reclaimed, either
// through expiry, context cancellation, an explicit Revoke, or the broker
// closing.
func (l *BrokerLease) Done() <-chan struct{} {
	return l.doneCh
}

// Extend renews the lease, resetting its expiry to ttl from now. It returns
// an error if the lease has already been reclaimed. Extend must be called
// before the lease expires; an Extend racing the expiry is not guaranteed
// to win.
func (l *BrokerLease) Extend(ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("plugin: lease ttl must be greater than zero")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	select {
	case <-l.doneCh:
		return fmt.Errorf("plugin: broker lease %d is no longer active", l.id)
	default:
	}

	l.timer.Reset(ttl)
	return nil
}

// Revoke reclaims the leased endpoint immediately. It is safe to call
// Revoke multiple times and from multiple goroutines.
//
// Revocation is serialized with dialling through the broker: a dial whose
// knock is processed before revocation connects to this endpoint, and a
// dial whose knock is processed after revocation fails deterministically.
func (l *BrokerLease) Revoke() error {
	l.once.Do(func() {
		// Record the tombstone before closing the listener so that any
		// knock processed from this point on is refused.
		l.broker.revokeLease(l.id)

		l.mu.Lock()
		l.timer.Stop()
		l.mu.Unlock()

		_ = l.ln.Close()

		close(l.doneCh)
	})
	return nil
}
