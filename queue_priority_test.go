// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package memberlist

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// prioBroadcast is a Broadcast that declares an explicit priority class.
type prioBroadcast struct {
	name string
	msg  []byte
	prio BroadcastPriority
}

func (b *prioBroadcast) Invalidates(other Broadcast) bool {
	ob, ok := other.(*prioBroadcast)
	return ok && b.name != "" && b.name == ob.name
}

func (b *prioBroadcast) Priority() BroadcastPriority { return b.prio }
func (b *prioBroadcast) Message() []byte             { return b.msg }
func (b *prioBroadcast) Finished()                   {}

// drainUserFn returns a userBroadcastFunc that serves fixed-size application
// messages from an ever-replenishing pool, recording how many it handed out.
func drainUserFn(t *testing.T, payload []byte, served *int) userBroadcastFunc {
	t.Helper()
	return func(overhead, limit int) [][]byte {
		// Hand out as many distinct messages as fit the byte budget. The
		// scheduler accounts each application message as overhead+len,
		// matching the capacity math here.
		capacity := limit / (overhead + len(payload))
		if capacity <= 0 {
			return nil
		}
		out := make([][]byte, 0, capacity)
		for i := 0; i < capacity; i++ {
			*served++
			out = append(out, append([]byte(nil), payload...))
		}
		return out
	}
}

// TestGetBroadcasts_MembershipBurstPriority verifies that a burst of member
// change messages is fully served before application traffic, even when the
// application has a large backlog.
func TestGetBroadcasts_MembershipBurstPriority(t *testing.T) {
	q := &TransmitLimitedQueue{RetransmitMult: 1, NumNodes: func() int { return 10 }}

	const msgLen = 8
	for i := 0; i < 5; i++ {
		q.QueueBroadcast(&memberlistBroadcast{
			node: "member" + string(rune('a'+i)),
			msg:  []byte("member00"),
		})
	}

	userServed := 0
	userFn := drainUserFn(t, []byte("user0000"), &userServed)

	// Big enough to hold all five member messages and one application
	// message (overhead 2, payloads 8 bytes).
	got := q.getBroadcasts(2, 6*(2+msgLen)+2, userFn)

	require.Len(t, got, 6)
	for i := 0; i < 5; i++ {
		require.Equal(t, "member00", string(got[i]))
	}
	require.Equal(t, "user0000", string(got[5]))
	require.Equal(t, 1, userServed)
}

// TestGetBroadcasts_AntiStarvation_PersistentChurn models a cluster that
// never stops churning: member change messages are continuously re-enqueued
// while application traffic also keeps arriving. Both classes must make
// progress; the application messages must not be locked out forever.
func TestGetBroadcasts_AntiStarvation_PersistentChurn(t *testing.T) {
	q := &TransmitLimitedQueue{
		RetransmitMult:  100, // keep member messages alive across calls
		MaxClassService: 3,
		NumNodes:        func() int { return 10 },
	}

	const (
		msgLen   = 8
		overhead = 2
	)

	// Seed a persistent churn pool: every call consumes some member
	// messages, and we replenish them afterwards to simulate ongoing churn.
	churn := 0
	for i := 0; i < 20; i++ {
		churn++
		q.QueueBroadcast(&memberlistBroadcast{
			node: "churn" + string(rune('a'+churn)),
			msg:  []byte("member00"),
		})
	}

	userServed := 0
	userFn := drainUserFn(t, []byte("user0000"), &userServed)

	memberSent := 0
	appSent := 0

	// Each call can carry many messages, but MaxClassService forces a
	// handoff after every three member messages; run enough rounds that
	// either class would starve the other if the policy were broken.
	limit := 12*(overhead+msgLen) + overhead
	for round := 0; round < 30; round++ {
		got := q.getBroadcasts(overhead, limit, userFn)
		for _, msg := range got {
			if string(msg) == "member00" {
				memberSent++
			} else {
				appSent++
			}
		}

		// Keep a steady churn backlog topped up to a small target: messages
		// that were merely retransmitted remain in the queue, so only the
		// ones actually removed need replacing. We replenish by distinct
		// node names so new messages enter at tier 0, just like a cluster
		// that keeps changing membership.
		for q.NumQueued() < 6 {
			churn++
			q.QueueBroadcast(&memberlistBroadcast{
				node: "churn" + strconv.Itoa(churn),
				msg:  []byte("member00"),
			})
		}
	}

	require.Greater(t, memberSent, 0, "membership messages must keep flowing during churn")
	require.Greater(t, appSent, 0, "application messages must not starve during churn")

	// With MaxClassService=3 and a 12-message packet, every round contains
	// several application messages; assert a meaningful fraction rather
	// than a single lucky delivery.
	require.Greater(t, appSent, memberSent/4, "application traffic gets regular turns")
}

