// Copyright IBM Corp. 2013, 2026
// SPDX-License-Identifier: MPL-2.0

package memberlist

/*
The broadcast mechanism works by maintaining a sorted list of messages to be
sent out. When a message is to be broadcast, the retransmit count
is set to zero and appended to the queue. The retransmit count serves
as the "priority", ensuring that newer messages get sent first. Once
a message hits the retransmit limit, it is removed from the queue.

Additionally, older entries can be invalidated by new messages that
are contradictory. For example, if we send "{suspect M1 inc: 1},
then a following {alive M1 inc: 2} will invalidate that message

Membership change messages occupy the high priority broadcast class while
application messages supplied via Delegate.GetBroadcasts occupy the lower
priority class. The queue interleaves them with an anti-starvation policy so
that sustained membership churn cannot permanently lock out application
traffic. When a logical broadcast is replaced by a newer version, the
replacement is enqueued anew with a fresh zeroed transmit count instead of
inheriting the consumed retransmit budget of the invalidated message.
*/

type memberlistBroadcast struct {
	node   string
	msg    []byte
	notify chan struct{}
}

func (b *memberlistBroadcast) Invalidates(other Broadcast) bool {
	// Check if that broadcast is a memberlist type
	mb, ok := other.(*memberlistBroadcast)
	if !ok {
		return false
	}

	// Invalidates any message about the same node
	return b.node == mb.node
}

// memberlist.NamedBroadcast optional interface
func (b *memberlistBroadcast) Name() string {
	return b.node
}

// Priority marks membership change messages as the high priority class so
// that bursts of them are served before application broadcasts.
func (b *memberlistBroadcast) Priority() BroadcastPriority {
	return PriorityMemberlist
}

func (b *memberlistBroadcast) Message() []byte {
	return b.msg
}

func (b *memberlistBroadcast) Finished() {
	select {
	case b.notify <- struct{}{}:
	default:
	}
}

// encodeAndBroadcast encodes a message and enqueues it for broadcast. Fails
// silently if there is an encoding error.
func (m *Memberlist) encodeAndBroadcast(node string, msgType messageType, msg any) {
	m.encodeBroadcastNotify(node, msgType, msg, nil)
}

// encodeBroadcastNotify encodes a message and enqueues it for broadcast
// and notifies the given channel when transmission is finished. Fails
// silently if there is an encoding error.
func (m *Memberlist) encodeBroadcastNotify(node string, msgType messageType, msg any, notify chan struct{}) {
	buf, err := encode(msgType, msg, m.config.MsgpackUseNewTimeFormat)
	if err != nil {
		m.logger.Printf("[ERR] memberlist: Failed to encode message for broadcast: %s", err)
	} else {
		m.queueBroadcast(node, buf.Bytes(), notify)
	}
}

// queueBroadcast is used to start dissemination of a message. It will be
// sent up to a configured number of times. The message could potentially
// be invalidated by a future message about the same node
func (m *Memberlist) queueBroadcast(node string, msg []byte, notify chan struct{}) {
	b := &memberlistBroadcast{node, msg, notify}
	m.broadcasts.QueueBroadcast(b)
}

// getBroadcasts is used to return a slice of broadcasts to send up to
// a maximum byte size, while imposing a per-broadcast overhead. This is used
// to fill a UDP packet with piggybacked data
func (m *Memberlist) getBroadcasts(overhead, limit int) [][]byte {
	d := m.config.Delegate
	if d == nil {
		return m.broadcasts.GetBroadcasts(overhead, limit)
	}

	// Application traffic is served from the lower priority class. The
	// queue applies anti-starvation between membership changes and these
	// messages; memberlist broadcasts still go first during a membership
	// burst.
	userFn := func(cbOverhead, cbLimit int) [][]byte {
		// cbOverhead is the memberlist per-message overhead. The delegate
		// must additionally account for the userMsg type byte, so it is
		// asked with cbOverhead+1.
		userMsgs := d.GetBroadcasts(cbOverhead+userMsgOverhead, cbLimit)

		// Frame each returned message with its userMsg type byte. The
		// resulting buffers are exactly what goes on the wire, so charging
		// cbOverhead+len(framed) in the scheduler matches the true cost.
		framed := make([][]byte, 0, len(userMsgs))
		for _, msg := range userMsgs {
			buf := make([]byte, 1, len(msg)+1)
			buf[0] = byte(userMsg)
			buf = append(buf, msg...)
			framed = append(framed, buf)
		}
		return framed
	}

	return m.broadcasts.getBroadcasts(overhead, limit, userFn)
}
