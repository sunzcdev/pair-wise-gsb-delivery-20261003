// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtp

import (
	"testing"

	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPacketizer_RED verifies the single-codec Packetizer remains usable when
// its payloader is an RFC 2198 RED payloader, and that the emitted packets can
// be depacketized with the packet timestamp used for redundancy recovery.
func TestPacketizer_RED(t *testing.T) {
	red := &codecs.REDPayloader{
		Payloader:     &codecs.OpusPayloader{},
		PayloadType:   111,
		MaxRedundancy: 2,
		FrameSamples:  960,
	}

	packetizer := NewPacketizer(1200, 63, 0xAABBCCDD, red, NewFixedSequencer(1), 48000)

	frames := [][]byte{{0x01, 0x02}, {0x03, 0x04, 0x05}, {0x06}}

	packets := make([]*Packet, 0, len(frames))

	for _, frame := range frames {
		packets = append(packets, packetizer.Packetize(frame, 960)...)
	}

	require.Len(t, packets, len(frames))

	// Packet 2 carries frame 0 and 1 as redundancy.
	depacketized := &codecs.REDPacket{}
	raw, err := depacketized.Unmarshal(packets[2].Payload)
	require.NoError(t, err)
	assert.Equal(t, frames[2], raw)
	require.Len(t, depacketized.Redundancies, 2)

	packetTS := packets[2].Timestamp

	assert.Equal(t, packets[0].Timestamp, depacketized.Redundancies[0].Timestamp(packetTS))
	assert.Equal(t, packets[1].Timestamp, depacketized.Redundancies[1].Timestamp(packetTS))
	assert.Equal(t, frames[0], depacketized.Redundancies[0].Payload)
	assert.Equal(t, frames[1], depacketized.Redundancies[1].Payload)

	// Sequence numbers and primary payload type are those of the outer packet.
	assert.Equal(t, uint16(1), packets[0].SequenceNumber)
	assert.Equal(t, uint8(63), packets[2].PayloadType)
	assert.Equal(t, uint32(0xAABBCCDD), packets[2].SSRC)
}
