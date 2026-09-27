// Copyright IBM Corp. 2016, 2026
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	grpctest "github.com/hashicorp/go-plugin/test/grpc"
	"google.golang.org/grpc"
)

// tagPingPongServer responds to pings with a unique tag so tests can verify
// exactly which endpoint a connection reached.
type tagPingPongServer struct {
	grpctest.UnimplementedPingPongServer
	tag string
}

func (p *tagPingPongServer) Ping(ctx context.Context, req *grpctest.PingRequest) (*grpctest.PongResponse, error) {
	return &grpctest.PongResponse{Msg: p.tag}, nil
}

func leasePingPongServer(tag string) func([]grpc.ServerOption) *grpc.Server {
	return func(opts []grpc.ServerOption) *grpc.Server {
		s := grpc.NewServer(opts...)
		grpctest.RegisterPingPongServer(s, &tagPingPongServer{tag: tag})
		return s
	}
}

// leasePing dials the given broker endpoint ID and issues a ping, returning
// the pong message. An error is returned if either the dial or the ping
// fails.
func leasePing(b *GRPCBroker, id uint32) (string, error) {
	conn, err := b.Dial(id)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := grpctest.NewPingPongClient(conn).Ping(ctx, &grpctest.PingRequest{})
	if err != nil {
		return "", err
	}
	return resp.Msg, nil
}

func leaseCount(b *GRPCBroker) int {
	b.Lock()
	defer b.Unlock()
	return len(b.leases)
}

func waitLeaseDone(t *testing.T, l *BrokerLease) {
	t.Helper()
	select {
	case <-l.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for lease to be reclaimed")
	}
}

// TestGRPCBrokerLease_RequestLifecycle verifies that a lease bound to a
// request context stays alive while the (blocking) request is in flight and
// is reclaimed once the request completes.
func TestGRPCBrokerLease_RequestLifecycle(t *testing.T) {
	t.Run("default", func(t *testing.T) { testGRPCBrokerLeaseRequestLifecycle(t, false) })
	t.Run("mux", func(t *testing.T) { testGRPCBrokerLeaseRequestLifecycle(t, true) })
}

func testGRPCBrokerLeaseRequestLifecycle(t *testing.T, multiplex bool) {
	client, server := TestPluginGRPCConn(t, multiplex, map[string]Plugin{
		"test": new(testGRPCInterfacePlugin),
	})
	defer func() { _ = client.Close() }()
	defer server.Stop()

	// Simulate a blocking RPC handler: the request context is cancelled
	// only once the handler is released and returns.
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-release
		cancel()
	}()

	lease, err := server.broker.LeaseAcceptAndServe(ctx, time.Minute, leasePingPongServer("pong"))
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	// While the request is blocked, the endpoint must be reachable.
	pong, err := leasePing(client.broker, lease.ID())
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if pong != "pong" {
		t.Fatalf("bad: %q", pong)
	}

	// The request finishes, cancelling its context. The lease must be
	// reclaimed without any explicit action from the caller.
	close(release)
	waitLeaseDone(t, lease)

	if n := leaseCount(server.broker); n != 0 {
		t.Fatalf("expected no active leases, got: %d", n)
	}

	// Dialling the reclaimed endpoint must fail.
	if _, err := leasePing(client.broker, lease.ID()); err == nil {
		t.Fatal("expected dialling a reclaimed lease to fail")
	}
}

// TestGRPCBrokerLease_Extend verifies that actively renewing a lease keeps
// the endpoint alive past its original TTL, and that stopping the renewals
// lets the lease expire.
func TestGRPCBrokerLease_Extend(t *testing.T) {
	t.Run("default", func(t *testing.T) { testGRPCBrokerLeaseExtend(t, false) })
	t.Run("mux", func(t *testing.T) { testGRPCBrokerLeaseExtend(t, true) })
}

