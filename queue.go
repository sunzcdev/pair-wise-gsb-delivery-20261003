// Copyright IBM Corp. 2013, 2026
// SPDX-License-Identifier: MPL-2.0

package memberlist

import (
	"math"
	"sync"

	"github.com/google/btree"
)

// TransmitLimitedQueue is used to queue messages to broadcast to
// the cluster (via gossip) but limits the number of transmits per
// message. It also prioritizes messages with lower transmit counts
// (hence newer messages).
//
// Messages are additionally organized into a fixed set of priority classes
// (see BroadcastPriority). Higher priority classes (membership change
// messages) are served first so a burst of them is not delayed by
// application traffic. Within a single GetBroadcasts call a class may only
// spend MaxClassService consecutive sends before a lower priority class is
// given a turn, which prevents sustained membership churn from starving
// application broadcasts forever. Class selection always walks the fixed
// class order rather than a map, so the result is deterministic.
type TransmitLimitedQueue struct {
	// NumNodes returns the number of nodes in the cluster. This is
	// used to determine the retransmit count, which is calculated
	// based on the log of this.
	NumNodes func() int

	// RetransmitMult is the multiplier used to determine the maximum
	// number of retransmissions attempted.
	RetransmitMult int

	// MaxClassService bounds how many broadcasts a single priority class
	// may contribute consecutively within one GetBroadcasts call before a
	// lower priority, non-empty class is given a turn. This prevents a
	// constant stream of high priority membership churn messages from
	// starving application broadcasts forever. A value <= 0 falls back to
	// defaultMaxClassService.
	MaxClassService int

	mu    sync.Mutex
	tq    *btree.BTree // stores *limitedBroadcast as btree.Item
	tm    map[string]*limitedBroadcast
	idGen int64

	// classLens counts the queued items belonging to each priority class.
	// Classes are addressed by their fixed BroadcastPriority index (never
	// by iterating a map), so selection order is deterministic.
	classLens [numPriorities]int
}

// BroadcastPriority selects one of a fixed set of priority classes that a
// queue may drain from. Lower numeric values are served first.
type BroadcastPriority int

const (
	// PriorityMemberlist is the high priority class used for membership
	// change messages (alive/dead/suspect). A burst of these is served
	// ahead of application traffic.
	PriorityMemberlist BroadcastPriority = 0

	// PriorityApplication is the lower priority class for application
	// generated broadcasts. It still makes progress during sustained
	// membership churn thanks to the anti-starvation policy.
	PriorityApplication BroadcastPriority = 1

	// numPriorities is the number of classes. It must stay in sync with
	// the constants above and bounds every fixed-size class index.
	numPriorities = 2

	// defaultMaxClassService is used when MaxClassService is unset.
	defaultMaxClassService = 8
)

// PrioritizedBroadcast is an optional extension of Broadcast that lets a
// message declare which priority class it belongs to. Broadcasts that do
// not implement it are assigned PriorityMemberlist, preserving the
// historical behavior where everything enqueued directly outranks the
// application traffic pulled from the Delegate.
type PrioritizedBroadcast interface {
	Broadcast
	Priority() BroadcastPriority
}

type limitedBroadcast struct {
	prio      BroadcastPriority // btree-key[0]: priority class, lower is served first
	transmits int               // btree-key[1]: number of transmissions attempted
	msgLen    int64             // btree-key[2]: copied from len(b.Message())
	id        int64             // btree-key[3]: unique incrementing id stamped at submission time
	b         Broadcast

	name string // set if Broadcast is a NamedBroadcast
}

// Less tests whether the current item is less than the given argument.
//
// This must provide a strict weak ordering.
// If !a.Less(b) && !b.Less(a), we treat this to mean a == b (i.e. we can only
// hold one of either a or b in the tree).
//
// default ordering is
// - [prio=0, ..., prio=inf]
// - [transmits=0, ..., transmits=inf]
// - [transmits=0:len=999, ..., transmits=0:len=2, ...]
// - [transmits=0:len=999,id=999, ..., transmits=0:len=999:id=1, ...]
func (b *limitedBroadcast) Less(than btree.Item) bool {
	o := than.(*limitedBroadcast)
	if b.prio < o.prio {
		return true
	} else if b.prio > o.prio {
		return false
	}
	if b.transmits < o.transmits {
		return true
	} else if b.transmits > o.transmits {
		return false
	}
	if b.msgLen > o.msgLen {
		return true
	} else if b.msgLen < o.msgLen {
		return false
	}
	return b.id > o.id
}

