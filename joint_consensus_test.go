// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCommitment_JointMajority(t *testing.T) {
	commitCh := make(chan struct{}, 4)
	commitment := newCommitment(commitCh, voters(3), 1)
	joint := JointConfiguration{
		Old: makeConfiguration([]string{"a", "b", "c"}),
		New: makeConfiguration([]string{"c", "d", "e"}),
	}
	commitment.setJointConfiguration(joint)

	// A majority in the old set plus only the shared voter is not a new
	// majority, so nothing may commit.
	commitment.match("a", 10)
	commitment.match("b", 10)
	commitment.match("c", 10)
	require.Equal(t, uint64(0), commitment.getCommitIndex())

	commitment.match("d", 10)
	require.Equal(t, uint64(10), commitment.getCommitIndex())
}

func TestRaft_ChangeConfiguration_JointTransition(t *testing.T) {
	c := MakeCluster(5, t, nil)
	defer c.Close()

	leader := c.Leader()
	target := c.getConfiguration(leader)
	target.Servers = append(target.Servers[:2], target.Servers[3:]...)
	require.Len(t, target.Servers, 4)

	change := leader.ChangeConfiguration(target, 0, 0)
	require.NoError(t, change.Error())
	require.Equal(t, MembershipChangeCommitted, leader.MembershipChangeStatus().Phase)

	c.EnsureSamePeers(t)
	configuration := c.getConfiguration(c.Leader())
	require.ElementsMatch(t, target.Servers, configuration.Servers)
}

func TestRaft_ChangeConfiguration_RemovedLeaderCompletes(t *testing.T) {
	c := MakeCluster(3, t, nil)
	defer c.Close()

	leader := c.Leader()
	target := c.getConfiguration(leader)
	var remaining []Server
	for _, server := range target.Servers {
		if server.ID != leader.localID {
			remaining = append(remaining, server)
		}
	}
	target.Servers = remaining

	err := leader.ChangeConfiguration(target, 0, 0).Error()
	require.True(t, err == nil || errors.Is(err, ErrLeadershipLost), "unexpected change error: %v", err)

	newLeader := c.Leader()
	require.NotEqual(t, leader.localID, newLeader.localID)
	require.Eventually(t, func() bool {
		status := newLeader.MembershipChangeStatus()
		return status.Phase == MembershipChangeCommitted
	}, time.Second, 5*time.Millisecond)
	require.ElementsMatch(t, target.Servers, c.getConfiguration(newLeader).Servers)
}

func TestRaft_ChangeConfiguration_RecoverJointSnapshot(t *testing.T) {
	conf := inmemConfig(t)
	conf.HeartbeatTimeout = time.Second
	conf.ElectionTimeout = time.Second
	conf.LeaderLeaseTimeout = time.Second
	conf.TrailingLogs = 0
	c := MakeClusterCustom(t, &MakeClusterOpts{
		Peers:           5,
		Bootstrap:       true,
		Conf:            conf,
		LongstopTimeout: 20 * time.Second,
	})
	defer c.Close()

	leader := c.Leader()

	// Freeze the cluster in the committed-joint phase so the snapshot is
	// guaranteed to capture joint state.
	hold := holdFinalConfigurationEntry(t)
	defer hold.release()

	target := c.getConfiguration(leader)
	target.Servers = append(
		[]Server{target.Servers[4]},
		target.Servers[:4]...,
	)
	require.Len(t, target.Servers, 5)
	change := proposeJointChange(t, c, target)
	leader = c.Leader()
	require.Eventually(t, func() bool {
		return leader.MembershipChangeStatus().Phase == MembershipChangeJointCommitted
	}, time.Second, 5*time.Millisecond)

	// Advance every FSM past the committed joint configuration index so a
	// snapshot at that index is possible on each node.
	require.NoError(t, leader.Apply([]byte("joint-snapshot-marker"), 0).Error())
	c.WaitForReplication(1)

	for _, node := range c.rafts {
		_, err := node.takeSnapshot()
		require.NoError(t, err)
	}
	snaps, err := leader.snapshots.List()
	require.NoError(t, err)
	require.NotEmpty(t, snaps)
	require.NotNil(t, snaps[0].JointConfiguration)
	require.Equal(t, SnapshotVersion(2), snaps[0].Version)

	// Restart one follower while the joint configuration is still the
	// committed configuration. Its on-disk snapshot must restore joint state.
	leader = c.Leader()
	leaderIdx := c.IndexOf(leader)
	followerIdx := (leaderIdx + 1) % len(c.rafts)
	require.NoError(t, c.rafts[followerIdx].Shutdown().Error())
	addr := c.trans[followerIdx].LocalAddr()
	require.NoError(t, c.trans[followerIdx].Close())
	_, newTrans := NewInmemTransport(addr)
	c.trans[followerIdx] = newTrans
	c.FullyConnect()

	restarted, err := NewRaft(c.conf, &MockFSM{}, c.stores[followerIdx],
		c.stores[followerIdx], c.snaps[followerIdx], newTrans)
	require.NoError(t, err)
	c.rafts[followerIdx] = restarted

	status := restarted.MembershipChangeStatus()
	require.Equal(t, MembershipChangeJointCommitted, status.Phase)
	require.NotEmpty(t, status.Target.Servers)

	// The recovered leader resumes and completes the membership change.
	hold.release()
	releaseFinalConfigurationEntry(c)
	if change != nil {
		if err := change.Error(); err != nil {
			require.True(t, errors.Is(err, ErrLeadershipLost), "unexpected change error: %v", err)
		}
	}

	require.Eventually(t, func() bool {
		return restarted.MembershipChangeStatus().Phase == MembershipChangeCommitted
	}, c.longstopTimeout, 10*time.Millisecond)
	c.EnsureSamePeers(t)
	require.ElementsMatch(t, target.Servers, c.getConfiguration(c.Leader()).Servers)
}

