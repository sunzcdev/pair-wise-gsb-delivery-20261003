// Copyright 2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz

import (
	"bytes"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"sort"

	"github.com/ulikunitz/xz/lzma"
)

// Default limits for the memory that a [RandomReader] is willing to
// allocate for index data, LZMA2 dictionaries and a single decompressed
// block. The values protect against crafted files that announce huge
// or contradictory sizes.
const (
	DefaultMaxIndexMemory = 16 * 1024 * 1024
	DefaultMaxDictCap     = 64 * 1024 * 1024
	DefaultMaxBlockMemory = 256 * 1024 * 1024
)

// RandomReaderConfig configures a [RandomReader]. The zero value is
// ready to use and applies the default limits.
type RandomReaderConfig struct {
	// DictCap sets the initial LZMA2 dictionary capacity. A block
	// header may request a larger dictionary; such requests are
	// rejected once they exceed MaxDictCap.
	DictCap int
	// MaxIndexMemory limits the number of bytes spent on the indexes
	// of all streams.
	MaxIndexMemory int64
	// MaxDictCap limits the LZMA2 dictionary capacity announced by
	// any block header.
	MaxDictCap int
	// MaxBlockMemory limits the uncompressed size of a single block
	// and therefore the buffer used to decompress a block.
	MaxBlockMemory int64
}

// Verify checks the random reader configuration and replaces zero
// values with defaults.
func (c *RandomReaderConfig) Verify() error {
	if c == nil {
		return errors.New("xz: random reader parameters are nil")
	}
	if c.MaxIndexMemory == 0 {
		c.MaxIndexMemory = DefaultMaxIndexMemory
	}
	if c.MaxIndexMemory < 0 {
		return errors.New("xz: MaxIndexMemory must not be negative")
	}
	if c.MaxDictCap == 0 {
		c.MaxDictCap = DefaultMaxDictCap
	}
	if c.MaxBlockMemory == 0 {
		c.MaxBlockMemory = DefaultMaxBlockMemory
	}
	if c.MaxBlockMemory < 0 {
		return errors.New("xz: MaxBlockMemory must not be negative")
	}
	lc := lzma.Reader2Config{DictCap: c.DictCap}
	if err := lc.Verify(); err != nil {
		return err
	}
	c.DictCap = lc.DictCap
	if c.MaxDictCap < c.DictCap {
		return errors.New("xz: MaxDictCap smaller than DictCap")
	}
	return nil
}

// blockInfo locates a single block in the compressed file and records
// its offset in the logical concatenated uncompressed data.
type blockInfo struct {
	// compressedOff is the offset of the block header.
	compressedOff int64
	// headerLen is the length of the block header.
	headerLen int
	// compressedSize is the length of the compressed filter data.
	compressedSize int64
	// unpaddedSize equals headerLen + compressedSize + checkLen.
	unpaddedSize int64
	// uncompressedSize is the number of uncompressed bytes of the
	// block as given in the index.
	uncompressedSize int64
	// unpOff is the uncompressed offset of the first byte of the
	// block in the concatenated stream data.
	unpOff int64
	// checkLen is the length of the block check field.
	checkLen int
	// dictCap is the dictionary capacity requested by the block.
	dictCap int
	// filters of the block header.
	filters []filter
}

// RandomReader supports random access to one or multiple concatenated
// xz streams, including legal inter-stream padding. The file layout is
// recovered by scanning the stream footers and indexes backwards and
// every block header is validated when the reader is opened.
//
// ReadAt decompresses only the blocks that overlap the requested
// interval and verifies the check value of each block it reads; blocks
// that are not read are not decompressed and their content check is
// not verified.
type RandomReader struct {
	RandomReaderConfig

	ra      io.ReaderAt
	size    int64
	blocks  []blockInfo
	unpSize int64
	newHash func() hash.Hash
	flags   byte
}

// NewRandomReader creates a random access reader for the xz data of
// the given size accessible through ra. The function parses all stream
// footers and indexes backwards, verifies their CRC-32 checksums,
// validates the size relations and validates every block header.
func NewRandomReader(ra io.ReaderAt, size int64) (*RandomReader, error) {
	return RandomReaderConfig{}.NewRandomReader(ra, size)
}

// padTo4 rounds n up to the next multiple of four.
func padTo4(n int64) int64 {
	return n + int64(padLen(n))
}

// rawStream keeps the information about a stream collected during the
// backwards scan.
type rawStream struct {
	start      int64
	indexStart int64
	flags      byte
	recs       []record
}

