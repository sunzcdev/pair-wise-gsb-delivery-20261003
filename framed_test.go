package hyperloglog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func framedSketch(t *testing.T, sparse bool, n int) *Sketch {
	t.Helper()
	sk := New16()
	if !sparse {
		sk = New16NoSparse()
	}
	for i := 0; i < n; i++ {
		sk.InsertHash(uint64(i*2654435761 + 1))
	}
	return sk
}

func TestFrame_RoundTrip(t *testing.T) {
	for _, sparse := range []bool{true, false} {
		src := framedSketch(t, sparse, 1000)
		var buf bytes.Buffer

		n, err := src.WriteTo(&buf)
		require.NoError(t, err)
		require.EqualValues(t, buf.Len(), n)
		require.Equal(t, "HLLF", string(buf.Bytes()[0:4]))

		var dst Sketch
		m, err := dst.ReadFrom(&buf)
		require.NoError(t, err)
		require.EqualValues(t, n, m)
		require.Zero(t, buf.Len())
		require.Equal(t, src.Estimate(), dst.Estimate())
	}
}

func TestFrame_Concatenated(t *testing.T) {
	first := framedSketch(t, true, 500)
	second := framedSketch(t, false, 5000)

	var buf bytes.Buffer
	n1, err := first.WriteTo(&buf)
	require.NoError(t, err)
	n2, err := second.WriteTo(&buf)
	require.NoError(t, err)

	var got1, got2 Sketch
	m1, err := got1.ReadFrom(&buf)
	require.NoError(t, err)
	require.EqualValues(t, n1, m1)
	m2, err := got2.ReadFrom(&buf)
	require.NoError(t, err)
	require.EqualValues(t, n2, m2)

	require.Equal(t, first.Estimate(), got1.Estimate())
	require.Equal(t, second.Estimate(), got2.Estimate())

	// A clean end of stream is reported as io.EOF, not as a truncated frame.
	var extra Sketch
	_, err = extra.ReadFrom(&buf)
	require.ErrorIs(t, err, io.EOF)
}

func TestFrame_TruncatedAtEveryPosition(t *testing.T) {
	src := framedSketch(t, true, 500)
	var buf bytes.Buffer
	n, err := src.WriteTo(&buf)
	require.NoError(t, err)
	frame := buf.Bytes()

	for cut := 0; cut < len(frame); cut++ {
		dst := framedSketch(t, false, 100)
		before := dst.Estimate()

		consumed, err := dst.ReadFrom(bytes.NewReader(frame[:cut]))
		if cut == 0 {
			require.ErrorIs(t, err, io.EOF)
			require.Zero(t, consumed)
		} else {
			require.ErrorIs(t, err, ErrorTruncatedFrame, "cut at %d of %d", cut, n)
			require.EqualValues(t, cut, consumed)
		}
		require.Equal(t, before, dst.Estimate(), "cut at %d must not modify the target", cut)
	}
}

func TestFrame_ErrorsAreDistinct(t *testing.T) {
	src := framedSketch(t, true, 500)
	var buf bytes.Buffer
	_, err := src.WriteTo(&buf)
	require.NoError(t, err)
	frame := buf.Bytes()

	corrupt := func(mutate func([]byte)) []byte {
		c := bytes.Clone(frame)
		mutate(c)
		return c
	}

	var sk Sketch

	_, err = sk.ReadFrom(bytes.NewReader(corrupt(func(c []byte) { c[0] ^= 0xff })))
	require.ErrorIs(t, err, ErrorBadMagic)

	_, err = sk.ReadFrom(bytes.NewReader(corrupt(func(c []byte) { c[4] = 99 })))
	require.ErrorIs(t, err, ErrorUnknownFrameVersion)

	_, err = sk.ReadFrom(bytes.NewReader(corrupt(func(c []byte) {
		binary.BigEndian.PutUint32(c[5:9], uint32(MaxFramePayloadSize)+1)
	})))
	require.ErrorIs(t, err, ErrorFrameTooLarge)

	// Flip one payload bit: the frame is complete but the checksum fails.
	_, err = sk.ReadFrom(bytes.NewReader(corrupt(func(c []byte) { c[len(c)-1] ^= 0x01 })))
	require.ErrorIs(t, err, ErrorChecksumMismatch)

	// A forged checksum that matches the corrupted payload decodes again:
	// CRC-32 detects accidents, it does not authenticate.
	dense := framedSketch(t, false, 500)
	var dbuf bytes.Buffer
	_, err = dense.WriteTo(&dbuf)
	require.NoError(t, err)
	forged := dbuf.Bytes()
	// Flip one dense register to a different valid value, then fix the
	// checksum so the tampered frame is accepted.
	forged[frameHeaderLen+8] ^= 0x01
	binary.BigEndian.PutUint32(forged[9:13], crc32.ChecksumIEEE(forged[frameHeaderLen:]))
	_, err = sk.ReadFrom(bytes.NewReader(forged))
	require.NoError(t, err)
}

func TestFrame_ChecksumFailureLeavesTargetUnchanged(t *testing.T) {
	src := framedSketch(t, true, 500)
	var buf bytes.Buffer
	_, err := src.WriteTo(&buf)
	require.NoError(t, err)
	frame := buf.Bytes()
	frame[len(frame)-1] ^= 0x01

	dst := framedSketch(t, false, 100)
	before := dst.Estimate()
	_, err = dst.ReadFrom(bytes.NewReader(frame))
	require.ErrorIs(t, err, ErrorChecksumMismatch)
	require.Equal(t, before, dst.Estimate())
}

// shortWriter accepts at most max bytes per Write call.
type shortWriter struct {
	max int
	buf bytes.Buffer
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > w.max {
		p = p[:w.max]
	}
	return w.buf.Write(p)
}

func TestFrame_WriteToShortWrites(t *testing.T) {
	src := framedSketch(t, false, 2000)

	var reference bytes.Buffer
	want, err := src.WriteTo(&reference)
	require.NoError(t, err)

	for _, max := range []int{1, 3, frameHeaderLen, 4096} {
		w := &shortWriter{max: max}
		n, err := src.WriteTo(w)
		require.NoError(t, err)
		require.EqualValues(t, want, n, "max write size %d", max)
		require.Equal(t, reference.Bytes(), w.buf.Bytes(), "max write size %d", max)
	}
}

// failAfterWriter fails once remaining bytes have been written.
type failAfterWriter struct {
	remaining int
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		p = p[:w.remaining]
	}
	w.remaining -= len(p)
	if w.remaining == 0 {
		return len(p), errors.New("boom")
	}
	return len(p), nil
}

func TestFrame_WriteToErrorStopsEarly(t *testing.T) {
	src := framedSketch(t, true, 10)
	n, err := src.WriteTo(&failAfterWriter{remaining: 7})
	require.Error(t, err)
	require.EqualValues(t, 7, n)
}
