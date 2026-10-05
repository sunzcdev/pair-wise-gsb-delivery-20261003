package hyperloglog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math/rand"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// Compile-time checks that the Sketch participates in the streaming
// interfaces as intended.
var (
	_ io.WriterTo   = (*Sketch)(nil)
	_ io.ReaderFrom = (*Sketch)(nil)
)

func mustMarshalFrame(t *testing.T, sk *Sketch) []byte {
	t.Helper()
	payload, err := sk.MarshalBinary()
	require.NoError(t, err)
	return payload
}

// requireSketchesEquivalent compares two sketches by re-encoding them, which
// avoids depending on unexported scratch state (such as intmap capacity) that a
// round trip normalizes but reflect.DeepEqual does not.
func requireSketchesEquivalent(t *testing.T, want, got *Sketch) {
	t.Helper()
	require.Equal(t, mustMarshalFrame(t, want), mustMarshalFrame(t, got))
	require.Equal(t, want.Estimate(), got.Estimate())
}

func mustWriteFrame(t *testing.T, sk *Sketch) []byte {
	t.Helper()
	var buf bytes.Buffer
	n, err := sk.WriteTo(&buf)
	require.NoError(t, err)
	require.Equal(t, int64(buf.Len()), n, "WriteTo must report wire bytes")
	return buf.Bytes()
}

func roundTripFrame(t *testing.T, wire []byte) *Sketch {
	t.Helper()
	var got Sketch
	n, err := got.ReadFrom(bytes.NewReader(wire))
	require.NoError(t, err)
	require.Equal(t, int64(len(wire)), n)
	return &got
}

func frameTestSketches(t *testing.T) map[string]*Sketch {
	t.Helper()

	sparse, err := NewSketch(14, true)
	require.NoError(t, err)
	for range 50 {
		sparse.InsertHash(rand.Uint64())
	}
	// Move the tmp set into the compressed list so the encoded shape does not
	// depend on intmap's internal capacity, which differs after a round trip.
	sparse.mergeSparse()

	dense := New16NoSparse()
	for range 5000 {
		dense.InsertHash(rand.Uint64())
	}

	empty, err := NewSketch(4, true)
	require.NoError(t, err)

	return map[string]*Sketch{
		"sparse": sparse,
		"dense":  dense,
		"empty":  empty,
	}
}

func TestFrame_RoundTrip(t *testing.T) {
	for name, sk := range frameTestSketches(t) {
		t.Run(name, func(t *testing.T) {
			wire := mustWriteFrame(t, sk)
			got := roundTripFrame(t, wire)
			requireSketchesEquivalent(t, sk, got)
		})
	}
}

func TestFrame_HeaderLayout(t *testing.T) {
	sk, err := NewSketch(4, true)
	require.NoError(t, err)
	sk.tmpSet = makeSet(2)
	sk.tmpSet.add(26)
	sk.tmpSet.add(40)
	for i := 0; i < 10; i++ {
		sk.sparseList.Append(uint32(i+1) << 1)
	}

	payload := mustMarshalFrame(t, sk)
	wire := mustWriteFrame(t, sk)

	require.Equal(t, frameHeaderSize+len(payload), len(wire))
	require.Equal(t, frameMagic[:], wire[:frameMagicLen], "magic")
	require.Equal(t, frameVersion, wire[frameVersionPos], "outer version")
	require.Equal(t, uint32(len(payload)), binary.BigEndian.Uint32(wire[frameLengthPos:]), "length")
	require.Equal(t, crc32.ChecksumIEEE(payload), binary.BigEndian.Uint32(wire[frameChecksumPos:]), "checksum")
	require.Equal(t, payload, wire[frameHeaderSize:], "payload follows the header verbatim")
}