// TestGetBroadcasts_AntiStarvation_UserFlood reverses the pressure: the user
// message source floods continuously while member changes also occur. Member
// changes must cut the line and be delivered first.
func TestGetBroadcasts_AntiStarvation_UserFlood(t *testing.T) {
	q := &TransmitLimitedQueue{
		RetransmitMult:  100,
		MaxClassService: 4,
		NumNodes:        func() int { return 10 },
	}

	const (
		msgLen   = 8
		overhead = 2
	)

	userServed := 0
	userFn := drainUserFn(t, []byte("user0000"), &userServed)

	// No member messages yet: the user flood fills packets normally.
	got := q.getBroadcasts(overhead, 4*(overhead+msgLen)+overhead, userFn)
	require.Len(t, got, 4)
	require.Equal(t, 4, userServed)

	// A membership change arrives mid-flood. Every subsequent call must
	// lead with the member message regardless of user backlog.
	for i := 0; i < 3; i++ {
		q.QueueBroadcast(&memberlistBroadcast{
			node: "alive-node",
			msg:  []byte("member00"),
		})
		got = q.getBroadcasts(overhead, 2*(overhead+msgLen)+overhead, userFn)
		require.Equal(t, "member00", string(got[0]), "membership change jumps the queue")
	}
}

// TestQueueBroadcast_ReplacementDoesNotInheritBudget verifies that when a
// logical broadcast is replaced by a newer version, the replacement starts
// with a fresh transmit budget at tier 0 instead of inheriting the consumed
// budget of the invalidated message.
func TestQueueBroadcast_ReplacementDoesNotInheritBudget(t *testing.T) {
	t.Run("named", func(t *testing.T) {
		q := &TransmitLimitedQueue{RetransmitMult: 1, NumNodes: func() int { return 10 }}
		require.Equal(t, 2, retransmitLimit(q.RetransmitMult, q.NumNodes()))

		// memberlistBroadcast is a NamedBroadcast and is what production uses.
		first := &memberlistBroadcast{node: "node1", msg: []byte("v1"), notify: make(chan struct{}, 1)}
		q.QueueBroadcast(first)

		// Consume the entire transmit budget of v1 (limit 2 => two sends).
		q.GetBroadcasts(1, 100)
		q.GetBroadcasts(1, 100)
		select {
		case <-first.notify:
		default:
			t.Fatalf("v1 should be Finished after exhausting its budget")
		}

		// Queue a replacement for the same logical node. With a buggy shared
		// budget it would be born expired; it must instead be delivered.
		second := &memberlistBroadcast{node: "node1", msg: []byte("v2"), notify: make(chan struct{}, 1)}
		q.QueueBroadcast(second)

		got := q.GetBroadcasts(1, 100)
		require.Len(t, got, 1)
		require.Equal(t, "v2", string(got[0]))

		// Fresh budget: it survives one send and only Finishes on the second.
		select {
		case <-second.notify:
			t.Fatalf("replacement must not inherit the exhausted budget")
		default:
		}
		got = q.GetBroadcasts(1, 100)
		require.Len(t, got, 1)
		require.Equal(t, "v2", string(got[0]))
		select {
		case <-second.notify:
		default:
			t.Fatalf("replacement should Finish after its own full budget")
		}
	})

	t.Run("invalidates", func(t *testing.T) {
		// Non-named broadcasts invalidate via Invalidates(); the
		// replacement must likewise be inserted at tier 0.
		q := &TransmitLimitedQueue{RetransmitMult: 1, NumNodes: func() int { return 10 }}

		first := &prioBroadcast{name: "k", msg: []byte("old")}
		q.QueueBroadcast(first)
		q.GetBroadcasts(1, 100)
		q.GetBroadcasts(1, 100)

		replacement := &prioBroadcast{name: "k", msg: []byte("new")}
		q.QueueBroadcast(replacement)

		view := q.orderedView()
		require.Len(t, view, 1)
		require.Equal(t, 0, view[0].transmits, "replacement resets transmit budget")
		require.Equal(t, "new", string(view[0].b.Message()))
	})
}

// TestGetBroadcasts_DeterministicSelection runs the same setup many times to
// ensure class/message selection never depends on Go map iteration order.
func TestGetBroadcasts_DeterministicSelection(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		q := &TransmitLimitedQueue{
			RetransmitMult:  100,
			MaxClassService: 2,
			NumNodes:        func() int { return 10 },
		}
		for i := 0; i < 4; i++ {
			q.QueueBroadcast(&memberlistBroadcast{
				node: "n" + string(rune('a'+i)),
				msg:  []byte("MMMMMMMM"),
			})
		}
		userFn := func(overhead, limit int) [][]byte {
			return [][]byte{[]byte("UUUUUUUU")}
		}

		got := q.getBroadcasts(1, 5*(1+8)+1, userFn)
		require.Len(t, got, 5)
		// First MaxClassService slots are membership messages, then the
		// application class receives its guaranteed turn, then membership
		// resumes and serves the remaining two. This order must be
		// identical on every iteration.
		require.Equal(t, "MMMMMMMM", string(got[0]))
		require.Equal(t, "MMMMMMMM", string(got[1]))
		require.Equal(t, "UUUUUUUU", string(got[2]))
		require.Equal(t, "MMMMMMMM", string(got[3]))
		require.Equal(t, "MMMMMMMM", string(got[4]))
	}
}

