// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtp

import (
	"testing"

	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testMediaPT = uint8(111)
	testREDPT   = uint8(63)
	testSamples = uint32(960)
)

func newTestREDPacketizer(t *testing.T, mtu uint16, maxRedundancy int) *REDPacketizer {
	t.Helper()

	inner := NewPacketizer(mtu, testMediaPT, 5000,
		&codecs.OpusPayloader{}, NewFixedSequencer(1), 48000)

	return NewREDPacketizer(mtu, testREDPT, maxRedundancy, inner)
}

func decodeRED(t *testing.T, packet *Packet) []codecs.REDBlock {
	t.Helper()

	red := &codecs.REDPacket{}
	_, err := red.Unmarshal(packet.Payload)
	require.NoError(t, err)

	return red.BlocksWithTimestamp(packet.Timestamp)
}

func TestREDPacketizer_BuildsRedundancy(t *testing.T) {
	mtu := uint16(1200)
	pkt := newTestREDPacketizer(t, mtu, 2)

	first := pkt.Packetize([]byte{0x01, 0x02}, testSamples)
	require.Len(t, first, 1)
	assert.Equal(t, testREDPT, first[0].PayloadType)
	assert.Len(t, decodeRED(t, first[0]), 1, "first packet has no redundancy")

	second := pkt.Packetize([]byte{0x03, 0x04}, testSamples)
	require.Len(t, second, 1)
	blocks := decodeRED(t, second[0])
	require.Len(t, blocks, 2)
	assert.False(t, blocks[0].Marker)
	assert.Equal(t, uint32(960), blocks[0].TimestampOffset)
	assert.Equal(t, []byte{0x01, 0x02}, blocks[0].Payload)
	assert.True(t, blocks[1].Marker)
	assert.Equal(t, []byte{0x03, 0x04}, blocks[1].Payload)

	third := pkt.Packetize([]byte{0x05, 0x06}, testSamples)
	blocks = decodeRED(t, third[0])
	require.Len(t, blocks, 3, "history bounded by MaxRedundancy+primary")
	assert.Equal(t, []byte{0x01, 0x02}, blocks[0].Payload)
	assert.Equal(t, []byte{0x03, 0x04}, blocks[1].Payload)
	assert.Equal(t, []byte{0x05, 0x06}, blocks[2].Payload)

	fourth := pkt.Packetize([]byte{0x07, 0x08}, testSamples)
	blocks = decodeRED(t, fourth[0])
	require.Len(t, blocks, 3, "oldest frame fell out of bounded history")
	assert.Equal(t, []byte{0x03, 0x04}, blocks[0].Payload)
	assert.Equal(t, []byte{0x07, 0x08}, blocks[2].Payload)

	for _, packets := range [][]*Packet{first, second, third, fourth} {
		raw, err := packets[0].Marshal()
		require.NoError(t, err)
		assert.LessOrEqual(t, len(raw), int(mtu))
	}
}

func TestREDPacketizer_VariableFrameSizesAndDropPolicy(t *testing.T) {
	// Tight MTU: a 100-byte frame fits on its own (101 bytes of payload) but
	// cannot be carried as redundancy beside a small primary block.
	mtu := uint16(117)
	pkt := newTestREDPacketizer(t, mtu, 4)

	// Large first frame: later small frames cannot carry it because even its
	// 4-byte header would exceed the budget.
	large := make([]byte, 100)
	for i := range large {
		large[i] = byte(0xA0 + i%16) // structured bytes, not a decoy header run
	}
	pkt.Packetize(large, testSamples)

	second := pkt.Packetize([]byte{0x01, 0x02}, testSamples)
	assert.Len(t, decodeRED(t, second[0]), 1, "large redundancy dropped to fit MTU")

	// A small frame that follows can be carried.
	third := pkt.Packetize([]byte{0x03, 0x04}, testSamples)
	blocks := decodeRED(t, third[0])
	require.Len(t, blocks, 2)
	assert.Equal(t, []byte{0x01, 0x02}, blocks[0].Payload)

	// A huge primary frame is emitted whole even though it exceeds the MTU:
	// audio is never truncated.
	huge := make([]byte, 400)
	for i := range huge {
		huge[i] = byte(0xA0 + i%16)
	}
	oversized := pkt.Packetize(huge, testSamples)
	require.Len(t, oversized, 1)
	blocks = decodeRED(t, oversized[0])
	require.Len(t, blocks, 1)
	assert.Len(t, blocks[0].Payload, 400)

	raw, err := oversized[0].Marshal()
	require.NoError(t, err)
	assert.Greater(t, len(raw), int(mtu))
}

func TestREDPacketizer_NewestRedundancyPreferred(t *testing.T) {
	// Payload budget is MTU-12. The primary is 4 bytes and each redundant
	// block 6 bytes, so two redundant blocks (16) fit at MTU 30 but three
	// (22) do not; the oldest frame must be shed first.
	mtu := uint16(30)
	pkt := newTestREDPacketizer(t, mtu, 3)

	f0 := pkt.Packetize([]byte{0x00, 0x00}, testSamples)
	pkt.Packetize([]byte{0x01, 0x01}, testSamples)
	pkt.Packetize([]byte{0x02, 0x02}, testSamples)
	current := pkt.Packetize([]byte{0x03, 0x03}, testSamples)

	blocks := decodeRED(t, current[0])
	require.Len(t, blocks, 3, "primary plus two newest redundant blocks")
	assert.Equal(t, []byte{0x01, 0x01}, blocks[0].Payload, "oldest frame shed first")
	assert.Equal(t, []byte{0x02, 0x02}, blocks[1].Payload)
	assert.Equal(t, []byte{0x03, 0x03}, blocks[2].Payload)

	// Every emitted packet respects the MTU.
	for _, packet := range append([]*Packet{f0[0]}, current...) {
		raw, err := packet.Marshal()
		require.NoError(t, err)
		assert.LessOrEqual(t, len(raw), int(mtu))
	}
}

