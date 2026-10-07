// Copyright 2014-2025 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz

import (
	"errors"
	"fmt"
	"io"
)

// x86 BCJ filter constants.
const (
	x86FilterID  = 0x04
	x86InstrLen  = 5
	x86CallJmpOp = 0xe8
	x86JccJmpOp  = 0xe9
)

// x86Filter represents the x86 BCJ filter stored in an xz block
// header.
type x86Filter struct {
	startOffset uint32
}

// String returns a representation of the x86 BCJ filter.
func (f x86Filter) String() string {
	return fmt.Sprintf("x86 BCJ start offset %#x", f.startOffset)
}

// id returns the ID of the x86 BCJ filter.
func (f x86Filter) id() uint64 { return x86FilterID }

// MarshalBinary converts the x86Filter into its encoded
// representation. A zero start offset is encoded without properties,
// matching the xz convention.
func (f x86Filter) MarshalBinary() (data []byte, err error) {
	if f.startOffset == 0 {
		return []byte{x86FilterID, 0}, nil
	}
	data = []byte{x86FilterID, 4, 0, 0, 0, 0}
	putUint32LE(data[2:], f.startOffset)
	return data, nil
}

// UnmarshalBinary unmarshals the encoded representation of the x86
// BCJ filter.
func (f *x86Filter) UnmarshalBinary(data []byte) error {
	switch {
	case len(data) == 2 && data[0] == x86FilterID && data[1] == 0:
		f.startOffset = 0
		return nil
	case len(data) == 6 && data[0] == x86FilterID && data[1] == 4:
		f.startOffset = uint32LE(data[2:])
		return nil
	default:
		if len(data) >= 1 && data[0] != x86FilterID {
			return errors.New("xz: wrong x86 BCJ filter id")
		}
		return errors.New("xz: wrong x86 BCJ filter properties")
	}
}

// last reports whether the filter must be the last filter in the
// chain. The x86 BCJ filter cannot be last.
func (f x86Filter) last() bool { return false }

// reader creates a new reader for the decoding x86 BCJ filter.
func (f x86Filter) reader(r io.Reader, c *ReaderConfig) (io.Reader, error) {
	return newByteFilterReader(r, &x86State{
		pos:     f.startOffset,
		pending: make([]byte, 0, x86InstrLen-1),
		decode:  true,
	}, 4096), nil
}

// writeCloser creates a new io.WriteCloser for the encoding x86 BCJ
// filter.
func (f x86Filter) writeCloser(w io.WriteCloser, c *WriterConfig,
) (io.WriteCloser, error) {
	bufSize := 4096
	if c != nil && c.BufSize > 0 {
		bufSize = c.BufSize
	}
	return newByteFilterWriter(w, &x86State{
		pos:     f.startOffset,
		pending: make([]byte, 0, x86InstrLen-1),
	}, bufSize), nil
}

// x86State is the stateful x86 BCJ transformation.
type x86State struct {
	// pos is the absolute stream position of the next byte that
	// is finalized (i.e. emitted or decoded), including the
	// pending candidate instruction.
	pos uint32
	// pending holds zero to five bytes carried over from a
	// previous chunk: an E8/E9 opcode plus up to four of its
	// argument bytes that may be completed by future input. All
	// bytes preceding this candidate have already been emitted.
	pending []byte
	decode  bool
}

// transform implements the byteFilter interface.
func (s *x86State) transform(dst, src []byte, flush bool,
) (dn, sn int, err error) {
	// A non-flush call finalizes only as many stitched bytes as
	// fit in dst while keeping a possible trailing E8/E9
	// instruction (at most five bytes) in reserve. A flush call
	// processes everything available; an incomplete trailing
	// instruction is emitted unchanged.
	pn := len(s.pending)

	use := len(src)
	// Drain only what fits in dst. Both normal and intermediate
	// flush calls reserve the final four bytes; the last flush
	// call (empty source) releases an incomplete trailing
	// instruction unchanged.
	if !flush || len(src) > 0 {
		// Leave room in dst for pending plus the new reserve.
		use = len(dst) - pn - (x86InstrLen - 1)
		if use > len(src) {
			use = len(src)
		}
		if use < 0 {
			use = 0
		}
	}

	buf := make([]byte, pn+use)
	copy(buf, s.pending)
	copy(buf[pn:], src[:use])

	// Determine the finalized region. Without flush reserve the
	// final four bytes; on flush with unconsumed source remaining
	// reserve as well; only the very last flush (all source in)
	// emits a possible incomplete trailing instruction unchanged.
	limit := len(buf)
	final := flush && len(src) == 0 && use == 0
	if !final && limit >= x86InstrLen-1 {
		limit -= x86InstrLen - 1
	}
	n := scanX86Limit(buf, s.pos, s.decode, limit)
	take := n
	if final {
		// The final flush emits an incomplete trailing E8/E9
		// sequence unchanged.
		take = len(buf)
	}

	if take > len(dst) {
		return 0, 0, errShortBuffer
	}
	copy(dst, buf[:take])

	s.pending = append([]byte(nil), buf[take:]...)
	s.pos += uint32(take)
	return take, use, nil
}

// scanX86Limit applies the x86 conversion to complete instructions
// wholly within buf[:limit] and returns the index of the first
// unfinalized byte. Bytes in [limit,len(buf)) are never touched and
// are meant to remain in reserve.
func scanX86Limit(buf []byte, base uint32, decode bool, limit int) int {
	n := 0
	for n < limit {
		b := buf[n]
		if b != x86CallJmpOp && b != x86JccJmpOp {
			n++
			continue
		}
		if n+x86InstrLen > limit {
			break
		}
		v := uint32(buf[n+1]) | uint32(buf[n+2])<<8 |
			uint32(buf[n+3])<<16 | uint32(buf[n+4])<<24
		p := base + uint32(n) + x86InstrLen
		if decode {
			v -= p
		} else {
			v += p
		}
		if msb := byte(v >> 24); msb == 0 || msb == 0xff {
			buf[n+1] = byte(v)
			buf[n+2] = byte(v >> 8)
			buf[n+3] = byte(v >> 16)
			buf[n+4] = msb
		}
		n += x86InstrLen
	}
	return n
}
