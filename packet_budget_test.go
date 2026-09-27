// Copyright IBM Corp. 2013, 2026
// SPDX-License-Identifier: MPL-2.0

package memberlist

import (
	"bytes"
	"crypto/rand"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// recordingBudgetTransport wraps a NodeAwareTransport, records the size of
// every packet written through it, and reports a per-destination packet
// budget, simulating paths (e.g. tunnels) with a smaller effective MTU.
type recordingBudgetTransport struct {
	NodeAwareTransport
	budgets map[string]int // destination addr -> budget; missing means no opinion

	mu     sync.Mutex
	sentTo map[string][]int // destination addr -> sizes of packets written
}

func (t *recordingBudgetTransport) WriteToAddress(b []byte, a Address) (time.Time, error) {
	t.mu.Lock()
	if t.sentTo == nil {
		t.sentTo = make(map[string][]int)
	}
	t.sentTo[a.Addr] = append(t.sentTo[a.Addr], len(b))
	t.mu.Unlock()
	return t.NodeAwareTransport.WriteToAddress(b, a)
}

// See PacketBudgetTransport.
func (t *recordingBudgetTransport) PacketBudget(a Address) int {
	return t.budgets[a.Addr]
}

func (t *recordingBudgetTransport) sizesFor(addr string) []int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]int(nil), t.sentTo[addr]...)
}

func mockAddr(t *testing.T, tr *MockTransport) string {
	t.Helper()
	ip, port, err := tr.FinalAdvertiseAddr("", 0)
	require.NoError(t, err)
	return joinHostPort(ip.String(), uint16(port))
}

// budgetTestCluster builds a sender and two peers on a MockNetwork where
// the "tunneled" peer has a smaller effective packet budget.
type budgetTestCluster struct {
	sender   *Memberlist
	direct   *Memberlist
	tunneled *Memberlist

	directDelegate   *MockDelegate
	tunneledDelegate *MockDelegate

	senderTransport *recordingBudgetTransport

	directAddr   string
	tunneledAddr string

	directNode   *Node
	tunneledNode *Node
}

func newBudgetTestCluster(t *testing.T, tunneledBudget int) *budgetTestCluster {
	t.Helper()

	mockNet := &MockNetwork{}
	st := &recordingBudgetTransport{
		NodeAwareTransport: mockNet.NewTransport("sender"),
		budgets:            map[string]int{},
	}
	dt := mockNet.NewTransport("direct")
	tt := mockNet.NewTransport("tunneled")

	directAddr := mockAddr(t, dt)
	tunneledAddr := mockAddr(t, tt)

	// The transport reports a smaller budget for the tunneled peer, and
	// the config hook reports budgets of its own: the effective budget
	// must be the minimum of all sources, with UDPBufferSize as the cap.
	st.budgets[tunneledAddr] = tunneledBudget

	sender := GetMemberlist(t, func(c *Config) {
		c.Transport = st
		c.PacketBudgetFunc = func(addr string) (int, bool) {
			switch addr {
			case tunneledAddr:
				return tunneledBudget + 200, true
			case directAddr:
				// Larger than UDPBufferSize on purpose: the global
				// UDPBufferSize must remain the upper bound.
				return 2 * c.UDPBufferSize, true
			}
			return 0, false
		}
	})
	t.Cleanup(func() { _ = sender.Shutdown() })

	directDelegate := &MockDelegate{}
	direct := GetMemberlist(t, func(c *Config) {
		c.Name = "direct"
		c.Transport = dt
		c.Delegate = directDelegate
	})
	t.Cleanup(func() { _ = direct.Shutdown() })

	tunneledDelegate := &MockDelegate{}
	tunneled := GetMemberlist(t, func(c *Config) {
		c.Name = "tunneled"
		c.Transport = tt
		c.Delegate = tunneledDelegate
	})
	t.Cleanup(func() { _ = tunneled.Shutdown() })

	// Register the sender and both peers in the sender's node map.
	selfIP, selfPort, err := st.FinalAdvertiseAddr("", 0)
	require.NoError(t, err)
	self := alive{
		Incarnation: 1,
		Node:        sender.config.Name,
		Addr:        selfIP,
		Port:        uint16(selfPort),
		Vsn:         sender.config.BuildVsnArray(),
	}
	sender.aliveNode(&self, nil, true)

	directIP, directPort, err := dt.FinalAdvertiseAddr("", 0)
	require.NoError(t, err)
	directNode := &Node{Name: "direct", Addr: directIP, Port: uint16(directPort), PMax: ProtocolVersionMax}
	aDirect := alive{
		Incarnation: 1,
		Node:        "direct",
		Addr:        directIP,
		Port:        uint16(directPort),
		Vsn:         sender.config.BuildVsnArray(),
	}
	sender.aliveNode(&aDirect, nil, false)

	tunneledIP, tunneledPort, err := tt.FinalAdvertiseAddr("", 0)
	require.NoError(t, err)
	tunneledNode := &Node{Name: "tunneled", Addr: tunneledIP, Port: uint16(tunneledPort), PMax: ProtocolVersionMax}
	aTunneled := alive{
		Incarnation: 1,
		Node:        "tunneled",
		Addr:        tunneledIP,
		Port:        uint16(tunneledPort),
		Vsn:         sender.config.BuildVsnArray(),
	}
	sender.aliveNode(&aTunneled, nil, false)

	return &budgetTestCluster{
		sender:           sender,
		direct:           direct,
		tunneled:         tunneled,
		directDelegate:   directDelegate,
		tunneledDelegate: tunneledDelegate,
		senderTransport:  st,
		directAddr:       directAddr,
		tunneledAddr:     tunneledAddr,
		directNode:       directNode,
		tunneledNode:     tunneledNode,
	}
}

