// Copyright 2026 The Go Authors; based on the Go Authors' gzip implementation.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pgzip

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"runtime"
	"sync"

	"github.com/klauspost/compress/flate"
)

const (
	bgzfMaxBlockSize = 65536
	bgzfHeaderSize   = 18
	bgzfTrailerSize  = 8

	// bgzfMaxUncompressed is the largest chunk guaranteed to fit a single
	// BGZF member even when incompressible: DEFLATE stored blocks cost at most
	// input + 5 bytes, plus 26 bytes of gzip member overhead.
	bgzfMaxUncompressed = bgzfMaxBlockSize - bgzfHeaderSize - bgzfTrailerSize - 5

	// bgzfDefaultBlockSize matches htslib (65280 - 225).
	bgzfDefaultBlockSize = bgzfMaxUncompressed - 225
)

var bgzfExtraField = [8]byte{
	0x06, 0x00, // XLEN: total length of the extra field (SI1 SI2 SLEN BSIZE)
	'B', 'C', // SI1, SI2
	0x02, 0x00, // SLEN
	0x00, 0x00, // BSIZE placeholder
}

// bgzfEOF is the standard 28-byte empty BGZF member terminating every stream.
var bgzfEOF = [28]byte{
	0x1f, 0x8b, 0x08, 0x04, 0x00, 0x00, 0x00, 0x00,
	0x00, 0xff, 0x06, 0x00, 0x42, 0x43, 0x02, 0x00,
	0x1b, 0x00, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00,
}

var errBGZFClosed = errors.New("pgzip: Write on closed BGZF writer")

// bgzfReservation tracks one dispatched input chunk, in input order. The
// compressor sends assembled members through blocks; the collector writes them
// and fills the offset fields. The member metadata is retained until Flush so
// that VOffset can resolve intra-block positions.
type bgzfReservation struct {
	blocks chan []bgzfBlock
	done   chan struct{}

	endOffset int64 // on-disk offset immediately after this chunk
}

// bgzfMember records the location of one written BGZF member.
type bgzfMember struct {
	cstart int64 // compressed-file offset of the member start
	rawLen int   // uncompressed payload length
}

// bgzfBlock carries one fully assembled member from a compressor to the
// collector. data is backed by a pooled bgzfMaxBlockSize buffer; rawLen is its
// uncompressed payload length.
type bgzfBlock struct {
	data   []byte
	rawLen int
}

// BGZFWriter is an io.WriteCloser producing BGZF (Blocked GNU Zip Format)
// output as defined by the hts-specs.
//
// Writes are split into independent gzip members ("blocks"). Every block is at
// most 65536 bytes on disk, even for incompressible input: a chunk whose
// compressed form would exceed the limit is recursively split and emitted as
// several members, always in input order.
//
// Up to blocks chunks compress in parallel; completed blocks are written to
// the underlying io.Writer in input order. The stream ends with the standard
// 28-byte BGZF EOF marker.
//
// VOffset returns the virtual offset of completed data: the compressed-file
// offset in the high 48 bits and the uncompressed offset within the block in
// the low 16 bits.
type BGZFWriter struct {
	w      io.Writer
	level  int
	closed bool

	blockSize int
	blocks    int

	current []byte
	rawSize int64

	started       bool
	results       chan *bgzfReservation
	reserved      []*bgzfReservation
	collectorDone chan struct{}
	// members records every written member in order. Entries are consumed by
	// VOffset and then cleared; the reservation done channels serialize access.
	members []bgzfMember

	errMu     sync.RWMutex
	err       error
	pushedErr chan struct{}
	wg        sync.WaitGroup

	flatePool sync.Pool
	srcPool   sync.Pool
	dstPool   sync.Pool
	blockPool sync.Pool
}

