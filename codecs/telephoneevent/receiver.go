// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package telephoneevent

import (
	"errors"
	"time"

	"github.com/pion/rtp"
)

const defaultTimeout = 100 * time.Millisecond

var (
	// ErrNilPacket is returned when HandlePacket receives nil.
	ErrNilPacket = errors.New("telephone-event RTP packet is nil")
	// ErrUnexpectedPayloadType is returned for another RTP payload type.
	ErrUnexpectedPayloadType = errors.New("unexpected RTP payload type")
)

// ChangeKind identifies a state-machine change delivered to the application.
type ChangeKind uint8

const (
	// EventStarted indicates one logical telephone-event press.
	EventStarted ChangeKind = iota + 1
	// EventEnded indicates one logical telephone-event release.
	EventEnded
)

// EndReason says how a logical event ended.
type EndReason uint8

const (
	// EndExplicit means the E bit was received.
	EndExplicit EndReason = iota + 1
	// EndTimeout means no update arrived before the configured timeout.
	EndTimeout
	// EndSuperseded means a newer event for the same SSRC replaced an unfinished event.
	EndSuperseded
)

// Change is the only press or release seen by the application.
type Change struct {
	Kind          ChangeKind
	SSRC          uint32
	Event         Event
	Volume        Volume
	RTPTimestamp  uint32
	Duration      time.Duration
	DurationUnits uint32
	EndReason     EndReason
}

// ReceiverConfig configures an RFC 4733 event receiver.
type ReceiverConfig struct {
	PayloadType      uint8
	ClockRate        uint32
	Timeout          time.Duration
	TombstoneTimeout time.Duration
	Now              func() time.Time
}

// Receiver collapses RFC 4733 update and repeated end packets into one press
// and one release per logical event.
type Receiver struct {
	payloadType      uint8
	clockRate        uint32
	timeout          time.Duration
	tombstoneTimeout time.Duration
	now              func() time.Time

	segments map[eventKey]*eventState
	active   map[uint32]eventKey
	latest   map[uint32]eventKey
}

type eventKey struct {
	ssrc      uint32
	event     Event
	timestamp uint32
}

type eventState struct {
	root         eventKey
	current      eventKey
	volume       Volume
	lastSequence uint16
	lastDuration uint32
	lastSeen     time.Time
	ended        bool
	endReason    EndReason
	hasEnd       bool
}

// NewReceiver creates a telephone-event receiver. ClockRate is the negotiated
// media sample clock and is not assumed to be 8 kHz.
func NewReceiver(config ReceiverConfig) (*Receiver, error) {
	if config.PayloadType > 127 {
		return nil, ErrInvalidPayloadType
	}

	if config.ClockRate == 0 {
		return nil, ErrInvalidClockRate
	}

	timeout := config.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}

	tombstoneTimeout := config.TombstoneTimeout
	if tombstoneTimeout == 0 {
		tombstoneTimeout = 2 * timeout
	}

	now := config.Now
	if now == nil {
		now = time.Now
	}

	return &Receiver{
		payloadType:      config.PayloadType,
		clockRate:        config.ClockRate,
		timeout:          timeout,
		tombstoneTimeout: tombstoneTimeout,
		now:              now,
		segments:         make(map[eventKey]*eventState),
		active:           make(map[uint32]eventKey),
		latest:           make(map[uint32]eventKey),
	}, nil
}

// HandlePacket processes one RTP packet. It returns only logical press/release
// transitions; repeated and out-of-order update packets are suppressed.
func (r *Receiver) HandlePacket(packet *rtp.Packet) ([]Change, error) {
	if packet == nil {
		return nil, ErrNilPacket
	}

	if packet.PayloadType != r.payloadType {
		return nil, ErrUnexpectedPayloadType
	}

	payload, err := UnmarshalPayload(packet.Payload)
	if err != nil {
		return nil, err
	}

	now := r.now()
	key := eventKey{
		ssrc:      packet.SSRC,
		event:     payload.Event,
		timestamp: packet.Timestamp,
	}

	if latestKey, ok := r.latest[key.ssrc]; ok && isOlderTimestamp(key.timestamp, latestKey.timestamp) {
		if state := r.segments[key]; state != nil {
			state.lastSeen = now
		}

		return nil, nil
	}

	if state := r.segments[key]; state != nil {
		return r.updateState(state, packet, payload, now), nil
	}

	if root := r.findContinuation(packet, key); root != nil {
		if root.ended {
			return nil, nil
		}

		r.segments[key] = root

		return r.updateContinuation(root, key, packet, payload, now), nil
	}

	changes, ok := r.supersede(key)
	if !ok {
		return nil, nil
	}

	state := &eventState{
		root:         key,
		current:      key,
		volume:       payload.Volume,
		lastSequence: packet.SequenceNumber,
		lastDuration: uint32(payload.Duration),
		lastSeen:     now,
	}
	r.segments[key] = state
	r.latest[key.ssrc] = key

	changes = append(changes, Change{
		Kind:          EventStarted,
		SSRC:          key.ssrc,
		Event:         payload.Event,
		Volume:        payload.Volume,
		RTPTimestamp:  packet.Timestamp,
		Duration:      samplesToDuration(uint32(payload.Duration), r.clockRate),
		DurationUnits: uint32(payload.Duration),
	})

	if !payload.End {
		r.active[key.ssrc] = key

		return changes, nil
	}

	r.finishState(state, packet, payload, EndExplicit)
	changes = append(changes, r.endChange(state.root, state))

	return changes, nil
}

// Expire infers releases for active events with no recent packet and removes
// old tombstones. Timeout-inferred releases use EndTimeout, never EndExplicit.
func (r *Receiver) Expire() []Change {
	return r.ExpireAt(r.now())
}