func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func userMsgFrame(payload []byte) []byte {
	msg := make([]byte, 1, len(payload)+1)
	msg[0] = byte(userMsg)
	return append(msg, payload...)
}

func containsMsg(msgs [][]byte, payload []byte) bool {
	for _, m := range msgs {
		if bytes.Equal(m, payload) {
			return true
		}
	}
	return false
}

// randomBytes returns incompressible payload data, so that compression
// (enabled by default) cannot shrink a message below its intended size.
func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	return buf
}

func TestMemberlist_PerPeerPacketBudget(t *testing.T) {
	const tunneledBudget = 600
	c := newBudgetTestCluster(t, tunneledBudget)
	m := c.sender

	// The effective budget is the minimum of UDPBufferSize, the config
	// hook, and the transport-reported budget.
	require.Equal(t, m.config.UDPBufferSize, m.packetBudget(Address{Addr: c.directAddr}))
	require.Equal(t, tunneledBudget, m.packetBudget(Address{Addr: c.tunneledAddr}))

	// small fits both budgets, big fits only the direct peer's budget, and
	// oversized can never fit anywhere.
	small := bytes.Repeat([]byte("s"), 99)
	big := bytes.Repeat([]byte("b"), 999)
	oversized := bytes.Repeat([]byte("o"), m.maxBroadcastBytes())

	require.Less(t, len(big)+compoundOverhead, m.packetBytesAvail(Address{Addr: c.directAddr}))
	require.Greater(t, len(big)+compoundOverhead, m.packetBytesAvail(Address{Addr: c.tunneledAddr}))
	require.Greater(t, len(oversized)+1, m.maxBroadcastBytes())

	m.queueBroadcast("n1", userMsgFrame(small), nil)
	m.queueBroadcast("n2", userMsgFrame(big), nil)

	// The oversized broadcast is rejected explicitly instead of being
	// stuck at the head of the queue forever.
	queuedBefore := m.broadcasts.NumQueued()
	oversizedNotify := make(chan struct{}, 1)
	m.queueBroadcast("n3", userMsgFrame(oversized), oversizedNotify)
	require.Equal(t, queuedBefore, m.broadcasts.NumQueued())
	select {
	case <-oversizedNotify:
	case <-time.After(time.Second):
		t.Fatal("oversized broadcast was rejected without notifying waiters")
	}

	// Gossip to both peers.
	m.gossip()

	// The small message reaches both peers; the big one only reaches the
	// direct peer.
	waitForCond(t, "direct peer to receive small and big messages", func() bool {
		msgs := c.directDelegate.getMessages()
		return containsMsg(msgs, small) && containsMsg(msgs, big)
	})
	waitForCond(t, "tunneled peer to receive the small message", func() bool {
		return containsMsg(c.tunneledDelegate.getMessages(), small)
	})

	// The big message must never be sent to the tunneled peer.
	time.Sleep(100 * time.Millisecond)
	require.False(t, containsMsg(c.tunneledDelegate.getMessages(), big),
		"message exceeding the per-peer budget was sent to the tunneled peer")

	// Every packet on the wire respected the effective budget of its
	// destination, and the global UDPBufferSize remains the upper bound.
	directSizes := c.senderTransport.sizesFor(c.directAddr)
	require.NotEmpty(t, directSizes)
	for _, size := range directSizes {
		require.LessOrEqual(t, size, m.config.UDPBufferSize,
			"packet to direct peer exceeds UDPBufferSize")
	}
	tunneledSizes := c.senderTransport.sizesFor(c.tunneledAddr)
	require.NotEmpty(t, tunneledSizes)
	for _, size := range tunneledSizes {
		require.LessOrEqual(t, size, tunneledBudget,
			"packet to tunneled peer exceeds its effective budget")
	}
}