// walkReadOnlyLocked calls f for each item in the queue traversing it in
// natural order (by Less) when reverse=false and the opposite when true. You
// must hold the mutex.
//
// This method panics if you attempt to mutate the item during traversal.  The
// underlying btree should also not be mutated during traversal.
func (q *TransmitLimitedQueue) walkReadOnlyLocked(reverse bool, f func(*limitedBroadcast) bool) {
	if q.lenLocked() == 0 {
		return
	}

	iter := func(item btree.Item) bool {
		cur := item.(*limitedBroadcast)

		prevTransmits := cur.transmits
		prevMsgLen := cur.msgLen
		prevID := cur.id

		keepGoing := f(cur)

		if prevTransmits != cur.transmits || prevMsgLen != cur.msgLen || prevID != cur.id {
			panic("edited queue while walking read only")
		}

		return keepGoing
	}

	if reverse {
		q.tq.Descend(iter) // end with transmit 0
	} else {
		q.tq.Ascend(iter) // start with transmit 0
	}
}

// Broadcast is something that can be broadcasted via gossip to
// the memberlist cluster.
type Broadcast interface {
	// Invalidates checks if enqueuing the current broadcast
	// invalidates a previous broadcast
	Invalidates(b Broadcast) bool

	// Returns a byte form of the message
	Message() []byte

	// Finished is invoked when the message will no longer
	// be broadcast, either due to invalidation or to the
	// transmit limit being reached
	Finished()
}

// NamedBroadcast is an optional extension of the Broadcast interface that
// gives each message a unique string name, and that is used to optimize
//
// You shoud ensure that Invalidates() checks the same uniqueness as the
// example below:
//
//	func (b *foo) Invalidates(other Broadcast) bool {
//		nb, ok := other.(NamedBroadcast)
//		if !ok {
//			return false
//		}
//		return b.Name() == nb.Name()
//	}
//
// Invalidates() isn't currently used for NamedBroadcasts, but that may change
// in the future.
type NamedBroadcast interface {
	Broadcast
	// The unique identity of this broadcast message.
	Name() string
}

// UniqueBroadcast is an optional interface that indicates that each message is
// intrinsically unique and there is no need to scan the broadcast queue for
// duplicates.
//
// You should ensure that Invalidates() always returns false if implementing
// this interface. Invalidates() isn't currently used for UniqueBroadcasts, but
// that may change in the future.
type UniqueBroadcast interface {
	Broadcast
	// UniqueBroadcast is just a marker method for this interface.
	UniqueBroadcast()
}

// QueueBroadcast is used to enqueue a broadcast
func (q *TransmitLimitedQueue) QueueBroadcast(b Broadcast) {
	q.queueBroadcast(b, 0)
}

// lazyInit initializes internal data structures the first time they are
// needed.  You must already hold the mutex.
func (q *TransmitLimitedQueue) lazyInit() {
	if q.tq == nil {
		q.tq = btree.New(32)
	}
	if q.tm == nil {
		q.tm = make(map[string]*limitedBroadcast)
	}
}

// queueBroadcast is like QueueBroadcast but you can use a nonzero value for
// the initial transmit tier assigned to the message. This is meant to be used
// for unit testing.
func (q *TransmitLimitedQueue) queueBroadcast(b Broadcast, initialTransmits int) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.lazyInit()

	if q.idGen == math.MaxInt64 {
		// it's super duper unlikely to wrap around within the retransmit limit
		q.idGen = 1
	} else {
		q.idGen++
	}
	id := q.idGen

	prio := priorityOf(b)

	lb := &limitedBroadcast{
		prio:      prio,
		transmits: initialTransmits,
		msgLen:    int64(len(b.Message())),
		id:        id,
		b:         b,
	}
	unique := false
	if nb, ok := b.(NamedBroadcast); ok {
		lb.name = nb.Name()
	} else if _, ok := b.(UniqueBroadcast); ok {
		unique = true
	}

	// Check if this message invalidates another.
	if lb.name != "" {
		if old, ok := q.tm[lb.name]; ok {
			old.b.Finished()
			q.deleteItem(old)
		}
	} else if !unique {
		// Slow path, hopefully nothing hot hits this.
		var remove []*limitedBroadcast
		q.tq.Ascend(func(item btree.Item) bool {
			cur := item.(*limitedBroadcast)

			// Special Broadcasts can only invalidate each other.
			switch cur.b.(type) {
			case NamedBroadcast:
				// noop
			case UniqueBroadcast:
				// noop
			default:
				if b.Invalidates(cur.b) {
					cur.b.Finished()
					remove = append(remove, cur)
				}
			}
			return true
		})
		for _, cur := range remove {
			q.deleteItem(cur)
		}
	}

	// Append to the relevant queue.
	q.addItem(lb)
}

