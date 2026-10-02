// Copyright 2017 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"compress/flate"
	"errors"
	"io"
	"strings"
	"sync"
)

const (
	minCompressionLevel     = -2 // flate.HuffmanOnly not defined in Go < 1.6
	maxCompressionLevel     = flate.BestCompression
	defaultCompressionLevel = 1

	// maxContextTakeoverWindow is the maximum size of the sliding window
	// (dictionary) retained across messages when context takeover is
	// negotiated (RFC 7692).
	maxContextTakeoverWindow = 1 << 15
)

var (
	flateWriterPools [maxCompressionLevel - minCompressionLevel + 1]sync.Pool
	flateReaderPool  = sync.Pool{New: func() interface{} {
		return flate.NewReader(nil)
	}}
)

func decompressNoContextTakeover(r io.Reader) io.ReadCloser {
	const tail =
	// Add four bytes as specified in RFC
	"\x00\x00\xff\xff" +
		// Add final block to squelch unexpected EOF error from flate reader.
		"\x01\x00\x00\xff\xff"

	fr, _ := flateReaderPool.Get().(io.ReadCloser)
	mr := io.MultiReader(r, strings.NewReader(tail))
	if err := fr.(flate.Resetter).Reset(mr, nil); err != nil {
		// Reset never fails, but handle error in case that changes.
		fr = flate.NewReader(mr)
	}
	return &flateReadWrapper{fr}
}

func isValidCompressionLevel(level int) bool {
	return minCompressionLevel <= level && level <= maxCompressionLevel
}

func compressNoContextTakeover(w io.WriteCloser, level int) io.WriteCloser {
	p := &flateWriterPools[level-minCompressionLevel]
	tw := &truncWriter{w: w}
	fw, _ := p.Get().(*flate.Writer)
	if fw == nil {
		fw, _ = flate.NewWriter(tw, level)
	} else {
		fw.Reset(tw)
	}
	return &flateWriteWrapper{fw: fw, tw: tw, p: p}
}

// compressContextTakeover returns a function that creates compression
// writers for a single connection. All writers created by the returned
// function share the same flate.Writer, so the sliding window (dictionary)
// is retained across messages as described in RFC 7692. The compression
// level of the first message is used for the lifetime of the connection.
//
// Note that the compress/flate compressor only retains the sliding window
// across flushes at compression levels 7 and higher, so lower levels are
// raised to 7.
func compressContextTakeover() func(io.WriteCloser, int) io.WriteCloser {
	c := &contextTakeoverCompressor{}
	return c.newWriter
}

// contextTakeoverCompressionLevel returns the flate compression level to
// use when context takeover is negotiated. The compress/flate compressor
// discards the sliding window at every flush for levels below 7, which
// would prevent context takeover from working.
func contextTakeoverCompressionLevel(level int) int {
	if level < 7 {
		return 7
	}
	return level
}

type contextTakeoverCompressor struct {
	fw *flate.Writer
	tw *truncWriter
}

func (c *contextTakeoverCompressor) newWriter(w io.WriteCloser, level int) io.WriteCloser {
	if c.fw == nil {
		c.tw = &truncWriter{w: w}
		c.fw, _ = flate.NewWriter(c.tw, contextTakeoverCompressionLevel(level))
	} else {
		// Redirect the output of the existing flate writer to the writer
		// for the new message and drop the four tail bytes buffered for
		// the previous message.
		c.tw.w = w
		c.tw.n = 0
	}
	return &flateContextTakeoverWriteWrapper{c: c}
}

// decompressContextTakeover returns a function that creates decompression
// readers for a single connection. The sliding window (dictionary) is
// retained across messages as described in RFC 7692 by seeding the flate
// reader for each message with the bytes decompressed from the previous
// messages.
func decompressContextTakeover() func(io.Reader) io.ReadCloser {
	d := &contextTakeoverDecompressor{}
	return d.newReader
}

type contextTakeoverDecompressor struct {
	dict []byte
}

func (d *contextTakeoverDecompressor) newReader(r io.Reader) io.ReadCloser {
	const tail =
	// Add four bytes as specified in RFC
	"\x00\x00\xff\xff" +
		// Add final block to squelch unexpected EOF error from flate reader.
		"\x01\x00\x00\xff\xff"

	fr, _ := flateReaderPool.Get().(io.ReadCloser)
	mr := io.MultiReader(r, strings.NewReader(tail))
	if err := fr.(flate.Resetter).Reset(mr, d.dict); err != nil {
		// Reset never fails, but handle error in case that changes.
		fr = flate.NewReaderDict(mr, d.dict)
	}
	return &flateContextTakeoverReadWrapper{d: d, fr: fr}
}

func (d *contextTakeoverDecompressor) appendDict(p []byte) {
	if len(p) >= maxContextTakeoverWindow {
		d.dict = append(d.dict[:0], p[len(p)-maxContextTakeoverWindow:]...)
		return
	}
	d.dict = append(d.dict, p...)
	if len(d.dict) > maxContextTakeoverWindow {
		copy(d.dict, d.dict[len(d.dict)-maxContextTakeoverWindow:])
		d.dict = d.dict[:maxContextTakeoverWindow]
	}
}