// TestGetBroadcasts_PriorityClasses enforces that application-priority
// enqueued broadcasts sort behind memberlist-priority ones while retransmit
// ordering inside a class is unchanged.
func TestGetBroadcasts_PriorityClasses(t *testing.T) {
	q := &TransmitLimitedQueue{RetransmitMult: 100, NumNodes: func() int { return 10 }}

	q.QueueBroadcast(&prioBroadcast{name: "a", msg: []byte("APP"), prio: PriorityApplication})
	q.QueueBroadcast(&prioBroadcast{name: "m", msg: []byte("MEM"), prio: PriorityMemberlist})

	// Drain order is driven by the priority scheduler, not by the btree
	// insertion position.
	view := q.orderedView()
	require.Len(t, view, 2)

	got := q.GetBroadcasts(0, 16)
	require.Equal(t, []string{"MEM", "APP"}, []string{string(got[0]), string(got[1])})

	// Both classes are represented in the queue bookkeeping.
	require.Equal(t, 1, q.classLens[PriorityMemberlist])
	require.Equal(t, 1, q.classLens[PriorityApplication])
}

// TestGetBroadcasts_TreeBackedAntiStarvation verifies that application
// broadcasts enqueued directly as PrioritizedBroadcast items receive the
// same anti-starvation interleaving as delegate-fed ones.
func TestGetBroadcasts_TreeBackedAntiStarvation(t *testing.T) {
	q := &TransmitLimitedQueue{
		RetransmitMult:  100,
		MaxClassService: 2,
		NumNodes:        func() int { return 10 },
	}

	for i := 0; i < 4; i++ {
		q.QueueBroadcast(&prioBroadcast{
			name: "m" + strconv.Itoa(i),
			msg:  []byte("MMMMMMMM"),
			prio: PriorityMemberlist,
		})
	}
	for i := 0; i < 4; i++ {
		q.QueueBroadcast(&prioBroadcast{
			name: "a" + strconv.Itoa(i),
			msg:  []byte("AAAAAAAA"),
			prio: PriorityApplication,
		})
	}

	// No delegate callback: only tree-backed items are available.
	got := q.GetBroadcasts(1, 8*(1+8)+1)

	var member, app int
	for _, msg := range got {
		switch string(msg) {
		case "MMMMMMMM":
			member++
		case "AAAAAAAA":
			app++
		}
	}
	require.Equal(t, 4, member)
	require.Equal(t, 4, app)
	// Strict alternating batches of two member then two app messages.
	require.Equal(t, []string{
		"MMMMMMMM", "MMMMMMMM",
		"AAAAAAAA", "AAAAAAAA",
		"MMMMMMMM", "MMMMMMMM",
		"AAAAAAAA", "AAAAAAAA",
	}, func() []string {
		out := make([]string, len(got))
		for i, m := range got {
			out[i] = string(m)
		}
		return out
	}())
}

// TestGetBroadcasts_UserFnNotHeldUnderLock confirms the application callback
// is never invoked while the queue mutex is held, so a blocking delegate
// cannot deadlock queue mutation.
func TestGetBroadcasts_UserFnNotHeldUnderLock(t *testing.T) {
	q := &TransmitLimitedQueue{RetransmitMult: 100, NumNodes: func() int { return 10 }}

	called := make(chan struct{})
	proceed := make(chan struct{})
	userFn := func(overhead, limit int) [][]byte {
		// Signal that the callback runs without the queue lock by allowing
		// an external lock acquisition to proceed.
		close(called)
		<-proceed
		return [][]byte{[]byte("UUUUUUUU")}
	}

	done := make(chan [][]byte, 1)
	go func() {
		done <- q.getBroadcasts(1, 32, userFn)
	}()

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatalf("user callback was not invoked")
	}

	// While the callback is parked, the queue lock must be acquirable.
	locked := make(chan struct{})
	go func() {
		q.mu.Lock()
		q.mu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		t.Fatalf("queue lock was held while user callback ran")
	}

	close(proceed)
	got := <-done
	<-called
	require.Equal(t, [][]byte{[]byte("UUUUUUUU")}, got)
}
