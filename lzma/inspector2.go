// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lzma

import (
	"errors"
	"fmt"
	"io"
)

// ChunkRecord describes the structure of a single chunk of an LZMA2
// stream. The record is produced by Inspector2 without decompressing
// the chunk payload. Therefore Verified is always false: a chunk
// listed by the inspector has passed structural checks only, its
// content has not been validated.
type ChunkRecord struct {
	// Offset is the position of the control byte in the stream.
	Offset int64
	// Type is the category of the chunk.
	Type chunkType
	// HeaderLen is the length of the chunk header in bytes.
	HeaderLen int
	// Uncompressed is the number of bytes the chunk payload
	// represents after decoding. It is zero for the EOS chunk.
	Uncompressed int64
	// Compressed is the number of payload bytes the chunk occupies
	// in the stream for LZMA-compressed chunks. It is zero for
	// uncompressed chunks and the EOS chunk; for uncompressed
	// chunks the payload length equals Uncompressed.
	Compressed int64
	// DictReset indicates that the chunk resets the dictionary.
	DictReset bool
	// StateReset indicates that the chunk resets the LZMA state.
	StateReset bool
	// PropsReset indicates that the chunk provides new properties.
	PropsReset bool
	// Props contains the properties of the chunk. It is valid only
	// if PropsReset is set.
	Props Properties
	// Verified reports whether the chunk content has been verified.
	// The inspector never decompresses payload data, so the field
	// is always false.
	Verified bool
}

// Size returns the total number of bytes the chunk occupies in the
// stream, including its header.
func (rec ChunkRecord) Size() int64 {
	n := int64(rec.HeaderLen)
	if uncompressed(rec.Type) {
		return n + rec.Uncompressed
	}
	if rec.Type == cEOS {
		return n
	}
	return n + rec.Compressed
}

// String returns a string representation of the chunk record.
func (rec ChunkRecord) String() string {
	return fmt.Sprintf("offset %d %s header %d uncompressed %d "+
		"compressed %d dictReset %t stateReset %t propsReset %t "+
		"verified %t",
		rec.Offset, rec.Type, rec.HeaderLen, rec.Uncompressed,
		rec.Compressed, rec.DictReset, rec.StateReset,
		rec.PropsReset, rec.Verified)
}

// errTrailingData indicates bytes after the EOS chunk of the LZMA2
// stream.
var errTrailingData = errors.New("lzma: data after end of LZMA2 stream")

// errPropsSum indicates properties with lc+lp > 4, which is invalid
// for LZMA2.
var errPropsSum = errors.New("lzma: sum of lc and lp exceeds 4")

// Inspector2 checks the structure of an LZMA2 chunk sequence without
// decompressing the chunk payloads. It reads the control bytes, the
// compressed and uncompressed lengths and the property resets of all
// chunk headers and tracks whether the dictionary, the properties and
// the LZMA state have been initialized. Uncompressed chunks permitted
// by the format are accepted, while illegal reset sequences are
// reported as errors.
//
// The inspector requires the stream to be terminated by an EOS chunk;
// a stream ending before it is reported as io.ErrUnexpectedEOF. Any
// data after the EOS chunk is rejected with an error.
//
// The memory used by the inspector is limited by a single chunk header
// and a fixed-size skip buffer; it doesn't grow with the size of the
// stream.
//
// Note that a clean inspection doesn't verify the content of the
// chunks; the Verified field of all chunk records remains false.
type Inspector2 struct {
	r io.Reader

	off    int64
	cstate chunkState

	dictInit  bool
	propsInit bool
	stateInit bool
	eos       bool

	hdr [6]byte
	buf [8 * 1024]byte

	err error
}

// NewInspector2 creates an inspector for an LZMA2 chunk sequence.
func NewInspector2(lzma2 io.Reader) *Inspector2 {
	return &Inspector2{r: lzma2, cstate: start}
}

// DictInit reports whether the dictionary has been initialized and has
// not been invalidated by a later chunk.
func (s *Inspector2) DictInit() bool { return s.dictInit }

// PropsInit reports whether valid properties are currently set.
func (s *Inspector2) PropsInit() bool { return s.propsInit }

// StateInit reports whether the LZMA state is currently initialized.
func (s *Inspector2) StateInit() bool { return s.stateInit }

