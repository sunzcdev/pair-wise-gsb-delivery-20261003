// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package codecs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const redTestPT uint8 = 111

func newREDPayloader(maxRedundancy int) *REDPayloader {
	return &REDPayloader{
		Payloader:     &OpusPayloader{},
		PayloadType:   redTestPT,
		MaxRedundancy: maxRedundancy,
	}
}

func TestREDPayloader_NilPayload(t *testing.T) {
	p := newREDPayloader(2)
	assert.Empty(t, p.PayloadAtTimestamp(1200, 1000, nil))
	assert.Empty(t, p.Payload(1200, nil))
}

func TestREDPayloader_PrimaryNeverTruncated(t *testing.T) {
	p := newREDPayloader(2)
	primary := []byte{0x01, 0x02, 0x03, 0x04, 0x05}

	// MTU equals exactly one primary header plus the payload: no room for any
	// redundancy, but the whole primary must survive.
	packets := p.PayloadAtTimestamp(uint16(1+len(primary)), 0, primary)
	require.Len(t, packets, 1)
	assert.Len(t, packets[0], 1+len(primary))

	decoded := &REDPacket{}
	raw, err := decoded.Unmarshal(packets[0])
	require.NoError(t, err)
	assert.Equal(t, primary, raw)
	assert.Empty(t, decoded.Redundancies)

	// MTU of zero means unlimited; a following packet carries redundancy.
	bigMTUPackets := p.PayloadAtTimestamp(0, 480, []byte{0x09})
	require.Len(t, bigMTUPackets, 1)

	decoded2 := &REDPacket{}
	_, err = decoded2.Unmarshal(bigMTUPackets[0])
	require.NoError(t, err)
	assert.Len(t, decoded2.Redundancies, 1)
	assert.Equal(t, primary, decoded2.Redundancies[0].Payload)
}

func TestREDPayloader_MTUDropsRedundancy(t *testing.T) {
	p := newREDPayloader(3)

	frameA := []byte{0x0A, 0x0B, 0x0C, 0x0D, 0x0E}
	frameB := []byte{0x0F, 0x10, 0x11, 0x12, 0x13}

	p.PayloadAtTimestamp(1200, 0, frameA)

	// Budget allows the primary (6 bytes) plus exactly one redundant block
	// (4 header + 5 data = 9), but not a second. Set MTU to 15.
	packets := p.PayloadAtTimestamp(15, 480, frameB)
	require.Len(t, packets, 1)
	assert.Len(t, packets[0], 15)

	decoded := &REDPacket{}
	_, err := decoded.Unmarshal(packets[0])
	require.NoError(t, err)
	require.Len(t, decoded.Redundancies, 1)
	assert.Equal(t, frameA, decoded.Redundancies[0].Payload)
	assert.Equal(t, frameB, decoded.PrimaryBlock.Payload)
}

func TestREDPayloader_HistoryBounded(t *testing.T) {
	p := newREDPayloader(2)

	frames := [][]byte{{0x01}, {0x02}, {0x03}, {0x04}}

	for i, frame := range frames {
		p.PayloadAtTimestamp(1200, uint32(i*480), frame)
	}

	require.Len(t, p.history, 2)
	assert.Equal(t, uint32(2*480), p.history[0].timestamp)
	assert.Equal(t, uint32(3*480), p.history[1].timestamp)

	// Zero redundancy never records history.
	none := newREDPayloader(0)
	none.PayloadAtTimestamp(1200, 0, frames[0])
	none.PayloadAtTimestamp(1200, 480, frames[1])
	assert.Empty(t, none.history)
}

func TestREDPayloader_FieldWidthSkipsBlock(t *testing.T) {
	p := newREDPayloader(1)

	// A primary block larger than the 10-bit length field cannot be sent as
	// redundancy; it is skipped rather than producing an invalid packet.
	huge := make([]byte, redMaxBlockLength+1)
	p.PayloadAtTimestamp(0, 0, huge)

	packets := p.PayloadAtTimestamp(0, 480, []byte{0x01})
	require.Len(t, packets, 1)

	decoded := &REDPacket{}
	_, err := decoded.Unmarshal(packets[0])
	require.NoError(t, err)
	assert.Empty(t, decoded.Redundancies)
	assert.Equal(t, []byte{0x01}, decoded.PrimaryBlock.Payload)
}

