package sftp

import (
	"io"

	"github.com/pkg/sftp/internal/sparse"
)

// SparseCapabilitiesReporter is an optional Handlers member that declares the
// sparse operations the backend can perform. A FileReader implementing
// SparseDataReaderAt advertises sparse.CapExtents, and a FileWriter
// implementing SparseHoleWriterAt advertises sparse.CapPunch.
//
// When absent, capabilities are taken from the local operating system, which
// matches the built-in os-backed server.
type SparseCapabilitiesReporter interface {
	SparseCapabilities() sparse.Capability
}

func (rs *RequestServer) sparseCapabilities() sparse.Capability {
	if rs.sparseCapsSet {
		return rs.sparseCaps
	}
	if r, ok := rs.Handlers.FileGet.(SparseCapabilitiesReporter); ok {
		return r.SparseCapabilities()
	}
	// A custom backend that does not declare its capabilities is assumed not
	// to implement the sparse handle interfaces.
	return 0
}

// WithRSSparseCapabilities overrides the sparse capabilities advertised in the
// SSH_FXP_VERSION response. It is mainly useful for in-memory or remote-backed
// handlers whose capabilities differ from those of the local operating system.
func WithRSSparseCapabilities(caps sparse.Capability) RequestServerOption {
	return func(rs *RequestServer) {
		rs.sparseCaps = caps
		rs.sparseCapsSet = true
	}
}

func (rs *RequestServer) openReaderAt(handle string) (io.ReaderAt, *Request, bool) {
	r, ok := rs.getRequest(handle)
	if !ok {
		return nil, nil, false
	}
	rd, _, rw := r.getAllReaderWriters()
	if rw != nil {
		return rw, r, true
	}
	return rd, r, rd != nil
}

func (rs *RequestServer) openWriterAt(handle string) (io.WriterAt, *Request, bool) {
	r, ok := rs.getRequest(handle)
	if !ok {
		return nil, nil, false
	}
	_, wr, rw := r.getAllReaderWriters()
	if rw != nil {
		return rw, r, true
	}
	return wr, r, wr != nil
}

func (rs *RequestServer) handleExtents(pkt *sshFxpExtentsPacket) responsePacket {
	rd, _, ok := rs.openReaderAt(pkt.Handle)
	if !ok {
		return statusFromError(pkt.ID, EBADF)
	}

	sr, ok := rd.(SparseDataReaderAt)
	if !ok {
		return statusFromError(pkt.ID, ErrSSHFxOpUnsupported)
	}

	extents, err := sr.DataExtents(int64(pkt.Offset), int64(pkt.Length))
	if err != nil {
		return statusFromError(pkt.ID, err)
	}

	reply := &sshFxpExtentsReplyPacket{ID: pkt.ID}
	for _, e := range extents {
		reply.Extents = append(reply.Extents, sparse.Extent(e))
	}
	return reply
}

func (rs *RequestServer) handlePunchHole(pkt *sshFxpPunchHolePacket) responsePacket {
	wr, _, ok := rs.openWriterAt(pkt.Handle)
	if !ok {
		return statusFromError(pkt.ID, EBADF)
	}

	sw, ok := wr.(SparseHoleWriterAt)
	if !ok {
		return statusFromError(pkt.ID, ErrSSHFxOpUnsupported)
	}

	if err := sw.PunchHole(int64(pkt.Offset), int64(pkt.Length)); err != nil {
		return statusFromError(pkt.ID, err)
	}
	return statusFromError(pkt.ID, nil)
}
