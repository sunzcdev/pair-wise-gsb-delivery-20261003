// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package codecs

const (
	// TelephoneEventPayloadSize is the size in bytes of an RFC 4733 telephone-event payload.
	TelephoneEventPayloadSize = 4

	telephoneEventMaxVolume = 0x3F
)

// TelephoneEvent represents a single RFC 4733 (telephone-event) payload.
//
// The wire format is:
//
//	 0                   1                   2                   3
//	 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|     event     |E|R| volume    |          duration             |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//
// Duration is expressed in units of the media clock, which is provided by
// the caller (commonly, but not necessarily, 8000 Hz).
type TelephoneEvent struct {
	// Event is the event code, e.g. 0-15 for DTMF digits 0-9, *, #, A-D.
	Event uint8
	// End indicates the final packet of the event (the E bit).
	End bool
	// Volume is the volume of the tone, 0-63 (-63 dBm0 to 0 dBm0).
	Volume uint8
	// Duration of the event so far, in media clock units.
	Duration uint16
}

// Marshal serializes the TelephoneEvent into its 4 byte wire representation.
func (t TelephoneEvent) Marshal() []byte {
	out := make([]byte, TelephoneEventPayloadSize)
	out[0] = t.Event
	if t.End {
		out[1] = 0x80
	}
	out[1] |= t.Volume & telephoneEventMaxVolume
	out[2] = byte(t.Duration >> 8)
	out[3] = byte(t.Duration)

	return out
}

// Unmarshal parses the passed byte slice and stores the result in the
// TelephoneEvent this method is called upon.
func (t *TelephoneEvent) Unmarshal(packet []byte) error {
	if packet == nil {
		return errNilPacket
	} else if len(packet) < TelephoneEventPayloadSize {
		return errShortPacket
	}

	t.Event = packet[0]
	t.End = packet[1]&0x80 != 0
	t.Volume = packet[1] & telephoneEventMaxVolume
	t.Duration = uint16(packet[2])<<8 | uint16(packet[3])

	return nil
}

// TelephoneEventPayloader payloads telephone-event packets.
type TelephoneEventPayloader struct{}

// Payload fragments a telephone-event payload across one or more byte arrays.
// A telephone-event payload always fits in a single packet.
func (p *TelephoneEventPayloader) Payload(_ uint16, payload []byte) [][]byte {
	if payload == nil {
		return [][]byte{}
	}

	out := make([]byte, len(payload))
	copy(out, payload)

	return [][]byte{out}
}

// TelephoneEventPacket represents the telephone-event payload of an RTP Packet.
type TelephoneEventPacket struct {
	Event   TelephoneEvent
	Payload []byte

	audioDepacketizer
}

// Unmarshal parses the passed byte slice and stores the result in the
// TelephoneEventPacket this method is called upon.
func (p *TelephoneEventPacket) Unmarshal(packet []byte) ([]byte, error) {
	if err := p.Event.Unmarshal(packet); err != nil {
		return nil, err
	}

	p.Payload = packet[:TelephoneEventPayloadSize]

	return p.Payload, nil
}
