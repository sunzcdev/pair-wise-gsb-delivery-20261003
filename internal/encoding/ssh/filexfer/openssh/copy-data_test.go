package openssh

import (
	"bytes"
	"testing"

	sshfx "github.com/pkg/sftp/internal/encoding/ssh/filexfer"
)

var _ sshfx.PacketMarshaller = &CopyDataExtendedPacket{}

func init() {
	RegisterExtensionCopyData()
}

func TestCopyDataExtendedPacket(t *testing.T) {
	const (
		id          = 42
		readHandle  = "readhandle"
		readOffset  = 1
		readLength  = 2
		writeHandle = "writehandle"
		writeOffset = 3
	)

	ep := &CopyDataExtendedPacket{
		ReadHandle:  readHandle,
		ReadOffset:  readOffset,
		ReadLength:  readLength,
		WriteHandle: writeHandle,
		WriteOffset: writeOffset,
	}

	data, err := sshfx.ComposePacket(ep.MarshalPacket(id, nil))
	if err != nil {
		t.Fatal("unexpected error:", err)
	}

	want := []byte{
		0x00, 0x00, 0x00, 71,
		200,
		0x00, 0x00, 0x00, 42,
		0x00, 0x00, 0x00, 9, 'c', 'o', 'p', 'y', '-', 'd', 'a', 't', 'a',
		0x00, 0x00, 0x00, 10, 'r', 'e', 'a', 'd', 'h', 'a', 'n', 'd', 'l', 'e',
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02,
		0x00, 0x00, 0x00, 11, 'w', 'r', 'i', 't', 'e', 'h', 'a', 'n', 'd', 'l', 'e',
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03,
	}

	if !bytes.Equal(data, want) {
		t.Fatalf("MarshalPacket() = %X, but wanted %X", data, want)
	}

	var p sshfx.ExtendedPacket

	// UnmarshalPacketBody assumes the (length, type, request-id) have already been consumed.
	if err := p.UnmarshalPacketBody(sshfx.NewBuffer(data[9:])); err != nil {
		t.Fatal("unexpected error:", err)
	}

	if p.ExtendedRequest != extensionCopyData {
		t.Errorf("UnmarshalPacketBody(): ExtendedRequest was %q, but expected %q", p.ExtendedRequest, extensionCopyData)
	}

	ep, ok := p.Data.(*CopyDataExtendedPacket)
	if !ok {
		t.Fatalf("UnmarshaledPacketBody(): Data was type %T, but expected *CopyDataExtendedPacket", p.Data)
	}

	if ep.ReadHandle != readHandle {
		t.Errorf("UnmarshalPacketBody(): ReadHandle was %q, but expected %q", ep.ReadHandle, readHandle)
	}

	if ep.ReadOffset != readOffset {
		t.Errorf("UnmarshalPacketBody(): ReadOffset was %d, but expected %d", ep.ReadOffset, readOffset)
	}

	if ep.ReadLength != readLength {
		t.Errorf("UnmarshalPacketBody(): ReadLength was %d, but expected %d", ep.ReadLength, readLength)
	}

	if ep.WriteHandle != writeHandle {
		t.Errorf("UnmarshalPacketBody(): WriteHandle was %q, but expected %q", ep.WriteHandle, writeHandle)
	}

	if ep.WriteOffset != writeOffset {
		t.Errorf("UnmarshalPacketBody(): WriteOffset was %d, but expected %d", ep.WriteOffset, writeOffset)
	}
}
