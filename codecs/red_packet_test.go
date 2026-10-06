// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package codecs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestREDEncodeDecode_ShortHeaders(t *testing.T) {
	blocks := []REDBlock{
		{PayloadType: 111, TimestampOffset: 200, Payload: []byte{0xA1, 0xA2, 0xA3}},
		{PayloadType: 111, TimestampOffset: 100, Payload: []byte{0xB1, 0xB2}},
		{PayloadType: 96, Payload: []byte{0x60, 0x61, 0x62, 0x63}},
	}

	payload, err := REDEncode(blocks)
	require.NoError(t, err)

	// 200<<10|3 = 0x032003, 100<<10|2 = 0x019002. Headers first, primary
	// data first in the data section.
	expected := []byte{
		0x6F, 0x03, 0x20, 0x03,
		0x6F, 0x01, 0x90, 0x02,
		0x60,
		0x60, 0x61, 0x62, 0x63, // primary data
		0xA1, 0xA2, 0xA3, // first redundant block data
		0xB1, 0xB2, // second redundant block data
	}
	assert.Equal(t, expected, payload)

	size, err := REDMarshalSize(blocks)
	require.NoError(t, err)
	assert.Equal(t, len(expected), size)

	decoded, err := REDDecode(payload)
	require.NoError(t, err)
	require.Len(t, decoded, 3)

	assert.Equal(t, uint8(111), decoded[0].PayloadType)
	assert.Equal(t, uint32(200), decoded[0].TimestampOffset)
	assert.False(t, decoded[0].Marker)
	assert.Equal(t, []byte{0xA1, 0xA2, 0xA3}, decoded[0].Payload)

	assert.Equal(t, uint8(111), decoded[1].PayloadType)
	assert.Equal(t, uint32(100), decoded[1].TimestampOffset)
	assert.False(t, decoded[1].Marker)
	assert.Equal(t, []byte{0xB1, 0xB2}, decoded[1].Payload)

	assert.Equal(t, uint8(96), decoded[2].PayloadType)
	assert.True(t, decoded[2].Marker)
	assert.Equal(t, uint32(0), decoded[2].TimestampOffset)
	assert.Equal(t, []byte{0x60, 0x61, 0x62, 0x63}, decoded[2].Payload)
}

func TestREDEncodeDecode_LongHeader(t *testing.T) {
	// Offset 20000 exceeds the 14-bit short field, forcing F=1.
	blocks := []REDBlock{
		{PayloadType: 111, TimestampOffset: 20000, Payload: []byte{0xA1, 0xA2, 0xA3}},
		{PayloadType: 96, Payload: []byte{}},
	}

	payload, err := REDEncode(blocks)
	require.NoError(t, err)

	// (20000<<2)|(3>>8) = 0x00013880, length low byte is 3.
	expected := []byte{
		0xEF, // F=1 | PT 111
		0x00, 0x01, 0x38, 0x80,
		0x03,
		0x60, // primary header (empty primary data)
		0xA1, 0xA2, 0xA3,
	}
	assert.Equal(t, expected, payload)

	decoded, err := REDDecode(payload)
	require.NoError(t, err)
	require.Len(t, decoded, 2)
	assert.Equal(t, uint8(111), decoded[0].PayloadType)
	assert.Equal(t, uint32(20000), decoded[0].TimestampOffset)
	assert.Equal(t, []byte{0xA1, 0xA2, 0xA3}, decoded[0].Payload)
	assert.True(t, decoded[1].Marker)
	assert.Empty(t, decoded[1].Payload)
}

func TestREDEncode_PrimaryOnly(t *testing.T) {
	blocks := []REDBlock{
		{PayloadType: 96, Payload: []byte{0x01, 0x02, 0x03}},
	}

	payload, err := REDEncode(blocks)
	require.NoError(t, err)
	assert.Equal(t, []byte{0x60, 0x01, 0x02, 0x03}, payload)

	decoded, err := REDDecode(payload)
	require.NoError(t, err)
	require.Len(t, decoded, 1)
	assert.True(t, decoded[0].Marker)
	assert.Equal(t, blocks[0].Payload, decoded[0].Payload)
}