func TestREDPayloader_VaryingFrameSizesAndRecovery(t *testing.T) {
	p := newREDPayloader(3)

	type sentFrame struct {
		timestamp uint32
		payload   []byte
	}

	frames := []sentFrame{
		{0, []byte{0xA0}},
		{480, make([]byte, 40)},
		{960, []byte{0xC0, 0xC1}},
		{1440, make([]byte, 5)},
		{1920, []byte{0xE0}},
	}

	// Distinguish each frame's payload for later equality checks.
	for i := range frames {
		for b := range frames[i].payload {
			frames[i].payload[b] = byte(i)
		}
	}

	const mtu = 1500

	packets := make([][]byte, 0, len(frames))

	for _, frame := range frames {
		packets = append(packets, p.PayloadAtTimestamp(mtu, frame.timestamp, frame.payload)...)
	}

	// Simulate losing the middle primary packet (frame index 2, ts 960).
	// Frame 3 (ts 1440) should carry frame 2 as redundancy.
	lostIndex := 2

	decoded := &REDPacket{}
	_, err := decoded.Unmarshal(packets[lostIndex+1])
	require.NoError(t, err)

	var recovered []byte

	var recoveredTS uint32

	for _, block := range decoded.Redundancies {
		if block.Timestamp(frames[lostIndex+1].timestamp) == frames[lostIndex].timestamp {
			recovered = block.Payload
			recoveredTS = block.Timestamp(frames[lostIndex+1].timestamp)
		}
	}

	require.NotNil(t, recovered, "lost middle frame must be recoverable from redundancy")
	assert.Equal(t, frames[lostIndex].payload, recovered)
	assert.Equal(t, frames[lostIndex].timestamp, recoveredTS)

	// Every received primary is intact and undecoded (raw bytes returned).
	for i, raw := range packets {
		if i == lostIndex {
			continue
		}

		d := &REDPacket{}
		primary, unmarshalErr := d.Unmarshal(raw)
		require.NoError(t, unmarshalErr)
		assert.Equal(t, frames[i].payload, primary)
		assert.Equal(t, frames[i].timestamp, d.PrimaryBlock.Timestamp(frames[i].timestamp))
	}
}

func TestREDPayloader_TimestampRollover(t *testing.T) {
	p := newREDPayloader(2)

	samples := uint32(480)
	ts0 := ^uint32(0) - samples + 1 // 0 - 480, wraps
	ts1 := uint32(0)

	p.PayloadAtTimestamp(1500, ts0, []byte{0x70})

	packets := p.PayloadAtTimestamp(1500, ts1, []byte{0x71})
	require.Len(t, packets, 1)

	decoded := &REDPacket{}
	_, err := decoded.Unmarshal(packets[0])
	require.NoError(t, err)
	require.Len(t, decoded.Redundancies, 1)

	assert.Equal(t, samples, uint32(decoded.Redundancies[0].TimestampOffset))

	recoveredTS := decoded.Redundancies[0].Timestamp(ts1)
	assert.Equal(t, ts0, recoveredTS)
	assert.Equal(t, []byte{0x70}, decoded.Redundancies[0].Payload)
}

func TestREDPayloader_RecoveryDeduplicates(t *testing.T) {
	p := newREDPayloader(2)

	p.PayloadAtTimestamp(1500, 0, []byte{0x01})
	packet1 := p.PayloadAtTimestamp(1500, 480, []byte{0x02})
	packet2 := p.PayloadAtTimestamp(1500, 960, []byte{0x03})

	// A receiver tracking seen timestamps can identify the same frame when it
	// arrives both as primary and as redundancy.
	d1 := &REDPacket{}
	_, err := d1.Unmarshal(packet1[0])
	require.NoError(t, err)

	d2 := &REDPacket{}
	_, err = d2.Unmarshal(packet2[0])
	require.NoError(t, err)
	require.Len(t, d2.Redundancies, 2)

	// Frame 0x02 is primary in packet1 and redundant in packet2, both at 480.
	assert.Equal(t, uint32(480), d1.PrimaryBlock.Timestamp(480))

	duplicateTS := d2.Redundancies[1].Timestamp(960)
	assert.Equal(t, uint32(480), duplicateTS)
	assert.Equal(t, d1.PrimaryBlock.Payload, d2.Redundancies[1].Payload)
}

func TestREDPayloader_ImplementsPayloader(t *testing.T) {
	p := newREDPayloader(2)
	p.FrameSamples = 480

	var payloader interface {
		Payload(mtu uint16, payload []byte) [][]byte
	} = p

	p.Reset(1000)
	assert.Equal(t, uint32(1000), p.timestamp)

	// Legacy Payload path advances the internal timestamp by FrameSamples.
	require.Len(t, payloader.Payload(1200, []byte{0x01}), 1)
	assert.Equal(t, uint32(1480), p.timestamp)
}
