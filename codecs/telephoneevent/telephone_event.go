// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

// Package telephoneevent implements RFC 4733 telephone-event payloads.
package telephoneevent

import (
	"encoding/binary"
	"errors"
	"math"
	"time"
)

const (
	// MaxDuration is the largest duration that fits in an RFC 4733 payload.
	MaxDuration = 1<<16 - 1

	maxVolume     = 1<<4 - 1
	endRedundancy = 3
)

var (
	// ErrShortPayload is returned when a payload does not contain four bytes.
	ErrShortPayload = errors.New("telephone-event payload is too short")
	// ErrInvalidEvent is returned for an unsupported event code.
	ErrInvalidEvent = errors.New("invalid telephone-event code")
	// ErrInvalidVolume is returned for a volume outside the 0-15 range.
	ErrInvalidVolume = errors.New("invalid telephone-event volume")
	// ErrInvalidClockRate is returned for a zero sample clock rate.
	ErrInvalidClockRate = errors.New("telephone-event clock rate must be greater than zero")
)

// Event is an RFC 4733 telephone-event code.
type Event uint8

const (
	Digit0 Event = iota
	Digit1
	Digit2
	Digit3
	Digit4
	Digit5
	Digit6
	Digit7
	Digit8
	Digit9
	DigitStar
	DigitPound
	DigitA
	DigitB
	DigitC
	DigitD
	EventFlash
)

// Volume is the 0-15 DTMF power level expressed as -dBm0.
type Volume uint8

// Payload represents a four-byte RFC 4733 telephone-event payload.
type Payload struct {
	Event    Event
	Volume   Volume
	End      bool
	Duration uint16
}

// Marshal encodes the telephone-event payload.
func (p Payload) Marshal() ([]byte, error) {
	if p.Event > EventFlash {
		return nil, ErrInvalidEvent
	}

	if p.Volume > maxVolume {
		return nil, ErrInvalidVolume
	}

	buf := make([]byte, 4)
	buf[0] = byte(p.Event)  //nolint:gosec // G115
	buf[1] = byte(p.Volume) //nolint:gosec // G115
	if p.End {
		buf[1] |= 0x80
	}
	binary.BigEndian.PutUint16(buf[2:], p.Duration)

	return buf, nil
}

// UnmarshalPayload decodes a four-byte RFC 4733 telephone-event payload.
func UnmarshalPayload(payload []byte) (Payload, error) {
	if payload == nil {
		return Payload{}, ErrShortPayload
	}

	if len(payload) < 4 {
		return Payload{}, ErrShortPayload
	}

	event := Event(payload[0])
	if event > EventFlash {
		return Payload{}, ErrInvalidEvent
	}

	return Payload{
		Event:    event,
		End:      payload[1]&0x80 != 0,
		Volume:   Volume(payload[1] & 0x0f),
		Duration: binary.BigEndian.Uint16(payload[2:4]),
	}, nil
}

// Duration returns the payload duration using the caller-provided sample clock.
func (p Payload) DurationTime(clockRate uint32) (time.Duration, error) {
	if clockRate == 0 {
		return 0, ErrInvalidClockRate
	}

	return samplesToDuration(uint32(p.Duration), clockRate), nil
}

func samplesToDuration(samples uint32, clockRate uint32) time.Duration {
	return time.Duration(uint64(samples) * uint64(time.Second) / uint64(clockRate))
}

func durationToSamples(duration time.Duration, clockRate uint32) (uint32, error) {
	if clockRate == 0 {
		return 0, ErrInvalidClockRate
	}

	if duration < 0 {
		return 0, ErrInvalidSamples
	}

	samples := duration.Seconds() * float64(clockRate)
	if samples < 0 || samples > math.MaxUint32 {
		return 0, ErrInvalidSamples
	}

	return uint32(samples + 0.5), nil
}
