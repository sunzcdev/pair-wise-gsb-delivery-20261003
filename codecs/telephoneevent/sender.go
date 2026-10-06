// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package telephoneevent

import (
	"errors"
	"time"

	"github.com/pion/rtp"
)

var (
	// ErrEventActive is returned when an event is started while another one is active.
	ErrEventActive = errors.New("telephone-event is active")
	// ErrNoEvent is returned when an event is extended or ended without being started.
	ErrNoEvent = errors.New("no active telephone-event")
	// ErrInvalidPayloadType is returned for an RTP payload type greater than 127.
	ErrInvalidPayloadType = errors.New("invalid telephone-event RTP payload type")
	// ErrInvalidSamples is returned for a negative or unsupported sample count.
	ErrInvalidSamples = errors.New("invalid telephone-event sample count")
)

// SenderConfig configures an RFC 4733 event sender.
type SenderConfig struct {
	PayloadType uint8
	SSRC        uint32
	ClockRate   uint32
	Sequencer   rtp.Sequencer
}

// Sender generates RFC 4733 telephone-event RTP packets.
type Sender struct {
	payloadType uint8
	ssrc        uint32
	clockRate   uint32
	sequencer   rtp.Sequencer

	event            Event
	volume           Volume
	startTimestamp   uint32
	segmentTimestamp uint32
	segmentStart     uint32
	duration         uint32
	segmentReported  uint32
	lastPacket       *rtp.Packet
	active           bool
	ended            bool
}

// NewSender creates a telephone-event sender. ClockRate is the RTP media clock and
// is not assumed to be 8 kHz.
func NewSender(config SenderConfig) (*Sender, error) {
	if config.PayloadType > 127 {
		return nil, ErrInvalidPayloadType
	}

	if config.ClockRate == 0 {
		return nil, ErrInvalidClockRate
	}

	sequencer := config.Sequencer
	if sequencer == nil {
		sequencer = rtp.NewRandomSequencer()
	}

	return &Sender{
		payloadType: config.PayloadType,
		ssrc:        config.SSRC,
		clockRate:   config.ClockRate,
		sequencer:   sequencer,
	}, nil
}

// StartEvent emits the first packet for a new event at rtpTimestamp.
func (s *Sender) StartEvent(event Event, volume Volume, rtpTimestamp uint32) ([]*rtp.Packet, error) {
	if s.active {
		return nil, ErrEventActive
	}

	if event > EventFlash {
		return nil, ErrInvalidEvent
	}

	if volume > maxVolume {
		return nil, ErrInvalidVolume
	}

	s.event = event
	s.volume = volume
	s.startTimestamp = rtpTimestamp
	s.segmentTimestamp = rtpTimestamp
	s.segmentStart = 0
	s.duration = 0
	s.segmentReported = 0
	s.lastPacket = nil
	s.active = true
	s.ended = false

	packets, err := s.buildPackets(0, false)
	if err != nil {
		return nil, err
	}

	return packets, nil
}

// ExtendEvent advances the active event by samples and emits its updated state.
func (s *Sender) ExtendEvent(samples uint32) ([]*rtp.Packet, error) {
	if !s.active || s.ended {
		return nil, ErrNoEvent
	}

	s.duration += samples

	return s.buildPackets(s.duration, false)
}

// ExtendEventFor advances the active event by duration using the configured clock.
func (s *Sender) ExtendEventFor(duration time.Duration) ([]*rtp.Packet, error) {
	samples, err := durationToSamples(duration, s.clockRate)
	if err != nil {
		return nil, err
	}

	return s.ExtendEvent(samples)
}

// EndEvent marks the active event as ended. Packets required to finish an
// in-progress duration segment are emitted before the three RFC 4733 end copies.
func (s *Sender) EndEvent() ([]*rtp.Packet, error) {
	if !s.active || s.ended {
		return nil, ErrNoEvent
	}

	packets, err := s.buildPackets(s.duration, true)
	if err != nil {
		return nil, err
	}

	s.active = false
	s.ended = true
	endIndex := len(packets) - 1
	endPacket := packets[endIndex]

	for i := 1; i < endRedundancy; i++ {
		packets = append(packets, s.packetFromPayload(
			endPacket.Timestamp,
			endPacket.Payload,
			endPacket.Marker,
		))
	}
	s.lastPacket = packets[len(packets)-1]

	return packets, nil
}

// RetransmitEnd sends another copy of the explicit end packet as required for loss.
func (s *Sender) RetransmitEnd() ([]*rtp.Packet, error) {
	if !s.ended || s.lastPacket == nil || s.lastPacket.Payload[1]&0x80 == 0 {
		return nil, ErrNoEvent
	}

	retransmitted := s.packetFromPayload(s.lastPacket.Timestamp, s.lastPacket.Payload, s.lastPacket.Marker)

	return []*rtp.Packet{retransmitted}, nil
}

func (s *Sender) buildPackets(duration uint32, end bool) ([]*rtp.Packet, error) {
	var packets []*rtp.Packet
	startingSegment := s.segmentStart

	for duration-s.segmentStart > MaxDuration {
		if s.segmentReported < MaxDuration {
			payload, err := s.payload(MaxDuration, false)
			if err != nil {
				return nil, err
			}

			packets = append(packets, s.packetFromPayload(s.segmentTimestamp, payload, false))
		}

		s.segmentStart += MaxDuration
		s.segmentTimestamp = s.startTimestamp + s.segmentStart
		s.segmentReported = 0
	}

	payloadDuration := uint16(duration - s.segmentStart) //nolint:gosec // G115
	payload, err := s.payload(payloadDuration, end)
	if err != nil {
		return nil, err
	}

	marker := s.segmentStart != startingSegment || payloadDuration == 0
	packet := s.packetFromPayload(s.segmentTimestamp, payload, marker)
	packets = append(packets, packet)
	s.segmentReported = uint32(payloadDuration)
	s.lastPacket = packet

	return packets, nil
}

func (s *Sender) payload(duration uint16, end bool) ([]byte, error) {
	return Payload{
		Event:    s.event,
		End:      end,
		Volume:   s.volume,
		Duration: duration,
	}.Marshal()
}

func (s *Sender) packetFromPayload(timestamp uint32, payload []byte, marker bool) *rtp.Packet {
	packet := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         marker,
			PayloadType:    s.payloadType,
			SequenceNumber: s.sequencer.NextSequenceNumber(),
			Timestamp:      timestamp,
			SSRC:           s.ssrc,
		},
		Payload: make([]byte, len(payload)),
	}
	copy(packet.Payload, payload)

	return packet
}
