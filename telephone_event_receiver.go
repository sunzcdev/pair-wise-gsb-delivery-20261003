// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtp

import (
	"time"

	"github.com/pion/rtp/codecs"
)

// DefaultTelephoneEventTimeout is the default time after which an event that
// stopped receiving updates is considered over. RFC 4733 senders typically
// refresh events every 50 ms.
const DefaultTelephoneEventTimeout = 200 * time.Millisecond

// TelephoneEventNotificationKind distinguishes begin and end notifications.
type TelephoneEventNotificationKind uint8

const (
	// TelephoneEventBegin is emitted exactly once when a new event is detected.
	TelephoneEventBegin TelephoneEventNotificationKind = iota
	// TelephoneEventEnd is emitted exactly once when an event finishes.
	TelephoneEventEnd
)

// TelephoneEventEndReason describes how the end of an event was determined.
type TelephoneEventEndReason uint8

const (
	// TelephoneEventEndNone is used for begin notifications.
	TelephoneEventEndNone TelephoneEventEndReason = iota
	// TelephoneEventEndExplicit means a packet with the end bit was received.
	TelephoneEventEndExplicit
	// TelephoneEventEndTimeout means the event stopped being refreshed and
	// its end was inferred; no packet with the end bit was ever received.
	TelephoneEventEndTimeout
)

// TelephoneEventNotification is emitted by the TelephoneEventReceiver exactly
// once per event begin and once per event end, no matter how many update or
// end packets were received for the event.
type TelephoneEventNotification struct {
	Kind      TelephoneEventNotificationKind
	SSRC      uint32
	Event     uint8
	Volume    uint8
	Timestamp uint32 // RTP timestamp of the start of the event
	Duration  uint32 // duration in media clock units (may exceed 0xFFFF for segmented events)
	Reason    TelephoneEventEndReason
}

// TelephoneEventReceiverOption configures a TelephoneEventReceiver.
type TelephoneEventReceiverOption func(*TelephoneEventReceiver)

// WithTelephoneEventReceiverClock sets the time source used for timeout
// detection. Defaults to time.Now. Tests can provide a virtual clock.
func WithTelephoneEventReceiverClock(now func() time.Time) TelephoneEventReceiverOption {
	return func(r *TelephoneEventReceiver) {
		r.now = now
	}
}

// WithTelephoneEventTimeout sets how long an event may go without updates
// before its end is inferred. Defaults to DefaultTelephoneEventTimeout.
func WithTelephoneEventTimeout(timeout time.Duration) TelephoneEventReceiverOption {
	return func(r *TelephoneEventReceiver) {
		r.timeout = timeout
	}
}

type telephoneEventActiveKey struct {
	ssrc  uint32
	event uint8
}

type telephoneEventKey struct {
	ssrc      uint32
	event     uint8
	timestamp uint32
}

type telephoneEventState struct {
	ssrc           uint32
	event          uint8
	volume         uint8
	startTimestamp uint32
	totalDuration  uint32
	lastSeen       time.Time
	ended          bool
	endedAt        time.Time
	endReason      TelephoneEventEndReason
}

// TelephoneEventReceiver is an optional state machine that collapses the
// redundant RFC 4733 packet stream into a single begin and a single end
// notification per event.
//
// Events are identified by SSRC, event code and the RTP timestamp of their
// first packet. Update and end packets may arrive multiple times or out of
// order. Contiguous segments of long events (same SSRC and event code, the
// new segment starting exactly where the previous segment's duration ends)
// are merged into one logical event.
type TelephoneEventReceiver struct {
	now     func() time.Time
	timeout time.Duration

	states map[telephoneEventKey]*telephoneEventState
	active map[telephoneEventActiveKey]*telephoneEventState // {ssrc, event} -> un-ended state
}

// NewTelephoneEventReceiver creates a TelephoneEventReceiver.
func NewTelephoneEventReceiver(options ...TelephoneEventReceiverOption) *TelephoneEventReceiver {
	r := &TelephoneEventReceiver{
		now:     time.Now,
		timeout: DefaultTelephoneEventTimeout,
		states:  map[telephoneEventKey]*telephoneEventState{},
		active:  map[telephoneEventActiveKey]*telephoneEventState{},
	}
	for _, option := range options {
		option(r)
	}

	return r
}

