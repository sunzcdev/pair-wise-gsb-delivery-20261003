// Copyright IBM Corp. 2013, 2026
// SPDX-License-Identifier: MPL-2.0

package memberlist

import (
	"bytes"
	"crypto/rand"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// budgetTransport wraps a MockTransport and reports per-peer packet budgets,
// simulating links (e.g. tunnels) with a lower MTU towards some peers.
type budgetTransport struct {
	*MockTransport
	budgets map[string]int // keyed by Address.Addr
}

// See PacketBudgetAwareTransport.
func (t *budgetTransport) PacketBudget(a Address) int {
	return t.budgets[a.Addr]
}

// packetCollector drains a packet channel in the background, recording every
// packet received for later inspection.
type packetCollector struct {
	mu      sync.Mutex
	packets [][]byte
}

func collectPackets(ch <-chan *Packet) *packetCollector {
	c := &packetCollector{}
	go func() {
		for p := range ch {
			c.mu.Lock()
			c.packets = append(c.packets, p.Buf)
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *packetCollector) all() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.packets))
	copy(out, c.packets)
	return out
}

// testBroadcast is a Broadcast that signals on a channel when it is finished.
type testBroadcast struct {
	msg      []byte
	finished chan struct{}
}

func newTestBroadcast(size int) *testBroadcast {
	msg := make([]byte, size)
	// Random bytes defeat compression so the wire size is predictable.
	_, _ = rand.Read(msg)
	return &testBroadcast{msg: msg, finished: make(chan struct{}, 1)}
}

func (b *testBroadcast) Invalidates(Broadcast) bool { return false }
func (b *testBroadcast) Message() []byte            { return b.msg }
func (b *testBroadcast) Finished() {
	select {
	case b.finished <- struct{}{}:
	default:
	}
}

func requireFinished(t *testing.T, b *testBroadcast, msg string) {
	t.Helper()
	select {
	case <-b.finished:
	case <-time.After(5 * time.Second):
		t.Fatal(msg)
	}
}

// addMockPeer registers a fake peer node so that gossip will target it.
func addMockPeer(t *testing.T, m *Memberlist, tr *MockTransport, name string) {
	t.Helper()
	ip, port, err := tr.FinalAdvertiseAddr("", 0)
	require.NoError(t, err)
	a := alive{
		Node:        name,
		Addr:        ip,
		Port:        uint16(port),
		Incarnation: 1,
		Vsn:         m.config.BuildVsnArray(),
	}
	m.aliveNode(&a, nil, false)
}

// TestPacketBudget_PerPeerWireSize sets up two peers with different
// transport-reported packet budgets and verifies that every packet put on
// the wire towards each peer (compound header, encryption and label
// included) respects that peer's budget.
func TestPacketBudget_PerPeerWireSize(t *testing.T) {
	mockNet := &MockNetwork{}
	peerBig := mockNet.NewTransport("peer-big")
	peerSmall := mockNet.NewTransport("peer-small")

	_, bigPort, err := peerBig.FinalAdvertiseAddr("", 0)
	require.NoError(t, err)
	_, smallPort, err := peerSmall.FinalAdvertiseAddr("", 0)
	require.NoError(t, err)

	bigAddr := joinHostPort("127.0.0.1", uint16(bigPort))
	smallAddr := joinHostPort("127.0.0.1", uint16(smallPort))

	const (
		globalBudget = 1400
		smallBudget  = 600
	)

	sender := &budgetTransport{
		MockTransport: mockNet.NewTransport("sender"),
		budgets: map[string]int{
			bigAddr:   5000, // tries to exceed the global cap: must be clamped
			smallAddr: smallBudget,
		},
	}

	secret := make([]byte, 16)
	_, err = rand.Read(secret)
	require.NoError(t, err)

	m1 := GetMemberlist(t, func(c *Config) {
		c.Transport = sender
		c.UDPBufferSize = globalBudget
		c.Label = "budget"
		c.SecretKey = secret
		c.GossipInterval = 0 // manual gossip only
		c.GossipNodes = 2
	})
	defer func() { _ = m1.Shutdown() }()

	addMockPeer(t, m1, peerBig, "peer-big")
	addMockPeer(t, m1, peerSmall, "peer-small")

	// Queue enough broadcasts to fill several packets at either budget.
	for i := 0; i < 12; i++ {
		m1.broadcasts.QueueBroadcast(newTestBroadcast(250))
	}

	bigColl := collectPackets(peerBig.packetCh)
	smallColl := collectPackets(peerSmall.packetCh)

	m1.gossip()
	time.Sleep(100 * time.Millisecond)

	bigPkts := bigColl.all()
	smallPkts := smallColl.all()
	require.NotEmpty(t, bigPkts, "expected packets towards the big-budget peer")
	require.NotEmpty(t, smallPkts, "expected packets towards the small-budget peer")

	for _, p := range bigPkts {
		require.LessOrEqual(t, len(p), globalBudget,
			"packet towards big-budget peer exceeds the global UDPBufferSize cap")
	}
	for _, p := range smallPkts {
		require.LessOrEqual(t, len(p), smallBudget,
			"packet towards small-budget peer exceeds its per-peer budget")
	}

	// Prove the budgets actually differ on the wire: the big-budget peer
	// must receive at least one packet that would not fit the small budget.
	sawLarge := false
	for _, p := range bigPkts {
		if len(p) > smallBudget {
			sawLarge = true
		}
	}
	require.True(t, sawLarge, "expected at least one packet larger than the small budget towards the big-budget peer")
}