// deleteItem removes the given item from the overall datastructure. You
// must already hold the mutex.
func (q *TransmitLimitedQueue) deleteItem(cur *limitedBroadcast) {
	_ = q.tq.Delete(cur)
	q.classLens[cur.prio]--
	if cur.name != "" {
		delete(q.tm, cur.name)
	}

	if q.tq.Len() == 0 {
		// At idle there's no reason to let the id generator keep going
		// indefinitely.
		q.idGen = 0
	}
}

// addItem adds the given item into the overall datastructure. You must already
// hold the mutex.
func (q *TransmitLimitedQueue) addItem(cur *limitedBroadcast) {
	_ = q.tq.ReplaceOrInsert(cur)
	q.classLens[cur.prio]++
	if cur.name != "" {
		q.tm[cur.name] = cur
	}
}

// priorityOf reports the class a broadcast belongs to. Broadcasts without
// PrioritizedBroadcast default to the high priority memberlist class.
func priorityOf(b Broadcast) BroadcastPriority {
	if pb, ok := b.(PrioritizedBroadcast); ok {
		p := pb.Priority()
		if p < 0 || p >= numPriorities {
			return PriorityMemberlist
		}
		return p
	}
	return PriorityMemberlist
}

// maxClassService returns the configured consecutive-service budget for a
// single priority class, applying the default when unset.
func (q *TransmitLimitedQueue) maxClassService() int {
	if q.MaxClassService <= 0 {
		return defaultMaxClassService
	}
	return q.MaxClassService
}

// getTransmitRangeLocked returns a pair of min/max transmit values for the
// items of the given priority class. Both values represent actual transmit
// values present in the tree. You must already hold the mutex.
func (q *TransmitLimitedQueue) getTransmitRangeLocked(prio BroadcastPriority) (minTransmit, maxTransmit int) {
	if q.classLens[prio] == 0 {
		return 0, 0
	}

	// All items of this class sort into [lowerBound, upperBound) by the
	// limitedBroadcast ordering.
	lowerBound := &limitedBroadcast{prio: prio, transmits: -1, msgLen: math.MinInt64, id: math.MaxInt64}
	upperBound := &limitedBroadcast{prio: prio + 1, transmits: -1, msgLen: math.MinInt64, id: math.MaxInt64}

	var minItem, maxItem btree.Item
	q.tq.AscendRange(lowerBound, upperBound, func(item btree.Item) bool {
		minItem = item
		return false
	})
	q.tq.DescendRange(upperBound, lowerBound, func(item btree.Item) bool {
		maxItem = item
		return false
	})

	if minItem == nil || maxItem == nil {
		return 0, 0
	}

	min := minItem.(*limitedBroadcast).transmits
	max := maxItem.(*limitedBroadcast).transmits

	return min, max
}

// userBroadcastFunc fetches application broadcasts to serve from the
// PriorityApplication class. It mirrors Delegate.GetBroadcasts: callers ask
// for up to limit bytes with per-message overhead already included, and the
// implementation drains and returns whatever pending messages fit. Returning
// nil means the application class currently has nothing to send.
type userBroadcastFunc func(overhead, limit int) [][]byte

// GetBroadcasts is used to get a number of broadcasts, up to a byte limit
// and applying a per-message overhead as provided.
func (q *TransmitLimitedQueue) GetBroadcasts(overhead, limit int) [][]byte {
	return q.getBroadcasts(overhead, limit, nil)
}

