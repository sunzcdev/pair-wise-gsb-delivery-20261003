// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package codecs

// redHistoryEntry is one previously sent primary block kept for redundancy.
type redHistoryEntry struct {
	payloadType uint8
	timestamp   uint32
	payload     []byte
}

// redInnerPayloader is the primary codec payloader. It mirrors rtp.Payloader
// without importing the parent package, which would create an import cycle.
// OpusPayloader and the other codec payloaders satisfy it.
type redInnerPayloader interface {
	Payload(mtu uint16, payload []byte) [][]byte
}

// REDPayloader is an RFC 2198 redundant audio data (RED) payloader.
//
// It wraps another Payloader (typically OpusPayloader) for the primary codec,
// keeps a bounded history of recent primary blocks, and prepends the still
// fitting redundant blocks to each outgoing packet. REDPayloader implements
// Payloader, so an existing single-codec Packetizer built with NewPacketizer
// keeps working unchanged; use PayloadAtTimestamp when the caller knows the
// RTP timestamp of the frame (recommended for RED).
type REDPayloader struct {
	// Payloader produces the primary blocks. It must not be nil.
	Payloader redInnerPayloader

	// PayloadType is the RTP payload type stored in every emitted block
	// (redundant and primary). RED requires blocks on a stream to share a
	// payload type when a single secondary codec is used.
	PayloadType uint8

	// MaxRedundancy bounds the history length, i.e. how many previous
	// primary blocks may be carried. Zero disables redundancy.
	MaxRedundancy int

	// FrameSamples is added to the running timestamp on every Payload call.
	// It is only used by the timestamp-less Payload method;
	// PayloadAtTimestamp ignores it.
	FrameSamples uint32

	timestamp uint32
	history   []redHistoryEntry
}

// Reset clears the redundancy history and sets the running timestamp used by
// Payload. It does not change configuration fields.
func (p *REDPayloader) Reset(timestamp uint32) {
	p.timestamp = timestamp
	p.history = p.history[:0]
}

// Payload implements Payloader for compatibility with NewPacketizer. It
// advances an internal RTP timestamp by FrameSamples on every call; prefer
// PayloadAtTimestamp when the real timestamp is known.
func (p *REDPayloader) Payload(mtu uint16, payload []byte) [][]byte {
	packets := p.PayloadAtTimestamp(mtu, p.timestamp, payload)
	p.timestamp += p.FrameSamples

	return packets
}

// PayloadAtTimestamp payloads one primary frame at the given absolute RTP
// timestamp and attaches redundancy from history.
//
// Primary audio is never truncated or dropped: if no redundancy fits, or the
// primary block alone fills the budget, a primary-only RED packet is emitted.
// History is scanned newest-first and every block that fits the budget and
// the protocol field widths (14-bit timestamp offset, 10-bit length) is
// included; blocks that do not fit are dropped without touching primary
// audio, then blocks are emitted oldest-first as required by RFC 2198. If the
// primary codec fragments one frame into several blocks, only the first block
// carries redundancy and is remembered.
func (p *REDPayloader) PayloadAtTimestamp(mtu uint16, timestamp uint32, payload []byte) [][]byte {
	p.timestamp = timestamp

	if payload == nil {
		return [][]byte{}
	}

	// Reserve one byte for the primary header; the primary data may then use
	// the full remaining budget without being truncated.
	primaryMTU := mtu
	if primaryMTU > 0 {
		primaryMTU--
	}

	primaryBlocks := p.Payloader.Payload(primaryMTU, payload)

	packets := make([][]byte, 0, len(primaryBlocks))

	for i, primary := range primaryBlocks {
		if i == 0 {
			red, err := p.encapsulate(mtu, timestamp, primary)
			if err != nil {
				// Unreachable: candidates are validated before selection.
				continue
			}

			packets = append(packets, red)
		} else {
			// Further fragments of the same frame are primary-only.
			red, err := marshalRED(nil, REDBlock{
				PayloadType: p.PayloadType,
				Payload:     primary,
				Primary:     true,
			})
			if err != nil {
				continue
			}

			packets = append(packets, red)
		}
	}

	return packets
}

