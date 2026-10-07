// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// Default limits for the random access support provided by NewReaderAt.
// They protect callers processing untrusted files from unbounded
// memory allocation.
const (
	// defaultIndexMemCap bounds the memory used for parsed indexes.
	defaultIndexMemCap = 64 * 1024 * 1024
	// defaultBlockDictCap bounds the dictionary capacity of a single
	// block that may be decoded.
	defaultBlockDictCap = 64 * 1024 * 1024
	// approxRecordMem is a conservative estimate for the memory a
	// single block contributes to the parsed index.
	approxRecordMem = 128
)

// seekBlock describes a single block located through an xz index.
type seekBlock struct {
	// flags is the stream checksum method.
	flags byte
	// off is the file offset of the block header.
	off int64
	// headerLen is the length of the block header in bytes.
	headerLen int
	// compressedStart is the file offset of the block payload.
	compressedStart int64
	// compressedSize is the payload length without padding and check.
	compressedSize int64
	// unpaddedSize is headerLen + compressedSize + check size.
	unpaddedSize int64
	// unpaddedOff is the logical uncompressed offset of the block.
	unpaddedOff int64
	// uncompressedSize is the decoded length of the block.
	uncompressedSize int64
	// dictCap is the LZMA2 dictionary capacity requested by the block.
	dictCap int64
}

// seekStream describes a located xz stream.
type seekStream struct {
	flags     byte
	header    int64
	indexOff  int64
	footerOff int64
	records   []record
}

// SeekReader provides random access to the uncompressed data of one or
// several concatenated xz streams. It is created by NewReaderAt and
// implements io.ReaderAt.
//
// The index of every stream is parsed and validated up front: header
// and footer flags, index CRC and the size relations between the index
// and the block headers. The content checksum of a block is verified
// only when that block is actually decoded; blocks that are never read
// are not content-verified.
type SeekReader struct {
	ReaderConfig

	r     io.ReaderAt
	block []seekBlock
}

// NewReaderAt creates a random access xz reader using the default
// parameters. See ReaderConfig.NewReaderAt for details.
func NewReaderAt(r io.ReaderAt, size int64) (*SeekReader, error) {
	return ReaderConfig{}.NewReaderAt(r, size)
}

// NewReaderAt creates a random access reader for the xz data accessible
// through r. The parameter size provides the total length of the
// compressed data in bytes.
//
// The method locates the streams backwards using their footer and
// index, supports multiple concatenated streams and the padding
// (sequences of zero bytes aligned to four) permitted between streams.
func (c ReaderConfig) NewReaderAt(r io.ReaderAt, size int64) (sr *SeekReader, err error) {
	if r == nil {
		return nil, errors.New("xz: reader is nil")
	}
	if size < 0 {
		return nil, errors.New("xz: negative xz size")
	}
	if err = c.Verify(); err != nil {
		return nil, err
	}
	indexCap := c.IndexMemCap
	if indexCap == 0 {
		indexCap = defaultIndexMemCap
	}
	dictCapLimit := c.BlockDictCap
	if dictCapLimit == 0 {
		dictCapLimit = defaultBlockDictCap
	}

	sr = &SeekReader{
		ReaderConfig: c,
		r:            r,
	}

	streams, err := locateStreams(r, size, indexCap)
	if err != nil {
		return nil, err
	}

	var blocks []seekBlock
	var maxDictCap int64
	for _, st := range streams {
		maxRecords := 0
		if indexCap > 0 {
			maxRecords = indexCap/approxRecordMem + 1
		}
		sb, stDict, err := parseStream(r, st, maxRecords)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, sb...)
		if stDict > maxDictCap {
			maxDictCap = stDict
		}
	}

	if dictCapLimit > 0 && maxDictCap > int64(dictCapLimit) {
		return nil, fmt.Errorf(
			"xz: block dictionary capacity %d exceeds limit %d",
			maxDictCap, dictCapLimit)
	}

	// Compute logical uncompressed offsets. Sizes have already been
	// checked for overflow when locating the blocks.
	var off int64
	for i := range blocks {
		blocks[i].unpaddedOff = off
		off += blocks[i].uncompressedSize
	}

	sr.block = blocks
	return sr, nil
}

