package sparse

import (
	"encoding/binary"
	"errors"
)

// Wire packet type constants from draft-ietf-secsh-filexfer-02.
const (
	packetTypeExtended      = 200 // SSH_FXP_EXTENDED
	packetTypeExtendedReply = 201 // SSH_FXP_EXTENDED_REPLY
)

var errShortPacket = errors.New("sparse: packet too short")

func appendString(b []byte, s string) []byte {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	b = append(b, n[:]...)
	return append(b, s...)
}

func consumeUint32(b []byte) (uint32, []byte, error) {
	if len(b) < 4 {
		return 0, nil, errShortPacket
	}
	return binary.BigEndian.Uint32(b[:4]), b[4:], nil
}

func consumeUint64(b []byte) (uint64, []byte, error) {
	if len(b) < 8 {
		return 0, nil, errShortPacket
	}
	return binary.BigEndian.Uint64(b[:8]), b[8:], nil
}

func consumeString(b []byte) (string, []byte, error) {
	n, b, err := consumeUint32(b)
	if err != nil {
		return "", nil, err
	}
	if uint32(len(b)) < n {
		return "", nil, errShortPacket
	}
	return string(b[:n]), b[n:], nil
}

func startPacket(reqid uint32, request string, bodyLen int) []byte {
	b := make([]byte, 0, 4+1+4+4+len(request)+bodyLen)
	b = append(b, 0, 0, 0, 0) // uint32(payload length), filled by MarshalPacket
	b = append(b, packetTypeExtended)
	var id [4]byte
	binary.BigEndian.PutUint32(id[:], reqid)
	b = append(b, id[:]...)
	return appendString(b, request)
}

// MarshalPacket returns the full SSH_FXP_EXTENDED packet for the request,
// including the leading uint32 length prefix.
func (r *ExtentsRequestPacket) MarshalPacket() ([]byte, error) {
	b := startPacket(r.ID, ExtentsRequest, 4+len(r.Handle)+8+8)
	b = appendString(b, r.Handle)
	var v [8]byte
	binary.BigEndian.PutUint64(v[:], r.Offset)
	b = append(b, v[:]...)
	binary.BigEndian.PutUint64(v[:], r.Length)
	b = append(b, v[:]...)
	binary.BigEndian.PutUint32(b[:4], uint32(len(b)-4))
	return b, nil
}

// ExtentsRequestPacket is the decoded extents@sftp.pkg.github.com request.
type ExtentsRequestPacket struct {
	ID     uint32
	Handle string
	Offset uint64
	Length uint64
}

// UnmarshalRequestBody decodes the packet-specific body, assuming the
// uint32(request-id) and the extended-request name string have already been
// consumed.
func (r *ExtentsRequestPacket) UnmarshalRequestBody(b []byte) error {
	var err error
	if r.Handle, b, err = consumeString(b); err != nil {
		return err
	}
	if r.Offset, b, err = consumeUint64(b); err != nil {
		return err
	}
	if r.Length, _, err = consumeUint64(b); err != nil {
		return err
	}
	return nil
}

// UnmarshalBinary decodes a complete SSH_FXP_EXTENDED packet carrying an
// extents request.
func (r *ExtentsRequestPacket) UnmarshalBinary(data []byte) error {
	b, err := consumeExtendedHeader(data, ExtentsRequest, &r.ID)
	if err != nil {
		return err
	}
	return r.UnmarshalRequestBody(b)
}

// PunchHolePacket is the decoded punch-hole@sftp.pkg.github.com request.
type PunchHolePacket struct {
	ID     uint32
	Handle string
	Offset uint64
	Length uint64
}

// MarshalPacket returns the full SSH_FXP_EXTENDED packet for the request.
func (p *PunchHolePacket) MarshalPacket() ([]byte, error) {
	b := startPacket(p.ID, PunchRequest, 4+len(p.Handle)+8+8)
	b = appendString(b, p.Handle)
	var v [8]byte
	binary.BigEndian.PutUint64(v[:], p.Offset)
	b = append(b, v[:]...)
	binary.BigEndian.PutUint64(v[:], p.Length)
	b = append(b, v[:]...)
	binary.BigEndian.PutUint32(b[:4], uint32(len(b)-4))
	return b, nil
}

