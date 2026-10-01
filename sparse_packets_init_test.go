package sftp

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSparseInitRoundTrip(t *testing.T) {
	pkt := &sshFxInitPacket{
		Version: sftpProtocolVersion,
		Extensions: []extensionPair{
			{Name: sparseFileExtension, Data: "1"},
		},
	}

	b, err := pkt.MarshalBinary()
	require.NoError(t, err)

	// b starts with the uint32 length. Skip length + type to reach body.
	declared := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	assert.Equal(t, uint32(0), declared, "length is filled later by sendPacket")

	var got sshFxInitPacket
	require.NoError(t, got.UnmarshalBinary(b[5:]))
	assert.Equal(t, uint32(sftpProtocolVersion), got.Version)
	require.Len(t, got.Extensions, 1)
	assert.Equal(t, sparseFileExtension, got.Extensions[0].Name)
}

func TestSparseInitSendPacket(t *testing.T) {
	pkt := &sshFxInitPacket{
		Version:    sftpProtocolVersion,
		Extensions: []extensionPair{{Name: sparseFileExtension, Data: "1"}},
	}

	var buf bytes.Buffer
	require.NoError(t, sendPacket(&buf, pkt))

	wire := buf.Bytes()
	declared := uint32(wire[0])<<24 | uint32(wire[1])<<16 | uint32(wire[2])<<8 | uint32(wire[3])
	assert.Equal(t, uint32(len(wire)-4), declared)

	var got sshFxInitPacket
	require.NoError(t, got.UnmarshalBinary(wire[5:]))
	require.Len(t, got.Extensions, 1)
	assert.Equal(t, "1", got.Extensions[0].Data)
}

func TestLegacyVersionExtensionsWire(t *testing.T) {
	extensions := (&Server{}).versionExtensions()

	pkt := &sshFxVersionPacket{
		Version:    sftpProtocolVersion,
		Extensions: extensions,
	}

	var buf bytes.Buffer
	require.NoError(t, sendPacket(&buf, pkt))

	wire := buf.Bytes()
	declared := uint32(wire[0])<<24 | uint32(wire[1])<<16 | uint32(wire[2])<<8 | uint32(wire[3])
	assert.Equal(t, uint32(len(wire)-4), declared, "version packet length mismatch")

	typ, data, err := recvPacket(bytes.NewReader(wire), nil, 0)
	require.NoError(t, err)
	assert.Equal(t, fxp(sshFxpVersion), typ)

	version, rest, err := unmarshalUint32Safe(data)
	require.NoError(t, err)
	assert.Equal(t, uint32(sftpProtocolVersion), version)

	for len(rest) > 0 {
		var pair extensionPair
		pair, rest, err = unmarshalExtensionPair(rest)
		require.NoError(t, err)
		t.Logf("extension: %q=%q", pair.Name, pair.Data)
	}
}