// NewRandomReader creates a random access reader using the
// configuration c.
func (c RandomReaderConfig) NewRandomReader(ra io.ReaderAt,
	size int64) (r *RandomReader, err error) {

	if ra == nil {
		return nil, errors.New("xz: ReaderAt is nil")
	}
	if size < 0 {
		return nil, errors.New("xz: negative file size")
	}
	if err = c.Verify(); err != nil {
		return nil, err
	}

	r = &RandomReader{
		RandomReaderConfig: c,
		ra:                 ra,
		size:               size,
	}

	// Scan the streams backwards. This finds every footer and index
	// even when the block headers omit compressed and uncompressed
	// sizes.
	var streams []rawStream
	var indexMemory int64

	pos := size
	for pos > 0 {
		// Skip legal inter-stream padding, which consists of groups
		// of four zero bytes.
		if pos >= 4 {
			p := make([]byte, 4)
			if _, err = ra.ReadAt(p, pos-4); err != nil {
				return nil, err
			}
			if allZeros(p) {
				pos -= 4
				continue
			}
		}

		if pos < HeaderLen+footerLen {
			return nil, errors.New("xz: random access: truncated stream")
		}

		// Parse and verify the footer.
		fp := make([]byte, footerLen)
		if _, err = ra.ReadAt(fp, pos-footerLen); err != nil {
			return nil, err
		}
		var f footer
		if err = f.UnmarshalBinary(fp); err != nil {
			return nil, err
		}
		idxSize := f.indexSize
		if idxSize < minIndexSize || idxSize > maxIndexSize ||
			idxSize%4 != 0 {
			return nil, errors.New("xz: invalid index size in footer")
		}
		if idxSize > pos-footerLen-HeaderLen {
			return nil, errors.New("xz: index size exceeds stream")
		}
		if indexMemory+idxSize < 0 ||
			indexMemory+idxSize > c.MaxIndexMemory {
			return nil, errors.New("xz: index memory limit exceeded")
		}
		indexMemory += idxSize

		idxOff := pos - footerLen - idxSize
		if idxOff < HeaderLen {
			return nil, errors.New("xz: index starts before header")
		}
		recs, err := readIndexAt(ra, idxOff, idxSize)
		if err != nil {
			return nil, err
		}

		// Walk the records backwards to locate the first block.
		off := idxOff
		for i := len(recs) - 1; i >= 0; i-- {
			rec := recs[i]
			if rec.unpaddedSize < 1 {
				return nil, fmt.Errorf(
					"xz: invalid unpadded size in index record %d", i)
			}
			if rec.uncompressedSize < 0 {
				return nil, fmt.Errorf(
					"xz: invalid uncompressed size in index record %d", i)
			}
			if rec.uncompressedSize > c.MaxBlockMemory {
				return nil, errors.New(
					"xz: block memory limit exceeded")
			}
			padded := padTo4(rec.unpaddedSize)
			if padded > off-HeaderLen {
				return nil, fmt.Errorf(
					"xz: record %d doesn't fit into stream", i)
			}
			off -= padded
		}

		// off is the offset directly after the stream header.
		streamStart := off - HeaderLen
		if streamStart < 0 {
			return nil, errors.New("xz: stream starts before file")
		}

		hp := make([]byte, HeaderLen)
		if _, err = ra.ReadAt(hp, streamStart); err != nil {
			return nil, err
		}
		var h header
		if err = h.UnmarshalBinary(hp); err != nil {
			return nil, err
		}
		if h.flags != f.flags {
			return nil, errors.New(
				"xz: stream flags in header and footer differ")
		}

		streams = append(streams, rawStream{
			start:      streamStart,
			indexStart: idxOff,
			flags:      h.flags,
			recs:       recs,
		})
		pos = streamStart
	}

	if len(streams) == 0 {
		return nil, errors.New("xz: random access: empty file")
	}

	first := streams[len(streams)-1]
	r.flags = first.flags
	if r.newHash, err = newHashFunc(r.flags); err != nil {
		return nil, err
	}

	// Walk the streams in file order, validate every block header and
	// build the uncompressed offset map.
	var unpSize int64
	for i := len(streams) - 1; i >= 0; i-- {
		s := streams[i]
		if s.flags != r.flags {
			return nil, errors.New(
				"xz: concatenated streams use different check methods")
		}
		off := s.start + HeaderLen
		for _, rec := range s.recs {
			hdrLen, bh, err := readBlockHeaderAt(ra, off)
			if err != nil {
				return nil, err
			}

			checkLen := checkSize(r.flags)
			compressedSize := rec.unpaddedSize - int64(hdrLen) -
				int64(checkLen)
			if compressedSize < 1 {
				return nil, fmt.Errorf(
					"xz: block at offset %d has invalid compressed size",
					off)
			}

			if bh.compressedSize >= 0 &&
				bh.compressedSize != compressedSize {
				return nil, fmt.Errorf(
					"xz: block header compressed size %d contradicts index %d",
					bh.compressedSize, compressedSize)
			}
			if bh.uncompressedSize >= 0 &&
				bh.uncompressedSize != rec.uncompressedSize {
				return nil, fmt.Errorf(
					"xz: block header uncompressed size %d contradicts index %d",
					bh.uncompressedSize, rec.uncompressedSize)
			}

			lf, ok := bh.filters[0].(*lzmaFilter)
			if !ok {
				return nil, errors.New("xz: unsupported block filter")
			}
			if lf.dictCap < 1 || int64(lf.dictCap) > int64(c.MaxDictCap) {
				return nil, errors.New(
					"xz: block dictionary capacity exceeds limit")
			}

			r.blocks = append(r.blocks, blockInfo{
				compressedOff:    off,
				headerLen:        hdrLen,
				compressedSize:   compressedSize,
				unpaddedSize:     rec.unpaddedSize,
				uncompressedSize: rec.uncompressedSize,
				unpOff:           unpSize,
				checkLen:         checkLen,
				dictCap:          int(lf.dictCap),
				filters:          bh.filters,
			})

			if unpSize+rec.uncompressedSize < 0 {
				return nil, errors.New("xz: uncompressed size overflow")
			}
			unpSize += rec.uncompressedSize
			off += padTo4(rec.unpaddedSize)
		}
		if off != s.indexStart {
			return nil, errors.New(
				"xz: block layout doesn't match index offset")
		}
	}

	r.unpSize = unpSize
	return r, nil
}

