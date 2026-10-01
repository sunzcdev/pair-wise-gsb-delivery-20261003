package sftp

import "os"

// sparseHandleOf adapts an open server file to the sparse operations.
// It returns ErrSparseUnsupported when the handle or platform cannot perform
// sparse operations.
func sparseHandleOf(f file) (SparseFile, error) {
	osf, ok := f.(*os.File)
	if !ok {
		return nil, ErrSparseUnsupported
	}
	return sparseFromOSFile(osf)
}

func (p *sshFxpExtentsPacket) respond(svr *Server) responsePacket {
	f, ok := svr.getHandle(p.Handle)
	if !ok {
		return statusFromError(p.ID, EBADF)
	}

	sf, err := sparseHandleOf(f)
	if err != nil {
		return statusFromError(p.ID, err)
	}

	extents, err := sf.DataExtents(int64(p.Offset), int64(p.Length))
	if err != nil {
		return statusFromError(p.ID, err)
	}

	return &sshFxpExtentsReplyPacket{ID: p.ID, Extents: extents}
}

func (p *sshFxpPunchHolePacket) respond(svr *Server) responsePacket {
	f, ok := svr.getHandle(p.Handle)
	if !ok {
		return statusFromError(p.ID, EBADF)
	}

	sf, err := sparseHandleOf(f)
	if err != nil {
		return statusFromError(p.ID, err)
	}

	if err := sf.PunchHole(int64(p.Offset), int64(p.Length)); err != nil {
		return statusFromError(p.ID, err)
	}

	return statusFromError(p.ID, nil)
}
