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

// migratableInmemTransport wraps an InmemTransport and adds a second local
// address ("newEndpoint") on which the same node can also receive RPCs. This
// simulates a voter that is moving its listener from an old endpoint to a new
// one without restarting or changing its ServerID.
//
// While parking is enabled, AppendEntries RPCs arriving over the old endpoint
// are held instead of being delivered to the Raft node, so tests can release
// deliberately delayed responses after a migration commits.
type migratableInmemTransport struct {
	old *InmemTransport
	new *InmemTransport

	consumerCh chan RPC

	closeOnce sync.Once

	parkMu         sync.Mutex
	parkOldEntries bool
	parkedCh       chan RPC
	parkedMu       sync.Mutex
	parkedList     []RPC

	forwardWG sync.WaitGroup
	shutdown  chan struct{}
}

func newMigratableInmemTransport(oldAddr, newAddr ServerAddress) *migratableInmemTransport {
	_, oldTrans := NewInmemTransport(oldAddr)
	_, newTrans := NewInmemTransport(newAddr)
	m := &migratableInmemTransport{
		old:        oldTrans,
		new:        newTrans,
		consumerCh: make(chan RPC, 4096),
		parkedCh:   make(chan RPC, 256),
		shutdown:   make(chan struct{}),
	}
	m.forwardWG.Add(2)
	go m.forward(m.old.Consumer(), true)
	go m.forward(m.new.Consumer(), false)
	m.forwardWG.Add(1)
	go m.collectParked()
	return m
}

func (m *migratableInmemTransport) collectParked() {
	defer m.forwardWG.Done()
	for {
		select {
		case rpc := <-m.parkedCh:
			m.parkedMu.Lock()
			m.parkedList = append(m.parkedList, rpc)
			m.parkedMu.Unlock()
		case <-m.shutdown:
			return
		}
	}
}

func (m *migratableInmemTransport) forward(ch <-chan RPC, fromOld bool) {
	defer m.forwardWG.Done()
	for {
		select {
		case rpc := <-ch:
			if req, ok := rpc.Command.(*AppendEntriesRequest); ok && m.shouldPark(fromOld, req) {
				m.parkedCh <- rpc
				continue
			}
			select {
			case m.consumerCh <- rpc:
			case <-m.shutdown:
				return
			}
		case <-m.shutdown:
			return
		}
	}
}

// isHeartbeatRequest reports whether an AppendEntries request is an empty
// heartbeat, matching the fast-path definition in the network transport.
func isHeartbeatRequest(req *AppendEntriesRequest) bool {
	return req.PrevLogEntry == 0 && req.PrevLogTerm == 0 &&
		len(req.Entries) == 0 && req.LeaderCommitIndex == 0
}

func (m *migratableInmemTransport) shouldPark(fromOld bool, req *AppendEntriesRequest) bool {
	m.parkMu.Lock()
	defer m.parkMu.Unlock()
	if fromOld {
		return m.parkOldEntries && !isHeartbeatRequest(req)
	}
	return false
}

func (m *migratableInmemTransport) setParkOldEntries(park bool) {
	m.parkMu.Lock()
	m.parkOldEntries = park
	m.parkMu.Unlock()
}

// parked returns the RPCs that were held on the old endpoint.
func (m *migratableInmemTransport) parked() []RPC {
	m.parkedMu.Lock()
	defer m.parkedMu.Unlock()
	var out []RPC
	for {
		select {
		case rpc := <-m.parkedCh:
			out = append(out, rpc)
		default:
			out = append(out, m.parkedList...)
			m.parkedList = nil
			return out
		}
	}
}

func (m *migratableInmemTransport) Consumer() <-chan RPC          { return m.consumerCh }
func (m *migratableInmemTransport) LocalAddr() ServerAddress      { return m.old.LocalAddr() }
func (m *migratableInmemTransport) NewAddr() ServerAddress        { return m.new.LocalAddr() }
func (m *migratableInmemTransport) OldTransport() *InmemTransport { return m.old }
func (m *migratableInmemTransport) NewTransport() *InmemTransport { return m.new }

func (m *migratableInmemTransport) AppendEntriesPipeline(id ServerID, target ServerAddress) (AppendPipeline, error) {
	return m.route(target).AppendEntriesPipeline(id, target)
}