// ProcessPacket feeds one received RTP packet carrying a telephone-event
// payload into the state machine and returns the notifications it triggers.
// Packets for already ended events are ignored.
func (r *TelephoneEventReceiver) ProcessPacket(pkt *Packet) ([]TelephoneEventNotification, error) {
	var payload codecs.TelephoneEvent
	if err := payload.Unmarshal(pkt.Payload); err != nil {
		return nil, err
	}

	now := r.now()
	key := telephoneEventKey{ssrc: pkt.SSRC, event: payload.Event, timestamp: pkt.Timestamp}

	if state, ok := r.states[key]; ok {
		return r.update(state, pkt.Timestamp, &payload, now), nil
	}

	// A segment of a long event starts exactly where the previous segment's
	// duration ends; merge it into the ongoing logical event.
	activeKey := telephoneEventActiveKey{pkt.SSRC, payload.Event}
	if state, ok := r.active[activeKey]; ok &&
		pkt.Timestamp == state.startTimestamp+state.totalDuration {
		r.states[key] = state

		return r.update(state, pkt.Timestamp, &payload, now), nil
	}

	state := &telephoneEventState{
		ssrc:           pkt.SSRC,
		event:          payload.Event,
		volume:         payload.Volume,
		startTimestamp: pkt.Timestamp,
		lastSeen:       now,
	}
	r.states[key] = state
	r.active[activeKey] = state

	out := []TelephoneEventNotification{{
		Kind:      TelephoneEventBegin,
		SSRC:      state.ssrc,
		Event:     state.event,
		Volume:    state.volume,
		Timestamp: state.startTimestamp,
	}}

	return append(out, r.update(state, pkt.Timestamp, &payload, now)...), nil
}

// update folds one packet into an existing event state.
func (r *TelephoneEventReceiver) update(
	state *telephoneEventState, timestamp uint32, payload *codecs.TelephoneEvent, now time.Time,
) []TelephoneEventNotification {
	if state.ended {
		// Duplicate or reordered packets for a finished event.
		return nil
	}

	state.lastSeen = now
	if duration := timestamp - state.startTimestamp + uint32(payload.Duration); duration > state.totalDuration {
		state.totalDuration = duration
	}

	if !payload.End {
		return nil
	}

	state.ended = true
	state.endedAt = now
	state.endReason = TelephoneEventEndExplicit
	delete(r.active, telephoneEventActiveKey{state.ssrc, state.event})

	return []TelephoneEventNotification{{
		Kind:      TelephoneEventEnd,
		SSRC:      state.ssrc,
		Event:     state.event,
		Volume:    state.volume,
		Timestamp: state.startTimestamp,
		Duration:  state.totalDuration,
		Reason:    state.endReason,
	}}
}

// Expire ends events that have not been refreshed within the configured
// timeout and returns the resulting notifications. Their Reason is
// TelephoneEventEndTimeout, distinguishing them from events terminated by an
// explicit end bit. It also purges states that ended long enough ago that no
// further retransmissions are expected.
func (r *TelephoneEventReceiver) Expire() []TelephoneEventNotification {
	now := r.now()
	var out []TelephoneEventNotification

	for key, state := range r.states {
		switch {
		case state.ended:
			if now.Sub(state.endedAt) > r.timeout {
				delete(r.states, key)
			}
		case now.Sub(state.lastSeen) > r.timeout:
			state.ended = true
			state.endedAt = now
			state.endReason = TelephoneEventEndTimeout
			delete(r.active, telephoneEventActiveKey{state.ssrc, state.event})
			out = append(out, TelephoneEventNotification{
				Kind:      TelephoneEventEnd,
				SSRC:      state.ssrc,
				Event:     state.event,
				Volume:    state.volume,
				Timestamp: state.startTimestamp,
				Duration:  state.totalDuration,
				Reason:    state.endReason,
			})
		}
	}

	return out
}
