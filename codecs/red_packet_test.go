// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package codecs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestREDPacket_UnmarshalNilAndEmpty(t *testing.T) {
	p := &REDPacket{}

	raw, err := p.Unmarshal(nil)
	assert.ErrorIs(t, err, errNilPacket)
	assert.Nil(t, raw)

	// Only a primary header byte but no primary data.
	raw, err = p.Unmarshal([]byte{111})
	assert.ErrorIs(t, err, errREDNoData)
	assert.Nil(t, raw)

	// Redundant header with no primary header at all.
	raw, err = p.Unmarshal([]byte{redFollowBit | 111, 0x00, 0x00, 0x01})
	assert.ErrorIs(t, err, errREDNoData)
	assert.Nil(t, raw)
}

func TestREDPacket_UnmarshalInvalidLength(t *testing.T) {
	p := &REDPacket{}

	// One redundant header claiming 3 bytes, but only 2 bytes remain after
	// the headers.
	packet := []byte{
		redFollowBit | 111,
		0x00, 0x00, 0x03, // offset 0, length 3
		111, // primary header
		0xAA,
		0xBB,
	}

	_, err := p.Unmarshal(packet)
	assert.ErrorIs(t, err, errREDInvalidLength)
}

func TestREDPacket_PrimaryOnlyRoundTrip(t *testing.T) {
	primary := []byte{0xDE, 0xAD, 0xBE, 0xEF}

	encoded, err := MarshalRED(nil, REDBlock{
		PayloadType: 111,
		Payload:     primary,
		Primary:     true,
	})
	require.NoError(t, err)

	p := &REDPacket{}

	raw, err := p.Unmarshal(encoded)
	require.NoError(t, err)
	assert.Equal(t, primary, raw)
	assert.Empty(t, p.Redundancies)
	assert.Equal(t, uint8(111), p.PrimaryBlock.PayloadType)
	assert.True(t, p.PrimaryBlock.Primary)
	assert.Equal(t, primary, p.PrimaryBlock.Payload)

	// Primary timestamp is the packet timestamp.
	assert.Equal(t, uint32(12345), p.PrimaryBlock.Timestamp(12345))
}

func TestREDPacket_BlocksAndTimestamps(t *testing.T) {
	oldest := []byte{0x01}
	older := []byte{0x02, 0x03}
	primary := []byte{0x10, 0x20, 0x30}

	encoded, err := MarshalRED([]REDBlock{
		{PayloadType: 111, TimestampOffset: 480, Payload: oldest},
		{PayloadType: 111, TimestampOffset: 240, Payload: older},
	}, REDBlock{
		PayloadType: 111,
		Payload:     primary,
		Primary:     true,
	})
	require.NoError(t, err)

	p := &REDPacket{}

	raw, err := p.Unmarshal(encoded)
	require.NoError(t, err)
	assert.Equal(t, primary, raw)

	require.Len(t, p.Redundancies, 2)

	assert.Equal(t, uint16(480), p.Redundancies[0].TimestampOffset)
	assert.Equal(t, oldest, p.Redundancies[0].Payload)
	assert.False(t, p.Redundancies[0].Primary)

	assert.Equal(t, uint16(240), p.Redundancies[1].TimestampOffset)
	assert.Equal(t, older, p.Redundancies[1].Payload)

	const packetTS uint32 = 100000

	assert.Equal(t, packetTS-480, p.Redundancies[0].Timestamp(packetTS))
	assert.Equal(t, packetTS-240, p.Redundancies[1].Timestamp(packetTS))
	assert.Equal(t, packetTS, p.PrimaryBlock.Timestamp(packetTS))
}

func TestREDBlock_TimestampRollover(t *testing.T) {
	// Packet timestamp sits just above zero; redundant block lives before the
	// rollover. Offset is small, recovered absolute timestamp wraps.
	block := REDBlock{TimestampOffset: 960}
	packetTS := uint32(100)
	expected := packetTS - 960 // wraps to 0xFFFFFFA4

	assert.Equal(t, packetTS-960, block.Timestamp(packetTS))
	assert.Equal(t, expected, block.Timestamp(packetTS))
	assert.Equal(t, ^uint32(0)-859, expected)
}

func TestREDPacket_MaxFieldWidthsRoundTrip(t *testing.T) {
	data := make([]byte, redMaxBlockLength)
	for i := range data {
		data[i] = byte(i)
	}

	encoded, err := MarshalRED([]REDBlock{
		{
			PayloadType:     111,
			TimestampOffset: redMaxTimestampOffset,
			Payload:         data,
		},
	}, REDBlock{
		PayloadType: 111,
		Payload:     []byte{0x99},
		Primary:     true,
	})
	require.NoError(t, err)

	p := &REDPacket{}
	_, err = p.Unmarshal(encoded)
	require.NoError(t, err)
	require.Len(t, p.Redundancies, 1)

	assert.Equal(t, uint16(redMaxTimestampOffset), p.Redundancies[0].TimestampOffset)
	assert.Len(t, p.Redundancies[0].Payload, redMaxBlockLength)
	assert.Equal(t, data, p.Redundancies[0].Payload)
}

func TestMarshalRED_FieldWidthErrors(t *testing.T) {
	_, err := MarshalRED([]REDBlock{
		{PayloadType: 111, TimestampOffset: redMaxTimestampOffset + 1},
	}, REDBlock{PayloadType: 111, Payload: []byte{1}, Primary: true})
	assert.ErrorIs(t, err, errREDOffsetOutOfRange)

	big := make([]byte, redMaxBlockLength+1)

	_, err = MarshalRED([]REDBlock{
		{PayloadType: 111, Payload: big},
	}, REDBlock{PayloadType: 111, Payload: []byte{1}, Primary: true})
	assert.ErrorIs(t, err, errREDBlockTooLong)
}

func TestREDPacket_IsPartition(t *testing.T) {
	p := &REDPacket{}
	assert.True(t, p.IsPartitionHead(nil))
	assert.True(t, p.IsPartitionTail(true, nil))
}

func TestREDPacket_BlocksAndDedupKey(t *testing.T) {
	encoded, err := MarshalRED([]REDBlock{
		{PayloadType: 111, TimestampOffset: 480, Payload: []byte{0x01}},
	}, REDBlock{
		PayloadType: 111,
		Payload:     []byte{0x02},
		Primary:     true,
	})
	require.NoError(t, err)

	p := &REDPacket{}
	_, err = p.Unmarshal(encoded)
	require.NoError(t, err)

	blocks := p.Blocks()
	require.Len(t, blocks, 2)
	assert.Equal(t, []byte{0x01}, blocks[0].Payload)
	assert.False(t, blocks[0].Primary)
	assert.Equal(t, []byte{0x02}, blocks[1].Payload)
	assert.True(t, blocks[1].Primary)

	// The (timestamp, payload type) pair is the upper-layer dedup key.
	const packetTS uint32 = 480
	assert.Equal(t, uint32(0), blocks[0].Timestamp(packetTS))
	assert.Equal(t, packetTS, blocks[1].Timestamp(packetTS))
}