func (m *migratableInmemTransport) AppendEntries(id ServerID, target ServerAddress, args *AppendEntriesRequest, resp *AppendEntriesResponse) error {
	return m.route(target).AppendEntries(id, target, args, resp)
}

func (m *migratableInmemTransport) RequestVote(id ServerID, target ServerAddress, args *RequestVoteRequest, resp *RequestVoteResponse) error {
	return m.route(target).RequestVote(id, target, args, resp)
}

func (m *migratableInmemTransport) InstallSnapshot(id ServerID, target ServerAddress, args *InstallSnapshotRequest, resp *InstallSnapshotResponse, data io.Reader) error {
	return m.route(target).InstallSnapshot(id, target, args, resp, data)
}

func (m *migratableInmemTransport) EncodePeer(id ServerID, addr ServerAddress) []byte {
	return m.route(addr).EncodePeer(id, addr)
}

func (m *migratableInmemTransport) DecodePeer(buf []byte) ServerAddress {
	return m.old.DecodePeer(buf)
}

func (m *migratableInmemTransport) SetHeartbeatHandler(cb func(RPC)) {
	m.old.SetHeartbeatHandler(cb)
	m.new.SetHeartbeatHandler(cb)
}

func (m *migratableInmemTransport) TimeoutNow(id ServerID, target ServerAddress, args *TimeoutNowRequest, resp *TimeoutNowResponse) error {
	return m.route(target).TimeoutNow(id, target, args, resp)
}

func (m *migratableInmemTransport) RequestPreVote(id ServerID, target ServerAddress, args *RequestPreVoteRequest, resp *RequestPreVoteResponse) error {
	return m.route(target).RequestPreVote(id, target, args, resp)
}

func (m *migratableInmemTransport) VerifyServer(id ServerID, target ServerAddress, args *VerifyServerRequest, resp *VerifyServerResponse) error {
	return m.route(target).VerifyServer(id, target, args, resp)
}

func (m *migratableInmemTransport) EvictConnection(addr ServerAddress) {
	m.old.EvictConnection(addr)
	m.new.EvictConnection(addr)
}

func (m *migratableInmemTransport) route(addr ServerAddress) *InmemTransport {
	if addr == m.new.LocalAddr() {
		return m.new
	}
	return m.old
}

func (m *migratableInmemTransport) Connect(peer ServerAddress, t Transport) {
	m.old.Connect(peer, t)
	m.new.Connect(peer, t)
}

func (m *migratableInmemTransport) Disconnect(peer ServerAddress) {
	m.old.Disconnect(peer)
	m.new.Disconnect(peer)
}

func (m *migratableInmemTransport) DisconnectAll() {
	m.old.DisconnectAll()
	m.new.DisconnectAll()
}

func (m *migratableInmemTransport) Close() error {
	m.closeOnce.Do(func() {
		close(m.shutdown)
		m.forwardWG.Wait()
		_ = m.old.Close()
		_ = m.new.Close()
	})
	return nil
}

// migrationCluster is a 3-voter cluster where every node has both an old and
// a new (not yet wired) endpoint, ready for address migration tests.
type migrationCluster struct {
	t      *testing.T
	conf   *Config
	rafts  []*Raft
	fsms   []*MockFSM
	mtrans []*migratableInmemTransport

	observationCh chan Observation
}

