// Copyright 2014-2025 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz

import (
	"errors"
	"fmt"
	"io"
)

// Delta filter constants.
const (
	deltaFilterID  = 0x03
	deltaFilterLen = 3
	deltaMinDist   = 1
	deltaMaxDist   = 256
)

// deltaFilter represents the Delta filter stored in an xz block
// header.
type deltaFilter struct {
	dist int
}

// String returns a representation of the Delta filter.
func (f deltaFilter) String() string {
	return fmt.Sprintf("Delta distance %d", f.dist)
}

// id returns the ID of the Delta filter.
func (f deltaFilter) id() uint64 { return deltaFilterID }

// MarshalBinary converts the deltaFilter into its encoded
// representation.
func (f deltaFilter) MarshalBinary() (data []byte, err error) {
	if f.dist < deltaMinDist || f.dist > deltaMaxDist {
		return nil, errors.New("xz: Delta distance out of range")
	}
	return []byte{deltaFilterID, 1, byte(f.dist - 1)}, nil
}

// UnmarshalBinary unmarshals the encoded representation of the Delta
// filter.
func (f *deltaFilter) UnmarshalBinary(data []byte) error {
	if len(data) != deltaFilterLen {
		return errors.New("xz: data for Delta filter has wrong length")
	}
	if data[0] != deltaFilterID {
		return errors.New("xz: wrong Delta filter id")
	}
	if data[1] != 1 {
		return errors.New("xz: wrong Delta filter size")
	}
	f.dist = int(data[2]) + 1
	return nil
}

// last reports whether the filter must be the last filter in the
// chain. The Delta filter cannot be last.
func (f deltaFilter) last() bool { return false }

// reader creates a new reader for the decoding Delta filter.
func (f deltaFilter) reader(r io.Reader, c *ReaderConfig) (io.Reader, error) {
	if f.dist < deltaMinDist || f.dist > deltaMaxDist {
		return nil, errors.New("xz: Delta distance out of range")
	}
	return newByteFilterReader(r, &deltaState{
		dist: f.dist, hist: make([]byte, f.dist), decode: true,
	}, 4096), nil
}

// writeCloser creates a new io.WriteCloser for the encoding Delta
// filter.
func (f deltaFilter) writeCloser(w io.WriteCloser, c *WriterConfig,
) (io.WriteCloser, error) {
	if f.dist < deltaMinDist || f.dist > deltaMaxDist {
		return nil, errors.New("xz: Delta distance out of range")
	}
	bufSize := 4096
	if c != nil && c.BufSize > 0 {
		bufSize = c.BufSize
	}
	return newByteFilterWriter(w, &deltaState{
		dist: f.dist, hist: make([]byte, f.dist),
	}, bufSize), nil
}

// deltaState is the stateful Delta transformation.
type deltaState struct {
	dist int
	pos  int
	hist []byte
	// decode selects addition (decoding); encoding subtracts.
	decode bool
}

// transform implements the byteFilter interface. Encoding computes
// out = in - history (modulo 256) and stores the unencoded input
// byte in history. Decoding computes out = in + history and stores
// the decoded output byte in history. In both cases the stored
// history value is the byte of the other direction, which equals the
// uncompressed stream byte.
func (s *deltaState) transform(dst, src []byte, flush bool,
) (dn, sn int, err error) {
	if len(dst) < len(src) {
		if flush {
			return 0, 0, errShortBuffer
		}
		src = src[:len(dst)]
	}
	pos := s.pos
	for i := 0; i < len(src); i++ {
		h := s.hist[pos]
		var out byte
		if s.decode {
			out = src[i] + h
		} else {
			out = src[i] - h
		}
		dst[i] = out
		// History keeps the uncompressed stream byte: src on
		// encoding, the decoded output on decoding.
		if s.decode {
			s.hist[pos] = out
		} else {
			s.hist[pos] = src[i]
		}
		pos++
		if pos == s.dist {
			pos = 0
		}
	}
	s.pos = pos
	return len(src), len(src), nil
}
