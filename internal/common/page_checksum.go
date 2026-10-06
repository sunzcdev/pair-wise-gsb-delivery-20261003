package common

import (
	"encoding/binary"
	"hash/fnv"
	"unsafe"
)

// PageChecksumSize is the number of bytes reserved at the very end of a
// checksummed page run for the checksum trailer.
const PageChecksumSize = 8

// Page checksum coverage rules (only for databases whose meta version is
// VersionPageChecksum):
//
//   - Leaf, branch and freelist pages carry a checksum trailer in the last
//     PageChecksumSize bytes of their page run. A page run is the page
//     itself plus all of its overflow pages, so overflow pages are covered
//     by the checksum of their head page and never carry a trailer of
//     their own.
//   - Meta pages (0 and 1) do not carry a trailer; they keep using their
//     built-in checksum field validated by Meta.Validate.
//   - Free and pending pages are not checksummed; their content is
//     undefined until they are reallocated and restamped at commit time.
//   - Inline bucket pages are payload of their parent leaf page and are
//     covered by the parent page's checksum.
//
// The trailer lives at a fixed location (the end of the page run) and is
// never part of the page header, so payload bytes can never be mistaken
// for a page header or for the checksum itself. The usable payload of a
// page run shrinks by PageChecksumSize bytes.

// PageRunSize returns the total size in bytes of the page run starting at
// this page, i.e. the page itself plus its overflow pages.
func (p *Page) PageRunSize(pageSize int) int {
	return (int(p.Overflow()) + 1) * pageSize
}

// WriteChecksum computes the checksum over the page run (excluding the
// trailer itself) and stores it in the trailer. It must be called once
// the page content is final, right before the page is written to disk.
func (p *Page) WriteChecksum(pageSize int) {
	run := p.PageRunSize(pageSize)
	buf := UnsafeByteSlice(unsafe.Pointer(p), 0, 0, run)
	binary.LittleEndian.PutUint64(buf[run-PageChecksumSize:], pageChecksum(buf[:run-PageChecksumSize]))
}

// Checksum returns the checksum stored in the page run trailer.
func (p *Page) Checksum(pageSize int) uint64 {
	run := p.PageRunSize(pageSize)
	buf := UnsafeByteSlice(unsafe.Pointer(p), 0, run-PageChecksumSize, run)
	return binary.LittleEndian.Uint64(buf)
}

// VerifyChecksum reports whether the checksum stored in the page run
// trailer matches the checksum computed over the page run content.
//
// Callers must ensure the whole page run is readable, i.e. that
// p.Id()+p.Overflow() is below the high water mark of the database.
func (p *Page) VerifyChecksum(pageSize int) bool {
	run := p.PageRunSize(pageSize)
	buf := UnsafeByteSlice(unsafe.Pointer(p), 0, 0, run)
	return binary.LittleEndian.Uint64(buf[run-PageChecksumSize:]) == pageChecksum(buf[:run-PageChecksumSize])
}

func pageChecksum(data []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(data)
	return h.Sum64()
}
