package openssh

import (
	sshfx "github.com/pkg/sftp/internal/encoding/ssh/filexfer"
)

const extensionCopyData = "copy-data"

// RegisterExtensionCopyData registers the "copy-data" extended packet with the encoding/ssh/filexfer package.
func RegisterExtensionCopyData() {
	sshfx.RegisterExtendedPacketType(extensionCopyData, func() sshfx.ExtendedData {
		return new(CopyDataExtendedPacket)
	})
}

// ExtensionCopyData returns an ExtensionPair suitable to append into an sshfx.InitPacket or sshfx.VersionPacket.
func ExtensionCopyData() *sshfx.ExtensionPair {
	return &sshfx.ExtensionPair{
		Name: extensionCopyData,
		Data: "1",
	}
}

// CopyDataExtendedPacket defines the copy-data extended packet.
//
// See: https://github.com/openssh/openssh-portable/blob/master/PROTOCOL
type CopyDataExtendedPacket struct {
	ReadHandle  string
	ReadOffset  uint64
	ReadLength  uint64
	WriteHandle string
	WriteOffset uint64
}

// Type returns the SSH_FXP_EXTENDED packet type.
func (ep *CopyDataExtendedPacket) Type() sshfx.PacketType {
	return sshfx.PacketTypeExtended
}

// MarshalPacket returns ep as a two-part binary encoding of the full extended packet.
func (ep *CopyDataExtendedPacket) MarshalPacket(reqid uint32, b []byte) (header, payload []byte, err error) {
	p := &sshfx.ExtendedPacket{
		ExtendedRequest: extensionCopyData,

		Data: ep,
	}
	return p.MarshalPacket(reqid, b)
}

// MarshalInto encodes ep into the binary encoding of the copy-data extended packet-specific data.
func (ep *CopyDataExtendedPacket) MarshalInto(buf *sshfx.Buffer) {
	buf.AppendString(ep.ReadHandle)
	buf.AppendUint64(ep.ReadOffset)
	buf.AppendUint64(ep.ReadLength)
	buf.AppendString(ep.WriteHandle)
	buf.AppendUint64(ep.WriteOffset)
}

// MarshalBinary encodes ep into the binary encoding of the copy-data extended packet-specific data.
//
// NOTE: This _only_ encodes the packet-specific data, it does not encode the full extended packet.
func (ep *CopyDataExtendedPacket) MarshalBinary() ([]byte, error) {
	// string(read-handle) + uint64(read-offset) + uint64(read-length) +
	// string(write-handle) + uint64(write-offset)
	size := 4 + len(ep.ReadHandle) + 8 + 8 + 4 + len(ep.WriteHandle) + 8

	buf := sshfx.NewBuffer(make([]byte, 0, size))
	ep.MarshalInto(buf)
	return buf.Bytes(), nil
}

// UnmarshalFrom decodes the copy-data extended packet-specific data from buf.
func (ep *CopyDataExtendedPacket) UnmarshalFrom(buf *sshfx.Buffer) (err error) {
	*ep = CopyDataExtendedPacket{
		ReadHandle:  buf.ConsumeString(),
		ReadOffset:  buf.ConsumeUint64(),
		ReadLength:  buf.ConsumeUint64(),
		WriteHandle: buf.ConsumeString(),
		WriteOffset: buf.ConsumeUint64(),
	}

	return buf.Err
}

// UnmarshalBinary decodes the copy-data extended packet-specific data into ep.
func (ep *CopyDataExtendedPacket) UnmarshalBinary(data []byte) (err error) {
	return ep.UnmarshalFrom(sshfx.NewBuffer(data))
}
