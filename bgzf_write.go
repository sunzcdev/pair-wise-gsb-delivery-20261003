// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pgzip

import (
	"errors"
	"io"
	"sort"
)

// drainEmitted absorbs metadata for blocks the collector already finished.
func (z *BGZFWriter) drainEmitted() {
	for {
		select {
		case m := <-z.emitted:
			z.meta = append(z.meta, m)
		default:
			return
		}
	}
}

// waitMeta blocks until metadata for at least n blocks (counting the EOF
// block) is available, or the collector stops.
func (z *BGZFWriter) waitMeta(n int) error {
	for {
		if err := z.fail(); err != nil {
			return err
		}
		if len(z.meta) >= n {
			return nil
		}
		select {
		case m := <-z.emitted:
			z.meta = append(z.meta, m)
		case <-z.done:
			z.drainEmitted()
			if len(z.meta) >= n {
				return nil
			}
			if err := z.fail(); err != nil {
				return err
			}
			return errors.New("bgzf: writer stopped before all blocks were written")
		}
	}
}

// acquireSlot reserves one of the bounded compression slots, applying
// backpressure when parallel blocks are already in flight.
func (z *BGZFWriter) acquireSlot() error {
	z.drainEmitted()
	if err := z.fail(); err != nil {
		return err
	}
	select {
	case <-z.tokens:
	case <-z.done:
		z.drainEmitted()
		return z.fail()
	}
	return nil
}

// submit sends one full chunk to the compressor pool. p is a pooled buffer
// whose ownership is transferred; the caller must not retain it.
func (z *BGZFWriter) submit(p []byte) error {
	if err := z.acquireSlot(); err != nil {
		z.inPool.Put(p)
		return err
	}
	uStart := z.size
	z.size += uint64(len(p))
	seq := z.nextSeq
	z.nextSeq++
	job := &bgzfJob{p: p, uStart: uStart, seq: seq}
	select {
	case z.jobs <- job:
	case <-z.done:
		z.drainEmitted()
		z.releaseToken()
		z.inPool.Put(p)
		return z.fail()
	}
	return nil
}

// flushBuffer submits the current pending buffer as a block when non-empty
// and replaces it with a fresh one. wait=true blocks until the block lands.
func (z *BGZFWriter) flushBuffer(wait bool) error {
	if len(z.buf) == 0 {
		return nil
	}
	p := z.buf
	z.buf = z.inPool.Get().([]byte)[:0]
	seq := z.nextSeq
	if err := z.submit(p); err != nil {
		return err
	}
	if wait {
		return z.waitMeta(seq + 1)
	}
	return nil
}

// Write writes the compressed form of p. Bytes are buffered up to the
// configured block size and then handed to a compressor; Write applies
// backpressure so that at most the configured number of blocks are
// compressed concurrently.
func (z *BGZFWriter) Write(p []byte) (int, error) {
	if z.closed {
		return 0, errors.New("bgzf: Write on closed writer")
	}
	if err := z.fail(); err != nil {
		return 0, err
	}
	if !z.started {
		z.startPipelines()
	}

	written := 0
	for len(p) > 0 {
		z.drainEmitted()
		if err := z.fail(); err != nil {
			return written, err
		}
		if cap(z.buf) == 0 {
			z.buf = z.inPool.Get().([]byte)[:0]
		}
		room := z.blockSize - len(z.buf)
		n := len(p)
		if n > room {
			n = room
		}
		z.buf = append(z.buf, p[:n]...)
		z.accepted += uint64(n)
		p = p[n:]
		written += n
		if len(z.buf) == z.blockSize {
			if err := z.flushBuffer(false); err != nil {
				return written, err
			}
		}
	}
	return written, z.fail()
}

// Flush forces any buffered data out as a BGZF block and waits until it has
// been written. It does not emit the EOF marker, so more data may be
// written afterwards.
func (z *BGZFWriter) Flush() error {
	if z.closed {
		return nil
	}
	if err := z.fail(); err != nil {
		return err
	}
	if !z.started {
		z.startPipelines()
	}
	return z.flushBuffer(true)
}

// Close flushes remaining data, waits for all blocks to be written in input
// order and appends the standard BGZF end-of-file marker. The underlying
// writer is not closed.
func (z *BGZFWriter) Close() error {
	if z.closed {
		return z.fail()
	}
	if err := z.fail(); err != nil {
		return err
	}
	if !z.started {
		z.startPipelines()
	}
	z.closed = true

	if err := z.flushBuffer(true); err != nil {
		return err
	}

	total := z.nextSeq
	// The collector writes the EOF block once it has seen every job.
	z.finish <- total

	// Metadata entries: one per data block plus the EOF block.
	if err := z.waitMeta(total + 1); err != nil {
		return err
	}
	close(z.jobs)
	z.workers.Wait()
	<-z.done
	return z.fail()
}

// UncompressedSize returns the number of uncompressed bytes submitted so
// far.
func (z *BGZFWriter) UncompressedSize() int {
	return int(z.accepted)
}

// VOffset returns the BGZF virtual file offset corresponding to the
// uncompressed byte position off. off may be any position between zero and
// UncompressedSize; the returned value packs the containing block's
// compressed offset in its upper 48 bits and the within-block offset in its
// lower 16 bits.
//
// Only data already written (after a Flush, Close, or enough Writes to
// complete a block) can be resolved; VOffset blocks until the block
// containing off has been written.
func (z *BGZFWriter) VOffset(off int64) (int64, error) {
	if off < 0 || uint64(off) > z.accepted {
		return 0, errors.New("bgzf: offset out of range")
	}
	target := uint64(off)

	// Wait until a completed block covering target has been written.
	for {
		z.drainEmitted()

		// If a block already covers target, its offset is final.
		idx := sort.Search(len(z.meta), func(i int) bool {
			return z.meta[i].uStart+z.meta[i].uLen > target
		})
		if idx < len(z.meta) {
			m := z.meta[idx]
			return int64(m.cOff<<16 | (target - m.uStart)), nil
		}

		// Data still held in the input buffer has no stable block yet.
		if target > z.size {
			return 0, errors.New("bgzf: offset refers to data that has not been flushed")
		}

		// The block covering target has been submitted to a compressor but
		// has not finished writing; wait for it in input order.
		if err := z.waitMeta(len(z.meta) + 1); err != nil {
			return 0, err
		}
	}
}

// EOFVOffset returns the virtual file offset of the canonical BGZF EOF block
// appended by Close, i.e. the position immediately following all data. The
// writer must be closed.
func (z *BGZFWriter) EOFVOffset() (int64, error) {
	if !z.closed {
		return 0, errors.New("bgzf: writer is not closed")
	}
	if err := z.waitMeta(1); err != nil {
		return 0, err
	}
	m := z.meta[len(z.meta)-1]
	if m.uLen != 0 {
		return 0, errors.New("bgzf: EOF block metadata missing")
	}
	return int64(m.cOff << 16), nil
}

var _ io.Writer = (*BGZFWriter)(nil)
