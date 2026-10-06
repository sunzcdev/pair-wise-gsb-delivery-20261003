// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package telephoneevent

import (
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPayloadMarshalUnmarshal(t *testing.T) {
	payload := Payload{
		Event:    DigitPound,
		End:      true,
		Volume:   10,
		Duration: 0x1234,
	}

	raw, err := payload.Marshal()
	require.NoError(t, err)
	assert.Equal(t, []byte{0x0b, 0x8a, 0x12, 0x34}, raw)

	decoded, err := UnmarshalPayload(raw)
	require.NoError(t, err)
	assert.Equal(t, payload, decoded)

	_, err = UnmarshalPayload([]byte{0, 0, 0})
	assert.ErrorIs(t, err, ErrShortPayload)

	_, err = Payload{Event: EventFlash + 1}.Marshal()
	assert.ErrorIs(t, err, ErrInvalidEvent)

	_, err = Payload{Volume: 16}.Marshal()
	assert.ErrorIs(t, err, ErrInvalidVolume)
}

func TestSenderLongEventAt48kHz(t *testing.T) {
	sender, err := NewSender(SenderConfig{
		PayloadType: 101,
		SSRC:        0x12345678,
		ClockRate:   48000,
		Sequencer:   rtp.NewFixedSequencer(10),
	})
	require.NoError(t, err)

	packets, err := sender.StartEvent(Digit5, 7, 1000)
	require.NoError(t, err)
	require.Len(t, packets, 1)
	assertTelephonePacket(t, packets[0], 10, 1000, Digit5, 7, 0, false, true, 0x12345678)

	packets, err = sender.ExtendEvent(MaxDuration + 1000)
	require.NoError(t, err)
	require.Len(t, packets, 2)
	assertTelephonePacket(t, packets[0], 11, 1000, Digit5, 7, MaxDuration, false, false, 0x12345678)
	assertTelephonePacket(t, packets[1], 12, 1000+MaxDuration, Digit5, 7, 1000, false, true, 0x12345678)

	packets, err = sender.EndEvent()
	require.NoError(t, err)
	require.Len(t, packets, endRedundancy)
	assertTelephonePacket(t, packets[0], 13, 1000+MaxDuration, Digit5, 7, 1000, true, false, 0x12345678)
	assertTelephonePacket(t, packets[1], 14, 1000+MaxDuration, Digit5, 7, 1000, true, false, 0x12345678)
	assertTelephonePacket(t, packets[2], 15, 1000+MaxDuration, Digit5, 7, 1000, true, false, 0x12345678)

	retransmitted, err := sender.RetransmitEnd()
	require.NoError(t, err)
	require.Len(t, retransmitted, 1)
	assert.Equal(t, uint16(16), retransmitted[0].SequenceNumber)
	assert.Equal(t, packets[2].Timestamp, retransmitted[0].Timestamp)
	assert.Equal(t, packets[2].Payload, retransmitted[0].Payload)
}

func TestSenderIncrementsAcrossSegments(t *testing.T) {
	sender, err := NewSender(SenderConfig{
		PayloadType: 101,
		ClockRate:   48000,
		Sequencer:   rtp.NewFixedSequencer(1),
	})
	require.NoError(t, err)

	_, err = sender.StartEvent(Digit1, 0, 100)
	require.NoError(t, err)

	packets, err := sender.ExtendEvent(MaxDuration)
	require.NoError(t, err)
	require.Len(t, packets, 1)
	assertTelephonePacket(t, packets[0], 2, 100, Digit1, 0, MaxDuration, false, false, 0)

	packets, err = sender.ExtendEvent(1)
	require.NoError(t, err)
	require.Len(t, packets, 1)
	assertTelephonePacket(t, packets[0], 3, 100+MaxDuration, Digit1, 0, 1, false, true, 0)

	packets, err = sender.ExtendEvent(1)
	require.NoError(t, err)
	require.Len(t, packets, 1)
	assertTelephonePacket(t, packets[0], 4, 100+MaxDuration, Digit1, 0, 2, false, false, 0)

	packets, err = sender.ExtendEvent(0)
	require.NoError(t, err)
	require.Len(t, packets, 1)
	assertTelephonePacket(t, packets[0], 5, 100+MaxDuration, Digit1, 0, 2, false, false, 0)
}

func TestSenderMultipleSegments(t *testing.T) {
	sender, err := NewSender(SenderConfig{
		PayloadType: 101,
		ClockRate:   48000,
		Sequencer:   rtp.NewFixedSequencer(1),
	})
	require.NoError(t, err)

	_, err = sender.StartEvent(Digit0, 0, 500)
	require.NoError(t, err)

	packets, err := sender.ExtendEvent(2*MaxDuration + 7)
	require.NoError(t, err)
	require.Len(t, packets, 3)
	assertTelephonePacket(t, packets[0], 2, 500, Digit0, 0, MaxDuration, false, false, 0)
	assertTelephonePacket(t, packets[1], 3, 500+MaxDuration, Digit0, 0, MaxDuration, false, false, 0)
	assertTelephonePacket(t, packets[2], 4, 500+2*MaxDuration, Digit0, 0, 7, false, true, 0)
}

func TestDurationConversionsUseProvidedClock(t *testing.T) {
	samples, err := durationToSamples(time.Millisecond, 8000)
	require.NoError(t, err)
	assert.Equal(t, uint32(8), samples)

	samples, err = durationToSamples(time.Millisecond, 48000)
	require.NoError(t, err)
	assert.Equal(t, uint32(48), samples)

	samples, err = durationToSamples(time.Second/16000, 48000)
	require.NoError(t, err)
	assert.Equal(t, uint32(3), samples)

	duration, err := Payload{Duration: 48}.DurationTime(48000)
	require.NoError(t, err)
	assert.Equal(t, time.Millisecond, duration)
}

func TestNewSenderAndReceiverValidateConfig(t *testing.T) {
	_, err := NewSender(SenderConfig{PayloadType: 128, ClockRate: 48000})
	assert.ErrorIs(t, err, ErrInvalidPayloadType)

	_, err = NewSender(SenderConfig{PayloadType: 101})
	assert.ErrorIs(t, err, ErrInvalidClockRate)

	_, err = NewReceiver(ReceiverConfig{PayloadType: 128, ClockRate: 48000})
	assert.ErrorIs(t, err, ErrInvalidPayloadType)

	_, err = NewReceiver(ReceiverConfig{PayloadType: 101})
	assert.ErrorIs(t, err, ErrInvalidClockRate)
}

func TestReceiverLongPressSegmentsAt48kHz(t *testing.T) {
	clock := newVirtualClock(time.Unix(10, 0))
	receiver, err := NewReceiver(ReceiverConfig{
		PayloadType: 101,
		ClockRate:   48000,
		Timeout:     50 * time.Millisecond,
		Now:         clock.now,
	})
	require.NoError(t, err)

	sender := newTestSender()
	packets, err := sender.ExtendEvent(MaxDuration + 100)
	require.NoError(t, err)

	changes, err := receiver.HandlePacket(packets[0])
	require.NoError(t, err)
	require.Equal(t, []Change{{
		Kind:          EventStarted,
		SSRC:          1,
		Event:         Digit5,
		Volume:        7,
		RTPTimestamp:  1000,
		DurationUnits: MaxDuration,
		Duration:      samplesToDuration(MaxDuration, 48000),
	}}, changes)

	clock.advance(20 * time.Millisecond)
	changes, err = receiver.HandlePacket(packets[1])
	require.NoError(t, err)
	assert.Empty(t, changes)

	packets, err = sender.EndEvent()
	require.NoError(t, err)
	clock.advance(20 * time.Millisecond)
	changes, err = receiver.HandlePacket(packets[0])
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, EventEnded, changes[0].Kind)
	assert.Equal(t, EndExplicit, changes[0].EndReason)
	assert.Equal(t, uint32(MaxDuration+100), changes[0].DurationUnits)
	assert.Equal(t, uint32(1000), changes[0].RTPTimestamp)
}