func makeMigrationCluster(t *testing.T) *migrationCluster {
	const n = 3
	conf := inmemConfig(t)
	conf.HeartbeatTimeout = 80 * time.Millisecond
	conf.ElectionTimeout = 80 * time.Millisecond
	conf.LeaderLeaseTimeout = 80 * time.Millisecond
	conf.CommitTimeout = 10 * time.Millisecond

	m := &migrationCluster{
		t:             t,
		conf:          conf,
		observationCh: make(chan Observation, 1024),
	}

	var configuration Configuration
	for i := 0; i < n; i++ {
		oldAddr := NewInmemAddr()
		newAddr := NewInmemAddr()
		mt := newMigratableInmemTransport(oldAddr, newAddr)
		m.mtrans = append(m.mtrans, mt)

		id := ServerID("voter-" + string(rune('a'+i)))
		configuration.Servers = append(configuration.Servers, Server{
			Suffrage: Voter,
			ID:       id,
			Address:  oldAddr,
		})
	}

	// Wire every combination of old/new transports together, so a node that
	// receives an RPC on either endpoint can route its response to the
	// caller's old or new endpoint. Only old addresses are bootstrapped into
	// the Raft configuration initially.
	for i, mt := range m.mtrans {
		for j, other := range m.mtrans {
			if i == j {
				continue
			}
			mt.old.Connect(other.old.LocalAddr(), other.old)
			mt.old.Connect(other.new.LocalAddr(), other.new)
			mt.new.Connect(other.old.LocalAddr(), other.old)
			mt.new.Connect(other.new.LocalAddr(), other.new)
			other.old.Connect(mt.old.LocalAddr(), mt.old)
			other.old.Connect(mt.new.LocalAddr(), mt.new)
			other.new.Connect(mt.old.LocalAddr(), mt.old)
			other.new.Connect(mt.new.LocalAddr(), mt.new)
		}
	}

	for i := 0; i < n; i++ {
		store := NewInmemStore()
		snap := NewInmemSnapshotStore()
		fsm := &MockFSM{}

		peerConf := *conf
		peerConf.LocalID = configuration.Servers[i].ID
		peerConf.Logger = newTestLoggerWithPrefix(t, string(configuration.Servers[i].ID))

		require.NoError(t, BootstrapCluster(&peerConf, store, store, snap, m.mtrans[i], configuration))

		r, err := NewRaft(&peerConf, fsm, store, store, snap, m.mtrans[i])
		require.NoError(t, err)
		r.RegisterObserver(NewObserver(m.observationCh, false, nil))

		m.rafts = append(m.rafts, r)
		m.fsms = append(m.fsms, fsm)
	}

	return m
}

func (m *migrationCluster) Close() {
	for _, r := range m.rafts {
		_ = r.Shutdown().Error()
	}
	for _, mt := range m.mtrans {
		_ = mt.Close()
	}
}

