// Copyright IBM Corp. 2016, 2026
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-plugin/internal/grpcmux"
	"github.com/hashicorp/go-plugin/internal/plugin"
	grpctest "github.com/hashicorp/go-plugin/test/grpc"
	"google.golang.org/grpc"
)

// leaseTestStreamer is an in-memory implementation of the streamer
// interface used to connect two brokers in tests.
type leaseTestStreamer struct {
	sendCh chan *plugin.ConnInfo
	recvCh chan *plugin.ConnInfo
	quit   chan struct{}
	once   sync.Once
}

func (s *leaseTestStreamer) Send(i *plugin.ConnInfo) error {
	select {
	case <-s.quit:
		return errors.New("streamer closed")
	case s.sendCh <- i:
		return nil
	}
}

func (s *leaseTestStreamer) Recv() (*plugin.ConnInfo, error) {
	select {
	case <-s.quit:
		return nil, errors.New("streamer closed")
	case i := <-s.recvCh:
		return i, nil
	}
}

func (s *leaseTestStreamer) Close() {
	s.once.Do(func() { close(s.quit) })
}

// newLeaseTestBrokerPair returns a server and client broker connected by an
// in-memory stream, with no multiplexing.
func newLeaseTestBrokerPair(t *testing.T) (server, client *GRPCBroker) {
	t.Helper()

	s2c := make(chan *plugin.ConnInfo)
	c2s := make(chan *plugin.ConnInfo)
	noMux := (*grpcmux.GRPCClientMuxer)(nil)

	server = newGRPCBroker(&leaseTestStreamer{sendCh: s2c, recvCh: c2s, quit: make(chan struct{})}, nil, UnixSocketConfig{}, nil, noMux)
	client = newGRPCBroker(&leaseTestStreamer{sendCh: c2s, recvCh: s2c, quit: make(chan struct{})}, nil, UnixSocketConfig{}, nil, noMux)

	go server.Run()
	go client.Run()

	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})

	return server, client
}

func pingPongServerFactory(opts []grpc.ServerOption) *grpc.Server {
	s := grpc.NewServer(opts...)
	grpctest.RegisterPingPongServer(s, &pingPongServer{})
	return s
}

func pingPong(t *testing.T, conn *grpc.ClientConn) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := grpctest.NewPingPongClient(conn).Ping(ctx, &grpctest.PingRequest{})
	if err != nil {
		t.Fatalf("Ping() error: %s", err)
	}
	if resp.Msg != "pong" {
		t.Fatalf("bad ping/pong: %q", resp.Msg)
	}
}

func waitRevoked(t *testing.T, lease *BrokerLease) {
	t.Helper()

	select {
	case <-lease.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("lease was not revoked in time")
	}
}

// TestGRPCBrokerLease_RequestLifecycle covers the primary use case: an RPC
// temporarily exposes a callback service, bound to the request's context.
// Cancelling the context reclaims the endpoint.
func TestGRPCBrokerLease_RequestLifecycle(t *testing.T) {
	server, client := newLeaseTestBrokerPair(t)

	ctx, cancel := context.WithCancel(context.Background())
	lease, err := server.AcceptAndServeWithLease(ctx, 42, 0, pingPongServerFactory)
	if err != nil {
		t.Fatalf("AcceptAndServeWithLease error: %s", err)
	}

	// A TTL-less lease is a no-op to renew.
	if err := lease.Renew(); err != nil {
		t.Fatalf("Renew on TTL-less lease error: %s", err)
	}

	conn, err := client.Dial(lease.ID())
	if err != nil {
		t.Fatalf("Dial error: %s", err)
	}
	pingPong(t, conn)

	// Ending the request lifecycle revokes the endpoint.
	cancel()
	waitRevoked(t, lease)

	// The listener is closed, so late connections are refused.
	addr := lease.Listener().Addr()
	lateConn, err := net.DialTimeout(addr.Network(), addr.String(), 500*time.Millisecond)
	if err == nil {
		_ = lateConn.Close()
		t.Fatal("expected listener to be closed after revocation")
	}

	// Dialing the revoked ID on the same broker fails fast.
	start := time.Now()
	if _, err := server.Dial(lease.ID()); !errors.Is(err, ErrBrokerLeaseRevoked) {
		t.Fatalf("expected ErrBrokerLeaseRevoked, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("dial of revoked ID took too long: %s", elapsed)
	}
}

// TestGRPCBrokerLease_Renew keeps a short-lived endpoint alive with active
// renewals, then lets it expire and checks the resources are reclaimed.
func TestGRPCBrokerLease_Renew(t *testing.T) {
	server, client := newLeaseTestBrokerPair(t)

	lease, err := server.AcceptAndServeWithLease(context.Background(), 7, 200*time.Millisecond, pingPongServerFactory)
	if err != nil {
		t.Fatalf("AcceptAndServeWithLease error: %s", err)
	}

	// Renew actively for well beyond the TTL; the lease must survive.
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(80 * time.Millisecond)
		if err := lease.Renew(); err != nil {
			t.Fatalf("Renew error: %s", err)
		}
	}
	select {
	case <-lease.Done():
		t.Fatal("lease revoked despite active renewals")
	default:
	}

	// The endpoint is still usable.
	conn, err := client.Dial(lease.ID())
	if err != nil {
		t.Fatalf("Dial error: %s", err)
	}
	pingPong(t, conn)

	// Stop renewing; the lease expires and is revoked.
	waitRevoked(t, lease)

	if err := lease.Renew(); !errors.Is(err, ErrBrokerLeaseRevoked) {
		t.Fatalf("expected ErrBrokerLeaseRevoked from Renew, got: %v", err)
	}
}

