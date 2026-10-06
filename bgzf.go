// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pgzip

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"runtime"
	"sync"

	"github.com/klauspost/compress/flate"
)

const (
	// bgzfMaxBlockSize is the maximum total size of a BGZF block. The
	// BSIZE field is 16 bits and stores the block size minus one.
	bgzfMaxBlockSize = 1 << 16 // 65536

	// bgzfHeaderSize is the fixed gzip header size, including the six byte
	// BC extra field.
	bgzfHeaderSize = 18

	// bgzfTrailerSize is the gzip trailer size (CRC32 plus ISIZE).
	bgzfTrailerSize = 8

	// bgzfMaxDeflate is the largest deflate stream that fits in a block.
	bgzfMaxDeflate = bgzfMaxBlockSize - bgzfHeaderSize - bgzfTrailerSize

	// bgzfMaxUncompressed is the largest chunk that may become one block.
	// When the input cannot be compressed, the flate backend emits a stored
	// deflate block. Its worst case expansion for the configured backend is
	// seven bytes, so a chunk this large always fits, even at
	// NoCompression; compressBlock still verifies the result defensively.
	bgzfMaxUncompressed = bgzfMaxDeflate - 7 // 65503

	// bgzfDefaultBlockSize is the default input chunk size and matches the
	// size used by other BGZF implementations.
	bgzfDefaultBlockSize = 65280
)

// bgzfEOF is the canonical 28 byte BGZF end-of-file marker: an empty
// member whose single block decompresses to zero bytes.
var bgzfEOF = [28]byte{
	0x1f, 0x8b, 0x08, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff,
	0x06, 0x00, 0x42, 0x43, 0x02, 0x00, 0x1b, 0x00,
	0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
}

// bgzfJob is one uncompressed chunk handed to a compressor goroutine.
type bgzfJob struct {
	p      []byte
	uStart uint64
	seq    int
	uLen   int
	block  []byte
	err    error
}

// bgzfBlockMeta records where a completed block landed in the output. It is
// used to map uncompressed positions to BGZF virtual file offsets.
type bgzfBlockMeta struct {
	uStart uint64 // first uncompressed byte in the block
	uLen   uint64 // number of uncompressed bytes in the block
	cOff   uint64 // offset of the block in the compressed output
	cLen   uint64 // size of the block in the compressed output
}

// BGZFWriter is an io.WriteCloser producing BGZF, the block gzip format
// used by SAM/BAM (see the BGZF specification in
// https://github.com/samtools/hts-specs).
//
// Input is split into chunks, each emitted as a standalone gzip member
// carrying the BC extra field. No block ever exceeds the 64 KiB BGZF
// limit, including for data that cannot be compressed. Chunks are compressed
// in parallel, bounded by the configured concurrency, but blocks are
// written to the underlying writer in input order. Close appends the
// standard 28 byte BGZF end-of-file marker.
//
// VOffset returns the BGZF virtual file offset of already-completed data,
// which can be used later to seek back to a record boundary.
//
// A BGZFWriter is not safe for concurrent Write, Flush or Close calls.
type BGZFWriter struct {
	w     io.Writer
	level int

	mu        sync.Mutex
	err       error
	started   bool
	closed    bool
	blockSize int
	parallel  int

	size     uint64 // uncompressed bytes submitted to compressors so far
	accepted uint64 // total uncompressed bytes accepted by Write
	buf      []byte // pending input not yet handed to a compressor
	meta     []bgzfBlockMeta

	nextSeq int
	cOff    uint64

	tokens  chan struct{}
	jobs    chan *bgzfJob
	results chan *bgzfJob
	emitted chan bgzfBlockMeta
	finish  chan int
	done    chan struct{}
	workers sync.WaitGroup

	inPool  sync.Pool
	outPool sync.Pool
	flPool  sync.Pool
}

// NewBGZFWriter returns a BGZFWriter using DefaultCompression.
func NewBGZFWriter(w io.Writer) *BGZFWriter {
	z, _ := NewBGZFWriterLevel(w, DefaultCompression)
	return z
}