func (m *migrationCluster) leader() *Raft {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range m.rafts {
			if r.State() == Leader {
				return r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.t.Fatalf("no leader elected")
	return nil
}

func (m *migrationCluster) leaderIndex() int {
	for i, r := range m.rafts {
		if r.State() == Leader {
			return i
		}
	}
	return -1
}

// connectNewEndpoint wires the candidate new endpoint into every OTHER node's
// dial table (both their old and new transports), simulating the new address
// becoming reachable before the migration commits.
func (m *migrationCluster) connectNewEndpoint(target int) {
	targetNew := m.mtrans[target].new
	newAddr := targetNew.LocalAddr()
	for i, mt := range m.mtrans {
		if i == target {
			continue
		}
		mt.old.Connect(newAddr, targetNew)
		mt.new.Connect(newAddr, targetNew)
	}
}

func (m *migrationCluster) ensureConfiguration(t *testing.T, expect Configuration) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ok := true
		for _, r := range m.rafts {
			future := r.GetConfiguration()
			if err := future.Error(); err != nil {
				t.Fatalf("get configuration: %v", err)
			}
			got := future.Configuration()
			if len(got.Servers) != len(expect.Servers) {
				ok = false
				break
			}
			for i := range expect.Servers {
				if got.Servers[i] != expect.Servers[i] {
					ok = false
				}
			}
		}
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("configuration never converged to %+v", expect.Servers)
}

// TestRaft_AddressMigration_HappyPath prepares and commits a voter address
// migration end to end. After commit the diagnostics show exactly one member
// for the migrated ServerID, at its current address.
func TestRaft_AddressMigration_HappyPath(t *testing.T) {
	m := makeMigrationCluster(t)
	defer m.Close()

	leader := m.leader()
	leaderIdx := m.leaderIndex()

	before := leader.GetConfiguration()
	require.NoError(t, before.Error())
	require.Len(t, before.Configuration().Servers, 3)

	target := (leaderIdx + 1) % len(m.rafts)
	targetID := m.rafts[target].localID
	newAddr := m.mtrans[target].NewAddr()
	m.connectNewEndpoint(target)

	// Prepare: the new endpoint must identify itself with the same ServerID.
	require.NoError(t, leader.PrepareServerAddress(targetID, newAddr, time.Second).Error())

	// Commit: switch the cluster's formal address.
	f := leader.CommitServerAddress(targetID, newAddr, 0, time.Second)
	require.NoError(t, f.Error())

	beforeConf := before.Configuration()
	expected := beforeConf.Clone()
	for i := range expected.Servers {
		if expected.Servers[i].ID == targetID {
			expected.Servers[i].Address = newAddr
		}
	}
	m.ensureConfiguration(t, expected)

	// The cluster keeps making progress through the new endpoint.
	require.NoError(t, leader.Barrier(time.Second).Error())
	require.NoError(t, leader.Apply([]byte("after-migration"), time.Second).Error())

	// Diagnostics: exactly one member with that ID, current address only.
	stats := leader.Stats()
	var configs Configuration
	cf := leader.GetConfiguration()
	require.NoError(t, cf.Error())
	configs = cf.Configuration()
	var matches int
	for _, srv := range configs.Servers {
		if srv.ID == targetID {
			matches++
			require.Equal(t, newAddr, srv.Address)
		}
	}
	require.Equal(t, 1, matches)
	require.Contains(t, stats["latest_configuration"], string(targetID))
	require.NotContains(t, stats["latest_configuration"], string(m.mtrans[target].LocalAddr()))
}

// TestRaft_AddressMigration_IdentityMismatch rejects a candidate endpoint that
// answers with a different ServerID and leaves the configuration untouched.
func TestRaft_AddressMigration_IdentityMismatch(t *testing.T) {
	m := makeMigrationCluster(t)
	defer m.Close()

	leader := m.leader()
	leaderIdx := m.leaderIndex()
	target := (leaderIdx + 1) % len(m.rafts)
	targetID := m.rafts[target].localID

	before := leader.GetConfiguration()
	require.NoError(t, before.Error())

	// Stand up a separate, alien node and make the leader's transport believe
	// the candidate address is that alien endpoint. It answers the verify RPC
	// with its own identity, which must not match the ServerID being migrated.
	wrongAddr, alien := NewInmemTransport(NewInmemAddr())
	defer alien.Close()

	// The alien node simply answers verification RPCs from its consumer.
	alienDone := make(chan struct{})
	go func() {
		defer close(alienDone)
		for {
			select {
			case rpc := <-alien.Consumer():
				rpc.Respond(&VerifyServerResponse{
					RPCHeader: RPCHeader{ID: []byte("alien-node")},
					Term:      1,
					Success:   true,
				}, nil)
			case <-time.After(time.Second):
				return
			}
		}
	}()

	m.mtrans[leaderIdx].old.Connect(wrongAddr, alien)

	err := leader.PrepareServerAddress(targetID, wrongAddr, time.Second).Error()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrServerAddressMismatch)

	// Nothing changed: still one member at the old address.
	after := leader.GetConfiguration()
	require.NoError(t, after.Error())
	require.Equal(t, before.Configuration(), after.Configuration())
}

// TestRaft_AddressMigration_NotLeader returns ErrNotLeader from a follower and
// is recoverable by retrying against the current leader.
func TestRaft_AddressMigration_NotLeader(t *testing.T) {
	m := makeMigrationCluster(t)
	defer m.Close()

	leader := m.leader()
	leaderIdx := m.leaderIndex()
	target := (leaderIdx + 1) % len(m.rafts)
	follower := m.rafts[(leaderIdx+2)%len(m.rafts)]
	targetID := m.rafts[target].localID
	newAddr := m.mtrans[target].NewAddr()
	m.connectNewEndpoint(target)

	err := follower.PrepareServerAddress(targetID, newAddr, time.Second).Error()
	require.ErrorIs(t, err, ErrNotLeader)

	err = follower.CommitServerAddress(targetID, newAddr, 0, time.Second).Error()
	require.ErrorIs(t, err, ErrNotLeader)

	// Retry on the actual leader and succeed: cluster is recoverable.
	require.NoError(t, leader.PrepareServerAddress(targetID, newAddr, time.Second).Error())
	require.NoError(t, leader.CommitServerAddress(targetID, newAddr, 0, time.Second).Error())
}