// EOS reports whether the EOS chunk of the stream has been read.
func (s *Inspector2) EOS() bool { return s.eos }

// Offset returns the stream offset of the next chunk header.
func (s *Inspector2) Offset() int64 { return s.off }

// fail stores the error and returns it.
func (s *Inspector2) fail(err error) error {
	s.err = err
	return err
}

// Next reads the next chunk header, skips the chunk payload and
// returns the record describing the chunk. After the record for the
// EOS chunk has been returned, the next call returns io.EOF if no
// further data follows and an error otherwise. Once an error has been
// returned all subsequent calls return the same error.
func (s *Inspector2) Next() (rec ChunkRecord, err error) {
	if s.err != nil {
		return rec, s.err
	}
	if s.eos {
		var b [1]byte
		if _, err = io.ReadFull(s.r, b[:]); err == nil {
			err = errTrailingData
		} else {
			err = io.EOF
		}
		return rec, s.fail(err)
	}

	if _, err = io.ReadFull(s.r, s.hdr[:1]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return rec, s.fail(err)
	}
	ctype, err := headerChunkType(s.hdr[0])
	if err != nil {
		return rec, s.fail(err)
	}
	hlen := headerLen(ctype)
	if _, err = io.ReadFull(s.r, s.hdr[1:hlen]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return rec, s.fail(err)
	}
	var h chunkHeader
	if err = h.UnmarshalBinary(s.hdr[:hlen]); err != nil {
		return rec, s.fail(err)
	}
	if err = s.cstate.next(ctype); err != nil {
		return rec, s.fail(err)
	}
	if (ctype == cLRN || ctype == cLRND) &&
		h.props.LC+h.props.LP > 4 {
		return rec, s.fail(errPropsSum)
	}

	rec = ChunkRecord{
		Offset:    s.off,
		Type:      ctype,
		HeaderLen: hlen,
	}
	switch ctype {
	case cUD:
		rec.DictReset = true
	case cLR:
		rec.StateReset = true
	case cLRN:
		rec.StateReset = true
		rec.PropsReset = true
		rec.Props = h.props
	case cLRND:
		rec.DictReset = true
		rec.StateReset = true
		rec.PropsReset = true
		rec.Props = h.props
	}

	// A dictionary reset requires new properties and a state reset
	// before the next compressed chunk.
	switch ctype {
	case cUD:
		s.dictInit = true
		s.propsInit = false
		s.stateInit = false
	case cLRND:
		s.dictInit = true
		s.propsInit = true
		s.stateInit = true
	case cLRN:
		s.propsInit = true
		s.stateInit = true
	case cLR:
		s.stateInit = true
	}

	var payload int64
	if ctype != cEOS {
		rec.Uncompressed = int64(h.uncompressed) + 1
		if uncompressed(ctype) {
			payload = rec.Uncompressed
		} else {
			rec.Compressed = int64(h.compressed) + 1
			payload = rec.Compressed
		}
	}
	if err = s.skip(payload); err != nil {
		return rec, s.fail(err)
	}
	s.off += int64(hlen) + payload
	if ctype == cEOS {
		s.eos = true
	}
	return rec, nil
}

// skip reads and discards n bytes from the stream.
func (s *Inspector2) skip(n int64) error {
	for n > 0 {
		m := int64(len(s.buf))
		if n < m {
			m = n
		}
		k, err := s.r.Read(s.buf[:m])
		n -= int64(k)
		if err == io.EOF {
			if n > 0 {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if err != nil {
			return err
		}
		if k == 0 {
			return errors.New("lzma: Inspector2 doesn't get data")
		}
	}
	return nil
}

// Inspect2 checks the structure of the complete LZMA2 stream read from
// lzma2 and calls fn for every chunk record. A nil fn is permitted and
// reduces the call to a structural validation of the stream. The
// function doesn't decompress chunk payloads, so the Verified field of
// all records passed to fn is false.
func Inspect2(lzma2 io.Reader, fn func(rec ChunkRecord) error) error {
	s := NewInspector2(lzma2)
	for {
		rec, err := s.Next()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if fn == nil {
			continue
		}
		if err = fn(rec); err != nil {
			return err
		}
	}
}