func testGRPCBrokerLeaseExtend(t *testing.T, multiplex bool) {
	client, server := TestPluginGRPCConn(t, multiplex, map[string]Plugin{
		"test": new(testGRPCInterfacePlugin),
	})
	defer func() { _ = client.Close() }()
	defer server.Stop()

	lease, err := server.broker.LeaseAcceptAndServe(context.Background(), 300*time.Millisecond, leasePingPongServer("pong"))
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	// Renew the lease well past its original TTL.
	for i := 0; i < 5; i++ {
		time.Sleep(150 * time.Millisecond)
		if err := lease.Extend(300 * time.Millisecond); err != nil {
			t.Fatalf("err: %s", err)
		}
	}

	// The endpoint must still be reachable after ~750ms, far beyond the
	// original 300ms TTL.
	pong, err := leasePing(client.broker, lease.ID())
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if pong != "pong" {
		t.Fatalf("bad: %q", pong)
	}

	// Stop renewing; the lease must expire on its own.
	waitLeaseDone(t, lease)

	if err := lease.Extend(time.Minute); err == nil {
		t.Fatal("expected extending an expired lease to fail")
	}

	if _, err := leasePing(client.broker, lease.ID()); err == nil {
		t.Fatal("expected dialling an expired lease to fail")
	}

	if n := leaseCount(server.broker); n != 0 {
		t.Fatalf("expected no active leases, got: %d", n)
	}
}

// TestGRPCBrokerLease_ExpiryReconnect verifies that once a lease expires,
// its ID is never reused: reconnecting requires a new lease with a new ID,
// and the stale ID keeps failing even while the new endpoint is live.
func TestGRPCBrokerLease_ExpiryReconnect(t *testing.T) {
	t.Run("default", func(t *testing.T) { testGRPCBrokerLeaseExpiryReconnect(t, false) })
	t.Run("mux", func(t *testing.T) { testGRPCBrokerLeaseExpiryReconnect(t, true) })
}

func testGRPCBrokerLeaseExpiryReconnect(t *testing.T, multiplex bool) {
	client, server := TestPluginGRPCConn(t, multiplex, map[string]Plugin{
		"test": new(testGRPCInterfacePlugin),
	})
	defer func() { _ = client.Close() }()
	defer server.Stop()

	first, err := server.broker.LeaseAcceptAndServe(context.Background(), 200*time.Millisecond, leasePingPongServer("first"))
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	pong, err := leasePing(client.broker, first.ID())
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if pong != "first" {
		t.Fatalf("bad: %q", pong)
	}

	// Let the first lease expire.
	waitLeaseDone(t, first)

	if _, err := leasePing(client.broker, first.ID()); err == nil {
		t.Fatal("expected dialling an expired lease to fail")
	}

	// Reconnect through a new lease; it must use a fresh ID.
	second, err := server.broker.LeaseAcceptAndServe(context.Background(), time.Minute, leasePingPongServer("second"))
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	defer func() { _ = second.Revoke() }()

	if second.ID() == first.ID() {
		t.Fatalf("lease ID was reused: %d", second.ID())
	}

	pong, err = leasePing(client.broker, second.ID())
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if pong != "second" {
		t.Fatalf("bad: %q", pong)
	}

	// A late connection attempt on the stale ID must not reach the new
	// service, even though it is live on the same broker.
	if _, err := leasePing(client.broker, first.ID()); err == nil {
		t.Fatal("expected stale lease ID to keep failing after reconnect")
	}
}

// TestGRPCBrokerLease_RevokeDialRace verifies that revoking a lease while a
// dial is in flight has a deterministic outcome: the dial either connects to
// the exact service that was leased, or it fails. It must never connect to
// a different endpoint.
func TestGRPCBrokerLease_RevokeDialRace(t *testing.T) {
	t.Run("default", func(t *testing.T) { testGRPCBrokerLeaseRevokeDialRace(t, false) })
	t.Run("mux", func(t *testing.T) { testGRPCBrokerLeaseRevokeDialRace(t, true) })
}