func TestREDPacketizer_FieldWidthLimit(t *testing.T) {
	mtu := uint16(1400)
	pkt := newTestREDPacketizer(t, mtu, 4)

	first := pkt.Packetize([]byte{0x01}, testSamples)

	// Skip enough samples that the offset exceeds the 30-bit field width.
	pkt.SkipSamples(codecs.REDMaxTimestampOffset + 1)
	second := pkt.Packetize([]byte{0x02}, testSamples)

	// The first frame cannot be encoded and is dropped, but the primary ships.
	assert.NotEqual(t, first[0].Timestamp, second[0].Timestamp)
	assert.Len(t, decodeRED(t, second[0]), 1)
}

func TestREDPacketizer_TimestampWraparound(t *testing.T) {
	mtu := uint16(1200)

	inner := NewPacketizerWithOptions(
		mtu,
		&codecs.OpusPayloader{},
		NewFixedSequencer(1),
		48000,
		WithPayloadType(testMediaPT),
		WithSSRC(5000),
		WithTimestamp(uint32(0xFFFFFFFF)-200),
	)
	pkt := NewREDPacketizer(mtu, testREDPT, 3, inner)

	pkt.Packetize([]byte{0xA1}, 100)
	wrapped := pkt.Packetize([]byte{0xA2}, 300)

	blocks := decodeRED(t, wrapped[0])
	require.Len(t, blocks, 2)
	assert.Equal(t, uint32(100), blocks[0].TimestampOffset)

	// First frame started 100 samples before the primary, recovering across
	// the uint32 wraparound to the initial timestamp.
	assert.Equal(t, uint32(0xFFFFFFFF)-200, blocks[0].Timestamp)
	assert.Equal(t, wrapped[0].Timestamp, blocks[1].Timestamp)
	assert.True(t, blocks[1].Timestamp > blocks[0].Timestamp, "second packet wrapped past zero")
}

func TestREDPacketizer_LossRecoveryAndDuplicateIdentification(t *testing.T) {
	mtu := uint16(1200)
	pkt := newTestREDPacketizer(t, mtu, 2)

	// frame is a recovered media frame keyed by its original RTP timestamp.
	type frame struct {
		pt        uint8
		timestamp uint32
		data      []byte
	}

	seen := map[uint32][]byte{}
	delivered := make([]frame, 0, 4)

	deliver := func(packet *Packet) {
		for _, block := range decodeRED(t, packet) {
			// Upper layer dedupes on the original frame timestamp.
			if _, ok := seen[block.Timestamp]; ok {
				continue
			}
			seen[block.Timestamp] = block.Payload
			delivered = append(delivered, frame{
				pt:        block.PayloadType,
				timestamp: block.Timestamp,
				data:      block.Payload,
			})
		}
	}

	frame0 := pkt.Packetize([]byte{0x10}, testSamples)
	frame1 := pkt.Packetize([]byte{0x11}, testSamples)
	frame2 := pkt.Packetize([]byte{0x12}, testSamples)
	frame3 := pkt.Packetize([]byte{0x13}, testSamples)

	// frame1 (the second packet) is lost on the wire. Recover its payload
	// from the redundancy carried in frame2.
	deliver(frame0[0])
	deliver(frame2[0])
	deliver(frame3[0])

	require.Len(t, delivered, 4, "lost primary recovered from redundancy")

	recovered := delivered[1]
	assert.Equal(t, testMediaPT, recovered.pt)
	assert.Equal(t, frame1[0].Timestamp, recovered.timestamp)
	assert.Equal(t, []byte{0x11}, recovered.data)

	// Delivering the delayed original again must not duplicate the frame.
	deliver(frame1[0])
	assert.Len(t, delivered, 4)

	// Order after recovery matches original timestamps.
	assert.Equal(t, frame0[0].Timestamp, delivered[0].timestamp)
	assert.Equal(t, frame3[0].Timestamp, delivered[3].timestamp)
}

func TestREDPacketizer_LegacyPacketizerUnaffected(t *testing.T) {
	// The single-codec Packetizer still emits plain media, not RED.
	plain := NewPacketizer(1200, testMediaPT, 5000,
		&codecs.OpusPayloader{}, NewFixedSequencer(1), 48000)

	packets := plain.Packetize([]byte{0x01, 0x02}, testSamples)
	require.Len(t, packets, 1)
	assert.Equal(t, testMediaPT, packets[0].PayloadType)
	assert.Equal(t, []byte{0x01, 0x02}, packets[0].Payload)

	// MaxRedundancy of zero makes the wrapper behave like plain packetization
	// apart from the wire payload type.
	redOnly := newTestREDPacketizer(t, 1200, 0)
	redPackets := redOnly.Packetize([]byte{0x01, 0x02}, testSamples)
	blocks := decodeRED(t, redPackets[0])
	require.Len(t, blocks, 1)
	assert.True(t, blocks[0].Marker)
	assert.Equal(t, []byte{0x01, 0x02}, blocks[0].Payload)

	// Padding and extension forwarding still work.
	padding := redOnly.GeneratePadding(1)
	assert.Len(t, padding, 1)
	redOnly.EnableAbsSendTime(1)
}