// getBroadcasts is the shared implementation of GetBroadcasts. When userFn
// is non-nil it additionally drains the PriorityApplication class.
//
// Classes are selected from a fixed, ascending priority order (never by map
// iteration). The memberlist class always goes first, so membership bursts
// are handled promptly. Within a single call each class may contribute up to
// MaxClassService consecutive messages; once it spends that budget the next
// non-empty class gets a turn, so sustained membership churn can never
// permanently starve application traffic.
func (q *TransmitLimitedQueue) getBroadcasts(overhead, limit int, userFn userBroadcastFunc) [][]byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.lazyInit()

	transmitLimit := retransmitLimit(q.RetransmitMult, q.NumNodes())
	maxService := q.maxClassService()

	var (
		bytesUsed int
		toSend    [][]byte
		reinsert  []*limitedBroadcast

		// appTried records whether userFn has already been consulted this
		// call. The delegate is pull-based and drains itself, so a single
		// empty answer means the application class cannot add more now.
		appTried bool

		// tierCursor[c] is the lowest transmit tier of class c still worth
		// scanning during this call. It only moves upward, mirroring the
		// original tier walk: an item bumped to the next tier is held in
		// reinsert until the end and must not be selected again here.
		tierCursor [numPriorities]int

		// skipClass temporarily suppresses immediate reselection of the
		// memberlist class after a batch, forcing an application turn.
		skipClass BroadcastPriority = -1
	)

	// classReady reports whether class c can contribute at least one more
	// message given the current packet state. A tree-backed class is ready
	// only when it holds an item that actually fits, so oversized items do
	// not wedge selection in a busy loop.
	classReady := func(c BroadcastPriority, free int64) bool {
		if free <= 0 {
			return false
		}
		if q.classLens[c] > 0 && q.hasFitting(c, free) {
			return true
		}
		// The application class may additionally be fed by the pull-based
		// delegate callback; it is consulted at most once per call.
		return c == PriorityApplication && userFn != nil && !appTried
	}

	// Selection rescans this fixed class order on every turn, so the active
	// class is always chosen deterministically rather than from a map.
	for {
		free := int64(limit - bytesUsed - overhead)
		if free <= 0 {
			break
		}

		var active BroadcastPriority = -1
		for c := BroadcastPriority(0); c < numPriorities; c++ {
			// A class that already got a turn this outer iteration is not
			// immediately reselected; lower classes are given a chance.
			if classReady(c, free) && c != skipClass {
				active = c
				break
			}
		}
		if active < 0 && skipClass >= 0 {
			// Only the skipped class remains: fall back to it and clear
			// the skip so a later no-progress turn terminates cleanly
			// instead of cycling forever.
			if classReady(skipClass, free) {
				active = skipClass
				skipClass = -1
			}
		}
		if active < 0 {
			break
		}

		// Serve up to maxService consecutive messages from the chosen
		// class, then rescan from the highest priority class. A burst of
		// higher priority work therefore jumps the queue, while a full (or
		// fitting-exhausted) batch yields the floor to lower classes.
		progress := false
		for served := 0; served < maxService; served++ {
			free = int64(limit - bytesUsed - overhead)
			if free <= 0 {
				break
			}

			// The application class may have tree-backed items and/or a
			// pull-based delegate. Prefer a tree-backed item; if none fits
			// (or the class is empty) consult the delegate. The delegate
			// may block, so it is never invoked while the mutex is held.
			if active == PriorityApplication && userFn != nil {
				var treeItem *limitedBroadcast
				if q.classLens[active] > 0 {
					treeItem, tierCursor[active] = q.pickFitting(active, tierCursor[active], free)
				}
				if treeItem != nil {
					progress = true
					msg := treeItem.b.Message()
					bytesUsed += overhead + len(msg)
					toSend = append(toSend, msg)

					q.deleteItem(treeItem)
					if treeItem.transmits+1 >= transmitLimit {
						treeItem.b.Finished()
					} else {
						treeItem.transmits++
						reinsert = append(reinsert, treeItem)
					}
					continue
				}

				q.mu.Unlock()
				msgs := userFn(overhead, int(free))
				q.mu.Lock()
				appTried = true
				for _, msg := range msgs {
					bytesUsed += overhead + len(msg)
					toSend = append(toSend, msg)
				}
				if len(msgs) > 0 {
					progress = true
				}
				break
			}

			var keep *limitedBroadcast
			keep, tierCursor[active] = q.pickFitting(active, tierCursor[active], free)
			if keep == nil {
				break
			}
			progress = true

			msg := keep.b.Message()
			bytesUsed += overhead + len(msg)
			toSend = append(toSend, msg)

			q.deleteItem(keep)
			if keep.transmits+1 >= transmitLimit {
				keep.b.Finished()
			} else {
				// Bump the item down a transmit tier, but delay
				// reinsertion until selection finishes; otherwise we could
				// re-select the same message while ascending tiers.
				keep.transmits++
				reinsert = append(reinsert, keep)
			}
		}

		// After a productive batch from any class, skip reselecting that
		// same class immediately so the other non-empty class receives its
		// guaranteed turn. When the skipped class is memberlist, the
		// pull-based delegate is also re-armed: it may have accumulated
		// messages since it was last consulted.
		if progress {
			skipClass = active
			if active == PriorityMemberlist && userFn != nil {
				appTried = false
			}
		} else {
			skipClass = -1
		}

		if !progress {
			// The active class cannot contribute right now.
			skipClass = -1
			anyReady := false
			for c := BroadcastPriority(0); c < numPriorities; c++ {
				if classReady(c, int64(limit-bytesUsed-overhead)) {
					anyReady = true
					break
				}
			}
			if !anyReady {
				break
			}
		}
	}

	for _, cur := range reinsert {
		q.addItem(cur)
	}

	return toSend
}

