package sftp

// Sparse file transfer extension.
//
// Modeled on the "sparse-file" extension described in
// https://www.ietf.org/archive/id/draft-ietf-secsh-filexfer-13.txt,
// negotiated through extension pairs on SSH_FXP_INIT / SSH_FXP_VERSION.
//
// The extension is negotiated independently for each direction, since a
// backend may be able to discover allocated data regions without being able
// to recreate holes, or vice versa:
//
//	sparse-file@ietf.org          full support (discover + allocate)
//	sparse-file@ietf.org=discover the sender can report allocated regions
//	sparse-file@ietf.org=allocate the receiver can allocate data regions
//
// The extension data is a comma-separated list of capability tokens.
const (
	sparseFileExtension = "sparse-file@ietf.org"

	sparseTokenDiscover = "discover"
	sparseTokenAllocate = "allocate"

	// extQueryAllocated is an SSH_FXP_EXTENDED request.
	//
	//	uint32   request-id
	//	string   extended-request ("query-allocated@ietf.org")
	//	string   handle
	//	uint64   offset
	//	uint32   region-count (must be >= 1)
	extQueryAllocated = "query-allocated@ietf.org"

	// extAllocateRegions is an SSH_FXP_EXTENDED request.
	//
	//	uint32   request-id
	//	string   extended-request ("allocate-regions@ietf.org")
	//	string   handle
	//	uint32   region-count
	//	repeated:
	//	    uint64   offset
	//	    uint64   length
	extAllocateRegions = "allocate-regions@ietf.org"
)

// FileRange describes a single contiguous, allocated region of a file.
// A region is backed by real storage; everything between regions is a hole
// that reads back as zero bytes without consuming space.
type FileRange struct {
	Offset uint64
	Length uint64
}

// End returns the offset immediately following the region.
func (r FileRange) End() uint64 { return r.Offset + r.Length }

// SparseCaps reports which parts of the sparse-file extension an endpoint
// supports.
type SparseCaps struct {
	// Discover is true when the endpoint can report allocated data regions
	// (it implements the query-allocated request).
	Discover bool
	// Allocate is true when the endpoint can allocate data regions and leave
	// the gaps between them as holes (it implements allocate-regions).
	Allocate bool
}

// Any reports whether either capability is supported.
func (c SparseCaps) Any() bool { return c.Discover || c.Allocate }

// parseSparseCaps parses the extension-data string advertised for the
// sparse-file extension. An empty value or "1" means full support, matching
// the convention used by other extensions.
func parseSparseCaps(data string) SparseCaps {
	if data == "" || data == "1" {
		return SparseCaps{Discover: true, Allocate: true}
	}

	var caps SparseCaps

	start := 0

	for i := 0; i <= len(data); i++ {
		if i != len(data) && data[i] != ',' {
			continue
		}

		switch data[start:i] {
		case sparseTokenDiscover:
			caps.Discover = true
		case sparseTokenAllocate:
			caps.Allocate = true
		case "1", "":
			caps.Discover = true
			caps.Allocate = true
		}

		start = i + 1
	}

	return caps
}

// marshalSparseCaps renders the capability set as an extension-data value.
func marshalSparseCaps(caps SparseCaps) string {
	if caps.Discover && caps.Allocate {
		return "1"
	}

	switch {
	case caps.Discover:
		return sparseTokenDiscover
	case caps.Allocate:
		return sparseTokenAllocate
	default:
		return ""
	}
}

// sshFxpQueryAllocatedPacket is the client-side "query-allocated@ietf.org"
// extended request.
type sshFxpQueryAllocatedPacket struct {
	ID      uint32
	Handle  string
	Offset  uint64
	Regions uint32
}

func (p *sshFxpQueryAllocatedPacket) id() uint32 { return p.ID }

func (p *sshFxpQueryAllocatedPacket) MarshalBinary() ([]byte, error) {
	l := 4 + 1 + 4 + // uint32(length) + byte(type) + uint32(id)
		4 + len(extQueryAllocated) +
		4 + len(p.Handle) +
		8 + 4

	b := make([]byte, 4, l)
	b = append(b, sshFxpExtended)
	b = marshalUint32(b, p.ID)
	b = marshalString(b, extQueryAllocated)
	b = marshalString(b, p.Handle)
	b = marshalUint64(b, p.Offset)
	b = marshalUint32(b, p.Regions)

	return b, nil
}

// sshFxpQueryAllocatedServerPacket is the server-side view of the request.
type sshFxpQueryAllocatedServerPacket struct {
	ID              uint32
	ExtendedRequest string
	Handle          string
	Offset          uint64
	Regions         uint32
}

func (p *sshFxpQueryAllocatedServerPacket) id() uint32        { return p.ID }
func (p *sshFxpQueryAllocatedServerPacket) readonly() bool    { return true }
func (p *sshFxpQueryAllocatedServerPacket) getHandle() string { return p.Handle }

func (p *sshFxpQueryAllocatedServerPacket) UnmarshalBinary(b []byte) error {
	var err error

	if p.ID, b, err = unmarshalUint32Safe(b); err != nil {
		return err
	} else if p.ExtendedRequest, b, err = unmarshalStringSafe(b); err != nil {
		return err
	} else if p.Handle, b, err = unmarshalStringSafe(b); err != nil {
		return err
	}

	p.Offset, b, err = unmarshalUint64Safe(b)
	if err != nil {
		return err
	}

	p.Regions, _, err = unmarshalUint32Safe(b)

	return err
}

// respond handles the query-allocated request on the legacy filesystem Server.
func (p *sshFxpQueryAllocatedServerPacket) respond(s *Server) responsePacket {
	f, ok := s.getHandle(p.Handle)
	if !ok {
		return statusFromError(p.ID, EBADF)
	}

	return respondQueryAllocated(p.ID, sparseAdapter(f), p)
}