// NewBGZFWriterLevel is like NewBGZFWriter but specifies the compression
// level. The accepted levels are the same as for NewWriterLevel.
func NewBGZFWriterLevel(w io.Writer, level int) (*BGZFWriter, error) {
	if level < ConstantCompression || level > BestCompression {
		return nil, fmt.Errorf("bgzf: invalid compression level: %d", level)
	}
	z := new(BGZFWriter)
	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		n = 1
	}
	z.init(w, level, bgzfDefaultBlockSize, n)
	return z, nil
}

func (z *BGZFWriter) init(w io.Writer, level, blockSize, parallel int) {
	if parallel < 1 {
		parallel = 1
	}
	if blockSize < 1 {
		blockSize = 1
	}
	if blockSize > bgzfMaxUncompressed {
		blockSize = bgzfMaxUncompressed
	}
	z.w = w
	z.level = level
	z.err = nil
	z.started = false
	z.closed = false
	z.blockSize = blockSize
	z.parallel = parallel
	z.size = 0
	z.accepted = 0
	z.buf = nil
	z.meta = z.meta[:0]
	z.nextSeq = 0
	z.cOff = 0
	z.tokens = make(chan struct{}, parallel)
	z.jobs = make(chan *bgzfJob)
	z.results = make(chan *bgzfJob, parallel)
	z.emitted = make(chan bgzfBlockMeta, parallel+1)
	z.finish = make(chan int, 1)
	z.done = make(chan struct{})
	for i := 0; i < parallel; i++ {
		z.tokens <- struct{}{}
	}
	z.inPool.New = func() any { return make([]byte, 0, blockSize) }
	z.outPool.New = func() any { return make([]byte, 0, bgzfMaxBlockSize) }
	z.flPool.New = func() any {
		f, err := flate.NewWriter(io.Discard, level)
		if err != nil {
			panic(fmt.Sprintf("bgzf: invalid compression level: %d", level))
		}
		return f
	}
}

// SetConcurrency sets the input block size and the maximum number of blocks
// compressed in parallel. Larger blocks may compress slightly better at the
// cost of latency and buffering. It must be called before the first Write,
// Flush or Close.
func (z *BGZFWriter) SetConcurrency(blockSize, blocks int) error {
	if z.started {
		return errors.New("bgzf: SetConcurrency must be called before the first Write")
	}
	if blockSize <= 0 {
		return errors.New("bgzf: block size cannot be zero or negative")
	}
	if blockSize > bgzfMaxUncompressed {
		return fmt.Errorf("bgzf: block size cannot exceed %d", bgzfMaxUncompressed)
	}
	if blocks <= 0 {
		return errors.New("bgzf: blocks cannot be zero or negative")
	}
	z.init(z.w, z.level, blockSize, blocks)
	return nil
}

func (z *BGZFWriter) setErr(err error) {
	z.mu.Lock()
	if z.err == nil {
		z.err = err
	}
	z.mu.Unlock()
}

func (z *BGZFWriter) fail() error {
	z.mu.Lock()
	err := z.err
	z.mu.Unlock()
	return err
}

func (z *BGZFWriter) startPipelines() {
	z.started = true
	for i := 0; i < z.parallel; i++ {
		z.workers.Add(1)
		go z.worker()
	}
	go z.collector()
}

func (z *BGZFWriter) worker() {
	defer z.workers.Done()
	for job := range z.jobs {
		block, err := z.compressBlock(job.p)
		job.block = block
		job.err = err
		job.uLen = len(job.p)
		select {
		case z.results <- job:
		case <-z.done:
			if block != nil {
				z.outPool.Put(block)
			}
			z.inPool.Put(job.p)
		}
	}
}

// releaseToken returns one backpressure slot. It is non-blocking because a
// slot was always acquired by submit before the job existed.
func (z *BGZFWriter) releaseToken() {
	select {
	case z.tokens <- struct{}{}:
	default:
	}
}

// bgzfHeaderBytes is the fixed gzip header for a BGZF data block. The final
// two bytes (BSIZE) are filled in once the total size is known.
var bgzfHeaderBytes = [bgzfHeaderSize]byte{
	gzipID1, gzipID2, gzipDeflate, flagExtra,
	0, 0, 0, 0, // MTIME = 0
	0,    // XFL
	255,  // OS = unknown
	6, 0, // XLEN = 6
	'B', 'C', 2, 0, // SI1, SI2 = "BC"; SLEN = 2
	0, 0, // BSIZE placeholder: total block size minus one
}

