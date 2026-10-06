// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtp

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// virtualClock is a manually advanced clock shared by sender and receiver.
type virtualClock struct {
	mu sync.Mutex
	t  time.Time
}

func newVirtualClock() *virtualClock {
	return &virtualClock{t: time.Unix(1700000000, 0)}
}

func (c *virtualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *virtualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func telephoneEventRTPPacket(ssrc, timestamp uint32, event codecs.TelephoneEvent) *Packet {
	return &Packet{
		Header:  Header{SSRC: ssrc, Timestamp: timestamp},
		Payload: event.Marshal(),
	}
}

func transmissionPacket(ssrc uint32, tr TelephoneEventTransmission) *Packet {
	return &Packet{
		Header:  Header{SSRC: ssrc, Timestamp: tr.Timestamp, Marker: tr.Marker},
		Payload: tr.Payload(),
	}
}

func TestTelephoneEventSenderBasic(t *testing.T) {
	clock := newVirtualClock()
	// 48 kHz media clock: the library must not assume 8 kHz.
	sender := NewTelephoneEventSender(48000, WithTelephoneEventSenderClock(clock.now))

	out := sender.Begin(1, 10, 48000)
	require.Len(t, out, 1)
	assert.Equal(t, TelephoneEventTransmission{
		Event: 1, Volume: 10, Duration: 0, Timestamp: 48000, Marker: true,
	}, out[0])
	assert.True(t, sender.Active())

	clock.advance(20 * time.Millisecond)
	out = sender.Extend()
	require.Len(t, out, 1)
	assert.Equal(t, uint16(960), out[0].Duration) // 20 ms at 48 kHz
	assert.Equal(t, uint32(48000), out[0].Timestamp)
	assert.False(t, out[0].Marker)
	assert.False(t, out[0].End)

	clock.advance(10 * time.Millisecond)
	out = sender.End()
	// Final packet plus DefaultTelephoneEventEndRetransmissions copies.
	require.Len(t, out, 1+DefaultTelephoneEventEndRetransmissions)
	for _, tr := range out {
		assert.True(t, tr.End)
		assert.Equal(t, uint16(1440), tr.Duration) // 30 ms at 48 kHz
		assert.Equal(t, uint32(48000), tr.Timestamp)
	}
	assert.False(t, sender.Active())
	assert.Nil(t, sender.Extend())
	assert.Nil(t, sender.End())
}

func TestTelephoneEventSenderLongEventSegmentation(t *testing.T) {
	clock := newVirtualClock()
	sender := NewTelephoneEventSender(8000, WithTelephoneEventSenderClock(clock.now))

	out := sender.Begin(5, 0, 0)
	require.Len(t, out, 1)

	// 10 s at 8 kHz = 80000 ticks > 0xFFFF, the event must be split.
	clock.advance(10 * time.Second)
	out = sender.Extend()
	require.Len(t, out, 2)

	// First segment is closed at the maximum expressible duration, no end bit.
	assert.Equal(t, uint32(0), out[0].Timestamp)
	assert.Equal(t, uint16(0xFFFF), out[0].Duration)
	assert.False(t, out[0].End)

	// The second segment starts exactly where the first one ends.
	assert.Equal(t, uint32(0xFFFF), out[1].Timestamp)
	assert.Equal(t, uint16(80000-0xFFFF), out[1].Duration)
	assert.False(t, out[1].End)

	clock.advance(1 * time.Second)
	out = sender.End()
	require.Len(t, out, 1+DefaultTelephoneEventEndRetransmissions)
	for _, tr := range out {
		assert.True(t, tr.End)
		assert.Equal(t, uint32(0xFFFF), tr.Timestamp)
		assert.Equal(t, uint16(80000-0xFFFF+8000), tr.Duration)
	}
}

func TestTelephoneEventSenderMultipleSegments(t *testing.T) {
	clock := newVirtualClock()
	sender := NewTelephoneEventSender(8000,
		WithTelephoneEventSenderClock(clock.now),
		WithTelephoneEventMaxDuration(100),
	)

	sender.Begin(2, 0, 1000)
	clock.advance(43750 * time.Microsecond) // 350 ticks at 8 kHz
	out := sender.Extend()
	require.Len(t, out, 4)

	// Segments close at 100 tick boundaries starting from the begin timestamp.
	assert.Equal(t, TelephoneEventTransmission{Event: 2, Duration: 100, Timestamp: 1000}, out[0])
	assert.Equal(t, TelephoneEventTransmission{Event: 2, Duration: 100, Timestamp: 1100}, out[1])
	assert.Equal(t, TelephoneEventTransmission{Event: 2, Duration: 100, Timestamp: 1200}, out[2])
	assert.Equal(t, TelephoneEventTransmission{Event: 2, Duration: 50, Timestamp: 1300}, out[3])
}

func TestTelephoneEventSenderBeginEndsPreviousEvent(t *testing.T) {
	clock := newVirtualClock()
	sender := NewTelephoneEventSender(8000, WithTelephoneEventSenderClock(clock.now))

	sender.Begin(1, 0, 0)
	clock.advance(100 * time.Millisecond)

	out := sender.Begin(2, 0, 800)
	// End packets for event 1, then the begin of event 2.
	require.Len(t, out, 1+DefaultTelephoneEventEndRetransmissions+1)
	for _, tr := range out[:1+DefaultTelephoneEventEndRetransmissions] {
		assert.True(t, tr.End)
		assert.Equal(t, uint8(1), tr.Event)
		assert.Equal(t, uint16(800), tr.Duration)
	}
	last := out[len(out)-1]
	assert.Equal(t, uint8(2), last.Event)
	assert.True(t, last.Marker)
	assert.Equal(t, uint32(800), last.Timestamp)
}

func TestTelephoneEventReceiverLongPress(t *testing.T) {
	clock := newVirtualClock()
	receiver := NewTelephoneEventReceiver(WithTelephoneEventReceiverClock(clock.now))
	const ssrc = 0x11223344

	// Begin of the event.
	out, err := receiver.ProcessPacket(telephoneEventRTPPacket(ssrc, 1000, codecs.TelephoneEvent{Event: 7, Volume: 10}))
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, TelephoneEventNotification{
		Kind: TelephoneEventBegin, SSRC: ssrc, Event: 7, Volume: 10, Timestamp: 1000,
	}, out[0])

	// Updates, including a duplicate and an out-of-order one.
	for _, duration := range []uint16{160, 160, 320, 160} {
		out, err = receiver.ProcessPacket(telephoneEventRTPPacket(ssrc, 1000, codecs.TelephoneEvent{
			Event: 7, Volume: 10, Duration: duration,
		}))
		require.NoError(t, err)
		assert.Empty(t, out)
		clock.advance(20 * time.Millisecond)
	}

	// The end packet is sent three times; only one end notification may result.
	for i := 0; i < 3; i++ {
		out, err = receiver.ProcessPacket(telephoneEventRTPPacket(ssrc, 1000, codecs.TelephoneEvent{
			Event: 7, Volume: 10, Duration: 480, End: true,
		}))
		require.NoError(t, err)
		if i == 0 {
			require.Len(t, out, 1)
			assert.Equal(t, TelephoneEventNotification{
				Kind: TelephoneEventEnd, SSRC: ssrc, Event: 7, Volume: 10,
				Timestamp: 1000, Duration: 480, Reason: TelephoneEventEndExplicit,
			}, out[0])
		} else {
			assert.Empty(t, out)
		}
	}

	// No timeout end may be inferred for an explicitly ended event.
	clock.advance(time.Second)
	assert.Empty(t, receiver.Expire())
}