// encapsulate builds one RED packet for the primary block, attaching every
// history block that fits within mtu and the protocol field widths.
func (p *REDPayloader) encapsulate(mtu uint16, timestamp uint32, primary []byte) ([]byte, error) {
	redundancies := make([]REDBlock, 0, len(p.history))
	used := 1 + len(primary) // primary header byte plus its data

	// Select redundancy newest-first so the freshest blocks win the limited
	// budget: a block that does not fit is dropped (never truncating primary
	// data), while older and typically smaller blocks are still considered.
	for i := len(p.history) - 1; i >= 0; i-- {
		entry := p.history[i]
		offset := timestamp - entry.timestamp

		// Field width limits from RFC 2198. Such blocks can never be carried.
		if offset > redMaxTimestampOffset || len(entry.payload) > redMaxBlockLength {
			continue
		}

		cost := 4 + len(entry.payload)

		if int(mtu) != 0 && used+cost > int(mtu) {
			// Redundancy is dropped here; primary data is left untouched.
			continue
		}

		used += cost

		redundancies = append(redundancies, REDBlock{
			PayloadType:     entry.payloadType,
			TimestampOffset: uint16(offset), //nolint:gosec // G115 guarded by redMaxTimestampOffset
			Payload:         entry.payload,
		})
	}

	// Selected newest-first; RFC 2198 requires oldest-first on the wire.
	for i, j := 0, len(redundancies)-1; i < j; i, j = i+1, j-1 {
		redundancies[i], redundancies[j] = redundancies[j], redundancies[i]
	}

	red, err := marshalRED(redundancies, REDBlock{
		PayloadType: p.PayloadType,
		Payload:     primary,
		Primary:     true,
	})
	if err != nil {
		return nil, err
	}

	p.remember(timestamp, primary)

	return red, nil
}

// remember stores a copy of the primary block and enforces the history bound.
func (p *REDPayloader) remember(timestamp uint32, primary []byte) {
	if p.MaxRedundancy <= 0 {
		return
	}

	stored := make([]byte, len(primary))
	copy(stored, primary)

	p.history = append(p.history, redHistoryEntry{
		payloadType: p.PayloadType,
		timestamp:   timestamp,
		payload:     stored,
	})

	if p.MaxRedundancy > 0 && len(p.history) > p.MaxRedundancy {
		drop := len(p.history) - p.MaxRedundancy
		p.history = append(p.history[:0], p.history[drop:]...)
	}
}

// MarshalRED serializes redundant blocks followed by one primary block in the
// RFC 2198 format. It is exported for callers that build RED packets directly
// without using REDPayloader's bounded history and MTU enforcement.
func MarshalRED(redundancies []REDBlock, primary REDBlock) ([]byte, error) {
	return marshalRED(redundancies, primary)
}

// marshalRED serializes redundant blocks followed by one primary block in the
// RFC 2198 format. It enforces the 14-bit timestamp offset and 10-bit block
// length field widths, but performs no MTU accounting.
func marshalRED(redundancies []REDBlock, primary REDBlock) ([]byte, error) {
	total := 1 + len(primary.Payload)

	for _, block := range redundancies {
		if block.TimestampOffset > redMaxTimestampOffset {
			return nil, errREDOffsetOutOfRange
		} else if len(block.Payload) > redMaxBlockLength {
			return nil, errREDBlockTooLong
		}

		total += 4 + len(block.Payload)
	}

	buf := make([]byte, 0, total)

	for _, block := range redundancies {
		offset := block.TimestampOffset
		length := len(block.Payload)

		buf = append(buf,
			redFollowBit|block.PayloadType&redBlockTypeMask,
			byte(offset>>6),                      //nolint:gosec // G115 bounded by 14-bit field
			byte(offset&0x3F)<<2|byte(length>>8), //nolint:gosec // G115 masked/guarded
			byte(length),                         //nolint:gosec // G115 bounded by 10-bit field
		)
	}

	buf = append(buf, primary.PayloadType&redBlockTypeMask)

	for _, block := range redundancies {
		buf = append(buf, block.Payload...)
	}

	buf = append(buf, primary.Payload...)

	return buf, nil
}
