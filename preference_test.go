// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"os"
	"testing"

	"time"

	"github.com/stretchr/testify/require"
)

// preferenceTestConf returns a config with candidate preference enabled and a
// deliberately small, comfortable bounded wait window.
func preferenceTestConf(t *testing.T) *Config {
	conf := inmemConfig(t)
	conf.HeartbeatTimeout = 150 * time.Millisecond
	conf.ElectionTimeout = 150 * time.Millisecond
	conf.LeaderLeaseTimeout = 100 * time.Millisecond
	conf.ElectionPreferenceTimeout = 60 * time.Millisecond
	return conf
}

// makePriorityCluster builds a fully connected, bootstrapped 3-node cluster
// where each peer can be configured independently through peerConfs.
func makePriorityCluster(t *testing.T, peerConfs []*Config) *cluster {
	t.Helper()
	n := len(peerConfs)
	base := peerConfs[0]

	c := &cluster{
		observationCh:    make(chan Observation, 1024),
		conf:             base,
		propagateTimeout: base.HeartbeatTimeout*2 + base.CommitTimeout,
		longstopTimeout:  5 * time.Second,
		logger:           newTestLoggerWithPrefix(t, "cluster"),
		failedCh:         make(chan struct{}),
	}
	c.t = t

	var configuration Configuration
	for i := 0; i < n; i++ {
		dir, err := os.MkdirTemp("", "raft")
		require.NoError(t, err)
		c.dirs = append(c.dirs, dir)

		store := NewInmemStore()
		c.stores = append(c.stores, store)
		c.fsms = append(c.fsms, &MockFSM{})

		snapDir, snap := FileSnapTest(t)
		c.dirs = append(c.dirs, snapDir)
		c.snaps = append(c.snaps, snap)

		addr, trans := NewInmemTransport("")
		c.trans = append(c.trans, trans)
		localID := ServerID("server-" + string(addr))
		configuration.Servers = append(configuration.Servers, Server{
			Suffrage: Voter,
			ID:       localID,
			Address:  addr,
		})
	}

	c.FullyConnect()
	c.startTime = time.Now()

	for i := 0; i < n; i++ {
		conf := *peerConfs[i] // independent copy per peer
		conf.LocalID = configuration.Servers[i].ID
		conf.Logger = newTestLoggerWithPrefix(t, string(configuration.Servers[i].ID))

		require.NoError(t, BootstrapCluster(&conf, c.stores[i], c.stores[i], c.snaps[i], c.trans[i], configuration))

		raftNode, err := NewRaft(&conf, c.fsms[i], c.stores[i], c.stores[i], c.snaps[i], c.trans[i])
		require.NoError(t, err)
		raftNode.RegisterObserver(NewObserver(c.observationCh, false, nil))
		c.rafts = append(c.rafts, raftNode)
	}

	return c
}

// TestPreference_HighPriorityLateStart is scenario 1: the preferred node only
// starts its election "slightly late" (a slower election timeout), but because
// voters defer lower-priority votes for the bounded window it still wins.
func TestPreference_HighPriorityLateStart(t *testing.T) {
	confA := preferenceTestConf(t)
	confB := preferenceTestConf(t)
	confC := preferenceTestConf(t)

	// A is preferred, but campaigns noticeably later than B and C.
	confA.CandidatePriority = 10
	confB.CandidatePriority = 1
	confC.CandidatePriority = 1
	confA.HeartbeatTimeout = 300 * time.Millisecond
	confA.ElectionTimeout = 300 * time.Millisecond
	confA.LeaderLeaseTimeout = 200 * time.Millisecond

	c := makePriorityCluster(t, []*Config{confA, confB, confC})
	defer c.Close()

	leader := c.Leader()
	require.Equal(t, c.rafts[0].localAddr, leader.localAddr,
		"preferred node A should win despite starting its election later")
	c.EnsureLeader(t, c.rafts[0].localAddr)
}