func TestTelephoneEventReceiverLostEndPacket(t *testing.T) {
	const ssrc = 0xAABBCCDD

	t.Run("retransmitted before timeout", func(t *testing.T) {
		clock := newVirtualClock()
		receiver := NewTelephoneEventReceiver(WithTelephoneEventReceiverClock(clock.now))

		_, err := receiver.ProcessPacket(telephoneEventRTPPacket(ssrc, 2000, codecs.TelephoneEvent{Event: 3}))
		require.NoError(t, err)
		clock.advance(50 * time.Millisecond)
		_, err = receiver.ProcessPacket(telephoneEventRTPPacket(ssrc, 2000, codecs.TelephoneEvent{Event: 3, Duration: 400}))
		require.NoError(t, err)

		// The first end packet is lost, the retransmission arrives later.
		clock.advance(100 * time.Millisecond)
		out, err := receiver.ProcessPacket(telephoneEventRTPPacket(ssrc, 2000, codecs.TelephoneEvent{
			Event: 3, Duration: 400, End: true,
		}))
		require.NoError(t, err)
		require.Len(t, out, 1)
		assert.Equal(t, TelephoneEventEnd, out[0].Kind)
		assert.Equal(t, TelephoneEventEndExplicit, out[0].Reason)
		assert.Equal(t, uint32(400), out[0].Duration)
	})

	t.Run("never retransmitted", func(t *testing.T) {
		clock := newVirtualClock()
		receiver := NewTelephoneEventReceiver(WithTelephoneEventReceiverClock(clock.now))

		_, err := receiver.ProcessPacket(telephoneEventRTPPacket(ssrc, 2000, codecs.TelephoneEvent{Event: 3}))
		require.NoError(t, err)
		_, err = receiver.ProcessPacket(telephoneEventRTPPacket(ssrc, 2000, codecs.TelephoneEvent{Event: 3, Duration: 400}))
		require.NoError(t, err)

		// All end packets are lost; the end must be inferred by timeout.
		clock.advance(DefaultTelephoneEventTimeout + time.Millisecond)
		out := receiver.Expire()
		require.Len(t, out, 1)
		assert.Equal(t, TelephoneEventEnd, out[0].Kind)
		assert.Equal(t, TelephoneEventEndTimeout, out[0].Reason)
		assert.Equal(t, uint32(400), out[0].Duration)

		// A late retransmission of the end packet must not produce a
		// second end notification.
		out, err = receiver.ProcessPacket(telephoneEventRTPPacket(ssrc, 2000, codecs.TelephoneEvent{
			Event: 3, Duration: 400, End: true,
		}))
		require.NoError(t, err)
		assert.Empty(t, out)
		assert.Empty(t, receiver.Expire())
	})
}

