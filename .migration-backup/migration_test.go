// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// proxyTransport wraps an InmemTransport. Inbound RPCs arrive on the wrapped
// transport and are re-published on the proxy's own consumer (the one Raft
// reads). Once Raft responds, the response can be delayed, letting tests
// simulate a slow reply still in flight over an old connection.
type proxyTransport struct {
	base *InmemTransport

	consumerCh chan RPC

	delayLock sync.Mutex
	delayGate chan struct{}
}

func newProxyTransport(addr ServerAddress) (ServerAddress, *proxyTransport) {
	a, base := NewInmemTransport(addr)
	p := &proxyTransport{
		base:       base,
		consumerCh: make(chan RPC, 16),
	}
	go p.forward()
	return a, p
}

func (p *proxyTransport) forward() {
	for rpc := range p.base.Consumer() {
		respCh := make(chan RPCResponse, 1)
		inbound := RPC{
			Command:  rpc.Command,
			Reader:   rpc.Reader,
			RespChan: respCh,
		}

		select {
		case p.consumerCh <- inbound:
		case <-time.After(time.Second):
			continue
		}

		var response RPCResponse
		select {
		case response = <-respCh:
		case <-time.After(5 * time.Second):
			return
		}

		// Hold data AppendEntries responses while armed, simulating an old
		// connection whose reply arrives only after the address has moved.
		if _, ok := rpc.Command.(*AppendEntriesRequest); ok {
			p.delayLock.Lock()
			gate := p.delayGate
			p.delayLock.Unlock()
			if gate != nil {
				select {
				case <-gate:
				case <-time.After(10 * time.Second):
					return
				}
			}
		}

		rpc.Respond(response.Response, response.Error)
	}
}

// armDelay makes subsequent AppendEntries responses block until releaseDelay
// is called.
func (p *proxyTransport) armDelay() {
	p.delayLock.Lock()
	defer p.delayLock.Unlock()
	if p.delayGate == nil {
		p.delayGate = make(chan struct{})
	}
}

func (p *proxyTransport) releaseDelay() {
	p.delayLock.Lock()
	defer p.delayLock.Unlock()
	if p.delayGate != nil {
		close(p.delayGate)
		p.delayGate = nil
	}
}

func (p *proxyTransport) Consumer() <-chan RPC { return p.consumerCh }

func (p *proxyTransport) LocalAddr() ServerAddress { return p.base.LocalAddr() }

func (p *proxyTransport) AppendEntriesPipeline(id ServerID, target ServerAddress) (AppendPipeline, error) {
	return p.base.AppendEntriesPipeline(id, target)
}

func (p *proxyTransport) AppendEntries(id ServerID, target ServerAddress, args *AppendEntriesRequest, resp *AppendEntriesResponse) error {
	return p.base.AppendEntries(id, target, args, resp)
}

func (p *proxyTransport) RequestVote(id ServerID, target ServerAddress, args *RequestVoteRequest, resp *RequestVoteResponse) error {
	return p.base.RequestVote(id, target, args, resp)
}

func (p *proxyTransport) RequestPreVote(id ServerID, target ServerAddress, args *RequestPreVoteRequest, resp *RequestPreVoteResponse) error {
	return p.base.RequestPreVote(id, target, args, resp)
}

func (p *proxyTransport) InstallSnapshot(id ServerID, target ServerAddress, args *InstallSnapshotRequest, resp *InstallSnapshotResponse, data io.Reader) error {
	return p.base.InstallSnapshot(id, target, args, resp, data)
}

func (p *proxyTransport) EncodePeer(id ServerID, addr ServerAddress) []byte {
	return p.base.EncodePeer(id, addr)
}

func (p *proxyTransport) DecodePeer(b []byte) ServerAddress {
	return p.base.DecodePeer(b)
}

func (p *proxyTransport) SetHeartbeatHandler(cb func(rpc RPC)) {
	p.base.SetHeartbeatHandler(cb)
}

