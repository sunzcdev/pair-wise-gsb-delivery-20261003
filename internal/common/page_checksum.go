package common

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"unsafe"

	"go.etcd.io/bbolt/errors"
)

// PageChecksumVersion is the on-file format version used by databases created
// with page checksumming enabled (see Options.PageChecksum).
//
// Page checksumming is an opt-in feature for newly created databases. There is
// no in-place upgrade: a version 2 database keeps the legacy format forever,
// and a version 3 database is always checksummmed.
const PageChecksumVersion uint32 = 3

// PageTrailerSize is the size, in bytes, of the page checksum trailer.
//
// For a version 3 database every physical block (a logical page plus its
// overflow pages) reserves its last 8 bytes for a little-endian FNV-1a 64-bit
// checksum. The trailer is deliberately placed at the end of the block rather
// than inside the 16-byte page header, so that:
//   - the page header layout shared with version 2 is unchanged, and
//   - the trailer can never be mistaken for leaf/branch payload, because the
//     usable capacity of the block is reduced by PageTrailerSize and no
//     element may point into it.
const PageTrailerSize = 8

// Covered page types:
//   - leaf pages (including the empty root leaf page at page id 3): the whole
//     block (16-byte header, leaf element records, key/value payload) is
//     covered, excluding the 8-byte trailer;
//   - inline buckets are not separate blocks: a small bucket is serialized
//     inside its parent leaf value, so its bytes are covered as part of that
//     leaf block instead of carrying their own trailer;
//   - branch pages: the whole block (header, branch element records, keys)
//     is covered, excluding the trailer field;
//   - overflow pages never carry their own header or trailer. They are the
//     continuation of a single leaf/branch/freelist block, so they are
//     covered transitively by the checksum stored in that block's trailer;
//   - freelist pages: the whole block (header plus free page ids, including
//     any overflow continuations) is covered, excluding the trailer field.
//
// Meta pages (ids 0 and 1) are not covered by the page trailer; they keep
// their own dedicated checksum in Meta.checksum.

// blockBytes returns the bytes backing a physical block spanning
// overflow+1 consecutive pages. pageSize is the database page size.
func blockBytes(p *Page, pageSize int) []byte {
	size := int(p.Overflow()+1) * pageSize
	return UnsafeByteSlice(unsafe.Pointer(p), 0, 0, size)
}

// pageChecksum computes the checksum of a physical block, ignoring the
// trailer field at the end of the block.
func pageChecksum(p *Page, pageSize int) uint64 {
	buf := blockBytes(p, pageSize)
	h := fnv.New64a()
	_, _ = h.Write(buf[:len(buf)-PageTrailerSize])
	return h.Sum64()
}

// SealPageChecksum computes and writes the checksum trailer for a physical
// block. It must be called after the block contents are final and before the
// block is written to disk.
func SealPageChecksum(p *Page, pageSize int) {
	buf := blockBytes(p, pageSize)
	binary.LittleEndian.PutUint64(buf[len(buf)-PageTrailerSize:], pageChecksum(p, pageSize))
}

// VerifyPageChecksum validates the checksum trailer of a physical block read
// from disk. It returns an error wrapping ErrPageChecksum, including the page
// id, when the stored checksum does not match the recomputed one.
func VerifyPageChecksum(p *Page, pageSize int) error {
	buf := blockBytes(p, pageSize)
	got := binary.LittleEndian.Uint64(buf[len(buf)-PageTrailerSize:])
	want := pageChecksum(p, pageSize)
	if got != want {
		return fmt.Errorf("page %d: %w: stored checksum %016x, recomputed %016x",
			p.Id(), errors.ErrPageChecksum, got, want)
	}
	return nil
}

// UsableBlockSize returns the payload capacity of a physical block of
// overflow+1 pages. Version 3 databases reserve PageTrailerSize bytes at the
// end of every block for the checksum.
func UsableBlockSize(pageSize int, overflow uint32, pageChecksum bool) int {
	size := int(overflow+1) * pageSize
	if pageChecksum {
		size -= PageTrailerSize
	}
	return size
}
