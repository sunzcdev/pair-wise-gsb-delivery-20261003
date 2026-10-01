package sftp

import (
	"encoding/binary"

	"github.com/pkg/sftp/internal/sparse"
)

// sshFxpExtentsPacket carries an extents@sftp.pkg.github.com extended request.
// It asks the server to report the allocated data extents of an open file.
type sshFxpExtentsPacket struct {
	ID     uint32
	Handle string
	Offset uint64
	Length uint64
}

func (p *sshFxpExtentsPacket) id() uint32 { return p.ID }

func (p *sshFxpExtentsPacket) MarshalBinary() ([]byte, error) {
	return (&sparse.ExtentsRequestPacket{
		ID:     p.ID,
		Handle: p.Handle,
		Offset: p.Offset,
		Length: p.Length,
	}).MarshalPacket()
}

func (p *sshFxpExtentsPacket) UnmarshalRequestBody(b []byte) error {
	body := sparse.ExtentsRequestPacket{}
	if err := body.UnmarshalRequestBody(b); err != nil {
		return err
	}
	p.Handle = body.Handle
	p.Offset = body.Offset
	p.Length = body.Length
	return nil
}

func (p *sshFxpExtentsPacket) readonly() bool { return true }

func (p *sshFxpExtentsPacket) UnmarshalBinary(data []byte) error {
	body := sparse.ExtentsRequestPacket{}
	if err := body.UnmarshalBinary(data); err != nil {
		return err
	}
	p.ID = body.ID
	p.Handle = body.Handle
	p.Offset = body.Offset
	p.Length = body.Length
	return nil
}

// sshFxpPunchHolePacket carries a punch-hole@sftp.pkg.github.com extended
// request. It asks the server to turn a byte range of an open file into a hole.
type sshFxpPunchHolePacket struct {
	ID     uint32
	Handle string
	Offset uint64
	Length uint64
}

func (p *sshFxpPunchHolePacket) id() uint32 { return p.ID }

func (p *sshFxpPunchHolePacket) MarshalBinary() ([]byte, error) {
	return (&sparse.PunchHolePacket{
		ID:     p.ID,
		Handle: p.Handle,
		Offset: p.Offset,
		Length: p.Length,
	}).MarshalPacket()
}

func (p *sshFxpPunchHolePacket) UnmarshalRequestBody(b []byte) error {
	body := sparse.PunchHolePacket{}
	if err := body.UnmarshalRequestBody(b); err != nil {
		return err
	}
	p.Handle = body.Handle
	p.Offset = body.Offset
	p.Length = body.Length
	return nil
}

func (p *sshFxpPunchHolePacket) UnmarshalBinary(data []byte) error {
	body := sparse.PunchHolePacket{}
	if err := body.UnmarshalBinary(data); err != nil {
		return err
	}
	p.ID = body.ID
	p.Handle = body.Handle
	p.Offset = body.Offset
	p.Length = body.Length
	return nil
}

func (p *sshFxpPunchHolePacket) readonly() bool { return false }

// sshFxpExtentsReplyPacket is the SSH_FXP_EXTENDED_REPLY for an extents
// request.
type sshFxpExtentsReplyPacket struct {
	ID      uint32
	Extents []sparse.Extent
}

func (p *sshFxpExtentsReplyPacket) MarshalBinary() ([]byte, error) {
	return (&sparse.ExtentsReplyPacket{
		ID:      p.ID,
		Extents: p.Extents,
	}).MarshalBinary()
}

func (p *sshFxpExtentsReplyPacket) UnmarshalBinary(data []byte) error {
	// As delivered to a client-side caller, data starts at the uint32(id)
	// (the length prefix and type byte have been consumed by the connection).
	// A full wire packet may also be unmarshalled when the type byte is present.
	if len(data) >= 5 && data[0] == sshFxpExtendedReply {
		reply := sparse.ExtentsReplyPacket{}
		if err := reply.UnmarshalBinary(data); err != nil {
			return err
		}
		p.ID = reply.ID
		p.Extents = reply.Extents
		return nil
	}

	if len(data) < 4 {
		return errShortPacket
	}
	p.ID = binary.BigEndian.Uint32(data[:4])
	b := data[4:]
	if len(b) < 4 {
		return errShortPacket
	}
	count := binary.BigEndian.Uint32(b[:4])
	b = b[4:]
	if uint64(len(b)) < uint64(count)*16 {
		return errShortPacket
	}

	p.Extents = make([]sparse.Extent, 0, count)
	for i := uint32(0); i < count; i++ {
		e := sparse.Extent{
			Offset: binary.BigEndian.Uint64(b[:8]),
			Length: binary.BigEndian.Uint64(b[8:16]),
		}
		b = b[16:]
		p.Extents = append(p.Extents, e)
	}
	return nil
}