// NewBGZFWriter returns a new BGZFWriter writing BGZF blocks to w at the
// default compression level.
//
// The caller must call Close when done. Writes may stay buffered until Flush
// or Close.
func NewBGZFWriter(w io.Writer) *BGZFWriter {
	z, _ := NewBGZFWriterLevel(w, DefaultCompression)
	return z
}

// NewBGZFWriterLevel is like NewBGZFWriter but selects the compression level.
func NewBGZFWriterLevel(w io.Writer, level int) (*BGZFWriter, error) {
	if level < ConstantCompression || level > BestCompression {
		return nil, fmt.Errorf("bgzf: invalid compression level: %d", level)
	}
	z := &BGZFWriter{w: w, level: level}
	z.pushedErr = make(chan struct{})
	z.SetConcurrency(bgzfDefaultBlockSize, runtime.GOMAXPROCS(0))
	return z, nil
}

// SetConcurrency sets the approximate uncompressed size of each compressed
// chunk and how many chunks may compress in parallel before Write blocks.
//
// blockSize must be between 1 and 65280. Chunks that still do not fit the
// 65536-byte BGZF limit after compression are split automatically. blocks must
// be positive.
func (z *BGZFWriter) SetConcurrency(blockSize, blocks int) error {
	if blockSize <= 0 || blockSize > bgzfMaxUncompressed {
		return fmt.Errorf("bgzf: block size must be between 1 and %d", bgzfMaxUncompressed)
	}
	if blocks <= 0 {
		return errors.New("bgzf: blocks cannot be zero or less")
	}
	z.blockSize = blockSize
	z.blocks = blocks
	z.results = make(chan *bgzfReservation, blocks)
	z.srcPool.New = func() any { return make([]byte, 0, blockSize) }
	z.dstPool.New = func() any { return make([]byte, 0, blockSize+(blockSize>>4)+64) }
	z.blockPool.New = func() any { return make([]byte, bgzfMaxBlockSize) }
	if z.flatePool.New == nil {
		z.flatePool.New = func() any {
			f, _ := flate.NewWriter(io.Discard, z.level)
			return f
		}
	}
	return nil
}

func (z *BGZFWriter) pushError(err error) {
	z.errMu.Lock()
	if z.err == nil {
		z.err = err
		close(z.pushedErr)
	}
	z.errMu.Unlock()
}

func (z *BGZFWriter) checkError() error {
	z.errMu.RLock()
	err := z.err
	z.errMu.RUnlock()
	return err
}

// Reset discards the writer's state and makes it equivalent to a fresh
// NewBGZFWriterLevel, but writing to w. Buffered data not yet written is lost.
func (z *BGZFWriter) Reset(w io.Writer) {
	if z.started && !z.closed {
		close(z.results)
		<-z.collectorDone
	}
	z.wg.Wait()
	z.w = w
	z.closed = false
	z.current = nil
	z.rawSize = 0
	z.started = false
	z.reserved = nil
	z.members = nil
	z.err = nil
	z.pushedErr = make(chan struct{})
	z.results = make(chan *bgzfReservation, z.blocks)
}

// startCollector runs the single goroutine that writes completed members to
// the underlying writer in input order. Reservation order on z.results is the
// input order (Write is single-goroutine), so receiving in that order and
// writing each reservation's members preserves block order.
func (z *BGZFWriter) startCollector() {
	z.started = true
	z.collectorDone = make(chan struct{})
	go func() {
		defer close(z.collectorDone)
		var coffset int64
		for r := range z.results {
			blocks := <-r.blocks
			failed := z.checkError() != nil
			for _, b := range blocks {
				mstart := coffset
				if !failed {
					n, err := z.w.Write(b.data)
					switch {
					case err != nil:
						z.pushError(err)
						failed = true
					case n != len(b.data):
						z.pushError(fmt.Errorf("bgzf: short write %d should be %d", n, len(b.data)))
						failed = true
					default:
						coffset += int64(len(b.data))
					}
				}
				z.members = append(z.members, bgzfMember{cstart: mstart, rawLen: b.rawLen})
				z.blockPool.Put(b.data[:bgzfMaxBlockSize])
			}
			r.endOffset = coffset
			close(r.done)
		}
		if z.checkError() == nil {
			n, err := z.w.Write(bgzfEOF[:])
			switch {
			case err != nil:
				z.pushError(err)
			case n != len(bgzfEOF):
				z.pushError(fmt.Errorf("bgzf: short EOF write %d should be %d", n, len(bgzfEOF)))
			}
		}
	}()
}

