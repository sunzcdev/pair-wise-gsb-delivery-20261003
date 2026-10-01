package sftp

import (
	"errors"
	"os"
)

// errInvalidSparseRanges is returned when allocate-regions receives empty,
// overflowing, or unordered/overlapping regions.
var errInvalidSparseRanges = errors.New("sftp: invalid sparse region list")

// SparseFileHandler is an optional Handlers interface for the RequestServer.
// When implemented, the server reports the sparse-file extension during
// negotiation. The returned capabilities describe what the registered
// handlers can do as a whole:
//
//   - Discover enables the query-allocated request; individual open file
//     handles must additionally implement AllocatedRanger.
//   - Allocate enables the allocate-regions request; individual open file
//     handles must additionally implement RegionAllocator.
//
// A handler that returns no capabilities is treated the same as one that
// does not implement this interface.
type SparseFileHandler interface {
	SparseCapabilities() SparseCaps
}

// sparseCapsFromHandlers returns the combined sparse capabilities advertised
// by the four RequestServer handlers.
func sparseCapsFromHandlers(h Handlers) SparseCaps {
	var caps SparseCaps

	seen := map[SparseFileHandler]bool{}

	for _, candidate := range []any{h.FileGet, h.FilePut, h.FileCmd, h.FileList} {
		sparse, ok := candidate.(SparseFileHandler)
		if !ok || seen[sparse] {
			continue
		}

		seen[sparse] = true

		c := sparse.SparseCapabilities()
		caps.Discover = caps.Discover || c.Discover
		caps.Allocate = caps.Allocate || c.Allocate
	}

	return caps
}

// sparseExtensionPairs converts the capability set into extension pairs.
func sparseExtensionPairs(caps SparseCaps) []sshExtensionPair {
	if !caps.Any() {
		return nil
	}

	return []sshExtensionPair{
		{Name: sparseFileExtension, Data: marshalSparseCaps(caps)},
	}
}

// versionExtensions returns the extension list advertised by the legacy
// filesystem Server, combining the standard extensions with the sparse-file
// capabilities this platform provides.
func (s *Server) versionExtensions() []sshExtensionPair {
	extensions := make([]sshExtensionPair, 0, len(sftpExtensions)+1)
	extensions = append(extensions, sftpExtensions...)
	extensions = append(extensions, sparseExtensionPairs(localSparseCapabilities())...)

	return extensions
}

// respondQueryAllocated handles the query-allocated request for a handle that
// implements AllocatedRanger.
func respondQueryAllocated(id uint32, f any, pkt *sshFxpQueryAllocatedServerPacket) responsePacket {
	ranger, ok := f.(AllocatedRanger)
	if !ok {
		return statusFromError(id, ErrSSHFxOpUnsupported)
	}

	if pkt.Regions == 0 {
		return statusFromError(id, errInvalidSparseRanges)
	}

	ranges, next, err := ranger.QueryAllocatedRanges(pkt.Offset, pkt.Regions)
	if err != nil {
		return statusFromError(id, err)
	}

	var logicalLength uint64

	if sizer, ok := f.(interface{ Stat() (os.FileInfo, error) }); ok {
		if info, err := sizer.Stat(); err == nil {
			logicalLength = uint64(info.Size())
		}
	}

	return &sshFxpAllocatedRangesPacket{
		ID:     id,
		Offset: next,
		Length: logicalLength,
		Ranges: ranges,
	}
}

// respondAllocateRegions handles the allocate-regions request for a handle
// that implements RegionAllocator.
func respondAllocateRegions(id uint32, f any, pkt *sshFxpAllocateRegionsServerPacket) responsePacket {
	allocator, ok := f.(RegionAllocator)
	if !ok {
		return statusFromError(id, ErrSSHFxOpUnsupported)
	}

	if err := validateRanges(pkt.Ranges); err != nil {
		return statusFromError(id, err)
	}

	return statusFromError(id, allocator.AllocateRegions(pkt.Ranges))
}

// validateRanges rejects empty, overflowing, or out-of-order/overlapping
// regions before they reach a backend.
func validateRanges(ranges []FileRange) error {
	var previous FileRange

	for i, r := range ranges {
		if r.Length == 0 {
			return errInvalidSparseRanges
		}

		if r.End() < r.Offset {
			return errInvalidSparseRanges
		}

		if i > 0 && r.Offset < previous.End() {
			return errInvalidSparseRanges
		}

		previous = r
	}

	return nil
}