func TestRaft_ChangeConfiguration_SmallIntersectionPartition(t *testing.T) {
	conf := inmemConfig(t)
	conf.HeartbeatTimeout = time.Second
	conf.ElectionTimeout = time.Second
	conf.LeaderLeaseTimeout = time.Second
	conf.CommitTimeout = 20 * time.Millisecond
	c := MakeClusterCustom(t, &MakeClusterOpts{
		Peers:           7,
		Bootstrap:       true,
		Conf:            conf,
		LongstopTimeout: 20 * time.Second,
	})
	defer c.Close()

	current := c.getConfiguration(c.Leader())

	// Two voter sets of four with only a single shared voter. The cluster
	// starts with the seven distinct voters that make up both sets.
	all := current.Servers
	require.Len(t, all, 7)
	oldSet := append([]Server{}, all[:4]...)
	shared := oldSet[0]
	newSet := []Server{shared}
	newSet = append(newSet, all[4:]...)
	require.Len(t, newSet, 4)
	target := Configuration{Servers: newSet}

	oldOnly := oldSet[1:]
	newOnly := all[4:]
	require.Len(t, oldOnly, 3)
	require.Len(t, newOnly, 3)

	c.FullyConnect()
	// Hold the committed-joint phase so the partition checks below are not
	// racing against the final configuration entry.
	hold := holdFinalConfigurationEntry(t)
	defer hold.release()

	change := proposeJointChange(t, c, target)

	leader := c.Leader()
	require.Equal(t, MembershipChangeJointCommitted, leader.MembershipChangeStatus().Phase)
	addressesExcept := func(servers []Server, excluded ServerID, n int) []ServerAddress {
		var picked []ServerAddress
		for _, server := range servers {
			if server.ID == excluded {
				continue
			}
			picked = append(picked, server.Address)
			if len(picked) == n {
				break
			}
		}
		require.Len(t, picked, n)
		return picked
	}

	// Isolate two old-only voters. The connected side holds a new-set
	// majority (shared + three new) but only two of the four old-set votes,
	// so nothing may commit on the new quorum alone.
	c.Partition(addressesExcept(oldOnly, leader.localID, 2))
	time.Sleep(c.propagateTimeout)
	// The connected side has a new-set majority but only two of five
	// old-set votes, so the final configuration cannot commit anywhere.
	assertNoFinalCommit(t, c)
	c.FullyConnect()
	time.Sleep(c.propagateTimeout)

	// Isolate two new-only voters: the old-set majority survives but the
	// new set cannot reach a majority.
	leader = c.Leader()
	c.Partition(addressesExcept(newOnly, leader.localID, 2))
	time.Sleep(c.propagateTimeout)
	// The isolated side keeps an old-set majority but cannot reach a
	// new-set majority, so the change must remain stuck in the joint phase.
	assertNoFinalCommit(t, c)

	c.FullyConnect()
	hold.release()
	releaseFinalConfigurationEntry(c)
	if change != nil {
		if err := change.Error(); err != nil {
			require.True(t, errors.Is(err, ErrLeadershipLost), "unexpected change error: %v", err)
		}
	}

	c.EnsureSamePeers(t)
	configuration := c.getConfiguration(c.Leader())
	require.ElementsMatch(t, target.Servers, configuration.Servers)
}