func TestFrame_TwoConcatenated(t *testing.T) {
	first, err := NewSketch(14, true)
	require.NoError(t, err)
	for range 40 {
		first.InsertHash(rand.Uint64())
	}
	first.mergeSparse()

	second, err := NewSketch(14, true)
	require.NoError(t, err)
	for range 60 {
		second.InsertHash(rand.Uint64())
	}
	second.mergeSparse()

	var stream bytes.Buffer
	n1, err := first.WriteTo(&stream)
	require.NoError(t, err)
	n2, err := second.WriteTo(&stream)
	require.NoError(t, err)

	r := bytes.NewReader(stream.Bytes())

	var gotFirst Sketch
	read1, err := gotFirst.ReadFrom(r)
	require.NoError(t, err)
	require.Equal(t, n1, read1, "first frame size")
	requireSketchesEquivalent(t, first, &gotFirst)

	var gotSecond Sketch
	read2, err := gotSecond.ReadFrom(r)
	require.NoError(t, err)
	require.Equal(t, n2, read2, "second frame size")
	requireSketchesEquivalent(t, second, &gotSecond)

	require.Equal(t, int64(stream.Len()), read1+read2, "both frames consumed")

	// A read at a clean stream end is io.EOF.
	_, err = (&Sketch{}).ReadFrom(r)
	require.ErrorIs(t, err, io.EOF)

	// The framed sketches merge exactly like the originals.
	var merged Sketch
	require.NoError(t, merged.Merge(&gotFirst))
	require.NoError(t, merged.Merge(&gotSecond))
	var want Sketch
	require.NoError(t, want.Merge(first))
	require.NoError(t, want.Merge(second))
	require.Equal(t, want.Estimate(), merged.Estimate())
}

// Two frames are concatenated and the stream is cut at every byte position.
// A cut with no second-frame bytes is a clean io.EOF; every other partial cut
// is a truncation. A partial frame must never surface as a checksum or
// bad-magic error.
func TestFrame_TruncationAtEveryPosition(t *testing.T) {
	first := New14()
	for i := range 10 {
		first.Insert([]byte("first-frame-payload-" + string(rune('a'+i)) + "-padding-padding-padding-padding"))
	}
	second := New16()
	for i := range 20 {
		second.Insert([]byte("second-frame-payload-" + string(rune('A'+i)) + "-padding-padding-padding-padding"))
	}

	var stream bytes.Buffer
	_, err := first.WriteTo(&stream)
	require.NoError(t, err)
	firstFrameLen := stream.Len()
	_, err = second.WriteTo(&stream)
	require.NoError(t, err)

	full := stream.Bytes()

	for cut := 0; cut < len(full); cut++ {
		prefix := full[:cut]
		r := bytes.NewReader(prefix)

		// First frame.
		var got Sketch
		seed := New14()
		seedEstimate := seed.Estimate()
		n1, err1 := got.ReadFrom(r)

		if cut < firstFrameLen {
			// Cut inside the first frame: a zero-length prefix is a clean
			// EOF, any other prefix is a truncation. The receiver is never
			// replaced in either case.
			if cut == 0 {
				require.ErrorIsf(t, err1, io.EOF, "cut=%d empty stream", cut)
			} else {
				require.ErrorIsf(t, err1, ErrorFrameTooShort, "cut=%d first frame", cut)
			}
			require.Equalf(t, int64(cut), n1, "cut=%d consumed bytes", cut)
			require.Equalf(t, seedEstimate, got.Estimate(), "cut=%d receiver replaced on error", cut)
			continue
		}

		// First frame is whole and must decode.
		require.NoErrorf(t, err1, "cut=%d first frame should decode", cut)
		require.Equalf(t, int64(firstFrameLen), n1, "cut=%d first frame size", cut)
		require.Equalf(t, first.Estimate(), got.Estimate(), "cut=%d first frame payload", cut)

		// Second frame is partial, so this is always a truncation.
		var got2 Sketch
		n2, err2 := got2.ReadFrom(r)
		if cut == firstFrameLen {
			// Exactly on the frame boundary: zero bytes of the second frame
			// are present, which reads as a clean end-of-stream.
			require.ErrorIsf(t, err2, io.EOF, "cut=%d second frame not started", cut)
		} else {
			require.ErrorIsf(t, err2, ErrorFrameTooShort, "cut=%d second frame", cut)
		}
		require.NotErrorIsf(t, err2, ErrorFrameChecksum, "cut=%d must not look like corruption", cut)
		require.Equalf(t, int64(cut-firstFrameLen), n2, "cut=%d second consumed bytes", cut)
		require.Zerof(t, got2.Estimate(), "cut=%d second receiver replaced on error", cut)
	}
}

// A single frame is also cut at every position, including the header bytes.
func TestFrame_SingleTruncationAtEveryPosition(t *testing.T) {
	wire := mustWriteFrame(t, frameTestSketches(t)["dense"])

	// Empty stream is a clean EOF, not a truncated frame.
	var empty Sketch
	_, err := empty.ReadFrom(bytes.NewReader(nil))
	require.ErrorIs(t, err, io.EOF)

	for cut := 1; cut < len(wire); cut++ {
		var got Sketch
		n, err := got.ReadFrom(bytes.NewReader(wire[:cut]))
		require.ErrorIsf(t, err, ErrorFrameTooShort, "cut=%d of %d", cut, len(wire))
		require.Equalf(t, int64(cut), n, "cut=%d consumed", cut)
		require.Zerof(t, got.Estimate(), "cut=%d receiver replaced on truncated frame", cut)
	}
}

