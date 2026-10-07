// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// x86 BCJ filter ID as defined by the xz file format.
const x86FilterID = 0x04

// X86Filter configures the x86 BCJ (Branch/Call/Jump) filter for an xz
// block. The filter converts the relative operands of x86 CALL and
// JMP instructions into absolute ones to improve LZMA2 compression.
//
// The X86Filter can only be used as a non-last filter of the filter
// chain, i.e. the LZMA2 filter has to follow.
type X86Filter struct {
	// StartOffset specifies the position of the first input byte
	// relative to the beginning of the executable. The xz file
	// format stores it as a 32-bit unsigned integer and it is
	// almost always zero.
	StartOffset uint32
}

// filter marks X86Filter as an implementation of the exported Filter
// interface.
func (f *X86Filter) filter() {}

// String returns a representation of the x86 BCJ filter.
func (f X86Filter) String() string {
	return fmt.Sprintf("x86 BCJ start offset %d", f.StartOffset)
}

// id returns the ID of the x86 BCJ filter.
func (f X86Filter) id() uint64 { return x86FilterID }

// MarshalBinary encodes the x86 BCJ filter properties as stored in an
// xz block header. The start offset is omitted when zero as permitted
// by the xz file format.
func (f X86Filter) MarshalBinary() (data []byte, err error) {
	if f.StartOffset == 0 {
		return []byte{x86FilterID, 0}, nil
	}
	data = make([]byte, 6)
	data[0] = x86FilterID
	data[1] = 4
	binary.LittleEndian.PutUint32(data[2:], f.StartOffset)
	return data, nil
}

// UnmarshalBinary decodes the x86 BCJ filter properties from the
// representation stored in an xz block header. Only a property size of
// zero or four is supported.
func (f *X86Filter) UnmarshalBinary(data []byte) error {
	if len(data) < 2 || data[0] != x86FilterID {
		return errors.New("xz: wrong x86 BCJ filter id")
	}
	switch data[1] {
	case 0:
		if len(data) != 2 {
			return errors.New("xz: wrong x86 BCJ filter size")
		}
		f.StartOffset = 0
	case 4:
		if len(data) != 6 {
			return errors.New("xz: wrong x86 BCJ filter size")
		}
		f.StartOffset = binary.LittleEndian.Uint32(data[2:])
	default:
		return errors.New("xz: wrong x86 BCJ filter size")
	}
	return nil
}

// reader creates a reader that applies the inverse x86 BCJ
// transformation.
func (f X86Filter) reader(r io.Reader, c *ReaderConfig) (io.Reader, error) {
	return &x86Reader{r: r, pos: f.StartOffset}, nil
}

// writeCloser creates a WriteCloser applying the x86 BCJ
// transformation.
func (f X86Filter) writeCloser(w io.WriteCloser, c *WriterConfig,
) (io.WriteCloser, error) {
	return &x86WriteCloser{
		WriteCloser: w,
		pos:         f.StartOffset,
	}, nil
}

// last returns false because the x86 BCJ filter cannot be the last
// filter in the chain.
func (f X86Filter) last() bool { return false }

// x86BranchLimit is the +/-16 MiB boundary of the near-jump range used
// by the reference implementation (LZMA SDK/liblzma). Only branches
// with a relative (encoder) or absolute (decoder) target operand in
// this range are transformed.
const x86BranchLimit uint32 = 0x01000000

// inRange reports whether x lies in the symmetric interval
// +/-x86BranchLimit around zero, interpreted as a signed 32-bit value.
func inRange(x uint32) bool {
	return x+x86BranchLimit < 2*x86BranchLimit
}