// readIndexAt reads and verifies the complete index at off. The index
// has the given size including the index indicator, the padding and
// the CRC-32 checksum.
func readIndexAt(ra io.ReaderAt, off, idxSize int64) ([]record, error) {
	buf := make([]byte, idxSize)
	if _, err := ra.ReadAt(buf, off); err != nil {
		return nil, err
	}

	bodyLen := idxSize - 4

	// CRC-32 over the indicator, number of records, records and
	// padding.
	crc := crc32.ChecksumIEEE(buf[:bodyLen])
	if crc != uint32LE(buf[bodyLen:]) {
		return nil, errors.New("xz: wrong checksum for index")
	}

	if buf[0] != 0 {
		return nil, errors.New("xz: missing index indicator")
	}

	r := bytes.NewReader(buf[1:bodyLen])

	u, _, err := readUvarint(r)
	if err != nil {
		return nil, err
	}
	if u == 0 {
		return nil, errors.New("xz: index without records")
	}
	nrec := int(u)
	if u >= uint64(idxSize)/2 || uint64(nrec) != u {
		return nil, errors.New("xz: record number overflow")
	}

	recs := make([]record, nrec)
	for i := range recs {
		recs[i], _, err = readRecord(r)
		if err != nil {
			return nil, err
		}
		if recs[i].unpaddedSize < 0 || recs[i].uncompressedSize < 0 {
			return nil, errors.New("xz: negative size in index record")
		}
	}

	// r.Size() is the number of bytes after the record count; the
	// current offset therefore gives the consumed record bytes.
	recordBytes, errSeek := r.Seek(0, io.SeekCurrent)
	if errSeek != nil {
		return nil, errSeek
	}
	idxLen := 1 + recordBytes // indicator plus consumed data
	pad := int64(padLen(idxLen))
	if int64(r.Len()) != pad {
		return nil, errors.New("xz: invalid index padding length")
	}
	if pad > 0 && !allZeros(buf[bodyLen-pad:bodyLen]) {
		return nil, errors.New("xz: non-zero byte in index padding")
	}

	return recs, nil
}

// readBlockHeaderAt reads and validates the block header at off. It
// returns the header length and the parsed block header.
func readBlockHeaderAt(ra io.ReaderAt, off int64) (int, *blockHeader, error) {
	var s [1]byte
	if _, err := ra.ReadAt(s[:], off); err != nil {
		return 0, nil, err
	}
	if s[0] == 0 {
		return 0, nil, errors.New("xz: index indicator where block expected")
	}
	headerLen := (int(s[0]) + 1) * 4
	buf := make([]byte, headerLen)
	if _, err := ra.ReadAt(buf, off); err != nil {
		return 0, nil, err
	}
	h := new(blockHeader)
	if err := h.UnmarshalBinary(buf); err != nil {
		return 0, nil, err
	}
	return headerLen, h, nil
}