func TestFrame_BadMagic(t *testing.T) {
	wire := mustWriteFrame(t, frameTestSketches(t)["dense"])
	wire[0] = 'X'

	var got Sketch
	n, err := got.ReadFrom(bytes.NewReader(wire))
	require.ErrorIs(t, err, ErrorBadMagic)
	require.NotErrorIs(t, err, ErrorFrameVersion)
	require.Equal(t, int64(frameHeaderSize), n, "header consumed before magic check")
	require.Zero(t, got.Estimate())

	// Raw, unframed inner encoding is rejected rather than misread: its first
	// byte is the inner version (1 or 2), not the frame magic.
	payload := mustMarshalFrame(t, frameTestSketches(t)["dense"])
	var unframed Sketch
	_, err = unframed.ReadFrom(bytes.NewReader(payload))
	require.ErrorIs(t, err, ErrorBadMagic)
}

func TestFrame_UnknownVersion(t *testing.T) {
	wire := mustWriteFrame(t, frameTestSketches(t)["dense"])
	wire[frameVersionPos] = frameVersion + 99

	var got Sketch
	_, err := got.ReadFrom(bytes.NewReader(wire))
	require.ErrorIs(t, err, ErrorFrameVersion)
	require.NotErrorIs(t, err, ErrorInvalidVersion, "outer version error is distinct from inner version error")
	require.Zero(t, got.Estimate())
}

func TestFrame_ChecksumMismatch(t *testing.T) {
	wire := mustWriteFrame(t, frameTestSketches(t)["dense"])

	// Flip a payload byte: the frame is complete and the right length, so this
	// is a checksum failure and not a truncation.
	corrupt := bytes.Clone(wire)
	corrupt[len(corrupt)-1] ^= 0xff
	var got Sketch
	n, err := got.ReadFrom(bytes.NewReader(corrupt))
	require.ErrorIs(t, err, ErrorFrameChecksum)
	require.NotErrorIs(t, err, ErrorFrameTooShort)
	require.Equal(t, int64(len(wire)), n, "whole corrupt frame consumed")
	require.Zero(t, got.Estimate(), "receiver not replaced on checksum failure")

	// Flipping the stored checksum is the same class of error.
	corrupt = bytes.Clone(wire)
	corrupt[frameChecksumPos] ^= 0xff
	var gotChecksum Sketch
	_, err = gotChecksum.ReadFrom(bytes.NewReader(corrupt))
	require.ErrorIs(t, err, ErrorFrameChecksum)
}

func TestFrame_TooLarge(t *testing.T) {
	wire := mustWriteFrame(t, frameTestSketches(t)["dense"])

	// Default limit, artificially tightened.
	t.Run("custom limit below declared length", func(t *testing.T) {
		payload, n, err := ReadFrame(bytes.NewReader(wire), 4)
		require.ErrorIs(t, err, ErrorFrameTooLarge)
		require.Nil(t, payload)
		require.Equal(t, int64(frameHeaderSize), n, "payload is not read")
	})

	// A declared length above the uint32/physical range still respects the
	// limit; craft a header claiming a huge payload.
	t.Run("declared length above default limit", func(t *testing.T) {
		var header [frameHeaderSize]byte
		copy(header[:frameMagicLen], frameMagic[:])
		header[frameVersionPos] = frameVersion
		binary.BigEndian.PutUint32(header[frameLengthPos:], DefaultMaxFramePayloadSize+1)

		_, _, err := ReadFrame(bytes.NewReader(header[:]), 0)
		require.ErrorIs(t, err, ErrorFrameTooLarge)
	})
}

// A frame whose checksum verifies but whose inner encoding is invalid must
// surface the inner error and leave the receiver alone.
func TestFrame_InvalidInnerPayload(t *testing.T) {
	wire := mustWriteFrame(t, frameTestSketches(t)["dense"])

	// Corrupt the inner version byte, then repair the checksum so the frame
	// itself validates and UnmarshalBinary does the rejecting.
	inner := bytes.Clone(wire)
	inner[frameHeaderSize] = 99
	binary.BigEndian.PutUint32(inner[frameChecksumPos:], crc32.ChecksumIEEE(inner[frameHeaderSize:]))

	seed := New16()
	seedEstimate := seed.Estimate()
	n, err := seed.ReadFrom(bytes.NewReader(inner))
	require.ErrorIs(t, err, ErrorInvalidVersion)
	require.NotErrorIs(t, err, ErrorFrameChecksum)
	require.Equal(t, int64(len(wire)), n)
	require.Equal(t, seedEstimate, seed.Estimate(), "receiver unchanged on inner error")
}