func TestReceiverTimeoutEndAndLateExplicitRetransmission(t *testing.T) {
	clock := newVirtualClock(time.Unix(20, 0))
	receiver, err := NewReceiver(ReceiverConfig{
		PayloadType:      101,
		ClockRate:        48000,
		Timeout:          50 * time.Millisecond,
		TombstoneTimeout: time.Second,
		Now:              clock.now,
	})
	require.NoError(t, err)

	changes, err := receiver.HandlePacket(makeTelephonePacket(1, 10, 1000, Digit7, 5, 400, false, true))
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, EventStarted, changes[0].Kind)

	clock.advance(50 * time.Millisecond)
	changes = receiver.Expire()
	require.Len(t, changes, 1)
	assert.Equal(t, EventEnded, changes[0].Kind)
	assert.Equal(t, EndTimeout, changes[0].EndReason)
	assert.Equal(t, uint32(400), changes[0].DurationUnits)

	clock.advance(10 * time.Millisecond)
	changes = receiver.Expire()
	assert.Empty(t, changes)

	changes, err = receiver.HandlePacket(makeTelephonePacket(1, 11, 1000, Digit7, 5, 480, true, true))
	require.NoError(t, err)
	assert.Empty(t, changes)
}

func TestReceiverNewKeySupersedesIncompleteOldEvent(t *testing.T) {
	clock := newVirtualClock(time.Unix(30, 0))
	receiver, err := NewReceiver(ReceiverConfig{
		PayloadType: 101,
		ClockRate:   48000,
		Timeout:     time.Minute,
		Now:         clock.now,
	})
	require.NoError(t, err)

	changes, err := receiver.HandlePacket(makeTelephonePacket(1, 10, 1000, Digit1, 5, 100, false, true))
	require.NoError(t, err)
	require.Len(t, changes, 1)

	clock.advance(20 * time.Millisecond)
	changes, err = receiver.HandlePacket(makeTelephonePacket(1, 11, 2000, Digit2, 5, 0, false, true))
	require.NoError(t, err)
	require.Len(t, changes, 2)
	assert.Equal(t, EventEnded, changes[0].Kind)
	assert.Equal(t, EndSuperseded, changes[0].EndReason)
	assert.Equal(t, Digit1, changes[0].Event)
	assert.Equal(t, uint32(100), changes[0].DurationUnits)
	assert.Equal(t, EventStarted, changes[1].Kind)
	assert.Equal(t, Digit2, changes[1].Event)

	clock.advance(10 * time.Millisecond)
	changes, err = receiver.HandlePacket(makeTelephonePacket(1, 12, 1000, Digit1, 5, 180, true, true))
	require.NoError(t, err)
	assert.Empty(t, changes)
}