func TestMemberlist_PacketBudgetGuard(t *testing.T) {
	const tunneledBudget = 600
	c := newBudgetTestCluster(t, tunneledBudget)
	m := c.sender

	// A user message that fits the global UDPBufferSize but not the
	// tunneled peer's budget is rejected explicitly.
	big := randomBytes(t, tunneledBudget)
	err := m.SendBestEffort(c.tunneledNode, big)
	require.ErrorContains(t, err, "exceeds effective budget")

	// The same message fits the direct peer's budget fine.
	require.NoError(t, m.SendBestEffort(c.directNode, big))

	// UDPBufferSize remains the upper bound even for peers without a
	// smaller budget. The wire length here includes the userMsg type byte
	// and the CRC header added for protocol >= 5 peers.
	tooBig := randomBytes(t, m.config.UDPBufferSize)
	err = m.SendBestEffort(c.directNode, tooBig)
	require.ErrorContains(t, err, "exceeds effective budget")

	// Nothing oversized may have hit the wire.
	for _, size := range c.senderTransport.sizesFor(c.tunneledAddr) {
		require.LessOrEqual(t, size, tunneledBudget)
	}
	for _, size := range c.senderTransport.sizesFor(c.directAddr) {
		require.LessOrEqual(t, size, m.config.UDPBufferSize)
	}
}

func TestMemberlist_PacketBudgetWithLabel(t *testing.T) {
	const tunneledBudget = 600
	mockNet := &MockNetwork{}
	st := &recordingBudgetTransport{
		NodeAwareTransport: mockNet.NewTransport("sender"),
		budgets:            map[string]int{},
	}
	tt := mockNet.NewTransport("tunneled")
	tunneledAddr := mockAddr(t, tt)
	st.budgets[tunneledAddr] = tunneledBudget

	tunneledIP, tunneledPort, err := tt.FinalAdvertiseAddr("", 0)
	require.NoError(t, err)

	// A label must not hide the transport-reported budget, and the label
	// itself counts against the wire budget.
	m := GetMemberlist(t, func(c *Config) {
		c.Transport = st
		c.Label = "budget"
	})
	t.Cleanup(func() { _ = m.Shutdown() })

	require.Equal(t, tunneledBudget, m.packetBudget(Address{Addr: tunneledAddr}))

	tunneledNode := &Node{Name: "tunneled", Addr: tunneledIP, Port: uint16(tunneledPort), PMax: ProtocolVersionMax}
	// Budget minus the label overhead leaves no room for this message.
	err = m.SendBestEffort(tunneledNode, randomBytes(t, tunneledBudget))
	require.ErrorContains(t, err, "exceeds effective budget")
}

func TestLabelWrappedTransport_PacketBudget(t *testing.T) {
	mockNet := &MockNetwork{}
	base := mockNet.NewTransport("node")

	// A budget reported by the wrapped transport is forwarded.
	wrapped := &labelWrappedTransport{
		label: "foo",
		NodeAwareTransport: &recordingBudgetTransport{
			NodeAwareTransport: base,
			budgets:            map[string]int{"127.0.0.1:9": 600},
		},
	}
	require.Equal(t, 600, wrapped.PacketBudget(Address{Addr: "127.0.0.1:9"}))

	// A wrapped transport without a budget reports no opinion.
	plain := &labelWrappedTransport{label: "foo", NodeAwareTransport: base}
	require.Equal(t, 0, plain.PacketBudget(Address{Addr: "127.0.0.1:9"}))
}