func (p *proxyTransport) TimeoutNow(id ServerID, target ServerAddress, args *TimeoutNowRequest, resp *TimeoutNowResponse) error {
	return p.base.TimeoutNow(id, target, args, resp)
}

func (p *proxyTransport) VerifyServer(id ServerID, target ServerAddress, args *VerifyServerRequest, resp *VerifyServerResponse) error {
	return p.base.VerifyServer(id, target, args, resp)
}

func (p *proxyTransport) EvictPeer(address ServerAddress) {}

func (p *proxyTransport) Close() error { return p.base.Close() }

// migrationNode is one member of a migration test cluster.
type migrationNode struct {
	id    ServerID
	addr  ServerAddress
	trans *proxyTransport
	raft  *Raft
	fsm   *MockFSM
}

// makeMigrationCluster builds a connected, bootstrapped 3-voter cluster whose
// transports are delay-capable proxies.
func makeMigrationCluster(t *testing.T) []*migrationNode {
	t.Helper()
	conf := inmemConfig(t)
	conf.HeartbeatTimeout = 200 * time.Millisecond
	conf.ElectionTimeout = 200 * time.Millisecond
	conf.LeaderLeaseTimeout = 200 * time.Millisecond

	nodes := make([]*migrationNode, 3)
	var configuration Configuration
	for i := range nodes {
		addr, trans := newProxyTransport("")
		id := ServerID("server-" + string(addr))
		nodes[i] = &migrationNode{
			id:    id,
			addr:  addr,
			trans: trans,
			fsm:   &MockFSM{},
		}
		configuration.Servers = append(configuration.Servers, Server{
			Suffrage: Voter, ID: id, Address: addr,
		})
	}

	for _, n := range nodes {
		store := NewInmemStore()
		_, snap := FileSnapTest(t)

		peerConf := *conf
		peerConf.LocalID = n.id
		peerConf.Logger = newTestLoggerWithPrefix(t, string(n.id))

		require.NoError(t, BootstrapCluster(&peerConf, store, store, snap, n.trans, configuration))
		r, err := NewRaft(&peerConf, n.fsm, store, store, snap, n.trans)
		require.NoError(t, err)
		n.raft = r
	}

	// Fully connect the underlying in-mem transports.
	for i, a := range nodes {
		for j, b := range nodes {
			if i == j {
				continue
			}
			a.trans.base.Connect(b.addr, b.trans.base)
			b.trans.base.Connect(a.addr, a.trans.base)
		}
	}

	t.Cleanup(func() {
		for _, n := range nodes {
			if n.raft != nil {
				_ = n.raft.Shutdown().Error()
			}
			_ = n.trans.Close()
		}
	})
	return nodes
}

