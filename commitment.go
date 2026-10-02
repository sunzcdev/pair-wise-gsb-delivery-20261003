// Copyright IBM Corp. 2013, 2025
// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"sort"
	"sync"
)

// Commitment is used to advance the leader's commit index. The leader and
// replication goroutines report in newly written entries with match(), and
// this notifies on commitCh when the commit index has advanced.
type commitment struct {
	// protects matchIndexes and commitIndex
	sync.Mutex
	// notified when commitIndex increases
	commitCh chan struct{}
	// One match-index map for each voter group. Commitment during a joint
	// change requires a majority in every group.
	voterGroups []map[ServerID]uint64
	// a quorum stores up through this log entry. monotonically increases.
	commitIndex uint64
	// the first index of this leader's term: this needs to be replicated to a
	// majority of the cluster before this leader may mark anything committed
	// (per Raft's commitment rule)
	startIndex uint64
}

// newCommitment returns a commitment struct that notifies the provided
// channel when log entries have been committed. A new commitment struct is
// created each time this server becomes leader for a particular term.
// 'configuration' is the servers in the cluster.
// 'startIndex' is the first index created in this term (see
// its description above).
func newCommitment(commitCh chan struct{}, configuration Configuration, startIndex uint64) *commitment {
	return &commitment{
		commitCh:    commitCh,
		voterGroups: []map[ServerID]uint64{newVoterMatchIndexes(configuration)},
		commitIndex: 0,
		startIndex:  startIndex,
	}
}

func newVoterMatchIndexes(configuration Configuration) map[ServerID]uint64 {
	matchIndexes := make(map[ServerID]uint64)
	for _, server := range configuration.Servers {
		if server.Suffrage == Voter {
			matchIndexes[server.ID] = 0
		}
	}
	return matchIndexes
}

// newJointCommitment creates a commitment which requires separate majorities
// from both joint voter groups. Existing match indexes are retained.
func newJointCommitment(commitCh chan struct{}, joint JointConfiguration, startIndex, commitIndex uint64, previous []map[ServerID]uint64) *commitment {
	commitment := &commitment{
		commitCh:    commitCh,
		voterGroups: make([]map[ServerID]uint64, 2),
		commitIndex: commitIndex,
		startIndex:  startIndex,
	}
	commitment.voterGroups[0] = reuseVoterMatchIndexes(joint.Old, previous)
	commitment.voterGroups[1] = reuseVoterMatchIndexes(joint.New, previous)
	return commitment
}

func reuseVoterMatchIndexes(configuration Configuration, previous []map[ServerID]uint64) map[ServerID]uint64 {
	matchIndexes := newVoterMatchIndexes(configuration)
	for _, group := range previous {
		for id, index := range group {
			if _, ok := matchIndexes[id]; ok && index > matchIndexes[id] {
				matchIndexes[id] = index
			}
		}
	}
	return matchIndexes
}

// Called when a new cluster membership configuration is created: it will be
// used to determine commitment from now on. 'configuration' is the servers in
// the cluster.
func (c *commitment) setConfiguration(configuration Configuration) {
	c.Lock()
	defer c.Unlock()
	c.voterGroups = []map[ServerID]uint64{reuseVoterMatchIndexes(configuration, c.voterGroups)}
	c.recalculate()
}

// setJointConfiguration switches to joint majority calculation.
func (c *commitment) setJointConfiguration(joint JointConfiguration) {
	c.Lock()
	defer c.Unlock()
	c.voterGroups = []map[ServerID]uint64{
		reuseVoterMatchIndexes(joint.Old, c.voterGroups),
		reuseVoterMatchIndexes(joint.New, c.voterGroups),
	}
	c.recalculate()
}

func (c *commitment) setConfigurations(configurations []Configuration) {
	c.Lock()
	defer c.Unlock()
	groups := make([]map[ServerID]uint64, len(configurations))
	for i, configuration := range configurations {
		groups[i] = reuseVoterMatchIndexes(configuration, c.voterGroups)
	}
	c.voterGroups = groups
	c.recalculate()
}

// Called by leader after commitCh is notified
func (c *commitment) getCommitIndex() uint64 {
	c.Lock()
	defer c.Unlock()
	return c.commitIndex
}

// Match is called once a server completes writing entries to disk: either the
// leader has written the new entry or a follower has replied to an
// AppendEntries RPC. The given server's disk agrees with this server's log up
// through the given index.
func (c *commitment) match(server ServerID, matchIndex uint64) {
	c.Lock()
	defer c.Unlock()
	matched := false
	for _, matchIndexes := range c.voterGroups {
		if prev, hasVote := matchIndexes[server]; hasVote && matchIndex > prev {
			matchIndexes[server] = matchIndex
			matched = true
		}
	}
	if matched {
		c.recalculate()
	}
}

func (c *commitment) contactQuorum(contacted map[ServerID]bool) bool {
	c.Lock()
	defer c.Unlock()
	for _, group := range c.voterGroups {
		contacts := 0
		for id := range group {
			if contacted[id] {
				contacts++
			}
		}
		if contacts < len(group)/2+1 {
			return false
		}
	}
	return true
}

// Internal helper to calculate new commitIndex from matchIndexes.
// Must be called with lock held.
func (c *commitment) recalculate() {
	quorumMatchIndex := c.quorumMatchIndexLocked()
	if quorumMatchIndex == 0 {
		return
	}

	if quorumMatchIndex > c.commitIndex && quorumMatchIndex >= c.startIndex {
		c.commitIndex = quorumMatchIndex
		asyncNotifyCh(c.commitCh)
	}
}

func (c *commitment) quorumMatchIndexLocked() uint64 {
	if len(c.voterGroups) == 0 {
		return 0
	}
	quorumMatchIndex := ^uint64(0)
	for _, matchIndexes := range c.voterGroups {
		if len(matchIndexes) == 0 {
			return 0
		}
		matched := make([]uint64, 0, len(matchIndexes))
		for _, idx := range matchIndexes {
			matched = append(matched, idx)
		}
		sort.Sort(uint64Slice(matched))
		groupMatchIndex := matched[(len(matched)-1)/2]
		if groupMatchIndex < quorumMatchIndex {
			quorumMatchIndex = groupMatchIndex
		}
	}
	return quorumMatchIndex
}
