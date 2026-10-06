// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtp

import "github.com/pion/rtp/codecs"

// redHistoryEntry is a primary block kept around for retransmission as a
// redundant block in later RED packets.
type redHistoryEntry struct {
	payloadType uint8
	timestamp   uint32
	payload     []byte
}

// REDPacketizer wraps another Packetizer and emits RFC 2198 RED audio packets.
//
// Every emitted packet carries the current frame as its primary block and may
// carry up to MaxRedundancy previous frames as redundant blocks. The bounded
// history only retains the most recent primary blocks.
//
// When the MTU or the 10-bit block length / 30-bit timestamp offset field
// widths are exceeded, redundant blocks are dropped by an explicit policy:
// blocks are considered newest-first and every selected block must fit in its
// entirety, so older redundancy is shed first and a primary block that does
// not fit the MTU is still sent, never truncated.
type REDPacketizer struct {
	// Inner builds the plain media packets. Its PayloadType is used for the
	// primary and redundant blocks; the emitted RTP packets use REDPayloadType.
	Inner Packetizer
	// REDPayloadType is the RTP payload type written on emitted packets.
	REDPayloadType uint8
	// MaxRedundancy bounds the number of redundant blocks per packet and the
	// number of history entries retained.
	MaxRedundancy int
	// MTU bounds the serialized RTP packet, including its header.
	MTU uint16

	history []redHistoryEntry
}

// NewREDPacketizer returns a REDPacketizer wrapping inner.
func NewREDPacketizer(
	mtu uint16,
	redPayloadType uint8,
	maxRedundancy int,
	inner Packetizer,
) *REDPacketizer {
	if maxRedundancy < 0 {
		maxRedundancy = 0
	}

	return &REDPacketizer{
		Inner:          inner,
		REDPayloadType: redPayloadType,
		MaxRedundancy:  maxRedundancy,
		MTU:            mtu,
		history:        make([]redHistoryEntry, 0, maxRedundancy),
	}
}

// Packetize packetizes the payload through the inner Packetizer and rewraps
// each resulting packet as an RFC 2198 RED packet with bounded redundancy.
func (p *REDPacketizer) Packetize(payload []byte, samples uint32) []*Packet {
	packets := p.Inner.Packetize(payload, samples)
	if len(packets) == 0 {
		return packets
	}

	for _, packet := range packets {
		primary := redHistoryEntry{
			payloadType: packet.PayloadType,
			timestamp:   packet.Timestamp,
			payload:     packet.Payload,
		}

		blocks := p.selectBlocks(primary, packet.Header.MarshalSize())

		redPayload, err := codecs.REDEncode(blocks)
		if err != nil {
			// Encoding never fails for the valid primary block; keep the
			// unwrapped packet rather than dropping audio.
			p.pushHistory(primary)

			continue
		}

		packet.PayloadType = p.REDPayloadType
		packet.Payload = redPayload
		p.pushHistory(primary)
	}

	return packets
}

// GeneratePadding forwards to the inner Packetizer. Padding packets are not
// wrapped with redundancy.
func (p *REDPacketizer) GeneratePadding(samples uint32) []*Packet {
	return p.Inner.GeneratePadding(samples)
}

// EnableAbsSendTime forwards to the inner Packetizer.
func (p *REDPacketizer) EnableAbsSendTime(value int) {
	p.Inner.EnableAbsSendTime(value)
}

// SkipSamples forwards to the inner Packetizer.
func (p *REDPacketizer) SkipSamples(skippedSamples uint32) {
	p.Inner.SkipSamples(skippedSamples)
}

// selectBlocks chooses the redundant blocks that fit beside primary, ordered
// oldest-first for the wire, followed by the primary block.
func (p *REDPacketizer) selectBlocks(primary redHistoryEntry, headerSize int) []codecs.REDBlock {
	// Budget for the RTP payload. When the primary alone exceeds the MTU the
	// budget goes negative; redundancy is simply disabled for that packet.
	budget := int(p.MTU) - headerSize

	primarySize := codecs.REDPrimaryHeaderSize + len(primary.payload)

	// Newest-first selection so overflow sheds the oldest redundancy first.
	selected := make([]codecs.REDBlock, 0, p.MaxRedundancy+1)

	for i := len(p.history) - 1; i >= 0 && len(selected) < p.MaxRedundancy; i-- {
		entry := p.history[i]

		offset := primary.timestamp - entry.timestamp
		if offset == 0 {
			continue
		}

		if len(entry.payload) > codecs.REDMaxBlockLength ||
			offset > codecs.REDMaxTimestampOffset {
			// Field width cannot represent this block; apply the drop
			// policy rather than failing the whole packet.
			continue
		}

		block := codecs.REDBlock{
			PayloadType:     entry.payloadType,
			TimestampOffset: offset,
			Payload:         entry.payload,
		}

		size := redBlockWireSize(block)

		// Account for the primary block and already selected blocks.
		used := primarySize + size
		for j := range selected {
			used += redBlockWireSize(selected[j])
		}

		if used > budget {
			continue
		}

		selected = append(selected, block)
	}

	// Reverse to chronological (oldest-first) wire order.
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}

	return append(selected, codecs.REDBlock{
		PayloadType: primary.payloadType,
		Payload:     primary.payload,
		Marker:      true,
	})
}

func redBlockWireSize(block codecs.REDBlock) int {
	if block.TimestampOffset > codecs.REDMaxShortTimestampOffset {
		return codecs.REDLongHeaderSize + len(block.Payload)
	}

	return codecs.REDShortHeaderSize + len(block.Payload)
}

func (p *REDPacketizer) pushHistory(entry redHistoryEntry) {
	if p.MaxRedundancy == 0 {
		p.history = p.history[:0]

		return
	}

	p.history = append(p.history, entry)
	if len(p.history) > p.MaxRedundancy {
		p.history = p.history[len(p.history)-p.MaxRedundancy:]
	}
}
