// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtp

import (
	"math"
	"time"

	"github.com/pion/rtp/codecs"
)

const (
	// DefaultTelephoneEventMaxDuration is the largest duration expressible in
	// the 16 bit duration field of an RFC 4733 telephone-event payload.
	DefaultTelephoneEventMaxDuration = math.MaxUint16

	// DefaultTelephoneEventEndRetransmissions is the number of redundant
	// copies of the end-of-event packet emitted in addition to the first one,
	// as recommended by RFC 4733.
	DefaultTelephoneEventEndRetransmissions = 2
)

// TelephoneEventTransmission is a single telephone-event packet that the
// TelephoneEventSender asks the caller to send. The caller is responsible for
// placing Payload() into an RTP packet with the given Timestamp and Marker
// values and its own payload type, SSRC and sequence numbers.
type TelephoneEventTransmission struct {
	Event     uint8
	End       bool
	Volume    uint8
	Duration  uint16
	Timestamp uint32
	Marker    bool
}

// Payload returns the RFC 4733 wire representation of the transmission.
func (t TelephoneEventTransmission) Payload() []byte {
	return codecs.TelephoneEvent{
		Event:    t.Event,
		End:      t.End,
		Volume:   t.Volume,
		Duration: t.Duration,
	}.Marshal()
}

// TelephoneEventSenderOption configures a TelephoneEventSender.
type TelephoneEventSenderOption func(*TelephoneEventSender)

// WithTelephoneEventSenderClock sets the time source used to measure event
// durations. Defaults to time.Now. Tests can provide a virtual clock.
func WithTelephoneEventSenderClock(now func() time.Time) TelephoneEventSenderOption {
	return func(s *TelephoneEventSender) {
		s.now = now
	}
}

// WithTelephoneEventMaxDuration sets the maximum duration, in media clock
// units, carried by a single event segment. Events longer than this are split
// into contiguous segments. Defaults to DefaultTelephoneEventMaxDuration.
func WithTelephoneEventMaxDuration(maxDuration uint32) TelephoneEventSenderOption {
	return func(s *TelephoneEventSender) {
		if maxDuration == 0 || maxDuration > math.MaxUint16 {
			maxDuration = math.MaxUint16
		}
		s.maxDuration = maxDuration
	}
}

// WithTelephoneEventEndRetransmissions sets how many redundant copies of the
// end-of-event packet are emitted in addition to the first one.
// Defaults to DefaultTelephoneEventEndRetransmissions.
func WithTelephoneEventEndRetransmissions(n int) TelephoneEventSenderOption {
	return func(s *TelephoneEventSender) {
		s.endRetransmissions = n
	}
}

// TelephoneEventSender is an optional state machine that turns explicit
// begin/extend/end calls into a stream of RFC 4733 telephone-event packets.
//
// Durations are measured with the caller supplied clock rate, there is no
// assumption that the media clock runs at 8 kHz. RTP timestamps are always
// provided by the caller (via Begin) or derived from it, so the event stream
// stays aligned with the associated audio stream.
type TelephoneEventSender struct {
	clockRate          uint32
	now                func() time.Time
	maxDuration        uint32
	endRetransmissions int

	active         bool
	event          uint8
	volume         uint8
	startTime      time.Time
	startTimestamp uint32
}

// NewTelephoneEventSender creates a TelephoneEventSender for the given media
// clock rate (e.g. 8000, 16000 or 48000 Hz).
func NewTelephoneEventSender(clockRate uint32, options ...TelephoneEventSenderOption) *TelephoneEventSender {
	s := &TelephoneEventSender{
		clockRate:          clockRate,
		now:                time.Now,
		maxDuration:        DefaultTelephoneEventMaxDuration,
		endRetransmissions: DefaultTelephoneEventEndRetransmissions,
	}
	for _, option := range options {
		option(s)
	}

	return s
}

// Active reports whether an event is currently in progress.
func (s *TelephoneEventSender) Active() bool {
	return s.active
}

// Begin starts a new event. timestamp is the RTP timestamp of the start of
// the event in the caller's media clock. If a previous event is still active
// it is ended first (its end packets are prepended to the result).
func (s *TelephoneEventSender) Begin(event, volume uint8, timestamp uint32) []TelephoneEventTransmission {
	var out []TelephoneEventTransmission
	if s.active {
		out = append(out, s.End()...)
	}

	s.active = true
	s.event = event
	s.volume = volume
	s.startTime = s.now()
	s.startTimestamp = timestamp

	return append(out, TelephoneEventTransmission{
		Event:     event,
		Volume:    volume,
		Duration:  0,
		Timestamp: timestamp,
		Marker:    true,
	})
}

// Extend refreshes the in-progress event and returns the packet(s) to send.
// It should be called periodically (e.g. every 50 ms) for as long as the
// event lasts. If the event grew beyond the maximum expressible duration it
// is split into contiguous segments, see RFC 4733 section 2.5.1.5.
func (s *TelephoneEventSender) Extend() []TelephoneEventTransmission {
	if !s.active {
		return nil
	}

	return s.segment(s.elapsed(), false)
}

// End terminates the in-progress event. The returned packets carry the end
// bit; the final packet is followed by the configured number of redundant
// retransmissions. Calling End without an active event returns nil.
func (s *TelephoneEventSender) End() []TelephoneEventTransmission {
	if !s.active {
		return nil
	}

	out := s.segment(s.elapsed(), true)
	s.active = false

	return out
}

// elapsed returns the duration of the current segment in media clock units.
func (s *TelephoneEventSender) elapsed() uint32 {
	d := s.now().Sub(s.startTime)
	if d <= 0 {
		return 0
	}

	return uint32(int64(d) * int64(s.clockRate) / int64(time.Second))
}

// segment emits the packet for the current segment, splitting the event into
// contiguous segments when duration exceeds maxDuration. If final is true the
// last packet (and its retransmissions) carries the end bit.
func (s *TelephoneEventSender) segment(duration uint32, final bool) []TelephoneEventTransmission {
	var out []TelephoneEventTransmission

	for duration > s.maxDuration {
		// Close the current segment at the maximum expressible duration
		// without the end bit and immediately start the next segment.
		out = append(out, TelephoneEventTransmission{
			Event:     s.event,
			Volume:    s.volume,
			Duration:  uint16(s.maxDuration),
			Timestamp: s.startTimestamp,
		})
		s.startTimestamp += s.maxDuration
		s.startTime = s.startTime.Add(time.Duration(int64(time.Second) * int64(s.maxDuration) / int64(s.clockRate)))
		duration -= s.maxDuration
	}

	out = append(out, TelephoneEventTransmission{
		Event:     s.event,
		End:       final,
		Volume:    s.volume,
		Duration:  uint16(duration),
		Timestamp: s.startTimestamp,
	})

	if final {
		last := out[len(out)-1]
		for i := 0; i < s.endRetransmissions; i++ {
			out = append(out, last)
		}
	}

	return out
}