// sshFxpAllocatedRangesPacket is the SSH_FXP_EXTENDED_REPLY returned for a
// query-allocated request.
type sshFxpAllocatedRangesPacket struct {
	ID uint32
	// Offset is the file offset at which the scan should continue (start of
	// the next hole, or the logical file size at EOF).
	Offset uint64
	// Length is the logical size of the file, allowing trailing holes to be
	// preserved even when no data follows them.
	Length uint64
	Ranges []FileRange
}

func (p *sshFxpAllocatedRangesPacket) id() uint32 { return p.ID }

func (p *sshFxpAllocatedRangesPacket) marshalPacket() ([]byte, []byte, error) {
	// header holds the length, type and request-id; payload is the body.
	header := make([]byte, 0, 4+1+4)
	header = append(header, 0, 0, 0, 0, sshFxpExtendedReply)
	header = marshalUint32(header, p.ID)

	payload := make([]byte, 0, 8+8+4+16*len(p.Ranges))
	payload = marshalUint64(payload, p.Offset)
	payload = marshalUint64(payload, p.Length)
	payload = marshalUint32(payload, uint32(len(p.Ranges)))

	for _, r := range p.Ranges {
		payload = marshalUint64(payload, r.Offset)
		payload = marshalUint64(payload, r.Length)
	}

	return header, payload, nil
}

func (p *sshFxpAllocatedRangesPacket) MarshalBinary() ([]byte, error) {
	header, payload, err := p.marshalPacket()
	return append(header, payload...), err
}

func (p *sshFxpAllocatedRangesPacket) unmarshalBody(id uint32, data []byte) error {
	p.ID = id

	var err error

	// data still carries the leading request-id, which the caller has already
	// verified; skip it.
	if _, data, err = unmarshalUint32Safe(data); err != nil {
		return err
	}

	if p.Offset, data, err = unmarshalUint64Safe(data); err != nil {
		return err
	} else if p.Length, data, err = unmarshalUint64Safe(data); err != nil {
		return err
	}

	count, data, err := unmarshalUint32Safe(data)
	if err != nil {
		return err
	}

	// Each range is 16 bytes; reject obviously malformed payloads.
	if uint64(count) > uint64(len(data))/16 {
		return errShortPacket
	}

	p.Ranges = make([]FileRange, count)

	for i := uint32(0); i < count; i++ {
		var r FileRange

		if r.Offset, data, err = unmarshalUint64Safe(data); err != nil {
			return err
		} else if r.Length, data, err = unmarshalUint64Safe(data); err != nil {
			return err
		}

		p.Ranges[i] = r
	}

	return nil
}

// sshFxpAllocateRegionsPacket is the client-side "allocate-regions@ietf.org"
// extended request. The regions must be non-overlapping and ordered.
type sshFxpAllocateRegionsPacket struct {
	ID     uint32
	Handle string
	Ranges []FileRange
}

func (p *sshFxpAllocateRegionsPacket) id() uint32 { return p.ID }

func (p *sshFxpAllocateRegionsPacket) MarshalBinary() ([]byte, error) {
	l := 4 + 1 + 4 + // uint32(length) + byte(type) + uint32(id)
		4 + len(extAllocateRegions) +
		4 + len(p.Handle) +
		4 + 16*len(p.Ranges)

	b := make([]byte, 4, l)
	b = append(b, sshFxpExtended)
	b = marshalUint32(b, p.ID)
	b = marshalString(b, extAllocateRegions)
	b = marshalString(b, p.Handle)
	b = marshalUint32(b, uint32(len(p.Ranges)))

	for _, r := range p.Ranges {
		b = marshalUint64(b, r.Offset)
		b = marshalUint64(b, r.Length)
	}

	return b, nil
}

// sshFxpAllocateRegionsServerPacket is the server-side view of the request.
type sshFxpAllocateRegionsServerPacket struct {
	ID              uint32
	ExtendedRequest string
	Handle          string
	Ranges          []FileRange
}

func (p *sshFxpAllocateRegionsServerPacket) id() uint32        { return p.ID }
func (p *sshFxpAllocateRegionsServerPacket) readonly() bool    { return false }
func (p *sshFxpAllocateRegionsServerPacket) notReadOnly()      {}
func (p *sshFxpAllocateRegionsServerPacket) getHandle() string { return p.Handle }

func (p *sshFxpAllocateRegionsServerPacket) UnmarshalBinary(b []byte) error {
	var err error

	if p.ID, b, err = unmarshalUint32Safe(b); err != nil {
		return err
	} else if p.ExtendedRequest, b, err = unmarshalStringSafe(b); err != nil {
		return err
	} else if p.Handle, b, err = unmarshalStringSafe(b); err != nil {
		return err
	}

	count, b, err := unmarshalUint32Safe(b)
	if err != nil {
		return err
	}

	if uint64(count) > uint64(len(b))/16 {
		return errShortPacket
	}

	p.Ranges = make([]FileRange, count)

	for i := range p.Ranges {
		if p.Ranges[i].Offset, b, err = unmarshalUint64Safe(b); err != nil {
			return err
		} else if p.Ranges[i].Length, b, err = unmarshalUint64Safe(b); err != nil {
			return err
		}
	}

	return nil
}

// respond handles the allocate-regions request on the legacy filesystem
// Server.
func (p *sshFxpAllocateRegionsServerPacket) respond(s *Server) responsePacket {
	f, ok := s.getHandle(p.Handle)
	if !ok {
		return statusFromError(p.ID, EBADF)
	}

	return respondAllocateRegions(p.ID, sparseAdapter(f), p)
}
