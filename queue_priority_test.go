// Copyright IBM Corp. 2013, 2026
// SPDX-License-Identifier: MPL-2.0

package memberlist

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// testPriorityBroadcast is a NamedBroadcast that also reports a priority
// class, used to simulate member changes vs. application traffic.
type testPriorityBroadcast struct {
	name     string
	msg      []byte
	priority BroadcastPriority
	finished chan struct{}
}

func (b *testPriorityBroadcast) Name() string                         { return b.name }
func (b *testPriorityBroadcast) BroadcastPriority() BroadcastPriority { return b.priority }
func (b *testPriorityBroadcast) Message() []byte                      { return b.msg }

func (b *testPriorityBroadcast) Invalidates(other Broadcast) bool {
	nb, ok := other.(NamedBroadcast)
	return ok && nb.Name() == b.name
}

func (b *testPriorityBroadcast) Finished() {
	if b.finished != nil {
		select {
		case b.finished <- struct{}{}:
		default:
		}
	}
}

// testPlainBroadcast does not implement PriorityBroadcast and therefore
// lands in the default class.
type testPlainBroadcast struct {
	name string
	msg  []byte
}

func (b *testPlainBroadcast) Name() string               { return b.name }
func (b *testPlainBroadcast) Message() []byte            { return b.msg }
func (b *testPlainBroadcast) Invalidates(Broadcast) bool { return false }
func (b *testPlainBroadcast) Finished()                  {}

func requireNotFinished(t *testing.T, ch chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s", msg)
	default:
	}
}

func requireFinished(t *testing.T, ch chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	default:
		t.Fatalf("%s", msg)
	}
}

func TestMemberlistBroadcast_Priority(t *testing.T) {
	var b Broadcast = &memberlistBroadcast{"test", nil, nil}
	pb, ok := b.(PriorityBroadcast)
	require.True(t, ok, "memberlistBroadcast should implement PriorityBroadcast")
	require.Equal(t, BroadcastPriorityHigh, pb.BroadcastPriority())
}

func TestBroadcastPriorityOf_Clamping(t *testing.T) {
	require.Equal(t, DefaultBroadcastPriority, broadcastPriorityOf(&testPlainBroadcast{}))
	require.Equal(t, BroadcastPriorityHigh, broadcastPriorityOf(&testPriorityBroadcast{priority: -5}))
	require.Equal(t, BroadcastPriorityLow, broadcastPriorityOf(&testPriorityBroadcast{priority: 99}))
	require.Equal(t, BroadcastPriorityNormal, broadcastPriorityOf(&testPriorityBroadcast{priority: BroadcastPriorityNormal}))
}

// TestTransmitLimited_PriorityFlood simulates a flood of low-priority
// application messages and verifies that a member-change message jumps
// ahead of them, and that default-priority traffic sits in between.
func TestTransmitLimited_PriorityFlood(t *testing.T) {
	q := &TransmitLimitedQueue{RetransmitMult: 1, NumNodes: func() int { return 10 }}

	// Flood the queue with low-priority "user" messages (21 bytes each).
	for i := 0; i < 10; i++ {
		q.QueueBroadcast(&testPriorityBroadcast{
			name:     fmt.Sprintf("user-%d", i),
			msg:      []byte(fmt.Sprintf("user flood message %02d", i)),
			priority: BroadcastPriorityLow,
		})
	}
	// One default-priority message (20 bytes).
	q.QueueBroadcast(&testPlainBroadcast{name: "gossip", msg: []byte("normal priority msg!")})
	// A single high-priority member change (13 bytes).
	q.QueueBroadcast(&testPriorityBroadcast{
		name:     "member",
		msg:      []byte("member change"),
		priority: BroadcastPriorityHigh,
	})

	// Only room for one message per call. Each message is retransmitted up
	// to the transmit limit, so look at the order of first occurrences:
	// the member change must go first despite the flood enqueued before
	// it, then default-priority traffic, then the flood.
	var order []string
	seen := make(map[string]bool)
	for q.NumQueued() > 0 {
		for _, msg := range q.GetBroadcasts(2, 30) {
			s := string(msg)
			if !seen[s] {
				seen[s] = true
				order = append(order, s)
			}
		}
	}
	require.Len(t, order, 12)
	require.Equal(t, "member change", order[0])
	require.Equal(t, "normal priority msg!", order[1])
	require.Contains(t, order[2], "user flood message")
}

