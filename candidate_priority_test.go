// Copyright IBM Corp. 2013, 2026
// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// makePriorityCluster builds a cluster whose servers carry the given
// candidate priorities in the bootstrap configuration.
func makePriorityCluster(t *testing.T, maxWait time.Duration, priorities ...uint32) *cluster {
	t.Helper()
	conf := inmemConfig(t)
	conf.MaxCandidatePriorityWait = maxWait
	c := MakeClusterNoBootstrap(len(priorities), t, conf)
	configuration := Configuration{}
	for i, r := range c.rafts {
		configuration.Servers = append(configuration.Servers, Server{
			Suffrage: Voter,
			ID:       r.localID,
			Address:  r.localAddr,
			Priority: priorities[i],
		})
	}
	for _, r := range c.rafts {
		if err := r.BootstrapCluster(configuration).Error(); err != nil {
			t.Fatalf("BootstrapCluster failed: %v", err)
		}
	}
	return c
}

func waitForSpecificLeader(t *testing.T, c *cluster, want *Raft) {
	t.Helper()
	deadline := time.Now().Add(c.longstopTimeout)
	for time.Now().Before(deadline) {
		if want.State() == Leader {
			agreed := true
			for _, r := range c.rafts {
				if addr, _ := r.LeaderWithID(); addr != want.localAddr {
					agreed = false
					break
				}
			}
			if agreed {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %v to become leader", want.localID)
}

// Scenario: the high-priority server only starts its election slightly late.
// The rest of the cluster must wait (within the bounded window) and still
// elect it once it shows up, rather than electing someone else first.
func TestRaft_CandidatePriority_PreferredWinsWhenSlightlyLate(t *testing.T) {
	maxWait := 2 * time.Second
	c := makePriorityCluster(t, maxWait, 10, 0, 0)
	defer c.Close()

	preferred := c.rafts[0]

	// Isolate the preferred server so the others time out first and try to
	// campaign on their own.
	c.Partition([]ServerAddress{preferred.localAddr})

	// Give the low-priority servers plenty of time to time out and attempt
	// elections, but stay well below the bounded wait so they must still be
	// holding off.
	time.Sleep(maxWait / 4)
	for _, r := range c.rafts {
		if r.State() == Leader {
			t.Fatalf("no leader should be elected while the preferred server is away")
		}
	}

	// The preferred server becomes reachable ("starts its election late")
	// and must still win promptly, in the same election episode.
	c.FullyConnect()
	waitForSpecificLeader(t, c, preferred)
}

// Scenario: the high-priority server is partitioned away from the majority.
// The bounded wait expires and the majority elects someone else; priority is
// a preference, not a pass around the quorum.
func TestRaft_CandidatePriority_PreferredPartitioned(t *testing.T) {
	maxWait := 500 * time.Millisecond
	c := makePriorityCluster(t, maxWait, 10, 0, 0)
	defer c.Close()

	preferred := c.rafts[0]
	require.Equal(t, preferred, c.Leader(), "preferred server should win the first election")

	// Partition the preferred leader away from the majority.
	c.Partition([]ServerAddress{preferred.localAddr})
	start := time.Now()

	// The majority must still elect a new leader. Poll manually because the
	// bounded wait deliberately stretches the election beyond the cluster
	// helper's stability window.
	var leader *Raft
	deadline := time.Now().Add(6 * maxWait)
	for time.Now().Before(deadline) {
		for _, r := range c.rafts[1:] {
			if r.State() == Leader {
				leader = r
			}
		}
		if leader != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NotNil(t, leader, "majority must elect a leader without the preferred server")
	require.NotEqual(t, preferred, leader, "partitioned preferred server cannot be leader")
	elapsed := time.Since(start)
	require.Greater(t, elapsed, maxWait, "withholding votes should delay the election")
	require.Less(t, elapsed, 6*maxWait, "the wait for the preferred server must be bounded")

	// The cluster under the new leader is fully functional.
	require.NoError(t, leader.Apply([]byte("still-working"), 0).Error())
}

// Log freshness is a safety rule and always beats candidate priority.
func TestRaft_CandidatePriority_LogFreshnessBeatsPriority(t *testing.T) {
	c := makePriorityCluster(t, 500*time.Millisecond, 10, 0, 0)
	defer c.Close()

	leader := c.Leader()
	preferred := c.rafts[0]
	require.Equal(t, preferred, leader, "preferred server should win the first election")
	require.NoError(t, leader.Apply([]byte("entry"), 0).Error())
	c.WaitForReplication(1)

	followers := c.Followers()
	f1, f2 := followers[0], followers[1]
	preferredTrans := c.trans[0]

	lastIdx, lastTerm := f1.getLastEntry()
	term := f1.getCurrentTerm() + 10

	// The LeadershipTransfer flag is used to bypass the "we have a leader"
	// check so that the log freshness rule is exercised in isolation.
	req := RequestVoteRequest{
		RPCHeader:          preferred.getRPCHeader(),
		Term:               term,
		LastLogIndex:       0,
		LastLogTerm:        0,
		LeadershipTransfer: true,
	}

	// The most preferred server with a stale log must be rejected.
	var resp RequestVoteResponse
	require.NoError(t, preferredTrans.RequestVote(f1.localID, f1.localAddr, &req, &resp))
	require.False(t, resp.Granted, "stale log must be rejected even for the preferred server")

	// The same server with an up-to-date log is granted the vote.
	req.LastLogIndex, req.LastLogTerm = lastIdx, lastTerm
	require.NoError(t, preferredTrans.RequestVote(f2.localID, f2.localAddr, &req, &resp))
	require.True(t, resp.Granted, "up-to-date preferred server should be granted the vote")
}

// Priority must never override the one-vote-per-term rule or term checks.
func TestRaft_CandidatePriority_VoteSafetyRules(t *testing.T) {
	c := makePriorityCluster(t, 500*time.Millisecond, 10, 0, 0)
	defer c.Close()

	c.Leader()
	preferred := c.rafts[0] // priority 10
	other := c.rafts[1]     // priority 0
	voter := c.rafts[2]     // never the sender of a crafted request below
	require.Equal(t, preferred, c.Leader(), "preferred server should win the first election")
	preferredTrans := c.trans[0]
	otherTrans := c.trans[1]

	lastIdx, lastTerm := voter.getLastEntry()
	term := voter.getCurrentTerm() + 10

	// The LeadershipTransfer flag bypasses the leader check and the priority
	// withholding so the term and one-vote-per-term rules are exercised in
	// isolation.
	voteReq := func(cand *Raft, term uint64) RequestVoteRequest {
		return RequestVoteRequest{
			RPCHeader:          cand.getRPCHeader(),
			Term:               term,
			LastLogIndex:       lastIdx,
			LastLogTerm:        lastTerm,
			LeadershipTransfer: true,
		}
	}
	var resp RequestVoteResponse

	// The low-priority candidate gets this term's vote first.
	req := voteReq(other, term)
	require.NoError(t, otherTrans.RequestVote(voter.localID, voter.localAddr, &req, &resp))
	require.True(t, resp.Granted)

	// The higher-priority candidate must not steal the recorded vote in the
	// same term.
	req = voteReq(preferred, term)
	require.NoError(t, preferredTrans.RequestVote(voter.localID, voter.localAddr, &req, &resp))
	require.False(t, resp.Granted, "priority must not override one vote per term")

	// The original candidate asking again is still granted (idempotent).
	req = voteReq(other, term)
	require.NoError(t, otherTrans.RequestVote(voter.localID, voter.localAddr, &req, &resp))
	require.True(t, resp.Granted)

	// An older term from the preferred server is rejected.
	req = voteReq(preferred, term-5)
	require.NoError(t, preferredTrans.RequestVote(voter.localID, voter.localAddr, &req, &resp))
	require.False(t, resp.Granted, "priority must not override the term check")
}

// Untargeted leadership transfer should prefer the highest-priority follower
// that participates in quorum.
func TestRaft_CandidatePriority_LeadershipTransferPrefersPriority(t *testing.T) {
	c := makePriorityCluster(t, 500*time.Millisecond, 0, 10, 5)
	defer c.Close()

	require.Equal(t, c.rafts[1], c.Leader(), "most preferred server should win the first election")

	// Transfer away from the priority-10 server: the priority-5 server is
	// the next most preferred target.
	require.NoError(t, c.Leader().LeadershipTransfer().Error())
	waitForSpecificLeader(t, c, c.rafts[2])

	// Transferring again goes back to the priority-10 server.
	require.NoError(t, c.Leader().LeadershipTransfer().Error())
	waitForSpecificLeader(t, c, c.rafts[1])
}

// Unit test the candidacy delay and vote withholding bounds.
func TestRaft_CandidatePriority_DelayAndWithholding(t *testing.T) {
	newRaft := func(localID ServerID, maxWait time.Duration, servers ...Server) *Raft {
		conf := inmemConfig(t)
		conf.MaxCandidatePriorityWait = maxWait
		r := &Raft{localID: localID}
		r.conf.Store(*conf)
		r.configurations.latest = Configuration{Servers: servers}
		return r
	}
	servers := []Server{
		{Suffrage: Voter, ID: "a", Priority: 1},
		{Suffrage: Voter, ID: "b", Priority: 5},
		{Suffrage: Voter, ID: "c", Priority: 0},
		{Suffrage: Nonvoter, ID: "d", Priority: 100}, // nonvoters never count
	}
	maxWait := 100 * time.Millisecond

	// Candidacy delay scales with the priority gap and is bounded.
	r := newRaft("a", maxWait, servers...)
	require.Equal(t, 80*time.Millisecond, r.candidatePriorityDelay())
	r = newRaft("c", maxWait, servers...)
	require.Equal(t, 100*time.Millisecond, r.candidatePriorityDelay())
	r = newRaft("b", maxWait, servers...)
	require.Zero(t, r.candidatePriorityDelay(), "most preferred server never delays")
	r = newRaft("d", maxWait, servers...)
	require.Zero(t, r.candidatePriorityDelay(), "nonvoter never delays")

	// Without priorities nothing changes.
	r = newRaft("a", maxWait,
		Server{Suffrage: Voter, ID: "a"},
		Server{Suffrage: Voter, ID: "b"})
	require.Zero(t, r.candidatePriorityDelay())
	require.False(t, r.shouldWithholdVote("b"))

	// Withholding applies only to lower-priority candidates and expires.
	r = newRaft("a", maxWait, servers...)
	require.True(t, r.shouldWithholdVote("c"), "withhold from lowest priority")
	require.True(t, r.shouldWithholdVote("c"), "still within the wait window")
	require.False(t, r.shouldWithholdVote("b"), "never withhold from the most preferred")
	require.False(t, r.shouldWithholdVote("d"), "nonvoter is rejected elsewhere")

	// Once the bounded wait expires, votes are granted normally again.
	r.priorityWaitStart = time.Now().Add(-2 * maxWait)
	require.False(t, r.shouldWithholdVote("c"), "wait is bounded")
	require.False(t, r.shouldWithholdVote("c"), "window does not re-arm within the same episode")

	// A granted vote (or leader contact) resets the window for the next
	// leaderless episode.
	r.priorityWaitStart = time.Time{}
	require.True(t, r.shouldWithholdVote("c"), "window re-arms after a reset")
}

// Old servers don't know the Priority field. Encoding and decoding must be
// compatible in both directions: old data decodes with zero priority, and
// old code can decode new data by ignoring the unknown field.
func TestRaft_CandidatePriority_WireCompatibility(t *testing.T) {
	type oldServer struct {
		Suffrage ServerSuffrage
		ID       ServerID
		Address  ServerAddress
	}
	type oldConfiguration struct {
		Servers []oldServer
	}

	// New -> old: an old server decodes a configuration carrying priorities
	// and simply ignores the unknown field.
	withPriorities := Configuration{Servers: []Server{
		{Suffrage: Voter, ID: "a", Address: "addr-a", Priority: 7},
		{Suffrage: Nonvoter, ID: "b", Address: "addr-b", Priority: 3},
	}}
	var decodedOld oldConfiguration
	require.NoError(t, decodeMsgPack(EncodeConfiguration(withPriorities), &decodedOld))
	require.Equal(t, oldConfiguration{Servers: []oldServer{
		{Suffrage: Voter, ID: "a", Address: "addr-a"},
		{Suffrage: Nonvoter, ID: "b", Address: "addr-b"},
	}}, decodedOld)

	// Old -> new: data written by an old server decodes with zero priority.
	oldBuf, err := encodeMsgPack(oldConfiguration{Servers: []oldServer{
		{Suffrage: Voter, ID: "a", Address: "addr-a"},
	}})
	require.NoError(t, err)
	decodedNew := DecodeConfiguration(oldBuf.Bytes())
	require.Equal(t, Configuration{Servers: []Server{
		{Suffrage: Voter, ID: "a", Address: "addr-a", Priority: 0},
	}}, decodedNew)
}