func testGRPCBrokerLeaseRevokeDialRace(t *testing.T, multiplex bool) {
	client, server := TestPluginGRPCConn(t, multiplex, map[string]Plugin{
		"test": new(testGRPCInterfacePlugin),
	})
	defer func() { _ = client.Close() }()
	defer server.Stop()

	for i := 0; i < 25; i++ {
		tag := fmt.Sprintf("pong-%d", i)
		lease, err := server.broker.LeaseAcceptAndServe(context.Background(), time.Minute, leasePingPongServer(tag))
		if err != nil {
			t.Fatalf("err: %s", err)
		}

		var wg sync.WaitGroup
		var pong string
		var dialErr error
		wg.Add(1)
		go func(id uint32) {
			defer wg.Done()
			pong, dialErr = leasePing(client.broker, id)
		}(lease.ID())

		if err := lease.Revoke(); err != nil {
			t.Fatalf("err: %s", err)
		}
		wg.Wait()

		if dialErr == nil && pong != tag {
			t.Fatalf("dial reached the wrong endpoint: got %q, want %q", pong, tag)
		}
	}

	// Once revocation has completed, further dials must fail.
	lease, err := server.broker.LeaseAcceptAndServe(context.Background(), time.Minute, leasePingPongServer("final"))
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if err := lease.Revoke(); err != nil {
		t.Fatalf("err: %s", err)
	}
	if _, err := leasePing(client.broker, lease.ID()); err == nil {
		t.Fatal("expected dialling a revoked lease to fail")
	}

	if n := leaseCount(server.broker); n != 0 {
		t.Fatalf("expected no active leases, got: %d", n)
	}
}

// TestGRPCBrokerLease_RevokeIdempotent verifies Revoke can be called
// multiple times and concurrently.
func TestGRPCBrokerLease_RevokeIdempotent(t *testing.T) {
	client, server := TestPluginGRPCConn(t, false, map[string]Plugin{
		"test": new(testGRPCInterfacePlugin),
	})
	defer func() { _ = client.Close() }()
	defer server.Stop()

	lease, err := server.broker.LeaseEndpoint(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := lease.Revoke(); err != nil {
				t.Errorf("err: %s", err)
			}
		}()
	}
	wg.Wait()
	waitLeaseDone(t, lease)

	if n := leaseCount(server.broker); n != 0 {
		t.Fatalf("expected no active leases, got: %d", n)
	}
}

// TestGRPCBrokerLease_BrokerClose verifies that closing the broker reclaims
// all active leases.
func TestGRPCBrokerLease_BrokerClose(t *testing.T) {
	client, server := TestPluginGRPCConn(t, false, map[string]Plugin{
		"test": new(testGRPCInterfacePlugin),
	})
	defer func() { _ = client.Close() }()

	lease, err := server.broker.LeaseEndpoint(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	server.Stop()

	select {
	case <-lease.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("expected broker close to reclaim the lease")
	}
}

// TestGRPCBrokerLease_InvalidTTL verifies invalid TTLs are rejected.
func TestGRPCBrokerLease_InvalidTTL(t *testing.T) {
	client, server := TestPluginGRPCConn(t, false, map[string]Plugin{
		"test": new(testGRPCInterfacePlugin),
	})
	defer func() { _ = client.Close() }()
	defer server.Stop()

	if _, err := server.broker.LeaseEndpoint(context.Background(), 0); err == nil {
		t.Fatal("expected error for zero ttl")
	}

	lease, err := server.broker.LeaseEndpoint(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	defer func() { _ = lease.Revoke() }()

	if err := lease.Extend(0); err == nil {
		t.Fatal("expected error for zero ttl extension")
	}
}

// TestGRPCBrokerLease_NoInterferenceWithStaticEndpoints verifies that
// regular AcceptAndServe endpoints are unaffected by lease revocation.
func TestGRPCBrokerLease_NoInterferenceWithStaticEndpoints(t *testing.T) {
	t.Run("default", func(t *testing.T) { testGRPCBrokerLeaseNoInterference(t, false) })
	t.Run("mux", func(t *testing.T) { testGRPCBrokerLeaseNoInterference(t, true) })
}

func testGRPCBrokerLeaseNoInterference(t *testing.T, multiplex bool) {
	client, server := TestPluginGRPCConn(t, multiplex, map[string]Plugin{
		"test": new(testGRPCInterfacePlugin),
	})
	defer func() { _ = client.Close() }()
	defer server.Stop()

	// A long-lived endpoint using the existing interface.
	staticID := server.broker.NextId()
	go server.broker.AcceptAndServe(staticID, leasePingPongServer("static"))

	lease, err := server.broker.LeaseAcceptAndServe(context.Background(), 200*time.Millisecond, leasePingPongServer("leased"))
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	waitLeaseDone(t, lease)

	// The static endpoint must still work after the lease expired.
	pong, err := leasePing(client.broker, staticID)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if !strings.HasPrefix(pong, "static") {
		t.Fatalf("bad: %q", pong)
	}
}