func TestREDDecode_ZeroFilledPrimary(t *testing.T) {
	// A redundant block followed by a zero-filled primary resolves to the
	// redundancy followed by the primary block.
	withRedundancy, err := REDEncode([]REDBlock{
		{PayloadType: 111, TimestampOffset: 10, Payload: []byte{0xAA}},
		{PayloadType: 96, Payload: []byte{0x00, 0x00}},
	})
	require.NoError(t, err)

	blocks, err := REDDecode(withRedundancy)
	require.NoError(t, err)
	require.Len(t, blocks, 2)
	assert.False(t, blocks[0].Marker)
	assert.Equal(t, []byte{0xAA}, blocks[0].Payload)
	assert.True(t, blocks[1].Marker)
	assert.Equal(t, []byte{0x00, 0x00}, blocks[1].Payload)
}

func TestREDEncode_Errors(t *testing.T) {
	_, err := REDEncode(nil)
	assert.ErrorIs(t, err, errREDNoBlocks)

	_, err = REDEncode([]REDBlock{
		{PayloadType: 128, TimestampOffset: 10, Payload: []byte{1}},
		{PayloadType: 96, Payload: []byte{1}},
	})
	assert.ErrorIs(t, err, errREDPayloadType)

	_, err = REDEncode([]REDBlock{
		{PayloadType: 111, TimestampOffset: 0, Payload: []byte{1}},
		{PayloadType: 96, Payload: []byte{1}},
	})
	assert.ErrorIs(t, err, errREDZeroTimestampOffset)

	_, err = REDEncode([]REDBlock{
		{PayloadType: 111, TimestampOffset: 10, Payload: make([]byte, REDMaxBlockLength+1)},
		{PayloadType: 96, Payload: []byte{1}},
	})
	assert.ErrorIs(t, err, errREDBlockLength)

	// A primary block may be larger than the 10-bit field.
	_, err = REDEncode([]REDBlock{
		{PayloadType: 96, Payload: make([]byte, REDMaxBlockLength+1)},
	})
	assert.NoError(t, err)

	_, err = REDEncode([]REDBlock{
		{PayloadType: 111, TimestampOffset: REDMaxTimestampOffset + 1, Payload: []byte{1}},
		{PayloadType: 96, Payload: []byte{1}},
	})
	assert.ErrorIs(t, err, errREDTimestampOffset)
}

func TestREDDecode_Errors(t *testing.T) {
	if _, err := REDDecode(nil); assert.ErrorIs(t, err, errNilPacket) {
		// expected
	}

	// Truncated long header.
	truncated := []byte{0xEF, 0x00, 0x01, 0x38}
	_, err := REDDecode(truncated)
	assert.ErrorIs(t, err, errShortPacket)

	// Empty payload.
	_, err = REDDecode([]byte{})
	assert.ErrorIs(t, err, errShortPacket)
}

func TestREDPacket_UnmarshalAndTimestamps(t *testing.T) {
	payload, err := REDEncode([]REDBlock{
		{PayloadType: 111, TimestampOffset: 960, Payload: []byte{0xA1}},
		{PayloadType: 96, Payload: []byte{0x01, 0x02}},
	})
	require.NoError(t, err)

	packet := &REDPacket{}
	primary, err := packet.Unmarshal(payload)
	require.NoError(t, err)
	assert.Equal(t, []byte{0x01, 0x02}, primary)

	// Unsigned subtraction recovers the original timestamp across wraparound.
	packetTimestamp := uint32(0x00000050) // nolint:gosec
	blocks := packet.BlocksWithTimestamp(packetTimestamp)
	require.Len(t, blocks, 2)
	assert.Equal(t, packetTimestamp, blocks[1].Timestamp)
	assert.Equal(t, packetTimestamp-uint32(960), blocks[0].Timestamp) // nolint:gosec

	wrapped := &REDPacket{}
	wrappedPayload, err := REDEncode([]REDBlock{
		{PayloadType: 111, TimestampOffset: 66, Payload: []byte{0xA1}},
		{PayloadType: 96, Payload: []byte{0x01}},
	})
	require.NoError(t, err)

	_, err = wrapped.Unmarshal(wrappedPayload)
	require.NoError(t, err)
	wrappedBlocks := wrapped.BlocksWithTimestamp(50)
	assert.Equal(t, uint32(0xFFFFFFF0), wrappedBlocks[0].Timestamp)
	assert.Equal(t, uint32(50), wrappedBlocks[1].Timestamp)

	// Depacketizer interface: every RED packet is a complete audio partition.
	assert.True(t, packet.IsPartitionHead(payload))
	assert.True(t, packet.IsPartitionTail(true, payload))

	if _, err := packet.Unmarshal(nil); assert.ErrorIs(t, err, errNilPacket) {
		// expected
	}
}
