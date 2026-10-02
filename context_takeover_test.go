// Copyright 2026 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"bytes"
	"compress/flate"
	"io"
	"math/rand"
	"testing"
)

func similarMessages() [][]byte {
	const tmpl = `{"event":"position","session":"abcdefghijklmnopqrstuvwxyz-0123456789","entity":"sensor-42","x":10000,"y":20000,"ts":1700000000000}`
	msgs := make([][]byte, 8)
	for i := range msgs {
		b := []byte(tmpl)
		b[len(b)-2] = byte('0' + i)
		msgs[i] = b
	}
	return msgs
}

// compressTakeover compresses every message through one persistent
// compressor and returns the wire payloads (sync tail stripped).
func compressTakeover(msgs [][]byte, level int) [][]byte {
	ct := newContextTakeoverCompressor()
	out := make([][]byte, len(msgs))
	for i, m := range msgs {
		var buf bytes.Buffer
		fw := &frameSinkCloser{&buf}
		w := ct.newWriter(fw, level)
		if _, err := w.Write(m); err != nil {
			panic(err)
		}
		if err := w.Close(); err != nil {
			panic(err)
		}
		out[i] = buf.Bytes()
	}
	return out
}

// compressStateless compresses each message with an independent fresh
// compressor (as a peer that negotiated no context takeover would).
func compressStateless(msgs [][]byte) [][]byte {
	out := make([][]byte, len(msgs))
	for i, m := range msgs {
		var buf bytes.Buffer
		fw := &frameSinkCloser{&buf}
		w := compressNoContextTakeover(fw, 1)
		if _, err := w.Write(m); err != nil {
			panic(err)
		}
		if err := w.Close(); err != nil {
			panic(err)
		}
		out[i] = buf.Bytes()
	}
	return out
}

type frameSinkCloser struct{ *bytes.Buffer }

func (f *frameSinkCloser) Close() error { return nil }

func TestContextTakeoverCompressionRatio(t *testing.T) {
	msgs := similarMessages()
	takeover := compressTakeover(msgs, 1)
	stateless := compressStateless(msgs)

	if len(takeover[1]) >= len(stateless[1]) {
		t.Fatalf("context takeover did not reduce size: %d >= %d", len(takeover[1]), len(stateless[1]))
	}
	// With a retained dictionary, later near-identical messages should be
	// dominated by the few changed bytes, not a new DEFLATE header.
	if got := len(takeover[len(takeover)-1]); got > 24 {
		t.Fatalf("last compressed message too long: %d bytes", got)
	}
	t.Logf("stateless=%d bytes, takeover first=%d last=%d",
		len(stateless[1]), len(takeover[0]), len(takeover[len(takeover)-1]))
}

func TestContextTakeoverRoundTrip(t *testing.T) {
	msgs := append(similarMessages(),
		nil, // empty message
		[]byte("totally different content after an empty message"),
		bytes.Repeat([]byte("Q"), 70000), // spans several 32KiB windows
		[]byte(`back to a small message`),
	)

	wire := compressTakeover(msgs, 1)
	d := newContextTakeoverDecompressor()
	for i, want := range msgs {
		r := d.newReader(bytes.NewReader(wire[i]))
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("message %d mismatch: got %d bytes, want %d bytes", i, len(got), len(want))
		}
		if err := r.Close(); err != nil {
			t.Fatalf("message %d close: %v", i, err)
		}
	}
}

// The first message of a context-takeover stream is self-contained and can
// be decoded by a receiver without context takeover. Later messages may refer
// back to earlier ones and must be rejected by a receiver that was told (via
// client_no_context_takeover / server_no_context_takeover) not to retain a
// dictionary. This asymmetry is why the direction parameters must be honored:
// a peer must never silently enable takeover.
func TestContextTakeoverFirstMessageDecodedByStatelessReceiver(t *testing.T) {
	msgs := similarMessages()
	wire := compressTakeover(msgs, 1)
	r := decompressNoContextTakeover(bytes.NewReader(wire[0]))
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, msgs[0]) {
		t.Fatalf("first message: %v", err)
	}
	r.Close()

	if len(wire) < 2 || len(wire[1]) >= len(wire[0]) {
		t.Fatalf("test setup: later message should depend on history")
	}
	r = decompressNoContextTakeover(bytes.NewReader(wire[1]))
	if _, err := io.ReadAll(r); err == nil {
		t.Fatalf("stateless receiver unexpectedly decoded a context-dependent message")
	}
}

// A context-takeover receiver must decode messages produced by a stateless
// sender and still update its dictionary with the decoded content.
func TestContextTakeoverReceiverAcceptsStatelessSender(t *testing.T) {
	msgs := similarMessages()
	wire := compressStateless(msgs)
	d := newContextTakeoverDecompressor()
	for i, want := range msgs {
		r := d.newReader(bytes.NewReader(wire[i]))
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("message %d mismatch", i)
		}
		r.Close()
	}
}