// hasFitting reports whether class prio holds at least one message that
// fits within free bytes. You must hold the mutex.
func (q *TransmitLimitedQueue) hasFitting(prio BroadcastPriority, free int64) bool {
	item, _ := q.pickFitting(prio, 0, free)
	return item != nil
}

// pickFitting finds the next broadcast of the given priority class that
// fits within free bytes. It walks transmit tiers from freshest to oldest and
// prefers larger messages within a tier. startTier is the first tier to
// examine. It returns the picked item (or nil) and the tier at which the
// next scan for this call should resume. You must hold the mutex.
func (q *TransmitLimitedQueue) pickFitting(prio BroadcastPriority, startTier int, free int64) (*limitedBroadcast, int) {
	minTr, maxTr := q.getTransmitRangeLocked(prio)
	transmits := startTier
	if transmits < minTr {
		transmits = minTr
	}
	for ; transmits <= maxTr; transmits++ {
		greaterOrEqual := &limitedBroadcast{
			prio:      prio,
			transmits: transmits,
			msgLen:    free,
			id:        math.MaxInt64,
		}
		lessThan := &limitedBroadcast{
			prio:      prio,
			transmits: transmits + 1,
			msgLen:    math.MaxInt64,
			id:        math.MaxInt64,
		}
		var found *limitedBroadcast
		q.tq.AscendRange(greaterOrEqual, lessThan, func(item btree.Item) bool {
			cur := item.(*limitedBroadcast)
			if int64(len(cur.b.Message())) > free {
				// If this happens it's a bug in the datastructure or
				// surrounding use doing something like having len(Message())
				// change over time. There's enough going on here that it's
				// probably sane to just skip it and move on for now.
				return true
			}
			found = cur
			return false
		})
		if found != nil {
			return found, transmits
		}
	}
	return nil, transmits
}

func (q *TransmitLimitedQueue) NumQueued() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.lenLocked()
}

// lenLocked returns the length of the overall queue datastructure. You must
// hold the mutex.
func (q *TransmitLimitedQueue) lenLocked() int {
	if q.tq == nil {
		return 0
	}
	return q.tq.Len()
}

// Reset clears all the queued messages. Should only be used for tests.
func (q *TransmitLimitedQueue) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.walkReadOnlyLocked(false, func(cur *limitedBroadcast) bool {
		cur.b.Finished()
		return true
	})

	q.tq = nil
	q.tm = nil
	q.classLens = [numPriorities]int{}
	q.idGen = 0
}

// Prune will retain the maxRetain latest messages, and the rest
// will be discarded. This can be used to prevent unbounded queue sizes
func (q *TransmitLimitedQueue) Prune(maxRetain int) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Do nothing if queue size is less than the limit
	for q.tq.Len() > maxRetain {
		item := q.tq.Max()
		if item == nil {
			break
		}
		cur := item.(*limitedBroadcast)
		cur.b.Finished()
		q.deleteItem(cur)
	}
}