// TestPacketBudget_OversizedBroadcastRejected verifies that a broadcast which
// can never fit into a packet (even at the global UDPBufferSize) is
// explicitly rejected instead of clogging the head of the queue, while
// smaller broadcasts still reach the peer.
func TestPacketBudget_OversizedBroadcastRejected(t *testing.T) {
	mockNet := &MockNetwork{}
	peer := mockNet.NewTransport("peer")

	logs := &bytes.Buffer{}
	m1 := GetMemberlist(t, func(c *Config) {
		c.Transport = mockNet.NewTransport("sender")
		c.UDPBufferSize = 600
		c.EnableCompression = false
		c.GossipInterval = 0 // manual gossip only
		c.GossipNodes = 1
		c.Logger = log.New(logs, "", 0)
	})
	defer func() { _ = m1.Shutdown() }()

	addMockPeer(t, m1, peer, "peer")

	// 800 bytes can never fit: 600 - overheads is the hard limit.
	big := newTestBroadcast(800)
	small := newTestBroadcast(100)
	m1.broadcasts.QueueBroadcast(big)
	m1.broadcasts.QueueBroadcast(small)

	coll := collectPackets(peer.packetCh)

	m1.gossip()

	// The oversized broadcast must be explicitly rejected: Finished is
	// invoked and it is removed from the queue.
	requireFinished(t, big, "oversized broadcast was not rejected")
	require.Contains(t, logs.String(), "Dropping broadcast")

	// The small broadcast must still be delivered to the peer, and the
	// oversized one must never appear on the wire.
	deadline := time.After(5 * time.Second)
	for {
		delivered := false
		for _, p := range coll.all() {
			require.NotContains(t, p, big.msg, "oversized broadcast escaped onto the wire")
			if bytes.Contains(p, small.msg) {
				delivered = true
			}
		}
		if delivered {
			break
		}
		select {
		case <-deadline:
			t.Fatal("small broadcast was never delivered to the peer")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestPacketBudget_Precedence checks that the effective per-peer budget is
// the smallest of the global UDPBufferSize, the transport-reported budget
// and the configured per-node budget, and that UDPBufferSize always remains
// the upper bound.
func TestPacketBudget_Precedence(t *testing.T) {
	mockNet := &MockNetwork{}
	sender := &budgetTransport{
		MockTransport: mockNet.NewTransport("sender"),
		budgets: map[string]int{
			"10.0.0.1:7946": 500,  // transport shrinks the budget
			"10.0.0.2:7946": 5000, // transport tries to grow past the cap
		},
	}

	m1 := GetMemberlist(t, func(c *Config) {
		c.Transport = sender
		c.UDPBufferSize = 1400
		// A label also exercises the forwarding of PacketBudget through
		// the labelWrappedTransport.
		c.Label = "budget"
		c.PacketBudgetForNode = func(name string) int {
			if name == "tunnel-node" {
				return 300
			}
			return 0
		}
	})
	defer func() { _ = m1.Shutdown() }()

	// Transport-reported budget applies.
	require.Equal(t, 500, m1.packetBudget(Address{Addr: "10.0.0.1:7946"}))
	// UDPBufferSize remains the upper bound.
	require.Equal(t, 1400, m1.packetBudget(Address{Addr: "10.0.0.2:7946"}))
	// Per-node configured budget applies.
	require.Equal(t, 300, m1.packetBudget(Address{Addr: "10.0.0.9:7946", Name: "tunnel-node"}))
	// No opinion from either source: global default.
	require.Equal(t, 1400, m1.packetBudget(Address{Addr: "10.0.0.9:7946", Name: "other"}))
	// Transport and per-node budgets combine: the smallest wins.
	require.Equal(t, 300, m1.packetBudget(Address{Addr: "10.0.0.1:7946", Name: "tunnel-node"}))
}

func TestTransmitLimitedQueue_PruneOversized(t *testing.T) {
	q := &TransmitLimitedQueue{RetransmitMult: 1}
	q.NumNodes = func() int { return 1 }

	big := newTestBroadcast(100)
	small := newTestBroadcast(10)
	q.QueueBroadcast(big)
	q.QueueBroadcast(small)

	dropped := q.PruneOversized(2, 50)
	require.Len(t, dropped, 1)
	require.Equal(t, big.msg, dropped[0])

	// The oversized broadcast is finished and gone, the small one stays.
	requireFinished(t, big, "Finished not called for oversized broadcast")
	require.Equal(t, 1, q.NumQueued())
	select {
	case <-small.finished:
		t.Fatal("small broadcast should not have been finished")
	default:
	}

	// Nothing left to prune.
	require.Nil(t, q.PruneOversized(2, 50))
}
