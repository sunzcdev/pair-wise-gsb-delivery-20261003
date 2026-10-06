// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package codecs

import "errors"

const (
	// redBlockTypeMask selects the 7-bit payload type of a RED header byte;
	// the high bit (F) indicates that another header follows.
	redBlockTypeMask = 0x7F

	// redFollowBit (F) is set on every header byte except the primary header.
	redFollowBit = 0x80

	// redTimestampOffsetMask selects the 14-bit timestamp offset field.
	redTimestampOffsetMask = 0x3FFF

	// redMaxBlockLength is the largest encodable redundant block payload
	// length; the block length field is 10 bits wide.
	redMaxBlockLength = 0x03FF

	// redMaxTimestampOffset is the largest encodable relative timestamp
	// offset; the field is 14 bits wide.
	redMaxTimestampOffset = 0x3FFF
)

var (
	errREDNoData           = errors.New("RED packet has no primary payload")
	errREDInvalidLength    = errors.New("RED block length exceeds remaining payload")
	errREDOffsetOutOfRange = errors.New("RED timestamp offset does not fit in 14 bits")
	errREDBlockTooLong     = errors.New("RED block payload does not fit in 10 bits")
)

// REDBlock is one redundant or primary block extracted from a RED packet.
type REDBlock struct {
	// PayloadType is the RTP payload type carried in the block header.
	PayloadType uint8

	// TimestampOffset is the unsigned 14-bit offset between the enclosing
	// RTP packet timestamp and this block's timestamp, as encoded on the
	// wire. It is always 0 for the primary block.
	TimestampOffset uint16

	// Payload is the encoded media of the block. It aliases the buffer
	// passed to Unmarshal and must not be retained beyond it without copying.
	Payload []byte

	// Primary reports whether this is the primary (last) block.
	Primary bool
}

// Timestamp returns the absolute RTP timestamp of the block. packetTimestamp
// is the timestamp of the enclosing RTP packet; subtraction is performed
// modulo 2^32 so blocks crossing the timestamp rollover decode correctly.
func (b REDBlock) Timestamp(packetTimestamp uint32) uint32 {
	return packetTimestamp - uint32(b.TimestampOffset)
}

// REDPacket represents an RFC 2198 redundant audio data (RED) RTP payload.
//
// A RED payload contains zero or more redundant blocks followed by exactly
// one primary block. Every block keeps its own payload type and length, and
// redundant blocks additionally carry a timestamp relative to the packet
// timestamp.
type REDPacket struct {
	// Redundancies holds the redundant blocks in wire (oldest to newest)
	// order. Unmarshal resets it before parsing.
	Redundancies []REDBlock

	// PrimaryBlock is the mandatory primary (last) block.
	PrimaryBlock REDBlock

	audioDepacketizer
}

// Unmarshal parses an RFC 2198 RED payload. It returns the primary payload so
// that REDPacket satisfies Depacketizer; every block, including redundant
// blocks and their relative timestamps, is also left on the receiver for loss
// recovery. The media payload is never decoded.
func (p *REDPacket) Unmarshal(packet []byte) ([]byte, error) {
	if packet == nil {
		return nil, errNilPacket
	}

	p.Redundancies = p.Redundancies[:0]
	p.PrimaryBlock = REDBlock{}

	type redHeader struct {
		payloadType     uint8
		timestampOffset uint16
		length          int
	}

	headers := make([]redHeader, 0, 1)
	pos := 0

	// Every header except the primary one has the F bit set and is four
	// bytes: F/PT, timestamp offset high byte, timestamp offset low 6 bits
	// plus length high 4 bits, length low 8 bits.
	for pos < len(packet) && packet[pos]&redFollowBit != 0 {
		if len(packet)-pos < 4 {
			return nil, errShortPacket
		}

		mixedByte := packet[pos+2]

		headers = append(headers, redHeader{
			payloadType: packet[pos] & redBlockTypeMask,
			// 14-bit timestamp offset: bits 14..7 in byte 1, bits 6..1 in
			// the top 6 bits of byte 2. 10-bit block length: the low 2 bits
			// of byte 2 are bits 9..8, byte 3 holds bits 7..0.
			timestampOffset: uint16(packet[pos+1])<<6 | uint16(mixedByte>>2),
			length:          int(mixedByte&0x03)<<8 | int(packet[pos+3]),
		})

		pos += 4
	}

	if pos >= len(packet) {
		return nil, errREDNoData
	}

	primaryType := packet[pos] & redBlockTypeMask
	dataPos := pos + 1

	// The data section follows the headers in the same order.
	for _, header := range headers {
		if dataPos+header.length > len(packet) {
			return nil, errREDInvalidLength
		}

		p.Redundancies = append(p.Redundancies, REDBlock{
			PayloadType:     header.payloadType,
			TimestampOffset: header.timestampOffset,
			Payload:         packet[dataPos : dataPos+header.length],
		})

		dataPos += header.length
	}

	p.PrimaryBlock = REDBlock{
		PayloadType: primaryType,
		Payload:     packet[dataPos:],
		Primary:     true,
	}

	if len(p.PrimaryBlock.Payload) == 0 {
		return nil, errREDNoData
	}

	return p.PrimaryBlock.Payload, nil
}

// Blocks returns every block in packet-timestamp order: the redundant blocks
// followed by the primary block. Pair each block with its absolute RTP
// timestamp via REDBlock.Timestamp(packetTimestamp) to identify frames that
// arrive more than once (once as primary, later as redundancy).
func (p *REDPacket) Blocks() []REDBlock {
	blocks := make([]REDBlock, 0, len(p.Redundancies)+1)
	blocks = append(blocks, p.Redundancies...)
	blocks = append(blocks, p.PrimaryBlock)

	return blocks
}
