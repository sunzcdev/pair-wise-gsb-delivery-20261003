// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
)

// checkSize returns the length of the block integrity check for the
// given stream flags.
func checkSize(flags byte) int {
	switch flags {
	case CRC32:
		return 4
	case CRC64:
		return 8
	case SHA256:
		return 32
	default:
		return 0
	}
}

// Size returns the total number of uncompressed bytes provided by all
// streams.
func (sr *SeekReader) Size() int64 {
	var n int64
	for i := range sr.block {
		n += sr.block[i].uncompressedSize
	}
	return n
}

// ReadAt reads uncompressed data beginning at the logical offset off.
// It implements io.ReaderAt: if fewer than len(p) bytes are available
// io.ErrUnexpectedEOF is returned; if off equals the uncompressed size
// zero bytes and io.EOF are returned.
//
// Only the blocks overlapping the requested interval are decompressed.
// When a block is decoded its padding, compressed/uncompressed sizes and
// content checksum are verified. Blocks outside the interval are not
// read and therefore are not content-verified.
func (sr *SeekReader) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, errors.New("xz: negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}

	size := sr.Size()
	if off >= size {
		if off == size {
			return 0, io.EOF
		}
		return 0, errors.New("xz: offset beyond end of stream")
	}
	if off+int64(len(p)) < off {
		return 0, errors.New("xz: length overflow")
	}

	idx := sort.Search(len(sr.block), func(i int) bool {
		b := sr.block[i]
		return off < b.unpaddedOff+b.uncompressedSize
	})
	if idx >= len(sr.block) {
		return 0, io.EOF
	}

	for n < len(p) {
		if idx >= len(sr.block) {
			return n, io.ErrUnexpectedEOF
		}
		b := sr.block[idx]
		blockEnd := b.unpaddedOff + b.uncompressedSize
		if off < b.unpaddedOff || off > blockEnd {
			// No block covers this offset (gap in the mapping).
			return n, errors.New("xz: offset not covered by index")
		}

		data, derr := sr.decodeBlock(b)
		if derr != nil {
			return n, derr
		}

		start := int(off - b.unpaddedOff)
		k := copy(p[n:], data[start:])
		n += k
		off += int64(k)
		idx++
	}

	if off > size {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

// decodeBlock decodes a complete block and verifies its content. The
// block header CRC and size relations were already checked while
// parsing the index; here the actual payload, padding and integrity
// check are verified.
func (sr *SeekReader) decodeBlock(b seekBlock) (data []byte, err error) {
	hdata := make([]byte, b.headerLen)
	if err = readAt(sr.r, hdata, b.off); err != nil {
		return nil, err
	}
	var bh blockHeader
	if err = bh.UnmarshalBinary(hdata); err != nil {
		return nil, err
	}

	newHash, err := newHashFunc(b.flags)
	if err != nil {
		return nil, err
	}

	checkLen := checkSize(b.flags)
	pad := padLen(b.compressedSize)
	// The section spans the compressed payload, the block padding and
	// the integrity check. Including the trailing bytes lets the LZMA2
	// decoder read its end-of-stream marker even when the payload ends
	// exactly on a boundary; we still verify below that the filter chain
	// consumed only the declared payload.
	sectionLen, err := addNonNeg(b.compressedSize, int64(pad+checkLen))
	if err != nil {
		return nil, err
	}
	sec := io.NewSectionReader(sr.r, b.compressedStart, sectionLen)
	lxz := &countingReader{r: sec}

	cfg := sr.ReaderConfig
	cfg.DictCap = int(b.dictCap)
	fr, err := cfg.newFilterReader(lxz, bh.filters)
	if err != nil {
		return nil, err
	}

	hash := newHash()
	// Decode until the LZMA2 stream signals end of stream. Reading in
	// a loop (rather than a single exact-length ReadFull) is required so
	// the decoder performs the final read that consumes the end-of-stream
	// marker; the streaming reader reaches it because io.Copy keeps
	// reading after the output buffer has been filled.
	var out bytes.Buffer
	var writer io.Writer = &out
	if hash.Size() != 0 {
		writer = io.MultiWriter(&out, hash)
	}
	tmp := make([]byte, 32*1024)
	for {
		nr, rerr := fr.Read(tmp)
		if nr > 0 {
			if _, werr := writer.Write(tmp[:nr]); werr != nil {
				return nil, werr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	data = out.Bytes()
	if int64(len(data)) != b.uncompressedSize {
		return nil, fmt.Errorf(
			"xz: block decoded %d bytes; index says %d",
			len(data), b.uncompressedSize)
	}

	// The filter chain must consume exactly the declared payload,
	// including the LZMA2 end-of-stream marker.
	if lxz.n != b.compressedSize {
		return nil, fmt.Errorf(
			"xz: block consumed %d compressed bytes; index says %d",
			lxz.n, b.compressedSize)
	}

	// Read the block padding and integrity check from the section.
	trailer := make([]byte, pad+checkLen)
	if _, err = io.ReadFull(sec, trailer); err != nil {
		return nil, err
	}
	if !allZeros(trailer[:pad]) {
		return nil, errors.New("xz: non-zero block padding")
	}
	if checkLen != 0 {
		checkSum := trailer[pad:]
		if !bytes.Equal(checkSum, hash.Sum(nil)) {
			return nil, errors.New("xz: checksum error for block")
		}
	}

	return data, nil
}
