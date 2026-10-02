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
	// Voter sets whose independent majorities must agree on an entry. During a
	// joint change this contains the old and next configurations; otherwise it
	// contains the current configuration.
	voters []Configuration
	// voter ID to log index: the server stores up through this log entry
	matchIndexByServer map[ServerID]uint64
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
		commitCh:           commitCh,
		voters:             []Configuration{configuration.Clone()},
		matchIndexByServer: initialMatchIndexes([]Configuration{configuration}),
		commitIndex:        0,
		startIndex:         startIndex,
	}
}

func newJointCommitment(commitCh chan struct{}, joint jointConfiguration, startIndex uint64) *commitment {
	return &commitment{
		commitCh:           commitCh,
		voters:             []Configuration{joint.Old.Clone(), joint.Next.Clone()},
		matchIndexByServer: initialMatchIndexes([]Configuration{joint.Old, joint.Next}),
		commitIndex:        0,
		startIndex:         startIndex,
	}
}

// Called when a new cluster membership configuration is created: it will be
// used to determine commitment from now on. 'configuration' is the servers in
// the cluster.
func (c *commitment) setConfiguration(configuration Configuration) {
	c.Lock()
	defer c.Unlock()
	c.setVotersLocked([]Configuration{configuration.Clone()})
	c.recalculate()
}

func (c *commitment) setJointConfiguration(joint jointConfiguration) {
	c.Lock()
	defer c.Unlock()
	c.setVotersLocked([]Configuration{joint.Old.Clone(), joint.Next.Clone()})
	c.recalculate()
}

func (c *commitment) setVotersLocked(voters []Configuration) {
	oldMatchIndexes := c.matchIndexByServer
	c.voters = make([]Configuration, 0, len(voters))
	for _, configuration := range voters {
		c.voters = append(c.voters, configuration.Clone())
	}
	c.matchIndexByServer = make(map[ServerID]uint64)
	for _, configuration := range c.voters {
		for _, server := range configuration.Servers {
			if server.Suffrage == Voter {
				c.matchIndexByServer[server.ID] = oldMatchIndexes[server.ID]
			}
		}
	}
}

func initialMatchIndexes(voters []Configuration) map[ServerID]uint64 {
	matchIndexes := make(map[ServerID]uint64)
	for _, configuration := range voters {
		for _, server := range configuration.Servers {
			if server.Suffrage == Voter {
				matchIndexes[server.ID] = 0
			}
		}
	}
	return matchIndexes
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
	if c.isVoter(server) {
		if matchIndex > c.matchIndexByServer[server] {
			c.matchIndexByServer[server] = matchIndex
		}
		c.recalculate()
	}
}

func (c *commitment) isVoter(server ServerID) bool {
	for _, configuration := range c.voters {
		if hasVote(configuration, server) {
			return true
		}
	}
	return false
}

// Internal helper to calculate new commitIndex from matchIndexes.
// Must be called with lock held.
func (c *commitment) recalculate() {
	if len(c.voters) == 0 {
		return
	}

	quorumMatchIndex := ^uint64(0)
	for _, configuration := range c.voters {
		matched := make([]uint64, 0, voterCount(configuration))
		for _, server := range configuration.Servers {
			if server.Suffrage == Voter {
				matched = append(matched, c.matchIndexByServer[server.ID])
			}
		}
		if len(matched) == 0 {
			return
		}
		sort.Sort(uint64Slice(matched))
		configurationMatchIndex := matched[(len(matched)-1)/2]
		if configurationMatchIndex < quorumMatchIndex {
			quorumMatchIndex = configurationMatchIndex
		}
	}

	if quorumMatchIndex > c.commitIndex && quorumMatchIndex >= c.startIndex {
		c.commitIndex = quorumMatchIndex
		asyncNotifyCh(c.commitCh)
	}
}