// readAt reads exactly len(p) bytes at off from r. A short read at the
// end of the underlying data is converted to ErrUnexpectedEOF.
func readAt(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if err == io.EOF && n == len(p) {
		err = nil
	}
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// locateStreams walks the data backwards and returns the streams in
// forward order, validating footer CRC and stream flags.
func locateStreams(r io.ReaderAt, size int64, indexCap int) (streams []seekStream, err error) {
	pos := size
	for pos >= HeaderLen {
		var magic [2]byte
		if err = readAt(r, magic[:], pos-2); err != nil {
			return nil, err
		}
		if !bytes.Equal(magic[:], footerMagic) {
			// Padding between streams is a multiple of four zero
			// bytes.
			if pos-4 < 0 {
				return nil, errors.New("xz: invalid trailer")
			}
			var pad [4]byte
			if err = readAt(r, pad[:], pos-4); err != nil {
				return nil, err
			}
			if !allZeros(pad[:]) {
				return nil, errors.New("xz: invalid trailer")
			}
			pos -= 4
			continue
		}

		if pos < footerLen {
			return nil, errors.New("xz: footer out of range")
		}
		footerOff := pos - footerLen
		fdata := make([]byte, footerLen)
		if err = readAt(r, fdata, footerOff); err != nil {
			return nil, err
		}
		var f footer
		if err = f.UnmarshalBinary(fdata); err != nil {
			return nil, err
		}

		indexEnd := footerOff
		indexStart := indexEnd - f.indexSize
		if indexStart < HeaderLen || f.indexSize <= 0 {
			return nil, errors.New("xz: index out of range")
		}
		if indexCap > 0 && f.indexSize > int64(indexCap) {
			return nil, errors.New("xz: index memory limit exceeded")
		}

		// Parse the index now: summing the padded block sizes yields
		// the start of the stream header.
		indexData := make([]byte, f.indexSize)
		if err = readAt(r, indexData, indexStart); err != nil {
			return nil, err
		}
		if indexData[0] != 0 {
			return nil, errors.New("xz: missing index indicator")
		}
		maxRecords := 0
		if indexCap > 0 {
			maxRecords = int(indexCap/approxRecordMem + 1)
		}
		records, rn, err := readIndexBody(bytes.NewReader(indexData[1:]), -1, maxRecords)
		if err != nil {
			return nil, err
		}
		if rn+1 != f.indexSize {
			return nil, fmt.Errorf(
				"xz: index size %d; footer provides %d",
				rn+1, f.indexSize)
		}
		var blocksLen int64
		for _, rec := range records {
			padded, perr := pad4(rec.unpaddedSize)
			if perr != nil {
				return nil, perr
			}
			blocksLen, err = addNonNeg(blocksLen, padded)
			if err != nil {
				return nil, err
			}
		}
		blocksAndHeader, err := addNonNeg(blocksLen, HeaderLen)
		if err != nil {
			return nil, err
		}
		if blocksAndHeader > indexStart {
			return nil, errors.New("xz: stream header out of range")
		}
		headerOff := indexStart - blocksAndHeader

		hdata := make([]byte, HeaderLen)
		if err = readAt(r, hdata, headerOff); err != nil {
			return nil, err
		}
		var h header
		if err = h.UnmarshalBinary(hdata); err != nil {
			return nil, err
		}
		if h.flags != f.flags {
			return nil, errors.New(
				"xz: stream header and footer flags differ")
		}

		streams = append(streams, seekStream{
			flags:     f.flags,
			header:    headerOff,
			indexOff:  indexStart,
			footerOff: footerOff,
			records:   records,
		})
		pos = headerOff
	}

	// The sequential reader tolerates stream padding before the first
	// stream as well; accept it if it consists of aligned zero bytes.
	if pos > 0 {
		if pos%4 != 0 {
			return nil, errors.New("xz: unaligned stream padding")
		}
		lead := make([]byte, pos)
		if err = readAt(r, lead, 0); err != nil {
			return nil, err
		}
		if !allZeros(lead) {
			return nil, errors.New("xz: invalid leading padding")
		}
	}
	if len(streams) == 0 {
		return nil, errors.New("xz: no xz stream found")
	}

	// Streams were collected from the last to the first; reverse them.
	for i, j := 0, len(streams)-1; i < j; i, j = i+1, j-1 {
		streams[i], streams[j] = streams[j], streams[i]
	}
	return streams, nil
}

// addNonNeg returns a+b and an error if the addition overflows int64.
func addNonNeg(a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, errors.New("xz: negative length")
	}
	s := a + b
	if s < a {
		return 0, errors.New("xz: length overflow")
	}
	return s, nil
}