// Historical inner encodings keep parsing inside a frame.
func TestFrame_InnerVersion1(t *testing.T) {
	// Precision 4, version 1 dense payload: 8 header bytes plus m/2 = 8 packed
	// register bytes (see the v1 dense fixtures in hyperloglog_test.go).
	v1Payload := []byte{
		0x01, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}

	var buf bytes.Buffer
	n, err := WriteFrame(&buf, v1Payload)
	require.NoError(t, err)
	require.Equal(t, int64(frameHeaderSize+len(v1Payload)), n)

	var got Sketch
	_, err = got.ReadFrom(&buf)
	require.NoError(t, err)
	require.Equal(t, uint8(4), got.p)
	require.Len(t, got.regs, 16)

	// Version 1 sparse control encoding also round-trips inside a frame.
	v1Sparse := []byte{
		0x01, 0x0e, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x01, 0x00,
	}
	buf.Reset()
	_, err = WriteFrame(&buf, v1Sparse)
	require.NoError(t, err)
	var gotSparse Sketch
	_, err = gotSparse.ReadFrom(&buf)
	require.NoError(t, err)
	require.True(t, gotSparse.sparse())
}

func TestFrame_ZeroValueSketch(t *testing.T) {
	var sk Sketch
	var buf bytes.Buffer
	_, err := sk.WriteTo(&buf)
	require.Error(t, err)
	require.Zero(t, buf.Len(), "nothing written when marshalling fails")
}

func TestFrame_WriteFrameEmptyPayload(t *testing.T) {
	var buf bytes.Buffer
	n, err := WriteFrame(&buf, nil)
	require.NoError(t, err)
	require.Equal(t, int64(frameHeaderSize), n)

	payload, rn, err := ReadFrame(&buf, 0)
	require.NoError(t, err)
	require.Equal(t, int64(frameHeaderSize), rn)
	require.Empty(t, payload)
}

// limitedWriter accepts at most perWrite bytes per Write and fails once total
// written reaches failAt with a sentinel, to exercise short writes and
// mid-write errors.
type limitedWriter struct {
	buf       bytes.Buffer
	perWrite  int
	written   int
	failAt    int
	failCalls int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.perWrite > 0 && len(p) > w.perWrite {
		p = p[:w.perWrite]
	}
	if w.failAt > 0 && w.written+len(p) > w.failAt {
		p = p[:max(0, w.failAt-w.written)]
	}
	n, _ := w.buf.Write(p)
	w.written += n
	if w.failAt > 0 && w.written >= w.failAt {
		w.failCalls++
		return n, errFrameWriterStopped
	}
	return n, nil
}

var errFrameWriterStopped = errors.New("writer stopped")

// A Writer that accepts fewer bytes than offered must be re-driven until the
// whole frame is written; WriteTo reports the complete wire byte count.
func TestFrame_ShortWrite(t *testing.T) {
	sk := frameTestSketches(t)["dense"]

	for _, perWrite := range []int{1, 3, 7, frameHeaderSize - 1, frameHeaderSize + 1} {
		w := &limitedWriter{perWrite: perWrite}
		n, err := sk.WriteTo(w)
		require.NoErrorf(t, err, "perWrite=%d", perWrite)

		var got Sketch
		_, err = got.ReadFrom(bytes.NewReader(w.buf.Bytes()))
		require.NoErrorf(t, err, "perWrite=%d frame should decode", perWrite)
		require.Truef(t, reflect.DeepEqual(&got, sk), "perWrite=%d payload mismatch", perWrite)
		require.Equalf(t, int64(w.written), n, "perWrite=%d wire bytes", perWrite)
	}
}

func TestFrame_MidWriteError(t *testing.T) {
	sk := frameTestSketches(t)["dense"]

	for _, failAt := range []int{1, frameHeaderSize - 1, frameHeaderSize, frameHeaderSize + 1} {
		w := &limitedWriter{perWrite: 3, failAt: failAt}
		n, err := sk.WriteTo(w)
		require.ErrorIsf(t, err, errFrameWriterStopped, "failAt=%d", failAt)
		require.Equalf(t, int64(failAt), n, "failAt=%d bytes reached the writer", failAt)
		require.Equal(t, 1, w.failCalls)
	}
}