// Write compresses p into BGZF blocks. p is copied and may be reused once
// Write returns. Errors occurring during asynchronous compression surface from
// Flush or Close.
func (z *BGZFWriter) Write(p []byte) (int, error) {
	if z.closed {
		return 0, errBGZFClosed
	}
	if err := z.checkError(); err != nil {
		return 0, err
	}
	if !z.started {
		z.startCollector()
	}
	written := len(p)
	for len(p) > 0 {
		if z.current == nil {
			z.current = z.srcPool.Get().([]byte)[:0]
		}
		n := len(p)
		if n > z.blockSize-len(z.current) {
			n = z.blockSize - len(z.current)
		}
		z.current = append(z.current, p[:n]...)
		z.rawSize += int64(n)
		p = p[n:]
		if len(z.current) == z.blockSize {
			z.dispatch(z.current)
			z.current = nil
			if err := z.checkError(); err != nil {
				return written - len(p), err
			}
		}
	}
	return written, z.checkError()
}

// dispatch reserves an output slot in input order and launches compression.
// The buffered results channel bounds parallelism and provides backpressure.
func (z *BGZFWriter) dispatch(chunk []byte) {
	r := &bgzfReservation{
		blocks: make(chan []bgzfBlock, 1),
		done:   make(chan struct{}),
	}
	select {
	case z.results <- r:
	case <-z.pushedErr:
		z.srcPool.Put(chunk[:cap(chunk)])
		return
	}
	z.reserved = append(z.reserved, r)
	z.wg.Add(1)
	go z.compressChunk(chunk, r)
}

// compressChunk runs in a worker goroutine and hands assembled members to the
// collector through the reservation channel.
func (z *BGZFWriter) compressChunk(p []byte, r *bgzfReservation) {
	defer z.wg.Done()
	blocks, err := z.encodeChunk(p)
	z.srcPool.Put(p[:cap(p)])
	if err != nil {
		z.pushError(err)
		blocks = nil
	}
	r.blocks <- blocks
}

// encodeChunk compresses p into complete BGZF members. When the compressed
// representation would exceed bgzfMaxBlockSize it splits p in half and encodes
// each half independently, guaranteeing the block limit for any input,
// including incompressible data.
func (z *BGZFWriter) encodeChunk(p []byte) ([]bgzfBlock, error) {
	deflated := z.dstPool.Get().([]byte)
	dest := bytes.NewBuffer(deflated[:0])
	fw := z.flatePool.Get().(*flate.Writer)
	fw.Reset(dest)
	if _, err := fw.Write(p); err != nil {
		z.flatePool.Put(fw)
		z.dstPool.Put(deflated[:0])
		return nil, err
	}
	if err := fw.Close(); err != nil {
		z.flatePool.Put(fw)
		z.dstPool.Put(deflated[:0])
		return nil, err
	}
	z.flatePool.Put(fw)
	out := dest.Bytes()

	if bgzfHeaderSize+len(out)+bgzfTrailerSize <= bgzfMaxBlockSize {
		buf := z.blockPool.Get().([]byte)
		block := bgzfAssemble(buf[:0], p, out)
		z.dstPool.Put(deflated[:0])
		return []bgzfBlock{{data: block, rawLen: len(p)}}, nil
	}
	z.dstPool.Put(deflated[:0])

	// Too large: split and encode independently. Both halves are well below
	// bgzfMaxUncompressed, so their stored-block worst case always fits.
	mid := len(p) / 2
	left, err := z.encodeChunk(p[:mid])
	if err != nil {
		return nil, err
	}
	right, err := z.encodeChunk(p[mid:])
	if err != nil {
		return nil, err
	}
	return append(left, right...), nil
}