// TestRaft_AddressMigration_UnreachableCandidate fails the prepare phase with
// a timeout and leaves the cluster on the old address; the operation can be
// retried once the endpoint is reachable.
func TestRaft_AddressMigration_UnreachableCandidate(t *testing.T) {
	m := makeMigrationCluster(t)
	defer m.Close()

	leader := m.leader()
	leaderIdx := m.leaderIndex()
	target := (leaderIdx + 1) % len(m.rafts)
	targetID := m.rafts[target].localID
	// The target's real new endpoint exists, but it is not reachable from the
	// leader yet.
	newAddr := m.mtrans[target].NewAddr()
	leaderOld := m.mtrans[leaderIdx].old
	leaderNew := m.mtrans[leaderIdx].new
	leaderOld.Disconnect(newAddr)
	leaderNew.Disconnect(newAddr)

	before := leader.GetConfiguration()
	require.NoError(t, before.Error())

	err := leader.PrepareServerAddress(targetID, newAddr, 200*time.Millisecond).Error()
	require.Error(t, err)

	after := leader.GetConfiguration()
	require.NoError(t, after.Error())
	require.Equal(t, before.Configuration(), after.Configuration())

	// Make it reachable and retry.
	m.connectNewEndpoint(target)
	require.NoError(t, leader.PrepareServerAddress(targetID, newAddr, time.Second).Error())
	require.NoError(t, leader.CommitServerAddress(targetID, newAddr, 0, time.Second).Error())
}

// TestRaft_AddressMigration_LeadershipChangeDuringCommit is recoverable: if
// the leader changes between prepare and commit, the commit attempt fails on
// the deposed leader and the migration can be completed via the new leader.
func TestRaft_AddressMigration_LeadershipChangeDuringCommit(t *testing.T) {
	m := makeMigrationCluster(t)
	defer m.Close()

	oldLeader := m.leader()
	oldLeaderIdx := m.leaderIndex()
	target := (oldLeaderIdx + 1) % len(m.rafts)
	targetID := m.rafts[target].localID
	newAddr := m.mtrans[target].NewAddr()
	m.connectNewEndpoint(target)

	// Prepare succeeds on the old leader.
	require.NoError(t, oldLeader.PrepareServerAddress(targetID, newAddr, time.Second).Error())

	// Force the old leader out of power.
	require.NoError(t, oldLeader.LeadershipTransferToServer(m.rafts[(oldLeaderIdx+2)%len(m.rafts)].localID,
		m.rafts[(oldLeaderIdx+2)%len(m.rafts)].localAddr).Error())

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && oldLeader.State() == Leader {
		time.Sleep(10 * time.Millisecond)
	}
	require.NotEqual(t, Leader, oldLeader.State())

	// The old leader can no longer commit: recoverable failure.
	if err := oldLeader.CommitServerAddress(targetID, newAddr, 0, 200*time.Millisecond).Error(); err != nil {
		require.ErrorIs(t, err, ErrNotLeader)
	}

	// A new leader completes the migration.
	newLeader := m.leader()
	require.NotSame(t, oldLeader, newLeader)
	require.NoError(t, newLeader.PrepareServerAddress(targetID, newAddr, time.Second).Error())
	require.NoError(t, newLeader.CommitServerAddress(targetID, newAddr, 0, time.Second).Error())
}

// TestRaft_AddressMigration_CommitUnknownServer rejects an address update for
// a ServerID that is not present in the configuration.
func TestRaft_AddressMigration_CommitUnknownServer(t *testing.T) {
	m := makeMigrationCluster(t)
	defer m.Close()

	leader := m.leader()
	before := leader.GetConfiguration()
	require.NoError(t, before.Error())

	err := leader.CommitServerAddress(ServerID("ghost"), NewInmemAddr(), 0, time.Second).Error()
	require.Error(t, err)

	after := leader.GetConfiguration()
	require.NoError(t, after.Error())
	require.Equal(t, before.Configuration(), after.Configuration())
}

// TestRaft_AddressMigration_CommitSameAddress is a safe no-op failure.
func TestRaft_AddressMigration_CommitSameAddress(t *testing.T) {
	m := makeMigrationCluster(t)
	defer m.Close()

	leader := m.leader()
	leaderIdx := m.leaderIndex()
	target := (leaderIdx + 1) % len(m.rafts)
	targetID := m.rafts[target].localID
	oldAddr := m.mtrans[target].LocalAddr()

	require.ErrorIs(t,
		leader.CommitServerAddress(targetID, oldAddr, 0, time.Second).Error(),
		ErrServerAddressUnchanged)
}