// TestGRPCBrokerLease_ExpiryReconnect verifies that an expired endpoint ID
// cannot be reused during the revoke grace period, and that the ID can be
// reconnected once the grace period and the dialer's stale connection info
// window have elapsed.
func TestGRPCBrokerLease_ExpiryReconnect(t *testing.T) {
	server, client := newLeaseTestBrokerPair(t)

	// Shorten the windows so the test runs fast while preserving the
	// invariant that the grace period outlives stale connection info.
	server.leaseRevokeGrace = 200 * time.Millisecond
	client.clientStreamTimeout = 100 * time.Millisecond

	lease, err := server.AcceptWithLease(context.Background(), 11, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("AcceptWithLease error: %s", err)
	}
	waitRevoked(t, lease)

	// During the grace period the ID is reserved: a late connection must
	// not be able to find a different service through the reused ID.
	if _, err := server.Accept(lease.ID()); !errors.Is(err, ErrBrokerLeaseRevoked) {
		t.Fatalf("expected ErrBrokerLeaseRevoked from Accept, got: %v", err)
	}
	if _, err := server.AcceptWithLease(context.Background(), lease.ID(), 0); !errors.Is(err, ErrBrokerLeaseRevoked) {
		t.Fatalf("expected ErrBrokerLeaseRevoked from AcceptWithLease, got: %v", err)
	}

	// Wait out the grace period and the dialer's stale info window, then
	// reconnect a new service on the same ID.
	time.Sleep(400 * time.Millisecond)

	lease2, err := server.AcceptAndServeWithLease(context.Background(), lease.ID(), 0, pingPongServerFactory)
	if err != nil {
		t.Fatalf("AcceptAndServeWithLease after grace error: %s", err)
	}

	conn, err := client.Dial(lease2.ID())
	if err != nil {
		t.Fatalf("Dial error: %s", err)
	}
	pingPong(t, conn)
}

// TestGRPCBrokerLease_RevokeDialRace verifies that a Dial racing with
// Revoke resolves deterministically to an error, and that once Revoke has
// returned no Dial of the revoked ID can succeed.
func TestGRPCBrokerLease_RevokeDialRace(t *testing.T) {
	server, _ := newLeaseTestBrokerPair(t)

	lease, err := server.AcceptWithLease(context.Background(), 21, 0)
	if err != nil {
		t.Fatalf("AcceptWithLease error: %s", err)
	}

	// Start dials that block waiting for connection info on this broker.
	const dials = 8
	errs := make([]error, dials)
	var wg sync.WaitGroup
	for i := 0; i < dials; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, err := server.Dial(lease.ID())
			if err == nil {
				_ = conn.Close()
			}
			errs[i] = err
		}(i)
	}

	// Wait for the pending stream to be registered, then revoke.
	for {
		server.Lock()
		_, ok := server.clientStreams[lease.ID()]
		server.Unlock()
		if ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := lease.Revoke(); err != nil {
		t.Fatalf("Revoke error: %s", err)
	}

	waitDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("dials did not resolve after revocation")
	}

	// Every racing dial resolved to ErrBrokerLeaseRevoked, whether it was
	// failed by the revocation itself or by the tombstone.
	for i, err := range errs {
		if !errors.Is(err, ErrBrokerLeaseRevoked) {
			t.Fatalf("dial %d: expected ErrBrokerLeaseRevoked, got: %v", i, err)
		}
	}

	// After Revoke has returned, dials fail immediately.
	start := time.Now()
	if _, err := server.Dial(lease.ID()); !errors.Is(err, ErrBrokerLeaseRevoked) {
		t.Fatalf("expected ErrBrokerLeaseRevoked, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("dial of revoked ID took too long: %s", elapsed)
	}

	// Revoke is idempotent.
	if err := lease.Revoke(); err != nil {
		t.Fatalf("second Revoke error: %s", err)
	}
}

