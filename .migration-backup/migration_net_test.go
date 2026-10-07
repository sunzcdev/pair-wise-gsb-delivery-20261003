// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"testing"
	"time"
)

// TestNetworkTransport_EvictPeer ensures EvictPeer closes and removes pooled
// connections to an old address after an address migration so subsequent RPCs
// dial the current endpoint.
func TestNetworkTransport_EvictPeer(t *testing.T) {
	trans, err := NewTCPTransportWithLogger("localhost:0", nil, 3, time.Second, newTestLogger(t))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer func() { _ = trans.Close() }()

	target := ServerAddress("127.0.0.1:1")

	// Evicting an absent address is a safe no-op and leaves no entry behind.
	trans.EvictPeer(target)

	trans.connPoolLock.Lock()
	if _, ok := trans.connPool[target]; ok {
		t.Fatalf("expected no pool entry after eviction of absent address")
	}
	trans.connPoolLock.Unlock()

	// Eviction after a real RPC returned its connection to the pool drops the
	// idle connection.
	server, err := NewTCPTransportWithLogger("localhost:0", nil, 3, time.Second, newTestLogger(t))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer func() { _ = server.Close() }()

	go func() {
		select {
		case rpc := <-server.Consumer():
			rpc.Respond(&AppendEntriesResponse{}, nil)
		case <-time.After(2 * time.Second):
		}
	}()

	args := makeAppendRPC()
	var out AppendEntriesResponse
	if err := trans.AppendEntries("id1", server.LocalAddr(), &args, &out); err != nil {
		t.Fatalf("err: %v", err)
	}

	trans.connPoolLock.Lock()
	before := len(trans.connPool[server.LocalAddr()])
	trans.connPoolLock.Unlock()
	if before != 1 {
		t.Fatalf("expected one pooled connection, got %d", before)
	}

	trans.EvictPeer(server.LocalAddr())

	trans.connPoolLock.Lock()
	_, present := trans.connPool[server.LocalAddr()]
	trans.connPoolLock.Unlock()
	if present {
		t.Fatalf("expected pooled connection to be evicted")
	}
}