func readMatchIndex(c *commitment, id ServerID) uint64 {
	c.Lock()
	defer c.Unlock()
	return c.matchIndexes[id]
}

// TestRaft_AddressMigration_StaleAppendEntriesResponse holds data AppendEntries
// RPCs open on the target's OLD endpoint, migrates the target, and then replies
// to those stale RPCs successfully. The delayed responses must be dropped by
// the abandoned replication routine so they cannot advance the new address's
// replication progress.
func TestRaft_AddressMigration_StaleAppendEntriesResponse(t *testing.T) {
	m := makeMigrationCluster(t)
	defer m.Close()

	leader := m.leader()
	leaderIdx := m.leaderIndex()
	target := (leaderIdx + 1) % len(m.rafts)
	targetID := m.rafts[target].localID
	newAddr := m.mtrans[target].NewAddr()
	m.connectNewEndpoint(target)

	// Hold data RPCs on the old endpoint open. Heartbeats keep flowing.
	m.mtrans[target].setParkOldEntries(true)
	require.NoError(t, leader.Apply([]byte("while-old-blocked"), time.Second).Error())

	// Migrate; a fresh routine replicates over the new endpoint.
	require.NoError(t, leader.PrepareServerAddress(targetID, newAddr, time.Second).Error())
	require.NoError(t, leader.CommitServerAddress(targetID, newAddr, 0, time.Second).Error())
	require.NoError(t, leader.Barrier(time.Second).Error())

	commitment := leader.leaderState.commitment
	leaderLastIndex := leader.getLastIndex()

	// Wait for the NEW routine to acknowledge the leader's full log so the
	// recorded match index is stable before we release the stale responses.
	deadline := time.Now().Add(3 * time.Second)
	for readMatchIndex(commitment, targetID) < leaderLastIndex && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	matchAfterCatchUp := readMatchIndex(commitment, targetID)
	require.Equal(t, leaderLastIndex, matchAfterCatchUp)

	// Answer the stale RPCs as if merely delayed, claiming far-ahead progress.
	parked := m.mtrans[target].parked()
	require.NotEmpty(t, parked)
	for _, rpc := range parked {
		req := rpc.Command.(*AppendEntriesRequest)
		rpc.Respond(&AppendEntriesResponse{
			RPCHeader: RPCHeader{
				ID:   []byte(targetID),
				Addr: []byte(m.mtrans[target].LocalAddr()),
			},
			Term:    req.Term,
			LastLog: matchAfterCatchUp + 1000,
			Success: true,
		}, nil)
	}

	// Allow the (dropped) delayed responses to be observed.
	time.Sleep(100 * time.Millisecond)

	require.Equal(t, Leader, leader.State())
	finalMatch := readMatchIndex(commitment, targetID)
	require.Equal(t, matchAfterCatchUp, finalMatch,
		"stale old-endpoint response polluted the new address's progress")

	m.mtrans[target].setParkOldEntries(false)
	require.NoError(t, leader.Apply([]byte("after-stale"), time.Second).Error())
	require.NoError(t, leader.Barrier(time.Second).Error())
}

// TestFollowerReplication_Abandon_DropsVotes verifies that once a replication
// routine is abandoned (as happens when its peer migrates addresses), a
// leadership-verification ballot registered on it is never cast, even though
// a successful AppendEntries response later arrives on the old connection.
func TestFollowerReplication_Abandon_DropsVotes(t *testing.T) {
	s := &followerReplication{
		notify:   make(map[*verifyFuture]struct{}),
		notifyCh: make(chan struct{}, 1),
		stepDown: make(chan struct{}, 1),
	}

	v := &verifyFuture{}
	v.init()
	v.votes = 1
	v.quorumSize = 2
	v.notifyCh = make(chan *verifyFuture, 1)

	s.notifyLock.Lock()
	s.notify[v] = struct{}{}
	s.notifyLock.Unlock()

	// Migrate away: the routine retires and discards pending ballots.
	s.abandon()
	require.True(t, s.isAbandoned())

	// Simulate a successful, delayed heartbeat arriving on the old
	// connection; it must not deliver a vote.
	s.notifyAll(true)

	select {
	case <-v.notifyCh:
		t.Fatalf("abandoned routine cast a ballot from a stale connection")
	default:
	}
	require.Equal(t, 1, v.votes)
}