// ExpireAt is Expire with an explicitly supplied virtual clock value.
func (r *Receiver) ExpireAt(now time.Time) []Change {
	var changes []Change

	for key, state := range r.segments {
		age := now.Sub(state.lastSeen)

		if state.ended {
			if age >= r.tombstoneTimeout {
				delete(r.segments, key)
				if r.latest[key.ssrc] == key {
					delete(r.latest, key.ssrc)
				}
			}

			continue
		}

		if age < r.timeout {
			continue
		}

		state.ended = true
		state.endReason = EndTimeout
		state.hasEnd = true
		state.lastSeen = now
		delete(r.active, key.ssrc)

		changes = append(changes, r.endChange(state.root, state))
	}

	return changes
}

func (r *Receiver) updateState(
	state *eventState,
	packet *rtp.Packet,
	payload Payload,
	now time.Time,
) []Change {
	return r.updateStateAt(state, state.current, packet, payload, now)
}

func (r *Receiver) updateContinuation(
	state *eventState,
	key eventKey,
	packet *rtp.Packet,
	payload Payload,
	now time.Time,
) []Change {
	current := state.current
	if timestampDistance(current.timestamp, key.timestamp) == MaxDuration {
		current = key
	}

	return r.updateStateAt(state, current, packet, payload, now)
}

func (r *Receiver) updateStateAt(
	state *eventState,
	current eventKey,
	packet *rtp.Packet,
	payload Payload,
	now time.Time,
) []Change {
	absoluteDuration := r.logicalDuration(state.root, payload.Event, packet.Timestamp, packet.SSRC, payload.Duration)
	if !r.isNewer(state, packet, absoluteDuration, payload.End) {
		state.lastSeen = now

		return nil
	}

	state.lastSeen = now
	state.lastSequence = packet.SequenceNumber
	state.lastDuration = absoluteDuration
	state.current = current

	if !payload.End {
		if !state.ended {
			r.active[packet.SSRC] = state.root
		}

		return nil
	}

	if state.ended {
		return nil
	}

	r.finishState(state, packet, payload, EndExplicit)

	return []Change{r.endChange(state.root, state)}
}

func (r *Receiver) finishState(state *eventState, packet *rtp.Packet, payload Payload, reason EndReason) {
	state.ended = true
	state.endReason = reason
	state.hasEnd = true
	state.lastSequence = packet.SequenceNumber
	state.lastDuration = r.logicalDuration(state.root, payload.Event, packet.Timestamp, packet.SSRC, payload.Duration)
	delete(r.active, packet.SSRC)
}

func (r *Receiver) findContinuation(packet *rtp.Packet, key eventKey) *eventState {
	activeKey, ok := r.active[key.ssrc]
	if !ok || activeKey.event != key.event {
		return nil
	}

	root := r.segments[activeKey]
	if root == nil || root.ended {
		return nil
	}

	offset := timestampDistance(root.root.timestamp, key.timestamp)
	if !packet.Marker || offset == 0 || offset%MaxDuration != 0 {
		return nil
	}

	return root
}

func (r *Receiver) isNewer(
	state *eventState,
	packet *rtp.Packet,
	duration uint32,
	end bool,
) bool {
	if duration > state.lastDuration {
		return true
	}

	if duration < state.lastDuration {
		return false
	}

	if end && !state.hasEnd {
		return true
	}

	return sequenceDistance(state.lastSequence, packet.SequenceNumber) > 0
}

func (r *Receiver) logicalDuration(
	root eventKey,
	event Event,
	timestamp uint32,
	ssrc uint32,
	segmentDuration uint16,
) uint32 {
	if root.ssrc != ssrc || root.event != event {
		return uint32(segmentDuration)
	}

	offset := timestampDistance(root.timestamp, timestamp)

	return offset + uint32(segmentDuration)
}

func (r *Receiver) endChange(key eventKey, state *eventState) Change {
	return Change{
		Kind:          EventEnded,
		SSRC:          key.ssrc,
		Event:         key.event,
		Volume:        state.volume,
		RTPTimestamp:  key.timestamp,
		Duration:      samplesToDuration(state.lastDuration, r.clockRate),
		DurationUnits: state.lastDuration,
		EndReason:     state.endReason,
	}
}

func (r *Receiver) supersede(key eventKey) ([]Change, bool) {
	activeKey, ok := r.active[key.ssrc]
	if !ok {
		return nil, true
	}

	state := r.segments[activeKey]
	if state == nil {
		delete(r.active, key.ssrc)

		return nil, true
	}

	if activeKey == key {
		return nil, true
	}

	if activeKey.event == key.event && activeKey.timestamp == key.timestamp {
		return nil, false
	}

	if isOlderTimestamp(key.timestamp, activeKey.timestamp) {
		return nil, false
	}

	delete(r.active, key.ssrc)
	state.ended = true
	state.endReason = EndSuperseded
	state.hasEnd = true

	return []Change{{
		Kind:          EventEnded,
		SSRC:          key.ssrc,
		Event:         activeKey.event,
		Volume:        state.volume,
		RTPTimestamp:  activeKey.timestamp,
		Duration:      samplesToDuration(state.lastDuration, r.clockRate),
		DurationUnits: state.lastDuration,
		EndReason:     EndSuperseded,
	}}, true
}

func isOlderTimestamp(candidate uint32, current uint32) bool {
	distance := current - candidate

	return distance > 0 && distance < 1<<31
}

func timestampDistance(from uint32, to uint32) uint32 {
	return to - from
}

func sequenceDistance(previous uint16, current uint16) uint16 {
	return current - previous
}