// x86Apply performs the BCJ transformation on a contiguous buffer in
// place. If encode is true, relative branch operands are converted to
// absolute ones; otherwise the inverse transformation is applied. It
// returns the number of leading bytes that are complete and have been
// transformed. If an E8/E9 opcode near the end has fewer than four
// operand bytes left, the opcode and everything after it is returned
// untouched; the streaming wrappers supply those bytes again together
// with the next chunk.
func x86Apply(buf []byte, pos uint32, encode bool) (n int, next uint32) {
	i := 0
	for i < len(buf) {
		if buf[i] != 0xe8 && buf[i] != 0xe9 {
			i++
			continue
		}
		if i+5 > len(buf) {
			// Instruction continues in a later chunk.
			return i, pos + uint32(i)
		}
		rel := uint32(buf[i+1]) | uint32(buf[i+2])<<8 |
			uint32(buf[i+3])<<16 | uint32(buf[i+4])<<24
		var v uint32
		if encode {
			// Convert only calls/jumps whose relative operand
			// addresses a target within +/-16 MiB.
			if !inRange(rel) {
				i++
				continue
			}
			// Store the absolute target.
			v = rel + pos + uint32(i) + 5
		} else {
			// The stored operand is the absolute target; convert
			// only if it lies within +/-16 MiB of the stream start.
			if !inRange(rel) {
				i++
				continue
			}
			// Decode to the relative operand.
			v = rel - (pos + uint32(i) + 5)
		}
		buf[i+1] = byte(v)
		buf[i+2] = byte(v >> 8)
		buf[i+3] = byte(v >> 16)
		buf[i+4] = byte(v >> 24)
		// The four operand bytes can't start an instruction.
		i += 5
	}
	return len(buf), pos + uint32(len(buf))
}

// x86Reader reverses the x86 BCJ transformation on the fly.
type x86Reader struct {
	r   io.Reader
	pos uint32
	// carry holds the untransformed bytes at the end of the previous
	// window that may begin an instruction (at most four bytes).
	in []byte
	// out holds transformed bytes already produced but not yet
	// returned to the caller.
	out []byte
}

// Read reads BCJ-filtered data and returns the unfiltered bytes. Bytes
// of an instruction crossing a chunk boundary are retained between
// calls so the transformation stays correct for arbitrary chunk sizes.
func (r *x86Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.out) > 0 {
		n := copy(p, r.out)
		r.out = r.out[n:]
		return n, nil
	}

	// Build the window from the carried suffix plus newly read
	// bytes. Reserve four trailing positions so a full read always
	// leaves room to hold back an incomplete instruction. The
	// upstream reader is a limited reader around an LZMA2 chunk and
	// must not be over-read: never request more than len(p) new
	// bytes.
	data := make([]byte, len(r.in)+len(p)+4)
	n := copy(data, r.in)
	r.in = r.in[:0]
	m, err := r.r.Read(data[n : n+len(p)])
	n += m

	stop, next := x86Apply(data[:n], r.pos, false)
	if err != nil {
		// End of stream: a trailing instruction with fewer than five
		// bytes passes through unchanged.
		stop = n
		next = r.pos + uint32(n)
	}
	r.pos = next

	// Carry the untransformed suffix (at most four bytes) to the
	// next call. It has been accounted for already: the position
	// counter doesn't include it and its bytes are passed again.
	r.in = append(r.in[:0], data[stop:n]...)

	k := copy(p, data[:stop])
	if k < stop {
		r.out = append(r.out[:0], data[k:stop]...)
	}
	return k, err
}

// x86WriteCloser applies the x86 BCJ transformation on the fly.
type x86WriteCloser struct {
	io.WriteCloser
	pos  uint32
	tail []byte
}

// Write writes the BCJ filtered bytes of p to the underlying writer.
// Up to four bytes of an instruction spanning Write calls are retained
// across calls so it is converted with the correct position.
func (w *x86WriteCloser) Write(p []byte) (int, error) {
	combined := make([]byte, len(w.tail)+len(p))
	n := copy(combined, w.tail)
	copy(combined[n:], p)

	stop, next := x86Apply(combined, w.pos, true)
	w.pos = next
	w.tail = append(w.tail[:0], combined[stop:]...)
	if stop > 0 {
		if _, err := w.WriteCloser.Write(combined[:stop]); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Close flushes the retained tail bytes and closes the underlying
// writer.
func (w *x86WriteCloser) Close() error {
	if len(w.tail) > 0 {
		tail := w.tail
		w.tail = nil
		// The tail is an instruction with fewer than five bytes; it
		// passes through unchanged.
		if _, err := w.WriteCloser.Write(tail); err != nil {
			return err
		}
		w.pos += uint32(len(tail))
	}
	return w.WriteCloser.Close()
}

// ensure the interface is satisfied.
var _ filter = (*X86Filter)(nil)