// compressBlock produces one self-contained BGZF block from p. No deflate
// dictionary is used so every block can be decompressed independently.
func (z *BGZFWriter) compressBlock(p []byte) ([]byte, error) {
	out := z.outPool.Get().([]byte)
	if cap(out) < bgzfMaxBlockSize {
		out = make([]byte, bgzfMaxBlockSize)
	}
	block := out[:bgzfMaxBlockSize]
	copy(block, bgzfHeaderBytes[:])

	// dst writes the deflate stream directly into the block, right after
	// the fixed header. Using a flat slice (instead of a bytes.Buffer)
	// guarantees BSIZE and the trailer never move when the buffer grows.
	dst := &sliceWriter{buf: block[bgzfHeaderSize:bgzfHeaderSize]}
	comp := z.flPool.Get().(*flate.Writer)
	comp.Reset(dst)
	if _, err := comp.Write(p); err != nil {
		z.flPool.Put(comp)
		z.outPool.Put(out)
		return nil, err
	}
	if err := comp.Close(); err != nil {
		z.flPool.Put(comp)
		z.outPool.Put(out)
		return nil, err
	}
	z.flPool.Put(comp)

	deflateLen := dst.Len()
	total := bgzfHeaderSize + deflateLen + bgzfTrailerSize
	if total > bgzfMaxBlockSize {
		z.outPool.Put(out)
		return nil, fmt.Errorf("bgzf: block of %d bytes exceeds the %d byte limit", total, bgzfMaxBlockSize)
	}

	put2(block[16:18], uint16(total-1))

	deflateEnd := bgzfHeaderSize + deflateLen
	put4(block[deflateEnd:deflateEnd+4], crc32.ChecksumIEEE(p))
	put4(block[deflateEnd+4:deflateEnd+8], uint32(len(p)))

	return block[:total], nil
}

// sliceWriter is an io.Writer that appends into a preallocated slice.
type sliceWriter struct {
	buf []byte
}

func (s *sliceWriter) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	return len(p), nil
}

func (s *sliceWriter) Len() int { return len(s.buf) }

// collector writes finished blocks in input order, publishes their metadata
// and releases one backpressure token per data block. Once every job is on
// disk it appends the canonical EOF marker.
func (z *BGZFWriter) collector() {
	defer close(z.done)
	pending := make(map[int]*bgzfJob)
	nextEmit := 0
	total := -1

	emitReady := func() {
		for {
			job, ok := pending[nextEmit]
			if !ok {
				return
			}
			delete(pending, nextEmit)
			if job.err != nil {
				z.setErr(job.err)
				z.inPool.Put(job.p)
				z.releaseToken()
				return
			}
			n, err := z.w.Write(job.block)
			if err != nil {
				z.setErr(err)
				z.outPool.Put(job.block)
				z.inPool.Put(job.p)
				z.releaseToken()
				return
			}
			if n != len(job.block) {
				z.setErr(io.ErrShortWrite)
				z.outPool.Put(job.block)
				z.inPool.Put(job.p)
				z.releaseToken()
				return
			}
			m := bgzfBlockMeta{
				uStart: job.uStart,
				uLen:   uint64(job.uLen),
				cOff:   z.cOff,
				cLen:   uint64(n),
			}
			z.cOff += uint64(n)
			z.outPool.Put(job.block)
			z.inPool.Put(job.p)
			// Release the backpressure slot before reporting the block, so
			// submit never has to wait on a blocked metadata channel.
			z.releaseToken()
			z.emitted <- m
			nextEmit++
		}
	}

	for {
		select {
		case job := <-z.results:
			pending[job.seq] = job
			emitReady()
			if z.fail() != nil {
				return
			}
		case t := <-z.finish:
			total = t
		}
		if total >= 0 && nextEmit == total {
			if _, err := z.w.Write(bgzfEOF[:]); err != nil {
				z.setErr(err)
				return
			}
			z.emitted <- bgzfBlockMeta{
				uStart: z.size,
				uLen:   0,
				cOff:   z.cOff,
				cLen:   uint64(len(bgzfEOF)),
			}
			return
		}
	}
}
