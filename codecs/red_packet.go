// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package codecs

import "encoding/binary"

// RED (RFC 2198) header sizes and field widths.
const (
	// REDPrimaryHeaderSize is the size in bytes of the one-byte primary header.
	REDPrimaryHeaderSize = 1
	// REDShortHeaderSize is the size in bytes of a redundant header with the
	// F bit clear (14-bit timestamp offset).
	REDShortHeaderSize = 4
	// REDLongHeaderSize is the size in bytes of a redundant header with the
	// F bit set (30-bit timestamp offset).
	REDLongHeaderSize = 6

	redFlagBit        = 0x80
	redPayloadTypeBit = 0x7F
	redBlockLenBits   = 10
	redBlockLenMask   = 0x3FF

	// REDMaxBlockLength is the maximum payload length in bytes of a redundant
	// block. The block length field is 10 bits wide.
	REDMaxBlockLength = 1<<redBlockLenBits - 1 // 1023
	// REDMaxShortTimestampOffset is the largest offset that fits in a header
	// with the F bit clear.
	REDMaxShortTimestampOffset = 1<<14 - 1 // 16383
	// REDMaxTimestampOffset is the largest offset that fits in a header with
	// the F bit set.
	REDMaxTimestampOffset = 1<<30 - 1
)

// REDBlock is one media block carried inside an RFC 2198 RED payload.
//
// Redundant blocks use TimestampOffset (the primary timestamp minus the block
// timestamp, computed in unsigned 32-bit arithmetic) and a zero Timestamp.
// The primary block is marked by Marker, has a zero TimestampOffset and carries
// the current frame in Payload.
type REDBlock struct {
	PayloadType     uint8
	TimestampOffset uint32
	Timestamp       uint32
	Payload         []byte
	Marker          bool
}

// REDEncode serializes blocks into an RFC 2198 RED payload.
//
// Redundant blocks must be passed first in chronological order (oldest first)
// and the primary block last. The primary block's TimestampOffset is ignored.
// Redundant blocks are limited by the 10-bit length and 30-bit timestamp
// offset fields; the primary block is not length limited on the wire.
func REDEncode(blocks []REDBlock) ([]byte, error) {
	size, err := REDMarshalSize(blocks)
	if err != nil {
		return nil, err
	}

	payload := make([]byte, size)

	pos := 0
	for i := range blocks {
		block := &blocks[i]

		if i == len(blocks)-1 {
			// Primary block: a single header byte with the F bit clear.
			payload[pos] = block.PayloadType
			pos++

			continue
		}

		length := len(block.Payload)

		switch offset := block.TimestampOffset; {
		case offset <= REDMaxShortTimestampOffset:
			// F=0: PT byte followed by 14-bit timestamp offset and 10-bit length.
			value := offset<<redBlockLenBits | uint32(length)
			payload[pos] = block.PayloadType
			payload[pos+1] = byte(value >> 16)
			payload[pos+2] = byte(value >> 8)
			payload[pos+3] = byte(value)
			pos += REDShortHeaderSize
		default:
			// F=1: PT byte followed by 30-bit timestamp offset and 10-bit length.
			value := offset<<2 | uint32(length>>8)
			payload[pos] = redFlagBit | block.PayloadType
			binary.BigEndian.PutUint32(payload[pos+1:], value)
			payload[pos+5] = byte(length)
			pos += REDLongHeaderSize
		}
	}

	// The data section holds the primary data first, followed by the
	// redundant block data in header order.
	pos += copy(payload[pos:], blocks[len(blocks)-1].Payload)

	for i := 0; i < len(blocks)-1; i++ {
		pos += copy(payload[pos:], blocks[i].Payload)
	}

	return payload, nil
}

// REDMarshalSize returns the serialized size of blocks without encoding them,
// and validates the redundant block field widths.
func REDMarshalSize(blocks []REDBlock) (int, error) {
	if len(blocks) == 0 {
		return 0, errREDNoBlocks
	}

	size := 0
	for i := range blocks {
		block := &blocks[i]

		if block.PayloadType > redPayloadTypeBit {
			return 0, errREDPayloadType
		}

		if i == len(blocks)-1 {
			size += REDPrimaryHeaderSize + len(block.Payload)

			continue
		}

		if len(block.Payload) > REDMaxBlockLength {
			return 0, errREDBlockLength
		}

		switch offset := block.TimestampOffset; {
		case offset == 0:
			return 0, errREDZeroTimestampOffset
		case offset <= REDMaxShortTimestampOffset:
			size += REDShortHeaderSize + len(block.Payload)
		case offset <= REDMaxTimestampOffset:
			size += REDLongHeaderSize + len(block.Payload)
		default:
			return 0, errREDTimestampOffset
		}
	}

	return size, nil
}