type finalConfigurationEntryHold struct {
	t        *testing.T
	released bool
}

// assertNoFinalCommit waits briefly and confirms that, while a partition
// prevents one joint quorum from being satisfied, no node observes the final
// configuration commit.
func assertNoFinalCommit(t *testing.T, c *cluster) {
	t.Helper()
	deadline := time.Now().Add(c.propagateTimeout * 3)
	for time.Now().Before(deadline) {
		for _, node := range c.rafts {
			if phase := node.MembershipChangeStatus().Phase; phase == MembershipChangeCommitted {
				t.Fatalf("node %s reached final commit without joint quorum", node.localID)
			}
		}
		time.Sleep(c.conf.CommitTimeout)
	}

	committed := 0
	for _, node := range c.rafts {
		if node.MembershipChangeStatus().Phase == MembershipChangeJointCommitted {
			committed++
		}
	}
	require.Greater(t, committed, 0, "expected at least one node to hold committed-joint state")
}

// proposeJointChange submits a ChangeConfiguration on the current leader and
// waits for the joint configuration to commit. The returned future completes
// only once the final configuration commits, so it must not be awaited while
// the committed-joint phase is deliberately held.
func proposeJointChange(t *testing.T, c *cluster, target Configuration) Future {
	t.Helper()
	deadline := time.Now().Add(c.longstopTimeout)
	var future IndexFuture
	attempted := false
	for {
		leader := c.Leader()
		phase := leader.MembershipChangeStatus().Phase
		if phase == MembershipChangeJointCommitted {
			return future
		}
		if attempted && phase == MembershipChangeJoint {
			break
		}

		if !attempted {
			// Ensure this leader has committed an entry in its term, after
			// which the configuration-change channel is enabled.
			if err := leader.Barrier(0).Error(); err != nil {
				if time.Now().After(deadline) {
					require.NoError(t, err)
				}
				time.Sleep(c.conf.CommitTimeout)
				continue
			}
			// Submit exactly once; a second submission would build a joint
			// configuration off the merged set.
			future = leader.ChangeConfiguration(target, 0, 0)
			attempted = true
		}
		require.Eventually(t, func() bool {
			switch c.Leader().MembershipChangeStatus().Phase {
			case MembershipChangeJoint, MembershipChangeJointCommitted:
				return true
			default:
				return false
			}
		}, c.longstopTimeout, c.conf.CommitTimeout)
	}

	require.Eventually(t, func() bool {
		return c.Leader().MembershipChangeStatus().Phase == MembershipChangeJointCommitted
	}, c.longstopTimeout, c.conf.CommitTimeout)
	return future
}

func holdFinalConfigurationEntry(t *testing.T) *finalConfigurationEntryHold {
	t.Helper()
	hold := &finalConfigurationEntryHold{t: t}
	testBlockFinalConfigurationEntry = func() bool {
		return !hold.released
	}
	t.Cleanup(func() { testBlockFinalConfigurationEntry = nil })
	return hold
}

func (h *finalConfigurationEntryHold) release() {
	if h.released {
		return
	}
	h.released = true
	testBlockFinalConfigurationEntry = nil
}

// releaseFinalConfigurationEntry nudges the current leader to retry appending
// the final configuration entry after a committed-joint hold is released.
func releaseFinalConfigurationEntry(c *cluster) {
	leader := c.Leader()
	require.Eventually(c.t, func() bool {
		future := leader.Apply([]byte("finish-membership-change"), 0)
		if future.Error() != nil {
			leader = c.Leader()
			return false
		}
		return leader.MembershipChangeStatus().Phase == MembershipChangeCommitted
	}, 5*time.Second, 10*time.Millisecond)
}
