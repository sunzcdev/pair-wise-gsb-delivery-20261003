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
type TransmitLimitedQueue struct {
	// NumNodes returns the number of nodes in the cluster. This is
	// used to determine the retransmit count, which is calculated
	// based on the log of this.
	NumNodes func() int

	// RetransmitMult is the multiplier used to determine the maximum
	// number of retransmissions attempted.
	RetransmitMult int

	// StarvationLimit bounds how many consecutive GetBroadcasts calls may
	// skip a non-empty lower-priority class in favor of higher-priority
	// traffic before the lower-priority class is served first. Values less
	// than 1 select DefaultStarvationLimit.
	StarvationLimit int

	mu      sync.Mutex
	classes [numBroadcastPriorityClasses]priorityClassQueue
	tm      map[string]*limitedBroadcast
	idGen   int64
	count   int
}

// BroadcastPriority identifies one of a small, fixed set of priority
// classes. Lower values are served first by GetBroadcasts, subject to the
// anti-starvation policy configured by StarvationLimit.
type BroadcastPriority int

const (
	// BroadcastPriorityHigh is intended for cluster membership changes
	// (alive/suspect/dead style messages) that should preempt ordinary
	// traffic.
	BroadcastPriorityHigh BroadcastPriority = iota
	// BroadcastPriorityNormal is the default class for broadcasts that do
	// not implement PriorityBroadcast.
	BroadcastPriorityNormal
	// BroadcastPriorityLow is intended for bulk application traffic that
	// should yield to everything else.
	BroadcastPriorityLow

	// numBroadcastPriorityClasses is the fixed number of priority classes.
	// Values outside [0, numBroadcastPriorityClasses) are clamped.
	numBroadcastPriorityClasses
)

// DefaultBroadcastPriority is the class assigned to broadcasts that do not
// implement PriorityBroadcast.
const DefaultBroadcastPriority = BroadcastPriorityNormal

// DefaultStarvationLimit is used when StarvationLimit is less than 1. A
// lower-priority class that has been bypassed this many consecutive times
// is served ahead of higher-priority classes on the next call.
const DefaultStarvationLimit = 2

// priorityClassQueue holds the per-class ordered queue plus the
// anti-starvation bookkeeping for that class.
type priorityClassQueue struct {
	tq *btree.BTree // stores *limitedBroadcast as btree.Item

	// bypassed counts consecutive GetBroadcasts calls in which this class
	// had pending messages but higher-priority traffic was served instead.
	bypassed int
}

// len returns the number of queued messages in this class.
func (c *priorityClassQueue) len() int {
	if c.tq == nil {
		return 0
	}
	return c.tq.Len()
}

// transmitRange returns a pair of min/max values for transmit values
// represented by the current class contents. Both values represent actual
// transmit values on the interval [0, len).
func (c *priorityClassQueue) transmitRange() (minTransmit, maxTransmit int) {
	if c.len() == 0 {
		return 0, 0
	}
	minItem, maxItem := c.tq.Min(), c.tq.Max()
	if minItem == nil || maxItem == nil {
		return 0, 0
	}

	min := minItem.(*limitedBroadcast).transmits
	max := maxItem.(*limitedBroadcast).transmits

	return min, max
}

type limitedBroadcast struct {
	transmits int   // btree-key[0]: Number of transmissions attempted.
	msgLen    int64 // btree-key[1]: copied from len(b.Message())
	id        int64 // btree-key[2]: unique incrementing id stamped at submission time
	b         Broadcast

	priority BroadcastPriority // the class this broadcast is queued in
	name     string            // set if Broadcast is a NamedBroadcast
}