// REDDecode parses an RFC 2198 RED payload.
//
// Redundant blocks are returned first (oldest first) and the primary block
// last with Marker set. Payload slices alias the input buffer. TimestampOffset
// is filled for every redundant block; callers recover the original RTP
// timestamp with unsigned subtraction (see BlocksWithTimestamp).
//
// The header section carries no block count (RFC 2198 section 8). The boundary
// between redundant headers and the one-byte primary header is located with the
// length-fit rule: an F=0 byte starts a redundant header only while the declared
// block data would still leave room for the primary header; otherwise it is the
// primary header itself. This greedy parse is what deployed RED receivers use.
func REDDecode(payload []byte) ([]REDBlock, error) {
	if payload == nil {
		return nil, errNilPacket
	}

	blocks := make([]REDBlock, 0, 1)
	lengths := make([]int, 0, 1)
	pos := 0
	reserved := 0

	for {
		if pos >= len(payload) {
			return nil, errShortPacket
		}

		first := payload[pos]

		if first&redFlagBit != 0 {
			// F=1: a 6-byte redundant header.
			if pos+REDLongHeaderSize+reserved >= len(payload) {
				return nil, errShortPacket
			}

			value := binary.BigEndian.Uint32(payload[pos+1 : pos+5])
			length := int(value&0x3)<<8 | int(payload[pos+5])
			if pos+REDLongHeaderSize+reserved+length >= len(payload) {
				return nil, errShortPacket
			}

			blocks = append(blocks, REDBlock{
				PayloadType:     first & redPayloadTypeBit,
				TimestampOffset: value >> 2,
			})
			lengths = append(lengths, length)
			pos += REDLongHeaderSize
			reserved += length

			continue
		}

		// F=0: a 4-byte redundant header whenever its declared data leaves
		// room for the trailing primary header; otherwise this byte terminates
		// the header section as the primary header.
		if pos+REDShortHeaderSize+reserved >= len(payload) {
			break
		}

		value := uint32(payload[pos+1])<<16 |
			uint32(payload[pos+2])<<8 |
			uint32(payload[pos+3])
		length := int(value & redBlockLenMask)

		if pos+REDShortHeaderSize+reserved+length >= len(payload) {
			break
		}

		blocks = append(blocks, REDBlock{
			PayloadType:     first & redPayloadTypeBit,
			TimestampOffset: value >> redBlockLenBits,
		})
		lengths = append(lengths, length)
		pos += REDShortHeaderSize
		reserved += length
	}

	primaryLength := len(payload) - pos - REDPrimaryHeaderSize - reserved
	if primaryLength < 0 {
		return nil, errShortPacket
	}

	blocks = append(blocks, REDBlock{
		PayloadType: payload[pos] & redPayloadTypeBit,
		Marker:      true,
	})
	lengths = append(lengths, primaryLength)
	pos += REDPrimaryHeaderSize

	// Walk the data section: primary data precedes the redundant block data.
	dataPos := pos
	primaryIndex := len(blocks) - 1

	blocks[primaryIndex].Payload = payload[dataPos : dataPos+lengths[primaryIndex]]
	dataPos += lengths[primaryIndex]

	for i := 0; i < primaryIndex; i++ {
		blocks[i].Payload = payload[dataPos : dataPos+lengths[i]]
		dataPos += lengths[i]
	}

	return blocks, nil
}

// REDPacket represents an RFC 2198 RED payload stored in an RTP packet.
type REDPacket struct {
	// Blocks holds redundant blocks first and the primary block last after a
	// successful Unmarshal.
	Blocks []REDBlock

	audioDepacketizer
}

// Unmarshal parses the RED payload and returns the primary block's payload.
// The full block set, including redundant blocks and their timestamp offsets,
// is available via Blocks and BlocksWithTimestamp. RED is transport only: it
// does not decode the encapsulated codec (for example Opus).
func (p *REDPacket) Unmarshal(packet []byte) ([]byte, error) {
	if packet == nil {
		return nil, errNilPacket
	}

	blocks, err := REDDecode(packet)
	if err != nil {
		return nil, err
	}

	p.Blocks = blocks

	return blocks[len(blocks)-1].Payload, nil
}

// BlocksWithTimestamp returns every media block with its original RTP
// timestamp, given the timestamp of the RTP packet carrying the RED payload.
// The result is computed in unsigned 32-bit arithmetic, so timestamp
// wraparound is handled automatically.
func (p *REDPacket) BlocksWithTimestamp(timestamp uint32) []REDBlock {
	blocks := make([]REDBlock, len(p.Blocks))

	for i := range p.Blocks {
		blocks[i] = p.Blocks[i]
		if blocks[i].Marker {
			blocks[i].Timestamp = timestamp
		} else {
			blocks[i].Timestamp = timestamp - blocks[i].TimestampOffset
		}
	}

	return blocks
}
