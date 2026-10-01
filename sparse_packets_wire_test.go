package sftp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueryAllocatedPacketWire(t *testing.T) {
	pkt := &sshFxpQueryAllocatedPacket{
		ID:      7,
		Handle:  "h",
		Offset:  0x1122334455667788,
		Regions: 3,
	}

	b, err := pkt.MarshalBinary()
	require.NoError(t, err)

	var got sshFxpQueryAllocatedServerPacket
	// Server unmarshal starts after the length prefix and packet type byte.
	require.NoError(t, got.UnmarshalBinary(b[5:]))

	assert.Equal(t, uint32(7), got.ID)
	assert.Equal(t, extQueryAllocated, got.ExtendedRequest)
	assert.Equal(t, "h", got.Handle)
	assert.Equal(t, uint64(0x1122334455667788), got.Offset)
	assert.Equal(t, uint32(3), got.Regions)
}

func TestAllocateRegionsPacketWire(t *testing.T) {
	pkt := &sshFxpAllocateRegionsPacket{
		ID:     9,
		Handle: "hdl",
		Ranges: []FileRange{
			{Offset: 100, Length: 200},
			{Offset: 500, Length: 64},
		},
	}

	b, err := pkt.MarshalBinary()
	require.NoError(t, err)

	var got sshFxpAllocateRegionsServerPacket
	require.NoError(t, got.UnmarshalBinary(b[5:]))

	assert.Equal(t, uint32(9), got.ID)
	assert.Equal(t, extAllocateRegions, got.ExtendedRequest)
	assert.Equal(t, "hdl", got.Handle)
	require.Len(t, got.Ranges, 2)
	assert.Equal(t, FileRange{100, 200}, got.Ranges[0])
	assert.Equal(t, FileRange{500, 64}, got.Ranges[1])

	// Malformed payloads (count exceeds available bytes) are rejected.
	truncated := b[:len(b)-8]
	var bad sshFxpAllocateRegionsServerPacket
	assert.Error(t, bad.UnmarshalBinary(truncated[5:]))
}

func TestAllocatedRangesReplyWire(t *testing.T) {
	pkt := &sshFxpAllocatedRangesPacket{
		ID:     11,
		Offset: 900,
		Length: 4096,
		Ranges: []FileRange{{Offset: 256, Length: 128}},
	}

	b, err := pkt.MarshalBinary()
	require.NoError(t, err)

	// sendPacket fills the leading length; emulate that for recvPacket.
	length := uint32(len(b) - 4)
	b[0], b[1], b[2], b[3] = byte(length>>24), byte(length>>16), byte(length>>8), byte(length)

	var reply sshFxpAllocatedRangesPacket
	require.NoError(t, reply.unmarshalBody(11, b[5:]))

	assert.Equal(t, uint32(11), reply.ID)
	assert.Equal(t, uint64(900), reply.Offset)
	assert.Equal(t, uint64(4096), reply.Length)
	require.Len(t, reply.Ranges, 1)
	assert.Equal(t, FileRange{256, 128}, reply.Ranges[0])
}

func TestValidateRanges(t *testing.T) {
	assert.NoError(t, validateRanges(nil))
	assert.NoError(t, validateRanges([]FileRange{{0, 1}, {1, 1}}))
	assert.Error(t, validateRanges([]FileRange{{0, 0}}))
	assert.Error(t, validateRanges([]FileRange{{0, 2}, {1, 1}}))  // overlap
	assert.Error(t, validateRanges([]FileRange{{5, 1}, {2, 1}}))  // unsorted
	assert.Error(t, validateRanges([]FileRange{{^uint64(0), 2}})) // overflow
}