// truncWriter is an io.Writer that writes all but the last four bytes of the
// stream to another io.Writer.
type truncWriter struct {
	w io.WriteCloser
	n int
	p [4]byte
}

func (w *truncWriter) Write(p []byte) (int, error) {
	n := 0

	// fill buffer first for simplicity.
	if w.n < len(w.p) {
		n = copy(w.p[w.n:], p)
		p = p[n:]
		w.n += n
		if len(p) == 0 {
			return n, nil
		}
	}

	m := len(p)
	if m > len(w.p) {
		m = len(w.p)
	}

	if nn, err := w.w.Write(w.p[:m]); err != nil {
		return n + nn, err
	}

	copy(w.p[:], w.p[m:])
	copy(w.p[len(w.p)-m:], p[len(p)-m:])
	nn, err := w.w.Write(p[:len(p)-m])
	return n + nn, err
}

type flateWriteWrapper struct {
	fw *flate.Writer
	tw *truncWriter
	p  *sync.Pool
}

func (w *flateWriteWrapper) Write(p []byte) (int, error) {
	if w.fw == nil {
		return 0, errWriteClosed
	}
	return w.fw.Write(p)
}

func (w *flateWriteWrapper) Close() error {
	if w.fw == nil {
		return errWriteClosed
	}
	err1 := w.fw.Flush()
	w.p.Put(w.fw)
	w.fw = nil
	if w.tw.p != [4]byte{0, 0, 0xff, 0xff} {
		return errors.New("websocket: internal error, unexpected bytes at end of flate stream")
	}
	err2 := w.tw.w.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

type flateReadWrapper struct {
	fr io.ReadCloser
}

func (r *flateReadWrapper) Read(p []byte) (int, error) {
	if r.fr == nil {
		return 0, io.ErrClosedPipe
	}
	n, err := r.fr.Read(p)
	if err == io.EOF {
		// Preemptively place the reader back in the pool. This helps with
		// scenarios where the application does not call NextReader() soon after
		// this final read.
		r.Close()
	}
	return n, err
}

func (r *flateReadWrapper) Close() error {
	if r.fr == nil {
		return io.ErrClosedPipe
	}
	err := r.fr.Close()
	flateReaderPool.Put(r.fr)
	r.fr = nil
	return err
}

// flateContextTakeoverWriteWrapper is an io.WriteCloser for a single
// message that shares the flate writer in a contextTakeoverCompressor with
// the other messages on the connection.
type flateContextTakeoverWriteWrapper struct {
	c      *contextTakeoverCompressor
	closed bool
}

func (w *flateContextTakeoverWriteWrapper) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errWriteClosed
	}
	return w.c.fw.Write(p)
}

func (w *flateContextTakeoverWriteWrapper) Close() error {
	if w.closed {
		return errWriteClosed
	}
	w.closed = true
	// Flush the message data to the network, but keep the flate writer (and
	// its dictionary) for the next message.
	err1 := w.c.fw.Flush()
	if w.c.tw.p != [4]byte{0, 0, 0xff, 0xff} {
		return errors.New("websocket: internal error, unexpected bytes at end of flate stream")
	}
	err2 := w.c.tw.w.Close()
	w.c.tw.w = nil
	if err1 != nil {
		return err1
	}
	return err2
}

// flateContextTakeoverReadWrapper is an io.ReadCloser for a single message
// that records the decompressed bytes in a contextTakeoverDecompressor for
// use as the dictionary of the following messages.
type flateContextTakeoverReadWrapper struct {
	d  *contextTakeoverDecompressor
	fr io.ReadCloser
}

func (r *flateContextTakeoverReadWrapper) Read(p []byte) (int, error) {
	if r.fr == nil {
		return 0, io.ErrClosedPipe
	}
	n, err := r.fr.Read(p)
	if n > 0 {
		r.d.appendDict(p[:n])
	}
	if err == io.EOF {
		// Preemptively place the reader back in the pool. This helps with
		// scenarios where the application does not call NextReader() soon
		// after this final read.
		r.close()
	}
	return n, err
}

func (r *flateContextTakeoverReadWrapper) Close() error {
	if r.fr == nil {
		return io.ErrClosedPipe
	}
	// Read the remainder of the message so that the dictionary stays in
	// sync with the peer when the application does not read the message to
	// the end.
	var buf [256]byte
	for {
		n, err := r.fr.Read(buf[:])
		if n > 0 {
			r.d.appendDict(buf[:n])
		}
		if err != nil {
			break
		}
	}
	r.close()
	return nil
}

func (r *flateContextTakeoverReadWrapper) close() {
	_ = r.fr.Close()
	flateReaderPool.Put(r.fr)
	r.fr = nil
}
