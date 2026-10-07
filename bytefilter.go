// Copyright 2014-2025 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz

import (
	"errors"
	"io"
)

// byteFilter is the stateful transformation used by the simple byte
// filters Delta and x86 BCJ. The transform operates incrementally: it
// consumes data from src (recording srcProcessed) and writes encoded
// or decoded bytes to dst (recording dstProcessed). The
// transformation is allowed to buffer data internally, so
// srcProcessed may be smaller than len(src) even when dst has
// capacity left. When flush is true, all source bytes must be
// consumed and any buffered tail bytes must be emitted; if dst is too
// small, transform returns errShortBuffer and will be called again
// with a sufficiently large buffer.
type byteFilter interface {
	transform(dst, src []byte, flush bool) (
		dstProcessed, srcProcessed int, err error)
}

// errShortBuffer reports that the destination buffer is too short for
// the byte filter to complete its work.
var errShortBuffer = errors.New("xz: short buffer for byte filter")

// byteFilterReader presents an io.Reader interface for a stateful
// byte filter decoding data from an underlying reader. Transformed
// bytes are staged in the internal out buffer so that filter output
// never aliases caller-owned storage.
type byteFilterReader struct {
	f byteFilter
	r io.Reader
	// in is the input staging buffer; in0 and inN delimit bytes
	// read from r but not yet consumed by the filter.
	in  []byte
	in0 int
	inN int
	// out is the staging area for transformed bytes; out0 and
	// outN delimit the bytes not yet delivered to callers.
	out  []byte
	out0 int
	outN int
	// state is bfNormal while the underlying reader can supply
	// data, bfFlush after io.EOF has been seen while the filter
	// still drains buffered bytes, and bfDone afterwards.
	state bfState
}

type bfState uint8

const (
	bfNormal bfState = iota
	bfFlush
	bfDone
)

// newByteFilterReader creates a new byteFilterReader.
func newByteFilterReader(r io.Reader, f byteFilter, bufSize int) *byteFilterReader {
	if bufSize < 64 {
		bufSize = 64
	}
	return &byteFilterReader{
		f:     f,
		r:     r,
		in:    make([]byte, bufSize),
		out:   make([]byte, bufSize),
		state: bfNormal,
	}
}

// Read implements the io.Reader interface.
func (r *byteFilterReader) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	for n < len(p) {
		// Deliver already transformed bytes.
		if r.out0 < r.outN {
			k := copy(p[n:], r.out[r.out0:r.outN])
			r.out0 += k
			n += k
			continue
		}
		r.out0, r.outN = 0, 0
		if r.state == bfDone {
			return n, io.EOF
		}

		// Refill from the underlying reader in the normal state.
		if r.state == bfNormal && r.in0 == r.inN {
			m, rerr := r.r.Read(r.in)
			r.in0, r.inN = 0, m
			if rerr != nil && rerr != io.EOF {
				// Pass through errors other than EOF so
				// corrupt input is never decoded as
				// unfiltered data.
				return n, rerr
			}
			if m == 0 && rerr == nil {
				return n, io.ErrNoProgress
			}
			if rerr == io.EOF {
				r.state = bfFlush
			}
		}

		// While flushing, keep calling transform with flush=true
		// until it produces no output and all input has been
		// consumed; filters drain their buffered tail bytes this
		// way (x86 needs a second empty-source flush call).
		flush := r.state == bfFlush
		src := r.in[r.in0:r.inN]
		dn, sn, terr := r.f.transform(r.out, src, flush)
		r.in0 += sn
		r.out0, r.outN = 0, dn
		if terr != nil {
			return n, terr
		}
		if flush && r.in0 == r.inN && dn == 0 {
			r.state = bfDone
		}
	}
	return n, nil
}

// byteFilterWriter presents an io.WriteCloser interface for a
// stateful byte filter encoding data written to an underlying writer.
type byteFilterWriter struct {
	f    byteFilter
	w    io.Writer
	buf  []byte
	err  error
	next io.Closer
}

// newByteFilterWriter creates a new byteFilterWriter.
func newByteFilterWriter(w io.WriteCloser, f byteFilter,
	bufSize int) *byteFilterWriter {
	if bufSize < 64 {
		bufSize = 64
	}
	return &byteFilterWriter{
		f:    f,
		w:    w,
		buf:  make([]byte, bufSize),
		next: w,
	}
}

// Write implements the io.Writer interface.
func (w *byteFilterWriter) Write(p []byte) (n int, err error) {
	if w.err != nil {
		return 0, w.err
	}
	src := p
	for len(src) > 0 {
		dn, sn, terr := w.f.transform(w.buf[:cap(w.buf)], src, false)
		if terr != nil {
			w.err = terr
			return n, terr
		}
		if _, err = w.w.Write(w.buf[:dn]); err != nil {
			w.err = err
			return n, err
		}
		src = src[sn:]
		n += sn
	}
	return n, nil
}

// Close implements the io.Closer interface. It flushes bytes that may
// remain buffered in the filter, e.g. the tail bytes of an incomplete
// x86 instruction, and then closes the next writer in the filter
// chain so that inner filters such as LZMA2 can emit their final
// stream marker.
func (w *byteFilterWriter) Close() error {
	if w.err != nil {
		return w.err
	}
	for {
		dn, sn, err := w.f.transform(w.buf[:cap(w.buf)], nil, true)
		if err != nil {
			w.err = err
			return err
		}
		if _, err = w.w.Write(w.buf[:dn]); err != nil {
			w.err = err
			return err
		}
		if dn == 0 && sn == 0 {
			break
		}
	}
	if w.next != nil {
		if err := w.next.Close(); err != nil {
			w.err = err
			return err
		}
	}
	w.err = errClosed
	return nil
}