// leaderNode returns the stable leader among the nodes.
func leaderNode(nodes []*migrationNode) *migrationNode {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			if n.raft.State() == Leader {
				return n
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// waitForConfig polls until the configuration on every node matches pred.
func waitForConfig(t *testing.T, nodes []*migrationNode, pred func(Configuration) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ok := true
		for _, n := range nodes {
			future := n.raft.GetConfiguration()
			if err := future.Error(); err != nil {
				ok = false
				break
			}
			if !pred(future.Configuration()) {
				ok = false
				break
			}
		}
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for configuration condition")
}

// forwardTransport bridges an extra listening InmemTransport onto the node's
// proxy consumer, emulating the same node being reachable on a second
// endpoint.
func forwardTransport(t *testing.T, proxy *proxyTransport, extra *InmemTransport) {
	t.Helper()
	go func() {
		for rpc := range extra.Consumer() {
			out := make(chan RPCResponse, 1)
			inbound := RPC{Command: rpc.Command, Reader: rpc.Reader, RespChan: out}
			select {
			case proxy.consumerCh <- inbound:
			case <-time.After(5 * time.Second):
				return
			}
			select {
			case resp := <-out:
				rpc.Respond(resp.Response, resp.Error)
			case <-time.After(5 * time.Second):
				return
			}
		}
	}()
}

// findNode returns the node with the given id.
func findNode(nodes []*migrationNode, id ServerID) *migrationNode {
	for _, n := range nodes {
		if n.id == id {
			return n
		}
	}
	return nil
}

func TestServerAddressMigration_HappyPath(t *testing.T) {
	nodes := makeMigrationCluster(t)
	leader := leaderNode(nodes)
	require.NotNil(t, leader)

	// Choose a follower to migrate.
	var target *migrationNode
	for _, n := range nodes {
		if n.id != leader.id {
			target = n
			break
		}
	}
	require.NotNil(t, target)

	// Stand up a second endpoint that serves the same node.
	newAddr, extraTrans := NewInmemTransport("")
	forwardTransport(t, target.trans, extraTrans)
	for _, n := range nodes {
		n.trans.base.Connect(newAddr, extraTrans)
	}

	// Prepare must succeed: the new endpoint identifies as the same server.
	require.NoError(t, leader.raft.PrepareServerAddress(target.id, target.addr, newAddr, time.Second).Error())

	// Committing without any intervening config change switches the address.
	future := leader.raft.CommitServerAddress(target.id, target.addr, newAddr, time.Second)
	require.NoError(t, future.Error())

	// Diagnostics show exactly one entry for the id, now at the new address.
	waitForConfig(t, nodes, func(c Configuration) bool {
		count := 0
		var addr ServerAddress
		for _, s := range c.Servers {
			if s.ID == target.id {
				count++
				addr = s.Address
			}
		}
		return count == 1 && addr == newAddr && len(c.Servers) == 3
	})

	// Replication still flows and a subsequent apply reaches everyone.
	require.NoError(t, leader.raft.Apply([]byte("post-migrate"), time.Second).Error())

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		same := true
		for _, n := range nodes {
			if len(n.fsm.Logs()) == 0 ||
				string(n.fsm.Logs()[len(n.fsm.Logs())-1]) != "post-migrate" {
				same = false
			}
		}
		if same {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("post-migration apply did not replicate")
}

func TestServerAddressMigration_IdentityMismatch(t *testing.T) {
	nodes := makeMigrationCluster(t)
	leader := leaderNode(nodes)
	require.NotNil(t, leader)

	var target *migrationNode
	for _, n := range nodes {
		if n.id != leader.id {
			target = n
			break
		}
	}

	// Point the new address at a *different* existing node (the other
	// follower). The probe response header carries that node's ID.
	var foreign *migrationNode
	for _, n := range nodes {
		if n.id != leader.id && n.id != target.id {
			foreign = n
			break
		}
	}

	// Preparing directly against the foreign node's real address must fail with
	// an identity mismatch and must not modify the configuration.
	err := leader.raft.PrepareServerAddress(target.id, target.addr, foreign.addr, time.Second).Error()
	require.ErrorIs(t, err, ErrAddressMigrationMismatch)

	cf := leader.raft.GetConfiguration()
	require.NoError(t, cf.Error())
	for _, s := range cf.Configuration().Servers {
		if s.ID == target.id {
			require.Equal(t, target.addr, s.Address)
		}
	}

	// Commit without a valid prepare fails, configuration untouched.
	bogus := NewInmemAddr()
	err = leader.raft.CommitServerAddress(target.id, target.addr, bogus, time.Second).Error()
	require.ErrorIs(t, err, ErrAddressMigrationNotPrepared)
}

func TestServerAddressMigration_Abort(t *testing.T) {
	nodes := makeMigrationCluster(t)
	leader := leaderNode(nodes)
	require.NotNil(t, leader)

	var target *migrationNode
	for _, n := range nodes {
		if n.id != leader.id {
			target = n
			break
		}
	}

	newAddr, extraTrans := NewInmemTransport("")
	forwardTransport(t, target.trans, extraTrans)
	for _, n := range nodes {
		n.trans.base.Connect(newAddr, extraTrans)
	}

	require.NoError(t, leader.raft.PrepareServerAddress(target.id, target.addr, newAddr, time.Second).Error())

	// A conflicting prepare is rejected while one is outstanding.
	other := NewInmemAddr()
	err := leader.raft.PrepareServerAddress(target.id, target.addr, other, time.Second).Error()
	require.ErrorIs(t, err, ErrAddressMigrationInProgress)

	require.NoError(t, leader.raft.AbortServerAddress(target.id, time.Second).Error())

	// The commit is no longer prepared.
	err = leader.raft.CommitServerAddress(target.id, target.addr, newAddr, time.Second).Error()
	require.ErrorIs(t, err, ErrAddressMigrationNotPrepared)

	// Abort again is a harmless no-op.
	require.NoError(t, leader.raft.AbortServerAddress(target.id, time.Second).Error())

	// After aborting, the same target can be prepared and committed cleanly.
	require.NoError(t, leader.raft.PrepareServerAddress(target.id, target.addr, newAddr, time.Second).Error())
	require.NoError(t, leader.raft.CommitServerAddress(target.id, target.addr, newAddr, time.Second).Error())

	waitForConfig(t, nodes, func(c Configuration) bool {
		for _, s := range c.Servers {
			if s.ID == target.id {
				return s.Address == newAddr
			}
		}
		return false
	})
}

func TestServerAddressMigration_LeadershipChange(t *testing.T) {
	nodes := makeMigrationCluster(t)
	leader := leaderNode(nodes)
	require.NotNil(t, leader)

	var target *migrationNode
	for _, n := range nodes {
		if n.id != leader.id {
			target = n
			break
		}
	}

	newAddr, extraTrans := NewInmemTransport("")
	forwardTransport(t, target.trans, extraTrans)
	for _, n := range nodes {
		n.trans.base.Connect(newAddr, extraTrans)
	}

	require.NoError(t, leader.raft.PrepareServerAddress(target.id, target.addr, newAddr, time.Second).Error())

	// Force the old leader out by shutting it down; the prepared migration is
	// term-scoped and is lost when leadership changes.
	oldLeader := leader
	require.NoError(t, oldLeader.raft.Shutdown().Error())

	var newLeader *migrationNode
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			if n.id != oldLeader.id && n.raft.State() == Leader {
				newLeader = n
			}
		}
		if newLeader != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NotNil(t, newLeader)

	// The deposed leader can no longer commit.
	err := oldLeader.raft.CommitServerAddress(target.id, target.addr, newAddr, time.Second).Error()
	require.Error(t, err)

	// The new leader has no prepared migration, so a direct commit fails and
	// the operation is recoverable by preparing again.
	targetNode := findNode(nodes, target.id)
	err = newLeader.raft.CommitServerAddress(targetNode.id, targetNode.addr, newAddr, time.Second).Error()
	require.ErrorIs(t, err, ErrAddressMigrationNotPrepared)

	require.NoError(t, newLeader.raft.PrepareServerAddress(targetNode.id, targetNode.addr, newAddr, time.Second).Error())
	require.NoError(t, newLeader.raft.CommitServerAddress(targetNode.id, targetNode.addr, newAddr, time.Second).Error())

	// Only the two live nodes are queried for the final configuration.
	live := nodes[:0]
	for _, n := range nodes {
		if n.id != oldLeader.id {
			live = append(live, n)
		}
	}
	waitForConfig(t, live, func(c Configuration) bool {
		for _, s := range c.Servers {
			if s.ID == target.id {
				return s.Address == newAddr
			}
		}
		return false
	})
}

func TestServerAddressMigration_ProbeIsReadOnly(t *testing.T) {
	nodes := makeMigrationCluster(t)
	leader := leaderNode(nodes)
	require.NotNil(t, leader)

	var target *migrationNode
	for _, n := range nodes {
		if n.id != leader.id {
			target = n
			break
		}
	}

	termBefore := target.raft.getCurrentTerm()
	stateBefore := target.raft.State()
	leaderAddrBefore, leaderIDBefore := target.raft.LeaderWithID()

	prober := WithServerProbe(leader.trans.base)
	req := &VerifyServerRequest{RPCHeader: leader.raft.getRPCHeader()}
	resp := &VerifyServerResponse{}
	require.NoError(t, prober.VerifyServer(target.id, target.addr, req, resp))

	// The responder reports its own identity and current term.
	require.Equal(t, target.id, ServerID(resp.ID))
	require.Equal(t, termBefore, resp.Term)

	// The probe must not mutate the responder: same term, state and leader.
	require.Equal(t, termBefore, target.raft.getCurrentTerm())
	require.Equal(t, stateBefore, target.raft.State())
	addrAfter, idAfter := target.raft.LeaderWithID()
	require.Equal(t, leaderAddrBefore, addrAfter)
	require.Equal(t, leaderIDBefore, idAfter)

	// No migration was prepared just by probing.
	_, exists := leader.raft.leaderState.migrations[target.id]
	require.False(t, exists)
}

func TestServerAddressMigration_DelayedOldConnectionResponse(t *testing.T) {
	nodes := makeMigrationCluster(t)
	leader := leaderNode(nodes)
	require.NotNil(t, leader)

	// Migrate a non-leader follower so its old replication routine is the one
	// with the delayed response.
	var target *migrationNode
	for _, n := range nodes {
		if n.id != leader.id {
			target = n
			break
		}
	}

	newAddr, extraTrans := NewInmemTransport("")
	forwardTransport(t, target.trans, extraTrans)
	for _, n := range nodes {
		n.trans.base.Connect(newAddr, extraTrans)
	}

	require.NoError(t, leader.raft.PrepareServerAddress(target.id, target.addr, newAddr, time.Second).Error())

	// Arm the delay on the target's old endpoint and send a command that the
	// leader replicates to it over the old connection; that response is held.
	target.trans.armDelay()
	require.NoError(t, leader.raft.Apply([]byte("before-migrate"), time.Second).Error())

	// Give the leader time to dispatch the append over the old connection.
	time.Sleep(100 * time.Millisecond)

	// Commit while the old AppendEntries response is outstanding. The commit
	// proceeds using the other two voters (majority).
	commitFuture := leader.raft.CommitServerAddress(target.id, target.addr, newAddr, time.Second)
	require.NoError(t, commitFuture.Error())

	// Release the stale response after the new address is official. It must not
	// be counted as an additional vote nor pollute replication progress.
	time.Sleep(50 * time.Millisecond)
	target.trans.releaseDelay()

	// Exactly one member entry for the id at the new address.
	waitForConfig(t, nodes, func(c Configuration) bool {
		count := 0
		var addr ServerAddress
		for _, s := range c.Servers {
			if s.ID == target.id {
				count++
				addr = s.Address
			}
		}
		return count == 1 && addr == newAddr && len(c.Servers) == 3
	})

	// The leader's commitment must have exactly one match slot per ServerID
	// (the delayed response never created a second voter).
	require.Eventually(t, func() bool {
		leader.raft.leaderState.commitment.Lock()
		defer leader.raft.leaderState.commitment.Unlock()
		return len(leader.raft.leaderState.commitment.matchIndexes) == 3
	}, 3*time.Second, 10*time.Millisecond)

	// The single replication routine for the target must now target the new
	// address; no extra routine was left behind for the old one.
	repl, ok := leader.raft.leaderState.replState[target.id]
	require.True(t, ok)
	require.Equal(t, newAddr, repl.peer.Address)
	require.False(t, repl.detached.Load())
	require.False(t, repl.probe)

	// A fresh write replicates through the new endpoint and reaches the target,
	// proving new-address replication progress is independent of the late ack.
	require.NoError(t, leader.raft.Apply([]byte("after-migrate"), time.Second).Error())
	require.Eventually(t, func() bool {
		logs := target.fsm.Logs()
		return len(logs) > 0 && string(logs[len(logs)-1]) == "after-migrate"
	}, 3*time.Second, 10*time.Millisecond)
}