// Less tests whether the current item is less than the given argument.
//
// This must provide a strict weak ordering.
// If !a.Less(b) && !b.Less(a), we treat this to mean a == b (i.e. we can only
// hold one of either a or b in the tree).
//
// default ordering is
// - [transmits=0, ..., transmits=inf]
// - [transmits=0:len=999, ..., transmits=0:len=2, ...]
// - [transmits=0:len=999,id=999, ..., transmits=0:len=999:id=1, ...]
func (b *limitedBroadcast) Less(than btree.Item) bool {
	o := than.(*limitedBroadcast)
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
// Classes are visited in deterministic priority order (highest first); the
// traversal never depends on map iteration order.
//
// This method panics if you attempt to mutate the item during traversal.  The
// underlying btree should also not be mutated during traversal.
func (q *TransmitLimitedQueue) walkReadOnlyLocked(reverse bool, f func(*limitedBroadcast) bool) {
	if q.count == 0 {
		return
	}

	for c := range q.classes {
		class := &q.classes[c]
		if class.len() == 0 {
			continue
		}

		keepGoing := true
		iter := func(item btree.Item) bool {
			cur := item.(*limitedBroadcast)

			prevTransmits := cur.transmits
			prevMsgLen := cur.msgLen
			prevID := cur.id

			keepGoing = f(cur)

			if prevTransmits != cur.transmits || prevMsgLen != cur.msgLen || prevID != cur.id {
				panic("edited queue while walking read only")
			}

			return keepGoing
		}

		if reverse {
			class.tq.Descend(iter) // end with transmit 0
		} else {
			class.tq.Ascend(iter) // start with transmit 0
		}

		if !keepGoing {
			return
		}
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

// PriorityBroadcast is an optional interface that a Broadcast can implement
// to control which priority class it is queued in. Broadcasts that do not
// implement it are assigned DefaultBroadcastPriority.
//
// Higher-priority classes are served first, but the anti-starvation policy
// (see StarvationLimit) guarantees that lower-priority classes still make
// progress under sustained higher-priority load.
type PriorityBroadcast interface {
	Broadcast
	// BroadcastPriority returns the priority class for this broadcast.
	// Values are clamped to [0, numBroadcastPriorityClasses).
	BroadcastPriority() BroadcastPriority
}

// broadcastPriorityOf resolves the priority class for a broadcast, applying
// the default and clamping out-of-range values.
func broadcastPriorityOf(b Broadcast) BroadcastPriority {
	pb, ok := b.(PriorityBroadcast)
	if !ok {
		return DefaultBroadcastPriority
	}
	p := pb.BroadcastPriority()
	if p < 0 {
		return 0
	}
	if p >= numBroadcastPriorityClasses {
		return numBroadcastPriorityClasses - 1
	}
	return p
}

// QueueBroadcast is used to enqueue a broadcast
func (q *TransmitLimitedQueue) QueueBroadcast(b Broadcast) {
	q.queueBroadcast(b, 0)
}

// lazyInit initializes internal data structures the first time they are
// needed.  You must already hold the mutex.
func (q *TransmitLimitedQueue) lazyInit() {
	for c := range q.classes {
		if q.classes[c].tq == nil {
			q.classes[c].tq = btree.New(32)
		}
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

	lb := &limitedBroadcast{
		transmits: initialTransmits,
		msgLen:    int64(len(b.Message())),
		id:        id,
		b:         b,
		priority:  broadcastPriorityOf(b),
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
			// The replacement is a new logical version of the broadcast:
			// the old entry is dropped entirely so the new one starts with
			// a fresh transmit budget rather than inheriting whatever the
			// old version had already consumed.
			old.b.Finished()
			q.deleteItem(old)
		}
	} else if !unique {
		// Slow path, hopefully nothing hot hits this.
		var remove []*limitedBroadcast
		for c := range q.classes {
			if q.classes[c].len() == 0 {
				continue
			}
			q.classes[c].tq.Ascend(func(item btree.Item) bool {
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
		}
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
	class := &q.classes[cur.priority]
	if class.tq != nil {
		_ = class.tq.Delete(cur)
		if class.tq.Len() == 0 {
			class.bypassed = 0
		}
	}
	if cur.name != "" {
		delete(q.tm, cur.name)
	}
	q.count--

	if q.count == 0 {
		// At idle there's no reason to let the id generator keep going
		// indefinitely.
		q.idGen = 0
	}
}

// addItem adds the given item into the overall datastructure. You must already
// hold the mutex.
func (q *TransmitLimitedQueue) addItem(cur *limitedBroadcast) {
	_ = q.classes[cur.priority].tq.ReplaceOrInsert(cur)
	if cur.name != "" {
		q.tm[cur.name] = cur
	}
	q.count++
}

// servingOrderLocked returns the deterministic order in which priority
// classes are visited by GetBroadcasts. Classes are normally visited from
// highest to lowest priority, except that any class that has been bypassed
// at least StarvationLimit consecutive times is moved to the front (ties
// broken by priority). The result depends only on per-class state, never on
// map iteration order. You must hold the mutex.
func (q *TransmitLimitedQueue) servingOrderLocked() []int {
	limit := q.StarvationLimit
	if limit < 1 {
		limit = DefaultStarvationLimit
	}

	order := make([]int, 0, len(q.classes))
	for pass := 0; pass < 2; pass++ {
		for c := range q.classes {
			if q.classes[c].len() == 0 {
				continue
			}
			starved := q.classes[c].bypassed >= limit
			if (pass == 0) == starved {
				order = append(order, c)
			}
		}
	}
	return order
}

// GetBroadcasts is used to get a number of broadcasts, up to a byte limit
// and applying a per-message overhead as provided.
func (q *TransmitLimitedQueue) GetBroadcasts(overhead, limit int) [][]byte {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Fast path the default case
	if q.count == 0 {
		return nil
	}

	transmitLimit := retransmitLimit(q.RetransmitMult, q.NumNodes())

	var (
		bytesUsed int
		toSend    [][]byte
		hadItems  [numBroadcastPriorityClasses]bool
		served    [numBroadcastPriorityClasses]bool
	)
	for c := range q.classes {
		hadItems[c] = q.classes[c].len() > 0
	}

	// Visit classes in deterministic priority order, except that classes
	// that have been starved for too long jump the queue.
	for _, classIdx := range q.servingOrderLocked() {
		if limit-bytesUsed-overhead <= 0 {
			break // bail out early
		}
		msgs := q.gatherClassLocked(&q.classes[classIdx], overhead, limit, &bytesUsed, transmitLimit)
		if len(msgs) > 0 {
			served[classIdx] = true
			toSend = append(toSend, msgs...)
		}
	}

	// Update the anti-starvation bookkeeping: a class that had messages
	// pending but was skipped while a higher-priority class was served
	// counts as bypassed.
	for c := range q.classes {
		class := &q.classes[c]
		if served[c] || class.len() == 0 {
			class.bypassed = 0
			continue
		}
		if !hadItems[c] {
			continue
		}
		for h := 0; h < c; h++ {
			if served[h] {
				class.bypassed++
				break
			}
		}
	}

	return toSend
}

// gatherClassLocked selects messages from a single priority class, up to
// the remaining byte budget. bytesUsed is shared with the caller so the
// per-message overhead accounting matches across classes. You must hold the
// mutex.
func (q *TransmitLimitedQueue) gatherClassLocked(class *priorityClassQueue, overhead, limit int, bytesUsed *int, transmitLimit int) [][]byte {
	if class.len() == 0 {
		return nil
	}

	var (
		toSend   [][]byte
		reinsert []*limitedBroadcast
	)

	// Visit fresher items first, but only look at stuff that will fit.
	// We'll go tier by tier, grabbing the largest items first.
	minTr, maxTr := class.transmitRange()
	for transmits := minTr; transmits <= maxTr; /*do not advance automatically*/ {
		free := int64(limit - *bytesUsed - overhead)
		if free <= 0 {
			break // bail out early
		}

		// Search for the least element on a given tier (by transmit count) as
		// defined in the limitedBroadcast.Less function that will fit into our
		// remaining space.
		greaterOrEqual := &limitedBroadcast{
			transmits: transmits,
			msgLen:    free,
			id:        math.MaxInt64,
		}
		lessThan := &limitedBroadcast{
			transmits: transmits + 1,
			msgLen:    math.MaxInt64,
			id:        math.MaxInt64,
		}
		var keep *limitedBroadcast
		class.tq.AscendRange(greaterOrEqual, lessThan, func(item btree.Item) bool {
			cur := item.(*limitedBroadcast)
			// Check if this is within our limits
			if int64(len(cur.b.Message())) > free {
				// If this happens it's a bug in the datastructure or
				// surrounding use doing something like having len(Message())
				// change over time. There's enough going on here that it's
				// probably sane to just skip it and move on for now.
				return true
			}
			keep = cur
			return false
		})
		if keep == nil {
			// No more items of an appropriate size in the tier.
			transmits++
			continue
		}

		msg := keep.b.Message()

		// Add to slice to send
		*bytesUsed += overhead + len(msg)
		toSend = append(toSend, msg)

		// Check if we should stop transmission
		q.deleteItem(keep)
		if keep.transmits+1 >= transmitLimit {
			keep.b.Finished()
		} else {
			// We need to bump this item down to another transmit tier, but
			// because it would be in the same direction that we're walking the
			// tiers, we will have to delay the reinsertion until we are
			// finished our search. Otherwise we'll possibly re-add the message
			// when we ascend to the next tier.
			keep.transmits++
			reinsert = append(reinsert, keep)
		}
	}

	for _, cur := range reinsert {
		q.addItem(cur)
	}

	return toSend
}

// NumQueued returns the number of queued messages
func (q *TransmitLimitedQueue) NumQueued() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.count
}

// Reset clears all the queued messages. Should only be used for tests.
func (q *TransmitLimitedQueue) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.walkReadOnlyLocked(false, func(cur *limitedBroadcast) bool {
		cur.b.Finished()
		return true
	})

	q.classes = [numBroadcastPriorityClasses]priorityClassQueue{}
	q.tm = nil
	q.idGen = 0
	q.count = 0
}

// Prune will retain the maxRetain latest messages, and the rest
// will be discarded. This can be used to prevent unbounded queue sizes
//
// Messages are discarded from the lowest-priority class first; within a
// class the oldest (most-transmitted) messages go first.
func (q *TransmitLimitedQueue) Prune(maxRetain int) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Do nothing if queue size is less than the limit
	for c := len(q.classes) - 1; c >= 0 && q.count > maxRetain; c-- {
		class := &q.classes[c]
		for q.count > maxRetain && class.len() > 0 {
			item := class.tq.Max()
			if item == nil {
				break
			}
			cur := item.(*limitedBroadcast)
			cur.b.Finished()
			q.deleteItem(cur)
		}
	}
}
