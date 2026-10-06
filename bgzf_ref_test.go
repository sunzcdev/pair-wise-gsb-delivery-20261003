// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pgzip

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// refBGZFReader is an independent, minimal BGZF reader implemented only on
// top of the standard library. It validates the BC extra field, BSIZE, CRC32
// and ISIZE of every member and supports seeking to BGZF virtual offsets.
type refBGZFReader struct {
	r io.ReaderAt
}

func newRefBGZFReader(r io.ReaderAt) *refBGZFReader {
	return &refBGZFReader{r: r}
}

type refBlock struct {
	data []byte
	cOff int64
	size int
}

// readBlock parses one BGZF member at off.
func (rr *refBGZFReader) readBlock(off int64) (refBlock, error) {
	var hdr [18]byte
	n, err := rr.r.ReadAt(hdr[:], off)
	if err == io.EOF && n == 0 {
		return refBlock{}, io.EOF
	}
	if err != nil && err != io.EOF {
		return refBlock{}, err
	}
	if n < 12 {
		return refBlock{}, errors.New("ref: truncated gzip header")
	}
	if hdr[0] != 0x1f || hdr[1] != 0x8b {
		return refBlock{}, errors.New("ref: bad gzip magic")
	}
	if hdr[2] != 8 {
		return refBlock{}, errors.New("ref: not a deflate member")
	}
	flg := hdr[3]
	if flg&0x04 == 0 {
		return refBlock{}, errors.New("ref: missing FEXTRA flag")
	}

	xlen := int(binary.LittleEndian.Uint16(hdr[10:12]))
	if xlen < 6 || 12+xlen > n {
		return refBlock{}, fmt.Errorf("ref: bad XLEN %d", xlen)
	}

	// Parse extra subfields looking for BC.
	extra := hdr[12 : 12+xlen]
	bsize := 0
	foundBC := false
	for i := 0; i+4 <= len(extra); {
		si1, si2 := extra[i], extra[i+1]
		slen := int(binary.LittleEndian.Uint16(extra[i+2 : i+4]))
		i += 4
		if slen > len(extra)-i {
			return refBlock{}, errors.New("ref: corrupt extra field")
		}
		if si1 == 'B' && si2 == 'C' {
			if slen != 2 {
				return refBlock{}, errors.New("ref: bad BC subfield length")
			}
			bsize = int(binary.LittleEndian.Uint16(extra[i : i+2]))
			foundBC = true
		}
		i += slen
	}
	if !foundBC {
		return refBlock{}, errors.New("ref: missing BC extra field")
	}

	total := bsize + 1
	if total < 18+8 {
		return refBlock{}, errors.New("ref: BSIZE too small")
	}
	block := make([]byte, total)
	if _, err := rr.r.ReadAt(block, off); err != nil {
		return refBlock{}, err
	}

	deflateEnd := total - 8
	deflateStart := 12 + xlen
	fr := flate.NewReader(bytes.NewReader(block[deflateStart:deflateEnd]))
	data, derr := io.ReadAll(fr)
	if derr != nil {
		return refBlock{}, fmt.Errorf("ref: inflate: %w", derr)
	}
	if err := fr.Close(); err != nil {
		return refBlock{}, err
	}

	crcWant := binary.LittleEndian.Uint32(block[deflateEnd : deflateEnd+4])
	isizeWant := binary.LittleEndian.Uint32(block[deflateEnd+4 : total])
	if crc32.ChecksumIEEE(data) != crcWant {
		return refBlock{}, errors.New("ref: CRC32 mismatch")
	}
	if uint32(len(data)) != isizeWant {
		return refBlock{}, errors.New("ref: ISIZE mismatch")
	}

	return refBlock{data: data, cOff: off, size: total}, nil
}

// readAtVirt decompresses the block selected by virtual offset vo and
// returns its data starting at the encoded within-block offset.
func (rr *refBGZFReader) readAtVirt(vo int64) ([]byte, error) {
	cOff := vo >> 16
	uOff := int(vo & 0xffff)
	blk, err := rr.readBlock(cOff)
	if err != nil {
		return nil, err
	}
	if uOff > len(blk.data) {
		return nil, errors.New("ref: virtual offset beyond block")
	}
	return blk.data[uOff:], nil
}

// readAll decompresses every member in order and returns the concatenated
// payload, stopping at the empty EOF member.
func (rr *refBGZFReader) readAll() ([]byte, error) {
	var out bytes.Buffer
	var off int64
	for {
		blk, err := rr.readBlock(off)
		if err != nil {
			return nil, err
		}
		if len(blk.data) == 0 {
			// Canonical EOF member.
			if blk.size != len(bgzfEOF) {
				return nil, errors.New("ref: unexpected empty member")
			}
			return out.Bytes(), nil
		}
		out.Write(blk.data)
		off += int64(blk.size)
	}
}