func TestReceiverOutOfOrderAndRepeatedEndPackets(t *testing.T) {
	clock := newVirtualClock(time.Unix(40, 0))
	receiver, err := NewReceiver(ReceiverConfig{
		PayloadType: 101,
		ClockRate:   48000,
		Timeout:     50 * time.Millisecond,
		Now:         clock.now,
	})
	require.NoError(t, err)

	changes, err := receiver.HandlePacket(makeTelephonePacket(1, 12, 1000, Digit9, 6, 300, true, true))
	require.NoError(t, err)
	require.Len(t, changes, 2)
	assert.Equal(t, EventStarted, changes[0].Kind)
	assert.Equal(t, EventEnded, changes[1].Kind)
	assert.Equal(t, EndExplicit, changes[1].EndReason)

	clock.advance(10 * time.Millisecond)
	changes, err = receiver.HandlePacket(makeTelephonePacket(1, 10, 1000, Digit9, 6, 100, false, true))
	require.NoError(t, err)
	assert.Empty(t, changes)

	clock.advance(10 * time.Millisecond)
	changes, err = receiver.HandlePacket(makeTelephonePacket(1, 13, 1000, Digit9, 6, 300, true, true))
	require.NoError(t, err)
	assert.Empty(t, changes)
}

func TestReceiverSeparatesEventCodeAndSSRC(t *testing.T) {
	clock := newVirtualClock(time.Unix(50, 0))
	receiver, err := NewReceiver(ReceiverConfig{
		PayloadType: 101,
		ClockRate:   48000,
		Timeout:     time.Minute,
		Now:         clock.now,
	})
	require.NoError(t, err)

	changes, err := receiver.HandlePacket(makeTelephonePacket(1, 10, 1000, Digit1, 5, 0, false, true))
	require.NoError(t, err)
	require.Len(t, changes, 1)

	changes, err = receiver.HandlePacket(makeTelephonePacket(2, 11, 1000, Digit1, 5, 0, false, true))
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, uint32(2), changes[0].SSRC)

	changes, err = receiver.HandlePacket(makeTelephonePacket(1, 12, 1000, Digit2, 5, 0, false, true))
	require.NoError(t, err)
	require.Len(t, changes, 2)
	assert.Equal(t, EndSuperseded, changes[0].EndReason)
	assert.Equal(t, Digit1, changes[0].Event)
	assert.Equal(t, EventStarted, changes[1].Kind)
	assert.Equal(t, Digit2, changes[1].Event)
}