func TestTelephoneEventReceiverNewEventWhileOldIncomplete(t *testing.T) {
	clock := newVirtualClock()
	receiver := NewTelephoneEventReceiver(WithTelephoneEventReceiverClock(clock.now))
	const ssrc = 0x55AA55AA

	// Event 4 starts but its updates stop (end packets lost).
	out, err := receiver.ProcessPacket(telephoneEventRTPPacket(ssrc, 1000, codecs.TelephoneEvent{Event: 4, Duration: 160}))
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, TelephoneEventBegin, out[0].Kind)

	// A new key is pressed before the old event completed.
	clock.advance(100 * time.Millisecond)
	out, err = receiver.ProcessPacket(telephoneEventRTPPacket(ssrc, 5000, codecs.TelephoneEvent{Event: 9}))
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, TelephoneEventNotification{
		Kind: TelephoneEventBegin, SSRC: ssrc, Event: 9, Timestamp: 5000,
	}, out[0])

	// The old event is ended by timeout inference, the new one explicitly.
	clock.advance(DefaultTelephoneEventTimeout - 99*time.Millisecond)
	expired := receiver.Expire()
	require.Len(t, expired, 1)
	assert.Equal(t, uint8(4), expired[0].Event)
	assert.Equal(t, TelephoneEventEndTimeout, expired[0].Reason)
	assert.Equal(t, uint32(160), expired[0].Duration)

	out, err = receiver.ProcessPacket(telephoneEventRTPPacket(ssrc, 5000, codecs.TelephoneEvent{Event: 9, Duration: 240, End: true}))
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, uint8(9), out[0].Event)
	assert.Equal(t, TelephoneEventEndExplicit, out[0].Reason)
}

func TestTelephoneEventReceiverOutOfOrderEnd(t *testing.T) {
	clock := newVirtualClock()
	receiver := NewTelephoneEventReceiver(WithTelephoneEventReceiverClock(clock.now))
	const ssrc = 0x01020304

	// The end packet overtakes the begin/update packets.
	out, err := receiver.ProcessPacket(telephoneEventRTPPacket(ssrc, 8000, codecs.TelephoneEvent{
		Event: 1, Duration: 480, End: true,
	}))
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, TelephoneEventBegin, out[0].Kind)
	assert.Equal(t, TelephoneEventEnd, out[1].Kind)
	assert.Equal(t, TelephoneEventEndExplicit, out[1].Reason)
	assert.Equal(t, uint32(480), out[1].Duration)

	// The delayed begin and update packets are ignored.
	for _, event := range []codecs.TelephoneEvent{
		{Event: 1},
		{Event: 1, Duration: 160},
	} {
		out, err = receiver.ProcessPacket(telephoneEventRTPPacket(ssrc, 8000, event))
		require.NoError(t, err)
		assert.Empty(t, out)
	}
}

func TestTelephoneEventSenderReceiverRoundTrip(t *testing.T) {
	clock := newVirtualClock()
	sender := NewTelephoneEventSender(8000, WithTelephoneEventSenderClock(clock.now))
	receiver := NewTelephoneEventReceiver(WithTelephoneEventReceiverClock(clock.now))
	const ssrc = 0xDEADBEEF

	var notifications []TelephoneEventNotification
	feed := func(transmissions []TelephoneEventTransmission) {
		for _, tr := range transmissions {
			out, err := receiver.ProcessPacket(transmissionPacket(ssrc, tr))
			require.NoError(t, err)
			notifications = append(notifications, out...)
		}
	}

	feed(sender.Begin(11, 5, 0))
	// Long press of 10 s: the sender segments the event, the receiver must
	// merge the segments back into a single begin/end pair.
	for i := 0; i < 100; i++ {
		clock.advance(100 * time.Millisecond)
		feed(sender.Extend())
	}
	feed(sender.End())

	require.Len(t, notifications, 2)
	assert.Equal(t, TelephoneEventNotification{
		Kind: TelephoneEventBegin, SSRC: ssrc, Event: 11, Volume: 5, Timestamp: 0,
	}, notifications[0])
	assert.Equal(t, TelephoneEventEnd, notifications[1].Kind)
	assert.Equal(t, TelephoneEventEndExplicit, notifications[1].Reason)
	assert.Equal(t, uint32(80000), notifications[1].Duration) // 10 s at 8 kHz
	assert.Equal(t, uint32(0), notifications[1].Timestamp)
}