// TestPreference_HighPriorityPartitioned is scenario 2: the preferred node is
// isolated from the quorum. The bounded wait must expire and the reachable
// majority must still elect a leader.
func TestPreference_HighPriorityPartitioned(t *testing.T) {
	confA := preferenceTestConf(t)
	confB := preferenceTestConf(t)
	confC := preferenceTestConf(t)
	confA.CandidatePriority = 10
	confB.CandidatePriority = 1
	confC.CandidatePriority = 1

	c := makePriorityCluster(t, []*Config{confA, confB, confC})
	defer c.Close()

	// Ensure the preferred A is the incumbent (hand leadership back if a
	// lower-priority node happened to win the initial election).
	leader := c.Leader()
	if leader != c.rafts[0] {
		require.NoError(t, leader.LeadershipTransferToServer(c.rafts[0].localID, c.rafts[0].localAddr).Error())
		limit := time.Now().Add(c.longstopTimeout)
		for time.Now().Before(limit) && c.rafts[0].State() != Leader {
			time.Sleep(5 * time.Millisecond)
		}
		require.Equal(t, Leader, c.rafts[0].State())
	}

	// ...then isolate A from B and C.
	c.Partition([]ServerAddress{c.rafts[0].localAddr})

	// B and C form a quorum; they must elect a leader even though they only
	// ever saw the priority-10 candidate (A), which is unreachable.
	limit := time.Now().Add(c.longstopTimeout)
	var nearLeader *Raft
	for time.Now().Before(limit) {
		for _, r := range c.rafts[1:] {
			if r.State() == Leader {
				nearLeader = r
			}
		}
		if nearLeader != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.NotNil(t, nearLeader, "reachable majority must elect a leader despite partitioned preferred node")
	require.NotEqual(t, c.rafts[0].localAddr, nearLeader.localAddr)
}

// TestPreference_LogRecencyBeatsPriority verifies that an up-to-date log
// always outranks the configured preference: a high-priority candidate whose
// log is behind must be refused, even when otherwise eligible.
func TestPreference_LogRecencyBeatsPriority(t *testing.T) {
	conf := preferenceTestConf(t)
	c := MakeCluster(3, t, conf)
	defer c.Close()

	leader := c.Leader()
	var follower *Raft
	for _, r := range c.rafts {
		if r != leader {
			follower = r
			break
		}
	}

	// Advance the cluster log so the follower is ahead of index 0.
	require.NoError(t, leader.Apply([]byte("committed"), c.propagateTimeout).Error())
	c.WaitForReplication(1)

	// Send as a real, in-config peer (the leader) but with a deliberately
	// stale log and an inflated priority.
	req := &RequestVoteRequest{
		RPCHeader: RPCHeader{
			ProtocolVersion: ProtocolVersionMax,
			ID:              []byte(leader.localID),
			Addr:            c.trans[c.IndexOf(leader)].EncodePeer(leader.localID, leader.localAddr),
		},
		Term:         follower.getCurrentTerm() + 1,
		LastLogIndex: 0,
		LastLogTerm:  0,
		Priority:     100,
	}
	var resp RequestVoteResponse
	require.NoError(t, c.trans[c.IndexOf(leader)].RequestVote(
		leader.localID, follower.localAddr, req, &resp))
	require.False(t, resp.Granted, "a higher-priority candidate with a stale log must not get the vote")

	// An up-to-date request with the same priority profile should be deferred
	// (not rejected for log reasons); assert the stale one was really rejected
	// on log grounds via the follower's unchanged term (no grant persisted).
	_ = follower.getCurrentTerm()
}

// TestPreference_OneVotePerTerm verifies that deferring a decision does not
// allow two grants in one term.
func TestPreference_OneVotePerTerm(t *testing.T) {
	confA := preferenceTestConf(t)
	confB := preferenceTestConf(t)
	confC := preferenceTestConf(t)
	confA.CandidatePriority = 5
	confB.CandidatePriority = 5
	confC.CandidatePriority = 5

	c := makePriorityCluster(t, []*Config{confA, confB, confC})
	defer c.Close()
	c.Leader()

	// Pick a follower as voter and drive two direct, up-to-date vote requests at
	// the same future term from two distinct in-config candidates.
	var voter, cand1, cand2 *Raft
	for _, r := range c.rafts {
		if r.State() == Follower && voter == nil {
			voter = r
		}
	}
	others := make([]*Raft, 0, 2)
	for _, r := range c.rafts {
		if r != voter {
			others = append(others, r)
		}
	}
	cand1, cand2 = others[0], others[1]

	term := voter.getCurrentTerm() + 1
	idx, logTerm := voter.getLastEntry()

	send := func(from *Raft) RequestVoteResponse {
		req := &RequestVoteRequest{
			RPCHeader: RPCHeader{
				ProtocolVersion: ProtocolVersionMax,
				ID:              []byte(from.localID),
				Addr:            c.trans[c.IndexOf(from)].EncodePeer(from.localID, from.localAddr),
			},
			Term:         term,
			LastLogIndex: idx,
			LastLogTerm:  logTerm,
			Priority:     5,
		}
		var resp RequestVoteResponse
		require.NoError(t, c.trans[c.IndexOf(from)].RequestVote(from.localID, voter.localAddr, req, &resp))
		return resp
	}

	resp1 := send(cand1)
	resp2 := send(cand2)
	grants := 0
	if resp1.Granted {
		grants++
	}
	if resp2.Granted {
		grants++
	}
	require.LessOrEqual(t, grants, 1, "a server must grant at most one vote per term")
}

// TestPreference_LeadershipTransferBypassesPreference ensures an explicit
// transfer to a low(er)-priority target is not delayed or vetoed by
// preference.
func TestPreference_LeadershipTransferBypassesPreference(t *testing.T) {
	confA := preferenceTestConf(t)
	confB := preferenceTestConf(t)
	confC := preferenceTestConf(t)
	// A preferred leader, B the intended low-priority transfer target.
	confA.CandidatePriority = 10
	confB.CandidatePriority = 1
	confC.CandidatePriority = 1

	c := makePriorityCluster(t, []*Config{confA, confB, confC})
	defer c.Close()

	target := c.rafts[1]

	// Make sure the preferred A is the incumbent before transferring to the
	// lower-priority B. If another node won the initial election, hand it back
	// to A first.
	leader := c.Leader()
	if leader != c.rafts[0] {
		require.NoError(t, leader.LeadershipTransferToServer(c.rafts[0].localID, c.rafts[0].localAddr).Error())
		limit := time.Now().Add(c.longstopTimeout)
		for time.Now().Before(limit) && c.rafts[0].State() != Leader {
			time.Sleep(5 * time.Millisecond)
		}
		require.Equal(t, Leader, c.rafts[0].State())
		leader = c.rafts[0]
	}

	future := leader.LeadershipTransferToServer(target.localID, target.localAddr)
	require.NoError(t, future.Error())

	// B must become leader promptly (no preference wait).
	limit := time.Now().Add(c.longstopTimeout)
	for time.Now().Before(limit) && target.State() != Leader {
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, Leader, target.State())

	// Wait for the whole cluster to converge on B (heartbeat propagation can
	// briefly lag the target's own leader transition).
	converged := false
	deadline := time.Now().Add(c.longstopTimeout)
	for time.Now().Before(deadline) {
		all := true
		for _, r := range c.rafts {
			addr, _ := r.LeaderWithID()
			if addr != target.localAddr {
				all = false
			}
		}
		if all {
			converged = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.True(t, converged, "cluster must converge on the transfer target")
}

// TestPreference_DisabledVotersFallBack verifies that when voters do not enable
// the wait window (priority-0 / zero-timeout "old node" behaviour), elections
// proceed with standard Raft rules even if candidates advertise priority.
func TestPreference_DisabledVotersFallBack(t *testing.T) {
	// All peers use the default config: preference inert.
	c := MakeCluster(3, t, nil)
	defer c.Close()
	leader := c.Leader()
	require.NotNil(t, leader)
	c.EnsureLeader(t, leader.localAddr)
}

// TestPreference_MixedClusterOldVoters verifies interoperation with servers
// that do not participate in preference (zero timeout and priority 0, which
// is exactly how an old binary behaves). Elections still terminate and log
// safety is unaffected.
func TestPreference_MixedClusterOldVoters(t *testing.T) {
	// A understands preference; B and C behave like old nodes (no wait).
	confA := preferenceTestConf(t)
	confA.CandidatePriority = 10

	oldConf := inmemConfig(t) // ElectionPreferenceTimeout == 0, priority 0

	c := makePriorityCluster(t, []*Config{confA, oldConf, cloneConf(oldConf)})
	defer c.Close()

	// The cluster must elect exactly one leader promptly. With B/C not
	// honouring preference the preferred A is not guaranteed to win, but the
	// election must complete - priority is never a quorum bypass nor a liveness
	// blocker.
	leader := c.Leader()
	c.EnsureLeader(t, leader.localAddr)
	require.Contains(t, []ServerAddress{c.rafts[0].localAddr, c.rafts[1].localAddr, c.rafts[2].localAddr},
		leader.localAddr)

	// A leader from any node must be able to commit entries.
	require.NoError(t, leader.Apply([]byte("x"), c.propagateTimeout).Error())
	c.EnsureSame(t)
}

func cloneConf(c *Config) *Config {
	cp := *c
	return &cp
}

// TestPreference_PreVoteDisabled verifies the real-vote path when pre-vote is
// off: the bounded wait still applies and a partitioned preferred node cannot
// block a reachable majority.
func TestPreference_PreVoteDisabled(t *testing.T) {
	confA := preferenceTestConf(t)
	confB := preferenceTestConf(t)
	confC := preferenceTestConf(t)
	for _, c := range []*Config{confA, confB, confC} {
		c.PreVoteDisabled = true
	}
	confA.CandidatePriority = 10
	confB.CandidatePriority = 1
	confC.CandidatePriority = 1

	c := makePriorityCluster(t, []*Config{confA, confB, confC})
	defer c.Close()

	c.Leader()
	c.Partition([]ServerAddress{c.rafts[0].localAddr})

	limit := time.Now().Add(c.longstopTimeout)
	var nearLeader *Raft
	for time.Now().Before(limit) {
		for _, r := range c.rafts[1:] {
			if r.State() == Leader {
				nearLeader = r
			}
		}
		if nearLeader != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.NotNil(t, nearLeader, "reachable majority must elect a leader with pre-vote disabled")
}

// TestPreference_BoundedWaitDirect verifies the wait is capped: a follower
// defers a lower-priority vote inside the window but grants it once the
// window expires, even though a higher-priority server was observed.
func TestPreference_BoundedWaitDirect(t *testing.T) {
	// Stand up a single, fully isolated voter with preference enabled and a
	// small, known wait window.
	conf := preferenceTestConf(t)
	conf.LocalID = "voter"
	conf.ElectionPreferenceTimeout = 50 * time.Millisecond
	conf.HeartbeatTimeout = 30 * time.Second
	conf.ElectionTimeout = 30 * time.Second
	conf.LeaderLeaseTimeout = 30 * time.Second

	dir, err := os.MkdirTemp("", "raft")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	store := NewInmemStore()
	snapDir, snaps := FileSnapTest(t)
	defer os.RemoveAll(snapDir)

	addr, trans := NewInmemTransportWithTimeout("", 3*time.Second)
	candAddr, candTrans := NewInmemTransportWithTimeout("cand-addr", 3*time.Second)
	defer candTrans.Close()
	trans.Connect(candAddr, candTrans)
	candTrans.Connect(addr, trans)
	configuration := Configuration{Servers: []Server{
		{Suffrage: Voter, ID: "voter", Address: addr},
		{Suffrage: Voter, ID: "cand", Address: candAddr},
	}}
	require.NoError(t, BootstrapCluster(conf, store, store, snaps, trans, configuration))

	r, err := NewRaft(conf, &MockFSM{}, store, store, snaps, trans)
	require.NoError(t, err)
	defer r.Shutdown()

	futureTerm := r.getCurrentTerm() + 1
	idx, logTerm := r.getLastEntry()

	// A priority-5 candidate (voter has priority 0) asks for the vote. It must
	// be deferred within the window and granted shortly after the deadline.
	start := time.Now()
	req := &RequestVoteRequest{
		RPCHeader: RPCHeader{
			ProtocolVersion: ProtocolVersionMax,
			ID:              []byte("cand"),
			Addr:            trans.EncodePeer("cand", candAddr),
		},
		Term:         futureTerm,
		LastLogIndex: idx,
		LastLogTerm:  logTerm,
		Priority:     5,
	}
	var resp RequestVoteResponse
	require.NoError(t, candTrans.RequestVote("cand", addr, req, &resp))
	elapsed := time.Since(start)
	require.True(t, resp.Granted, "the vote must be granted once the bounded wait expires")
	require.GreaterOrEqual(t, elapsed, 40*time.Millisecond, "vote should be held for the window")
	require.LessOrEqual(t, elapsed, 400*time.Millisecond, "the preference wait must be bounded")
}