// bgzfAssemble appends one complete BGZF member for raw (whose DEFLATE form is
// deflated) to dst and returns it.
func bgzfAssemble(dst, raw, deflated []byte) []byte {
	total := bgzfHeaderSize + len(deflated) + bgzfTrailerSize

	dst = append(dst,
		gzipID1, gzipID2, gzipDeflate,
		flagExtra,  // FLG: FEXTRA only
		0, 0, 0, 0, // MTIME = 0
		0,   // XFL
		255, // OS = unknown
	)
	extra := bgzfExtraField
	put2(extra[6:8], uint16(total-1))
	dst = append(dst, extra[:]...)

	dst = append(dst, deflated...)

	var trailer [bgzfTrailerSize]byte
	put4(trailer[0:4], crc32.ChecksumIEEE(raw))
	put4(trailer[4:8], uint32(len(raw)))
	dst = append(dst, trailer[:]...)
	return dst
}

// Flush emits buffered data as BGZF block(s) and waits until all blocks
// dispatched so far are written to the underlying writer. An empty buffer
// produces no extra block.
func (z *BGZFWriter) Flush() error {
	if z.closed || !z.started {
		return z.checkError()
	}
	if err := z.checkError(); err != nil {
		return err
	}
	if z.current != nil && len(z.current) > 0 {
		chunk := z.current
		z.current = nil
		z.dispatch(chunk)
	}
	z.waitReserved()
	return z.checkError()
}

// waitReserved blocks until every dispatched reservation is fully written.
func (z *BGZFWriter) waitReserved() {
	for _, r := range z.reserved {
		<-r.done
	}
	z.reserved = z.reserved[:0]
}

// VOffset flushes buffered and in-flight data and returns the BGZF virtual
// offset of the end of all completed input. The returned value's high 48 bits
// are the compressed-file offset of the member containing that position and
// its low 16 bits are the uncompressed offset within that member, so it can be
// handed to a BGZF reader to seek to the corresponding record boundary.
//
// Calling VOffset does not force the current member to end early; the offset
// may therefore point inside a member.
func (z *BGZFWriter) VOffset() (uint64, error) {
	if err := z.Flush(); err != nil {
		return 0, err
	}
	if z.rawSize == 0 {
		return 0, nil
	}
	// Walk the written members in order to find the one containing the current
	// end of the uncompressed stream.
	rawLen := z.rawSize
	var pos int64
	for _, m := range z.members {
		if pos+int64(m.rawLen) >= rawLen {
			return uint64(m.cstart)<<16 | uint64(rawLen-pos), nil
		}
		pos += int64(m.rawLen)
	}
	return 0, fmt.Errorf("bgzf: internal error: cannot resolve virtual offset for %d bytes", rawLen)
}

// UncompressedSize returns the number of uncompressed bytes written.
func (z *BGZFWriter) UncompressedSize() int64 {
	return z.rawSize
}

// Close flushes pending data, writes the standard BGZF EOF marker and closes
// the writer, without closing the underlying io.Writer.
func (z *BGZFWriter) Close() error {
	if z.closed {
		return z.checkError()
	}
	z.closed = true
	if err := z.checkError(); err != nil {
		return err
	}
	if !z.started {
		// Empty input: output consists solely of the EOF marker.
		z.startCollector()
	} else if z.current != nil && len(z.current) > 0 {
		chunk := z.current
		z.current = nil
		z.dispatch(chunk)
	}
	z.waitReserved()
	close(z.results)
	<-z.collectorDone
	return z.checkError()
}