// TestTransmitLimited_AntiStarvation keeps producing high-priority member
// changes on every round and verifies that a low-priority application
// broadcast still makes progress and finishes within a bounded number of
// rounds.
func TestTransmitLimited_AntiStarvation(t *testing.T) {
	q := &TransmitLimitedQueue{
		RetransmitMult:  1,
		NumNodes:        func() int { return 10 },
		StarvationLimit: 2,
	}
	transmitLimit := retransmitLimit(q.RetransmitMult, q.NumNodes())
	require.Equal(t, 2, transmitLimit, "sanity check transmit limit")

	lowFinished := make(chan struct{}, 1)
	q.QueueBroadcast(&testPriorityBroadcast{
		name:     "app",
		msg:      []byte("low priority message"),
		priority: BroadcastPriorityLow,
		finished: lowFinished,
	})

	// The low-priority message is served at most StarvationLimit+1 rounds
	// apart, so it must finish within transmitLimit*(StarvationLimit+1)
	// rounds.
	maxRounds := transmitLimit * (q.StarvationLimit + 1)

	highSends := 0
	lowSends := 0
	for round := 1; ; round++ {
		require.LessOrEqual(t, round, maxRounds, "low-priority broadcast starved by continuous churn")

		// Sustained cluster churn: a fresh member change every round.
		q.QueueBroadcast(&testPriorityBroadcast{
			name:     fmt.Sprintf("churn-%d", round),
			msg:      []byte(fmt.Sprintf("member change msg %03d", round)),
			priority: BroadcastPriorityHigh,
		})

		// Budget fits exactly one message per round.
		got := q.GetBroadcasts(2, 25)
		require.Len(t, got, 1, "every round should send exactly one message")
		if string(got[0]) == "low priority message" {
			lowSends++
		} else {
			highSends++
		}

		select {
		case <-lowFinished:
			require.Equal(t, transmitLimit, lowSends, "low message finished after a full fresh budget")
			require.Greater(t, highSends, 0, "high-priority churn also made progress")
			require.Greater(t, highSends, lowSends, "high-priority traffic was preferred overall")
			return
		default:
		}
	}
}

// TestTransmitLimited_ReplacementFreshBudget verifies that when a new
// version of a logical broadcast replaces an older one (even across
// priority classes), the replacement gets a full, fresh transmit budget
// instead of inheriting the partially consumed one.
func TestTransmitLimited_ReplacementFreshBudget(t *testing.T) {
	q := &TransmitLimitedQueue{RetransmitMult: 2, NumNodes: func() int { return 10 }}
	transmitLimit := retransmitLimit(q.RetransmitMult, q.NumNodes())
	require.Equal(t, 4, transmitLimit, "sanity check transmit limit")

	// v1 of the broadcast consumes half of its budget.
	finished1 := make(chan struct{}, 1)
	q.QueueBroadcast(&testPriorityBroadcast{
		name:     "node1",
		msg:      []byte("node1 v1 message....."),
		priority: BroadcastPriorityHigh,
		finished: finished1,
	})
	for i := 0; i < transmitLimit/2; i++ {
		got := q.GetBroadcasts(2, 100)
		require.Equal(t, []string{"node1 v1 message....."}, messageStrings(got))
	}
	requireNotFinished(t, finished1, "v1 finished before its budget was consumed")

	// v2 replaces v1, in a different priority class. v1 must be finished
	// via invalidation and must not pass its consumed budget to v2.
	finished2 := make(chan struct{}, 1)
	q.QueueBroadcast(&testPriorityBroadcast{
		name:     "node1",
		msg:      []byte("node1 v2 message....."),
		priority: BroadcastPriorityLow,
		finished: finished2,
	})
	requireFinished(t, finished1, "v1 should have been finished by invalidation")
	require.Equal(t, 1, q.NumQueued(), "replacement should not duplicate the queue entry")

	// v2 gets the full transmitLimit sends before Finished, not just the
	// remainder of v1's budget.
	for i := 0; i < transmitLimit-1; i++ {
		got := q.GetBroadcasts(2, 100)
		require.Equal(t, []string{"node1 v2 message....."}, messageStrings(got))
		requireNotFinished(t, finished2, "v2 inherited a partially consumed budget")
	}
	got := q.GetBroadcasts(2, 100)
	require.Equal(t, []string{"node1 v2 message....."}, messageStrings(got))
	requireFinished(t, finished2, "v2 should finish after its own full budget")
	require.Equal(t, 0, q.NumQueued())
}

// TestTransmitLimited_DeterministicOrder runs the same mixed-priority
// scenario repeatedly and requires byte-identical serving sequences,
// proving class selection does not depend on Go map iteration order.
func TestTransmitLimited_DeterministicOrder(t *testing.T) {
	scenario := func() []string {
		q := &TransmitLimitedQueue{RetransmitMult: 1, NumNodes: func() int { return 10 }}
		for i := 0; i < 3; i++ {
			q.QueueBroadcast(&testPriorityBroadcast{
				name:     fmt.Sprintf("low-%d", i),
				msg:      []byte(fmt.Sprintf("low message %d", i)),
				priority: BroadcastPriorityLow,
			})
			q.QueueBroadcast(&testPlainBroadcast{
				name: fmt.Sprintf("normal-%d", i),
				msg:  []byte(fmt.Sprintf("normal message %d", i)),
			})
			q.QueueBroadcast(&testPriorityBroadcast{
				name:     fmt.Sprintf("high-%d", i),
				msg:      []byte(fmt.Sprintf("high message %d", i)),
				priority: BroadcastPriorityHigh,
			})
		}

		var out []string
		for q.NumQueued() > 0 {
			for _, msg := range q.GetBroadcasts(3, 64) {
				out = append(out, string(msg))
			}
		}
		return out
	}

	want := scenario()
	require.NotEmpty(t, want)
	for i := 0; i < 25; i++ {
		require.Equal(t, want, scenario(), "serving order must be deterministic")
	}
}

func messageStrings(msgs [][]byte) []string {
	out := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		out = append(out, string(msg))
	}
	return out
}