// TestGRPCBrokerLease_BlockingCallback verifies that resource reclamation
// does not wait for the revoke callback, even if the callback blocks.
func TestGRPCBrokerLease_BlockingCallback(t *testing.T) {
	server, _ := newLeaseTestBrokerPair(t)

	callbackStarted := make(chan struct{})
	unblock := make(chan struct{})

	lease, err := server.AcceptWithLease(context.Background(), 31, 0, WithRevokeCallback(func() {
		close(callbackStarted)
		<-unblock
	}))
	if err != nil {
		t.Fatalf("AcceptWithLease error: %s", err)
	}
	addr := lease.Listener().Addr()

	if err := lease.Revoke(); err != nil {
		t.Fatalf("Revoke error: %s", err)
	}

	// Wait until the callback is running and blocked.
	select {
	case <-callbackStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("revoke callback did not run")
	}

	// Despite the blocked callback, the lease is fully reclaimed.
	select {
	case <-lease.Done():
	default:
		t.Fatal("lease Done channel not closed while callback is blocked")
	}

	server.Lock()
	_, leased := server.leases[lease.ID()]
	_, reserved := server.revokedLeases[lease.ID()]
	server.Unlock()
	if leased {
		t.Fatal("lease still registered while callback is blocked")
	}
	if !reserved {
		t.Fatal("revoked ID not reserved while callback is blocked")
	}

	lateConn, err := net.DialTimeout(addr.Network(), addr.String(), 500*time.Millisecond)
	if err == nil {
		_ = lateConn.Close()
		t.Fatal("listener still accepting while callback is blocked")
	}

	close(unblock)
}

// TestGRPCBrokerLease_BrokerClose verifies that closing the broker revokes
// its outstanding leases.
func TestGRPCBrokerLease_BrokerClose(t *testing.T) {
	server, _ := newLeaseTestBrokerPair(t)

	lease, err := server.AcceptWithLease(context.Background(), 51, 0)
	if err != nil {
		t.Fatalf("AcceptWithLease error: %s", err)
	}

	if err := server.Close(); err != nil {
		t.Fatalf("Close error: %s", err)
	}
	waitRevoked(t, lease)
}

// TestGRPCBrokerLease_ExistingInterface verifies that ordinary long-lived
// broker endpoints keep working through the existing interface, and that
// they cannot shadow an active lease.
func TestGRPCBrokerLease_ExistingInterface(t *testing.T) {
	server, client := newLeaseTestBrokerPair(t)

	go server.AcceptAndServe(61, pingPongServerFactory)

	conn, err := client.Dial(61)
	if err != nil {
		t.Fatalf("Dial error: %s", err)
	}
	pingPong(t, conn)

	// A plain Accept must not shadow an ID held by an active lease.
	lease, err := server.AcceptWithLease(context.Background(), 62, 0)
	if err != nil {
		t.Fatalf("AcceptWithLease error: %s", err)
	}
	if _, err := server.Accept(lease.ID()); !errors.Is(err, ErrBrokerLeaseConflict) {
		t.Fatalf("expected ErrBrokerLeaseConflict, got: %v", err)
	}

	// A second lease must not shadow an active lease on the same ID.
	if _, err := server.AcceptWithLease(context.Background(), lease.ID(), 0); !errors.Is(err, ErrBrokerLeaseConflict) {
		t.Fatalf("expected ErrBrokerLeaseConflict, got: %v", err)
	}
}