func TestReceiverOutOfOrderNextSegmentMarker(t *testing.T) {
	clock := newVirtualClock(time.Unix(60, 0))
	receiver, err := NewReceiver(ReceiverConfig{
		PayloadType: 101,
		ClockRate:   48000,
		Timeout:     time.Minute,
		Now:         clock.now,
	})
	require.NoError(t, err)

	changes, err := receiver.HandlePacket(makeTelephonePacket(1, 10, 1000, Digit3, 5, 100, false, true))
	require.NoError(t, err)
	require.Len(t, changes, 1)

	changes, err = receiver.HandlePacket(makeTelephonePacket(1, 13, 1000+MaxDuration, Digit3, 5, 0, false, true))
	require.NoError(t, err)
	assert.Empty(t, changes)

	changes, err = receiver.HandlePacket(makeTelephonePacket(1, 12, 1000, Digit3, 5, MaxDuration, false, false))
	require.NoError(t, err)
	assert.Empty(t, changes)

	changes, err = receiver.HandlePacket(makeTelephonePacket(1, 15, 1000+2*MaxDuration, Digit3, 5, 0, false, true))
	require.NoError(t, err)
	assert.Empty(t, changes)

	changes, err = receiver.HandlePacket(makeTelephonePacket(1, 14, 1000+MaxDuration, Digit3, 5, 20, true, false))
	require.NoError(t, err)
	assert.Empty(t, changes)

	changes, err = receiver.HandlePacket(makeTelephonePacket(1, 16, 1000+2*MaxDuration, Digit3, 5, 20, true, false))
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, EndExplicit, changes[0].EndReason)
	assert.Equal(t, uint32(2*MaxDuration+20), changes[0].DurationUnits)
}

type virtualClock struct {
	current time.Time
}

func newVirtualClock(initial time.Time) *virtualClock {
	return &virtualClock{current: initial}
}

func (c *virtualClock) advance(duration time.Duration) {
	c.current = c.current.Add(duration)
}

func (c *virtualClock) now() time.Time {
	return c.current
}

func newTestSender() *Sender {
	sender, err := NewSender(SenderConfig{
		PayloadType: 101,
		SSRC:        1,
		ClockRate:   48000,
		Sequencer:   rtp.NewFixedSequencer(100),
	})
	if err != nil {
		panic(err)
	}

	_, err = sender.StartEvent(Digit5, 7, 1000)
	if err != nil {
		panic(err)
	}

	return sender
}

func makeTelephonePacket(
	ssrc uint32,
	sequence uint16,
	timestamp uint32,
	event Event,
	volume Volume,
	duration uint16,
	end bool,
	marker bool,
) *rtp.Packet {
	payload, err := Payload{
		Event:    event,
		Volume:   volume,
		Duration: duration,
		End:      end,
	}.Marshal()
	if err != nil {
		panic(err)
	}

	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         marker,
			PayloadType:    101,
			SequenceNumber: sequence,
			Timestamp:      timestamp,
			SSRC:           ssrc,
		},
		Payload: payload,
	}
}

func assertTelephonePacket(
	t *testing.T,
	packet *rtp.Packet,
	sequence uint16,
	timestamp uint32,
	event Event,
	volume Volume,
	duration uint16,
	end bool,
	marker bool,
	ssrc uint32,
) {
	t.Helper()

	assert.Equal(t, uint8(101), packet.PayloadType)
	assert.Equal(t, ssrc, packet.SSRC)
	assert.Equal(t, sequence, packet.SequenceNumber)
	assert.Equal(t, timestamp, packet.Timestamp)
	assert.Equal(t, marker, packet.Marker)

	payload, err := UnmarshalPayload(packet.Payload)
	require.NoError(t, err)
	assert.Equal(t, event, payload.Event)
	assert.Equal(t, volume, payload.Volume)
	assert.Equal(t, duration, payload.Duration)
	assert.Equal(t, end, payload.End)
}