// pad4 rounds n up to the next multiple of four.
func pad4(n int64) (int64, error) {
	if n < 0 {
		return 0, errors.New("xz: negative length")
	}
	return addNonNeg(n, int64(padLen(n)))
}

// parseStream validates the index of a single stream and locates all of
// its blocks. It returns the blocks in forward order and the maximum
// dictionary capacity requested by any block.
func parseStream(r io.ReaderAt, st seekStream, maxRecords int) (blocks []seekBlock, maxDictCap int64, err error) {
	indexEnd := st.footerOff
	if indexEnd < st.indexOff {
		return nil, 0, errors.New("xz: inverted index bounds")
	}
	records := st.records
	if maxRecords > 0 && len(records) > maxRecords {
		return nil, 0, errors.New("xz: index exceeds record count limit")
	}

	blockOff := st.header + HeaderLen
	for i, rec := range records {
		if rec.unpaddedSize <= 0 || rec.uncompressedSize < 0 {
			return nil, 0, errors.New("xz: invalid index record size")
		}
		var sizeByte [1]byte
		if err = readAt(r, sizeByte[:], blockOff); err != nil {
			return nil, 0, err
		}
		if sizeByte[0] == 0 {
			return nil, 0, errIndexIndicator
		}
		headerLen := (int(sizeByte[0]) + 1) * 4
		hbuf := make([]byte, headerLen)
		if err = readAt(r, hbuf, blockOff); err != nil {
			return nil, 0, err
		}
		var bh blockHeader
		if err = bh.UnmarshalBinary(hbuf); err != nil {
			return nil, 0, err
		}
		if len(bh.filters) != 1 {
			return nil, 0, errors.New("xz: unsupported filter count")
		}
		lf, ok := bh.filters[0].(*lzmaFilter)
		if !ok {
			return nil, 0, errors.New("xz: unsupported block filter")
		}
		if lf.dictCap > maxDictCap {
			maxDictCap = lf.dictCap
		}

		cs := checkSize(st.flags)
		payloadEnd, err := addNonNeg(blockOff, rec.unpaddedSize)
		if err != nil {
			return nil, 0, err
		}
		if payloadEnd > indexEnd {
			return nil, 0, errors.New("xz: block extends past its index")
		}
		compressedSize := rec.unpaddedSize - int64(headerLen) -
			int64(cs)
		if compressedSize < 0 {
			return nil, 0, errors.New("xz: block size underflow")
		}
		compressedStart, err := addNonNeg(blockOff, int64(headerLen))
		if err != nil {
			return nil, 0, err
		}

		// A size present in the block header must match the index.
		if bh.compressedSize >= 0 &&
			bh.compressedSize != compressedSize {
			return nil, 0, fmt.Errorf(
				"xz: block %d compressed size mismatch: "+
					"header %d index %d",
				i, bh.compressedSize, compressedSize)
		}
		if bh.uncompressedSize >= 0 &&
			bh.uncompressedSize != rec.uncompressedSize {
			return nil, 0, fmt.Errorf(
				"xz: block %d uncompressed size mismatch: "+
					"header %d index %d",
				i, bh.uncompressedSize,
				rec.uncompressedSize)
		}

		blocks = append(blocks, seekBlock{
			flags:            st.flags,
			off:              blockOff,
			headerLen:        headerLen,
			compressedStart:  compressedStart,
			compressedSize:   compressedSize,
			unpaddedSize:     rec.unpaddedSize,
			uncompressedSize: rec.uncompressedSize,
			dictCap:          lf.dictCap,
		})

		padded, err := pad4(rec.unpaddedSize)
		if err != nil {
			return nil, 0, err
		}
		blockOff, err = addNonNeg(blockOff, padded)
		if err != nil {
			return nil, 0, err
		}
	}

	if blockOff != st.indexOff {
		return nil, 0, fmt.Errorf(
			"xz: block total %d does not match index offset %d",
			blockOff, st.indexOff)
	}
	return blocks, maxDictCap, nil
}