// UnmarshalRequestBody decodes the packet-specific body, assuming the
// uint32(request-id) and the extended-request name string have already been
// consumed.
func (p *PunchHolePacket) UnmarshalRequestBody(b []byte) error {
	var err error
	if p.Handle, b, err = consumeString(b); err != nil {
		return err
	}
	if p.Offset, b, err = consumeUint64(b); err != nil {
		return err
	}
	if p.Length, _, err = consumeUint64(b); err != nil {
		return err
	}
	return nil
}

// UnmarshalBinary decodes a complete SSH_FXP_EXTENDED packet carrying a
// punch-hole request.
func (p *PunchHolePacket) UnmarshalBinary(data []byte) error {
	b, err := consumeExtendedHeader(data, PunchRequest, &p.ID)
	if err != nil {
		return err
	}
	return p.UnmarshalRequestBody(b)
}

// consumeExtendedHeader validates the type byte, request id and the
// extended-request name, returning the remaining packet-specific body.
func consumeExtendedHeader(data []byte, request string, id *uint32) ([]byte, error) {
	if len(data) < 9 || data[4] != packetTypeExtended {
		return nil, errShortPacket
	}
	b := data[5:]
	var err error
	if *id, b, err = consumeUint32(b); err != nil {
		return nil, err
	}
	name, b, err := consumeString(b)
	if err != nil {
		return nil, err
	}
	if name != request {
		return nil, errors.New("sparse: unexpected extended request " + name)
	}
	return b, nil
}

// ExtentsReplyPacket is the SSH_FXP_EXTENDED_REPLY for an extents request.
//
// The extents are the allocated, half-open [Offset, Offset+Length) ranges of
// the queried region, sorted by offset, non-overlapping and non-adjacent.
// Every byte of the queried region not covered by an extent is a hole.
type ExtentsReplyPacket struct {
	ID      uint32
	Extents []Extent
}

// MarshalBinary encodes the reply as a complete SSH_FXP_EXTENDED_REPLY packet.
func (p *ExtentsReplyPacket) MarshalBinary() ([]byte, error) {
	size := 4 + 1 + 4 + 4 + len(p.Extents)*16
	b := make([]byte, 0, size)
	b = append(b, 0, 0, 0, 0) // length prefix
	b = append(b, packetTypeExtendedReply)
	var v [8]byte
	var u [4]byte
	binary.BigEndian.PutUint32(u[:], p.ID)
	b = append(b, u[:]...)
	binary.BigEndian.PutUint32(u[:], uint32(len(p.Extents)))
	b = append(b, u[:]...)
	for _, e := range p.Extents {
		binary.BigEndian.PutUint64(v[:], e.Offset)
		b = append(b, v[:]...)
		binary.BigEndian.PutUint64(v[:], e.Length)
		b = append(b, v[:]...)
	}
	binary.BigEndian.PutUint32(b[:4], uint32(len(b)-4))
	return b, nil
}

// UnmarshalBinary decodes a complete SSH_FXP_EXTENDED_REPLY extents packet.
func (p *ExtentsReplyPacket) UnmarshalBinary(data []byte) error {
	if len(data) < 9 || data[4] != packetTypeExtendedReply {
		return errShortPacket
	}
	b := data[5:]
	var err error
	if p.ID, b, err = consumeUint32(b); err != nil {
		return err
	}
	count, b, err := consumeUint32(b)
	if err != nil {
		return err
	}
	if uint64(len(b)) < uint64(count)*16 {
		return errShortPacket
	}
	p.Extents = make([]Extent, 0, count)
	for i := uint32(0); i < count; i++ {
		var e Extent
		if e.Offset, b, err = consumeUint64(b); err != nil {
			return err
		}
		if e.Length, b, err = consumeUint64(b); err != nil {
			return err
		}
		p.Extents = append(p.Extents, e)
	}
	return nil
}