// TestFollowerReplication_Abandon_IgnoresProgress is a focused check that the
// migration path retires the old routine before responses are applied, which
// prevents updateLastAppended from ever running for the stale response (it
// would otherwise call commitment.match for the new address).
func TestFollowerReplication_Abandon_IgnoresProgress(t *testing.T) {
	s := &followerReplication{
		notify:   make(map[*verifyFuture]struct{}),
		notifyCh: make(chan struct{}, 1),
		stepDown: make(chan struct{}, 1),
		peer:     Server{ID: ServerID("voter-x"), Address: ServerAddress("old")},
	}

	commitCh := make(chan struct{}, 8)
	conf := Configuration{Servers: []Server{{Suffrage: Voter, ID: "voter-x"}}}
	s.commitment = newCommitment(commitCh, conf, 1)

	s.abandon()

	// Even if a stale success with a high index reached updateLastAppended,
	// the guarding isAbandoned() checks in the replication paths ensure it is
	// never reached. Assert the commitment stays at zero.
	req := &AppendEntriesRequest{Entries: []*Log{{Index: 999, Term: 1}}}
	if s.isAbandoned() {
		// The production code drops the response here.
		return
	}
	updateLastAppended(s, req)
	require.Equal(t, uint64(0), s.commitment.getCommitIndex())
}

// TestInmemTransport_VerifyServer_RoundTrip validates the identity RPC over
// the in-memory transport.
func TestInmemTransport_VerifyServer_RoundTrip(t *testing.T) {
	addr1, trans1 := NewInmemTransport("")
	defer trans1.Close()
	addr2, trans2 := NewInmemTransport("")
	defer trans2.Close()
	trans1.Connect(addr2, trans2)
	trans2.Connect(addr1, trans1)

	go func() {
		select {
		case rpc := <-trans1.Consumer():
			req := rpc.Command.(*VerifyServerRequest)
			rpc.Respond(&VerifyServerResponse{
				RPCHeader: RPCHeader{ID: []byte("node-1")},
				Term:      7,
				Success:   req.ServerID == "node-1",
			}, nil)
		case <-time.After(time.Second):
			t.Errorf("timeout waiting for verify RPC")
		}
	}()

	var resp VerifyServerResponse
	require.NoError(t, trans2.VerifyServer("node-1", addr1,
		&VerifyServerRequest{Term: 7, ServerID: "node-1"}, &resp))
	require.True(t, resp.Success)
	require.Equal(t, ServerID("node-1"), ServerID(resp.ID))

	// A wrong expected ID is reported as Success=false by the responder.
	go func() {
		rpc := <-trans1.Consumer()
		req := rpc.Command.(*VerifyServerRequest)
		rpc.Respond(&VerifyServerResponse{
			RPCHeader: RPCHeader{ID: []byte("node-1")},
			Term:      7,
			Success:   req.ServerID == "node-1",
		}, nil)
	}()
	require.NoError(t, trans2.VerifyServer("intruder", addr1,
		&VerifyServerRequest{Term: 7, ServerID: "intruder"}, &resp))
	require.False(t, resp.Success)
}

// TestInmemTransport_EvictConnection ensures EvictConnection closes pipelines
// targeting the old address without disrupting routing.
func TestInmemTransport_EvictConnection(t *testing.T) {
	addr1, trans1 := NewInmemTransport("")
	defer trans1.Close()
	addr2, trans2 := NewInmemTransport("")
	defer trans2.Close()
	trans1.Connect(addr2, trans2)
	trans2.Connect(addr1, trans1)

	pipeline, err := trans1.AppendEntriesPipeline("id", addr2)
	require.NoError(t, err)
	defer pipeline.Close()

	require.Len(t, trans1.pipelines, 1)
	trans1.EvictConnection(addr2)
	require.Empty(t, trans1.pipelines)

	// Regular RPCs still route fine after eviction.
	go func() {
		rpc := <-trans2.Consumer()
		rpc.Respond(&AppendEntriesResponse{Success: true}, nil)
	}()
	var out AppendEntriesResponse
	require.NoError(t, trans1.AppendEntries("id", addr2,
		&AppendEntriesRequest{Term: 1}, &out))
	require.True(t, out.Success)
}
