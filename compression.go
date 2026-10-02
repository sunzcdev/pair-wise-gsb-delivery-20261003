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

	// maxDeflateWindow is the LZ77 sliding window size defined in RFC 7692
	// for permessage-deflate (server_max_window_bits / client_max_window_bits
	// default to 15, that is 2^15 bytes).
	maxDeflateWindow = 1 << 15
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

// contextTakeoverCompressor keeps a single flate writer alive for the lifetime
// of a connection so that the LZ77 dictionary is retained between messages.
// Each message is terminated with a sync flush (00 00 ff ff). The four sync
// bytes are withheld by the tail stripper and never hit the wire, as required
// by RFC 7692 section 7.2.1.
type contextTakeoverCompressor struct {
	fw *flate.Writer
	tw *takeoverTailWriter
}

func newContextTakeoverCompressor() *contextTakeoverCompressor {
	return &contextTakeoverCompressor{}
}

// newWriter returns the compression writer for one message. The flate writer
// is created lazily (with the connection's current compression level) and is
// reused for every message. Unlike compressNoContextTakeover, the writer is
// never returned to a pool and is never Reset because doing either would drop
// the retained dictionary.
func (ct *contextTakeoverCompressor) newWriter(w io.WriteCloser, level int) io.WriteCloser {
	if ct.tw == nil {
		ct.tw = &takeoverTailWriter{}
	}
	ct.tw.reset(w)
	if ct.fw == nil {
		if !isValidCompressionLevel(level) {
			level = defaultCompressionLevel
		}
		fw, err := flate.NewWriter(ct.tw, level)
		if err != nil {
			// NewWriter only fails for an invalid level. The level is
			// validated through SetCompressionLevel, but fall back to the
			// default instead of failing the first message.
			fw, _ = flate.NewWriter(ct.tw, defaultCompressionLevel)
		}
		ct.fw = fw
	}
	return &takeoverFlateWriteWrapper{fw: ct.fw, tw: ct.tw}
}

// takeoverTailWriter passes every byte through except for the four sync flush
// bytes (00 00 ff ff) that terminate each message. reset starts a new message
// and points the writer at the message's frame writer.
type takeoverTailWriter struct {
	w io.WriteCloser
	n int
	p [4]byte
}

func (w *takeoverTailWriter) reset(dst io.WriteCloser) {
	w.w = dst
	w.n = 0
}

func (w *takeoverTailWriter) Write(p []byte) (int, error) {
	n := 0

	// Keep the most recent four bytes in the holdback buffer so that the sync
	// marker is never forwarded to the frame.
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

type takeoverFlateWriteWrapper struct {
	fw     *flate.Writer
	tw     *takeoverTailWriter
	closed bool
}

func (w *takeoverFlateWriteWrapper) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errWriteClosed
	}
	return w.fw.Write(p)
}

func (w *takeoverFlateWriteWrapper) Close() error {
	if w.closed {
		return errWriteClosed
	}
	w.closed = true
	err1 := w.fw.Flush()
	// Do not close or reset w.fw: its dictionary is retained for the next
	// message.
	if w.tw.p != [4]byte{0, 0, 0xff, 0xff} {
		return errors.New("websocket: internal error, unexpected bytes at end of flate stream")
	}
	dst := w.tw.w
	w.tw.w = nil
	err2 := dst.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// contextTakeoverDecompressor is created once per connection for the receive
// direction that negotiated context takeover. The receiver's LZ77 dictionary
// cannot be inspected inside compress/flate, so every message is decoded with
// a fresh flate reader initialized with the most recent 32KiB of previously
// decompressed message data (flate.NewReaderDict).
type contextTakeoverDecompressor struct {
	hist []byte
}

func newContextTakeoverDecompressor() *contextTakeoverDecompressor {
	return &contextTakeoverDecompressor{}
}

func (d *contextTakeoverDecompressor) newReader(r io.Reader) io.ReadCloser {
	const tail =
	// RFC 7692 section 7.2.2: append the sync flush bytes that are stripped
	// from the wire.
	"\x00\x00\xff\xff" +
		// Add a final empty block so the flate reader surfaces io.EOF after
		// exactly one message without consuming bytes of the next message.
		"\x01\x00\x00\xff\xff"

	fr := flate.NewReaderDict(io.MultiReader(r, strings.NewReader(tail)), d.hist)
	return &takeoverFlateReadWrapper{fr: fr, d: d}
}

// appendHistory retains the most recent 32KiB of decompressed data. The
// dictionary handed to future flate readers must never exceed the negotiated
// window size.
func (d *contextTakeoverDecompressor) appendHistory(p []byte) {
	d.hist = append(d.hist, p...)
	if len(d.hist) > maxDeflateWindow {
		d.hist = d.hist[len(d.hist)-maxDeflateWindow:]
	}
}

type takeoverFlateReadWrapper struct {
	fr     io.ReadCloser
	d      *contextTakeoverDecompressor
	closed bool
}

func (r *takeoverFlateReadWrapper) Read(p []byte) (int, error) {
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	n, err := r.fr.Read(p)
	if n > 0 {
		r.d.appendHistory(p[:n])
	}
	return n, err
}

func (r *takeoverFlateReadWrapper) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	// Release the per-message reader. The retained dictionary lives on the
	// connection-level decompressor and is untouched here.
	return r.fr.Close()
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