// checkSize returns the length of the block check field for the given
// stream flags.
func checkSize(flags byte) int {
	switch flags {
	case None:
		return newNoneHash().Size()
	case CRC32:
		return 4
	case CRC64:
		return 8
	case SHA256:
		return 32
	default:
		panic("xz: invalid check flags")
	}
}

// Size returns the total uncompressed size of all concatenated
// streams.
func (r *RandomReader) Size() int64 {
	return r.unpSize
}

// ReadAt reads uncompressed data starting at the uncompressed offset
// off. The method satisfies [io.ReaderAt]: it always reads the full
// requested length unless the end of the data is reached, and parallel
// calls are supported.
//
// Only blocks overlapping [off, off+len(p)) are decompressed. Each
// read block is fully decompressed and its block check is verified
// before the requested bytes are returned; blocks outside the interval
// are not touched and their content is not verified.
func (r *RandomReader) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, errors.New("xz: negative offset")
	}
	if off >= r.unpSize {
		if len(p) == 0 {
			return 0, nil
		}
		return 0, io.EOF
	}

	end := off + int64(len(p))
	if end < off || end > r.unpSize {
		end = r.unpSize
	}

	// Locate the first block whose uncompressed interval extends past
	// off.
	idx := sort.Search(len(r.blocks), func(i int) bool {
		b := r.blocks[i]
		return b.unpOff+b.uncompressedSize > off
	})

	for idx < len(r.blocks) && int64(n) < end-off {
		b := r.blocks[idx]
		bStart, bEnd := b.unpOff, b.unpOff+b.uncompressedSize
		data, err := r.readBlock(b)
		if err != nil {
			return n, err
		}

		start := off + int64(n)
		if start < bStart {
			start = bStart
		}
		stop := end
		if stop > bEnd {
			stop = bEnd
		}
		if stop > start {
			n += copy(p[n:], data[start-bStart:stop-bStart])
		}
		idx++
	}

	if int64(n) < int64(len(p)) {
		return n, io.EOF
	}
	return n, nil
}

// readBlock decompresses the complete block, verifies that exactly the
// announced compressed size has been consumed and verifies the block
// padding and check value.
func (r *RandomReader) readBlock(b blockInfo) (data []byte, err error) {
	if b.uncompressedSize > r.MaxBlockMemory {
		return nil, errors.New("xz: block memory limit exceeded")
	}
	if b.compressedSize > r.MaxBlockMemory {
		return nil, errors.New("xz: block memory limit exceeded")
	}

	compressedOff := b.compressedOff + int64(b.headerLen)

	// Load the compressed block in a single random read. The buffer
	// is bounded by MaxBlockMemory.
	compressed := make([]byte, b.compressedSize)
	if _, err = r.ra.ReadAt(compressed, compressedOff); err != nil {
		return nil, err
	}
	sr := bytes.NewReader(compressed)

	cfg := ReaderConfig{DictCap: r.DictCap}
	fr, err := cfg.newFilterReader(sr, b.filters)
	if err != nil {
		return nil, err
	}

	h := r.newHash()
	if h.Size() != 0 {
		fr = io.TeeReader(fr, h)
	}

	out := make([]byte, b.uncompressedSize)
	if _, err = io.ReadFull(fr, out); err != nil {
		return nil, err
	}

	// Consume the mandatory LZMA2 end-of-stream marker and reject any
	// additional decompressed data.
	var z [1]byte
	k, err := fr.Read(z[:])
	if k != 0 {
		return nil, errors.New("xz: block produced too much data")
	}
	if err != io.EOF {
		return nil, errors.New("xz: block not terminated by end marker")
	}

	if sr.Len() != 0 {
		return nil, fmt.Errorf(
			"xz: block left %d unconsumed compressed bytes", sr.Len())
	}

	// Read padding and check value.
	pad := padLen(b.compressedSize)
	tail := make([]byte, pad+b.checkLen)
	if _, err = r.ra.ReadAt(tail, compressedOff+b.compressedSize); err != nil {
		return nil, err
	}
	if !allZeros(tail[:pad]) {
		return nil, errors.New("xz: non-zero block padding")
	}
	if b.checkLen != 0 {
		check := tail[pad:]
		sum := h.Sum(nil)
		if !bytes.Equal(check, sum) {
			return nil, errors.New("xz: checksum error for block")
		}
	}

	return out, nil
}