// Independent implementation: build a persistent flate.Writer/flate.Reader
// pair manually without using the package helper, then cross-decode with the
// package helpers in both directions.
func TestContextTakeoverCrossImplementation(t *testing.T) {
	msgs := similarMessages()

	// Independent compressor.
	var indWire [][]byte
	var out bytes.Buffer
	fw, _ := flate.NewWriter(&out, flate.DefaultCompression)
	for _, m := range msgs {
		out.Reset()
		if _, err := fw.Write(m); err != nil {
			t.Fatal(err)
		}
		if err := fw.Flush(); err != nil {
			t.Fatal(err)
		}
		b := append([]byte(nil), out.Bytes()...)
		indWire = append(indWire, b[:len(b)-4])
	}

	d := newContextTakeoverDecompressor()
	for i, want := range msgs {
		r := d.newReader(bytes.NewReader(indWire[i]))
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("independent sender message %d: got %q err %v", i, got, err)
		}
		r.Close()
	}

	// Independent receiver decoding the package compressor output.
	pkgWire := compressTakeover(msgs, 6)
	var hist []byte
	for i, want := range pkgWire {
		var raw bytes.Buffer
		mr := io.MultiReader(bytes.NewReader(want),
			bytes.NewReader([]byte{0x00, 0x00, 0xff, 0xff, 0x01, 0x00, 0x00, 0xff, 0xff}))
		fr := flate.NewReaderDict(mr, hist)
		if _, err := raw.ReadFrom(fr); err != nil {
			t.Fatalf("independent receiver message %d: %v", i, err)
		}
		if !bytes.Equal(raw.Bytes(), msgs[i]) {
			t.Fatalf("independent receiver message %d mismatch", i)
		}
		hist = append(hist, raw.Bytes()...)
		if len(hist) > maxDeflateWindow {
			hist = hist[len(hist)-maxDeflateWindow:]
		}
	}
}

// Compression dictionaries must not leak between connections. Two fresh
// receivers must each decode the first message of their respective streams
// without relying on history from the other.
func TestContextTakeoverNoLeakBetweenConnections(t *testing.T) {
	msgs := similarMessages()
	wire := compressTakeover(msgs, 1)

	d1 := newContextTakeoverDecompressor()
	r1 := d1.newReader(bytes.NewReader(wire[0]))
	got1, err := io.ReadAll(r1)
	if err != nil || !bytes.Equal(got1, msgs[0]) {
		t.Fatalf("first connection: %v", err)
	}

	// A brand new connection-level receiver starts with an empty dictionary.
	d2 := newContextTakeoverDecompressor()
	if len(d2.hist) != 0 {
		t.Fatalf("new decompressor carries history: %d bytes", len(d2.hist))
	}
	r2 := d2.newReader(bytes.NewReader(wire[0]))
	got2, err := io.ReadAll(r2)
	if err != nil || !bytes.Equal(got2, msgs[0]) {
		t.Fatalf("second connection decodes message 0: %v", err)
	}

	// A new compressor must also start with an empty dictionary: the first
	// wire message of two compressors must be identical.
	other := compressTakeover(msgs, 1)
	if !bytes.Equal(other[0], wire[0]) {
		t.Fatalf("first message differs between fresh compressors")
	}
}

// The connection's compression level is used when the persistent flate
// writer is created on the first message.
func TestContextTakeoverCompressionLevel(t *testing.T) {
	msg := bytes.Repeat([]byte("abcdefghij"), 50)
	loose := compressTakeover([][]byte{msg}, flate.HuffmanOnly)[0]
	best := compressTakeover([][]byte{msg}, flate.BestCompression)[0]
	if len(loose) <= len(best) {
		t.Fatalf("compression level ignored: huffman=%d best=%d", len(loose), len(best))
	}

	// Both remain decodable by a matching receiver.
	for i, wire := range [][]byte{loose, best} {
		d := newContextTakeoverDecompressor()
		r := d.newReader(bytes.NewReader(wire))
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("level %d decode: %v", i, err)
		}
	}
}

// Randomized round trip through a retained dictionary: random payloads of
// varied sizes (including binary data that does not compress) exercise
// window wrap and flushed-block boundaries.
func TestContextTakeoverRandomized(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	ct := newContextTakeoverCompressor()
	d := newContextTakeoverDecompressor()

	for i := 0; i < 60; i++ {
		var msg []byte
		switch i % 4 {
		case 0:
			msg = nil
		case 1:
			msg = make([]byte, rng.Intn(50000))
			rng.Read(msg)
		case 2:
			// Mostly repetitive with random deltas.
			base := bytes.Repeat([]byte("THE QUICK BROWN FOX JUMPS OVER THE LAZY DOG. "), 200)
			for j := 0; j < 5; j++ {
				base[rng.Intn(len(base))] = byte(rng.Intn(256))
			}
			msg = base
		case 3:
			msg = make([]byte, 40000+rng.Intn(40000))
			for j := range msg {
				msg[j] = byte(rng.Intn(4))
			}
		}

		var buf bytes.Buffer
		w := ct.newWriter(&frameSinkCloser{&buf}, flate.DefaultCompression)
		if _, err := w.Write(msg); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}

		r := d.newReader(bytes.NewReader(buf.Bytes()))
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("message %d (len=%d): err=%v match=%v", i, len(msg), err, bytes.Equal(got, msg))
		}
		if err := r.Close(); err != nil {
			t.Fatalf("reader close %d: %v", i, err)
		}
	}
}
