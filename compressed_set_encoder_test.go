package ksuid

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"
)

func TestCompressedSetEncoder(t *testing.T) {
	tests := []struct {
		scenario string
		function func(*testing.T)
	}{
		{
			scenario: "every Add partition produces the sorted deduped set",
			function: testCompressedSetEncoderPartitions,
		},
		{
			scenario: "every write byte boundary is retried or recovered",
			function: testCompressedSetEncoderChunkedWrites,
		},
		{
			scenario: "a large stream survives every single cut and chunk size",
			function: testCompressedSetEncoderLargeStream,
		},
		{
			scenario: "a failure at every byte boundary is sticky and leaves no usable set",
			function: testCompressedSetEncoderWriteFailures,
		},
		{
			scenario: "short writes follow the io.Writer contract",
			function: testCompressedSetEncoderShortWrites,
		},
		{
			scenario: "empty input has no output and a repeatable Finish",
			function: testCompressedSetEncoderEmpty,
		},
		{
			scenario: "duplicates are ignored",
			function: testCompressedSetEncoderDuplicates,
		},
		{
			scenario: "an out-of-order id fails when it is consumed",
			function: testCompressedSetEncoderOutOfOrder,
		},
		{
			scenario: "Finish flushes the tail once and never closes the writer",
			function: testCompressedSetEncoderFinish,
		},
		{
			scenario: "a ksuid.Sequence stream matches the batch encoder",
			function: testCompressedSetEncoderSequence,
		},
	}

	for _, test := range tests {
		t.Run(test.scenario, test.function)
	}
}

// idAt builds a KSUID from its timestamp and 128-bit payload; the payload
// arguments are the high and low 64 bits, in that order.
func idAt(ts uint32, high, low uint64) KSUID {
	return makeUint128(high, low).ksuid(ts)
}

// encoderGroundTruth returns a sorted set of unique KSUIDs exercising every
// record type and varint width of the format:
//   - payload runs (payloadRange), including a run of a single id,
//   - one- and eight-byte payload deltas (payloadDelta),
//   - a payload delta crossing the high 64 bits (nine-byte varint),
//   - one-, three-, and four-byte time deltas (timeDelta),
//   - a time change on an equal payload.
func encoderGroundTruth() []KSUID {
	return []KSUID{
		idAt(1000, 0, 100),
		idAt(1000, 0, 101),
		idAt(1000, 0, 102),
		idAt(1000, 0, 110),
		idAt(1000, 0, 111),
		idAt(1000, 0, 200),
		idAt(1000, 2, 0),
		idAt(1000, 2, 1),
		idAt(1255, 1, 1),
		idAt(1256, 1, 5),
		idAt(70000, 1, 5),
		idAt(16847216, 1, 5),
		idAt(16847217, 1, 6),
		idAt(16847217, 1, 8),
	}
}

// encoderPartitionStream is the ground truth with duplicates interleaved,
// still sorted.
func encoderPartitionStream() []KSUID {
	u := encoderGroundTruth()
	return []KSUID{
		u[0], u[1], u[1], u[2],
		u[3], u[4], u[4],
		u[5], u[6], u[7],
		u[8], u[9], u[10], u[11],
		u[11], u[12], u[13],
	}
}

// encodeWithPartitions feeds stream to a fresh encoder, splitting the
// variadic Add calls at every internal cut selected by mask: cut k is taken
// when bit k of mask is set.
func encodeWithPartitions(t *testing.T, w io.Writer, stream []KSUID, mask uint64) *CompressedSetEncoder {
	t.Helper()
	e := NewCompressedSetEncoder(w)
	start := 0
	for i := 0; i < len(stream); i++ {
		if i == len(stream)-1 || mask&(uint64(1)<<uint(i)) != 0 {
			if err := e.Add(stream[start : i+1]...); err != nil {
				t.Fatalf("Add(%d:%d) failed: %v", start, i+1, err)
			}
			start = i + 1
		}
	}
	if err := e.Finish(); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	return e
}

// decodeSet decodes a compressed set defensively: the iterator panics or
// spins forever on some malformed/truncated records, so a recovery guard and
// an iteration cap are used. ok reports whether the stream ended cleanly
// without overflowing the cap.
func decodeSet(t *testing.T, b []byte, capN int) (ids []KSUID, ok bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()
	it := CompressedSet(b).Iter()
	for it.Next() {
		if len(ids) >= capN {
			return ids, false
		}
		ids = append(ids, it.KSUID)
	}
	return ids, true
}

// recordBoundaries returns the offsets at which top-level records end in a
// well-formed compressed set. The format is self-framing, so a prefix cut at
// a boundary decodes cleanly while any other cut is a partial record.
func recordBoundaries(t *testing.T, b []byte) []int {
	t.Helper()
	out := []int{0}
	for off := 0; off < len(b); {
		tag := int(b[off]) & (rawKSUID | timeDelta | payloadDelta | payloadRange)
		cnt := int(b[off]) & ^(rawKSUID | timeDelta | payloadDelta | payloadRange)
		off++
		switch tag {
		case rawKSUID:
			off += byteLength
		case timeDelta:
			off += cnt + payloadLengthInBytes
		case payloadDelta:
			off += cnt
		case payloadRange:
			off += cnt
		default:
			t.Fatalf("unknown tag 0x%02x at offset %d", b[off-1], off-1)
		}
		out = append(out, off)
	}
	return out
}

// chunkWriter accepts at most max bytes per Write call, returning a legal
// short write (n < len(p), err == nil) to force the encoder retry loop.
type chunkWriter struct {
	buf bytes.Buffer
	max int
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.max > 0 && n > w.max {
		n = w.max
	}
	w.buf.Write(p[:n])
	return n, nil
}

// failWriter accepts bytes until exactly failAfter bytes have been written,
// then fails permanently. When partial is true the record in progress is
// partially accepted with the error; otherwise the whole failing Write is
// rejected. Calls made after the failure are counted in callsAfterFailure.
type failWriter struct {
	failAfter int
	partial   bool
	err       error

	accepted          int
	failed            bool
	callsAfterFailure int
}

func (w *failWriter) Write(p []byte) (int, error) {
	if w.failed {
		w.callsAfterFailure++
		return 0, w.err
	}

	remaining := w.failAfter - w.accepted
	if remaining >= len(p) {
		w.accepted += len(p)
		return len(p), nil
	}

	w.failed = true
	if w.partial && remaining > 0 {
		w.accepted += remaining
		return remaining, w.err
	}
	return 0, w.err
}

// closeCheckWriter wraps a buffer and records whether Close was called; the
// encoder must leave the lifetime of the underlying writer to its owner.
type closeCheckWriter struct {
	bytes.Buffer
	closed bool
}

func (w *closeCheckWriter) Close() error {
	w.closed = true
	return nil
}

func testCompressedSetEncoderPartitions(t *testing.T) {
	stream := encoderPartitionStream()
	want := []byte(AppendCompressed(nil, encoderGroundTruth()...))

	// The stream has fewer than 64 ids, so every one of the 2^(n-1)
	// partitions into Add batches is enumerated.
	if len(stream) >= 64 {
		t.Fatalf("test stream too large for exhaustive partitioning: %d", len(stream))
	}

	for mask := uint64(0); mask < uint64(1)<<uint(len(stream)-1); mask++ {
		var buf bytes.Buffer
		encodeWithPartitions(t, &buf, stream, mask)
		if !bytes.Equal(buf.Bytes(), want) {
			t.Fatalf("partition %0*b: bytes differ from AppendCompressed\n got: %x\nwant: %x",
				len(stream)-1, mask, buf.Bytes(), want)
		}

		got, ok := decodeSet(t, buf.Bytes(), len(want)+1)
		if !ok {
			t.Fatalf("partition %0*b: output did not decode cleanly", len(stream)-1, mask)
		}
		if !reflect.DeepEqual(got, encoderGroundTruth()) {
			t.Fatalf("partition %0*b: decoded set is not the sorted deduped input", len(stream)-1, mask)
		}
	}
}

func testCompressedSetEncoderChunkedWrites(t *testing.T) {
	stream := encoderPartitionStream()
	want := []byte(AppendCompressed(nil, encoderGroundTruth()...))

	for _, max := range []int{1, 2, 3, 5, 7, 13, 64} {
		for mask := uint64(0); mask < uint64(1)<<uint(len(stream)-1); mask++ {
			w := &chunkWriter{max: max}
			encodeWithPartitions(t, w, stream, mask)
			if !bytes.Equal(w.buf.Bytes(), want) {
				t.Fatalf("chunk=%d partition %0*b: bytes differ from AppendCompressed",
					max, len(stream)-1, mask)
			}
		}
	}

	// An unlimited writer still produces identical output.
	w := &chunkWriter{max: 0}
	encodeWithPartitions(t, w, stream, 0)
	if !bytes.Equal(w.buf.Bytes(), want) {
		t.Fatalf("unlimited writer: bytes differ from AppendCompressed")
	}
}

func testCompressedSetEncoderLargeStream(t *testing.T) {
	// Build a deterministic sorted stream with runs, sparse deltas, time
	// changes, varint boundaries, and interleaved duplicates.
	var unique []KSUID
	var stream []KSUID

	ts := uint32(1000000)
	v := makeUint128(0, 1)

	for group := 0; group < 24; group++ {
		if group%6 == 5 {
			ts += 1 << uint(2*(group/6)) // 1, 4, 16, 64 ... second steps
		}

		switch group % 4 {
		case 0: // run of consecutive payloads
			for k := 0; k < 4; k++ {
				v = incr128(v)
				unique = append(unique, v.ksuid(ts))
				stream = append(stream, unique[len(unique)-1])
			}
		case 1: // sparse payload delta
			v = add128(v, makeUint128(0, uint64(100+group)))
			unique = append(unique, v.ksuid(ts))
			stream = append(stream, unique[len(unique)-1], unique[len(unique)-1]) // adjacent dupe
		case 2: // cross the high 64 bits: 9-byte payload varint
			v = makeUint128(uint64(group), 0)
			unique = append(unique, v.ksuid(ts))
			stream = append(stream, unique[len(unique)-1])
			v = add128(v, makeUint128(0, 1))
			unique = append(unique, v.ksuid(ts))
			stream = append(stream, unique[len(unique)-1])
		case 3: // same payload, new timestamp
			ts += uint32(group) + 1
			unique = append(unique, v.ksuid(ts))
			stream = append(stream, unique[len(unique)-1])
		}
	}

	want := []byte(AppendCompressed(nil, unique...))

	// Every single cut of the Add stream, one id per call as the extreme,
	// plus the one-shot Add.
	w := &chunkWriter{max: 1}
	e := NewCompressedSetEncoder(w)
	for i := range stream {
		if err := e.Add(stream[i]); err != nil {
			t.Fatalf("Add(%d) failed: %v", i, err)
		}
	}
	if err := e.Finish(); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	if !bytes.Equal(w.buf.Bytes(), want) {
		t.Fatalf("one id per Add: bytes differ from AppendCompressed\n got: %x\nwant: %x", w.buf.Bytes(), want)
	}

	var one bytes.Buffer
	oneShot := NewCompressedSetEncoder(&one)
	if err := oneShot.Add(stream...); err != nil {
		t.Fatalf("Add(all) failed: %v", err)
	}
	if err := oneShot.Finish(); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	if !bytes.Equal(one.Bytes(), want) {
		t.Fatalf("one-shot Add: bytes differ from AppendCompressed")
	}

	got, ok := decodeSet(t, one.Bytes(), len(unique)+1)
	if !ok || !reflect.DeepEqual(got, unique) {
		t.Fatalf("decoded large stream is not the sorted deduped input (ok=%v, got %d ids want %d)",
			ok, len(got), len(unique))
	}
}

func testCompressedSetEncoderWriteFailures(t *testing.T) {
	stream := encoderPartitionStream()
	full := []byte(AppendCompressed(nil, encoderGroundTruth()...))

	// For every byte boundary, and for both whole-record rejection and
	// mid-record partial acceptance, the encoder must return the writer's
	// error, go sticky, freeze output, and leave a prefix that can never be
	// consumed as the complete set.
	for failAfter := 0; failAfter < len(full); failAfter++ {
		for _, partial := range []bool{false, true} {
			w := &failWriter{
				failAfter: failAfter,
				partial:   partial,
				err:       errors.New("synthetic write failure"),
			}

			e := NewCompressedSetEncoder(w)
			prefix := full[:failAfter]
			err := e.Add(stream...)
			if err == nil {
				err = e.Finish()
			}
			if err != w.err {
				t.Fatalf("failAfter=%d partial=%v: expected synthetic error, got %v",
					failAfter, partial, err)
			}

			// Nothing past the failure point must have been produced. A
			// partial-acceptance failure lands exactly on failAfter; a
			// whole-record rejection lands on the record boundary at or
			// before failAfter.
			wantAccepted := failAfter
			if !partial {
				for _, b := range recordBoundaries(t, full) {
					if b <= failAfter {
						wantAccepted = b
					}
				}
			}
			if w.accepted != wantAccepted {
				t.Fatalf("failAfter=%d partial=%v: accepted %d bytes, want %d",
					failAfter, partial, w.accepted, wantAccepted)
			}

			// The retained prefix is itself a prefix of the final set.
			if !bytes.Equal(full[:w.accepted], prefix[:w.accepted]) {
				t.Fatalf("failAfter=%d partial=%v: output is not a prefix of the set",
					failAfter, partial)
			}

			// Sticky failure: later calls return the same error and the
			// writer is never touched again.
			if err := e.Add(stream[0]); err != w.err {
				t.Fatalf("failAfter=%d: Add after failure returned %v", failAfter, err)
			}
			if err := e.Finish(); err != w.err {
				t.Fatalf("failAfter=%d: Finish after failure returned %v", failAfter, err)
			}
			if w.callsAfterFailure != 0 {
				t.Fatalf("failAfter=%d: encoder called Write %d times after the sticky failure",
					failAfter, w.callsAfterFailure)
			}

			// The bytes already on the wire cannot be used as the complete
			// set: decoding either fails on a partial record or yields a
			// strict, unannounced subset.
			ids, clean := decodeSet(t, prefix, len(full)+1)
			if clean && reflect.DeepEqual(ids, encoderGroundTruth()) {
				t.Fatalf("failAfter=%d: truncated output decodes as the complete set", failAfter)
			}
		}
	}
}

// oneShortWriter behaves as an io.Writer that makes one short, error-free
// write per shortAfter accepted bytes (modeShort) or always accepts zero
// bytes without an error (modeZero), which the io.Writer contract treats as
// io.ErrShortWrite.
type oneShortWriter struct {
	buf       bytes.Buffer
	mode      int
	shortAt   int
	accepted  int
	shorted   bool
	writeCall int
}

const (
	oneShortRecover = iota
	oneShortZero
)

func (w *oneShortWriter) Write(p []byte) (int, error) {
	w.writeCall++
	if w.mode == oneShortZero {
		return 0, nil
	}
	if !w.shorted && w.accepted >= w.shortAt {
		w.shorted = true
		if len(p) > 1 {
			w.buf.Write(p[:1])
			w.accepted++
			return 1, nil
		}
	}
	w.buf.Write(p)
	w.accepted += len(p)
	return len(p), nil
}

func testCompressedSetEncoderShortWrites(t *testing.T) {
	stream := encoderPartitionStream()
	want := []byte(AppendCompressed(nil, encoderGroundTruth()...))

	// A single error-free short write at every byte boundary is retried from
	// the unwritten suffix and the final set is unchanged.
	for at := 0; at < len(want); at++ {
		w := &oneShortWriter{mode: oneShortRecover, shortAt: at}
		e := NewCompressedSetEncoder(w)
		if err := e.Add(stream...); err != nil {
			t.Fatalf("at=%d: Add failed: %v", at, err)
		}
		if err := e.Finish(); err != nil {
			t.Fatalf("at=%d: Finish failed: %v", at, err)
		}
		if !bytes.Equal(w.buf.Bytes(), want) {
			t.Fatalf("at=%d: recovered short write changed the output", at)
		}
	}

	// Zero progress without an error is io.ErrShortWrite and is sticky.
	w := &oneShortWriter{mode: oneShortZero}
	e := NewCompressedSetEncoder(w)
	if err := e.Add(stream...); err != io.ErrShortWrite {
		t.Fatalf("expected io.ErrShortWrite, got %v", err)
	}
	if err := e.Finish(); err != io.ErrShortWrite {
		t.Fatalf("Finish after short write returned %v", err)
	}
	calls := w.writeCall
	if err := e.Add(stream[0]); err != io.ErrShortWrite {
		t.Fatalf("Add after short write returned %v", err)
	}
	if w.writeCall != calls {
		t.Fatal("encoder kept calling Write after io.ErrShortWrite")
	}
}

func testCompressedSetEncoderEmpty(t *testing.T) {
	var buf bytes.Buffer
	e := NewCompressedSetEncoder(&buf)

	if err := e.Finish(); err != nil {
		t.Fatalf("first Finish failed: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("empty encoder wrote %d bytes", buf.Len())
	}

	// Finish is repeatable and never writes a trailer a second time.
	if err := e.Finish(); err != nil {
		t.Fatalf("second Finish failed: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("repeat Finish wrote %d bytes", buf.Len())
	}

	// Adding after Finish is rejected without producing output; the encoder
	// remains finished and Finish keeps succeeding.
	if err := e.Add(New()); err == nil {
		t.Fatal("Add after Finish succeeded")
	}
	if buf.Len() != 0 {
		t.Fatalf("Add after Finish wrote %d bytes", buf.Len())
	}
	if err := e.Finish(); err != nil {
		t.Fatalf("Finish after rejected Add failed: %v", err)
	}
}

func testCompressedSetEncoderDuplicates(t *testing.T) {
	u := encoderGroundTruth()
	want := []byte(AppendCompressed(nil, u...))

	// Duplicates interleaved within and spread across Add calls.
	var buf bytes.Buffer
	e := NewCompressedSetEncoder(&buf)
	if err := e.Add(u[0], u[0], u[1], u[1], u[2]); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if err := e.Add(u[2], u[2], u[3], u[3]); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if err := e.Add(u[4:]...); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	// The current last id is repeated again: duplicates are tolerated while
	// the run closes, and produce no extra record.
	if err := e.Add(u[len(u)-1], u[len(u)-1]); err != nil {
		t.Fatalf("repeated Add failed: %v", err)
	}
	if err := e.Finish(); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("duplicate handling differs from AppendCompressed\n got: %x\nwant: %x", buf.Bytes(), want)
	}

	// A single id repeated arbitrarily many times encodes once.
	id := New()
	var one bytes.Buffer
	rep := NewCompressedSetEncoder(&one)
	for i := 0; i < 64; i++ {
		if err := rep.Add(id); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
	}
	if err := rep.Finish(); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	if !bytes.Equal(one.Bytes(), []byte(AppendCompressed(nil, id))) {
		t.Fatalf("repeated single id encoded more than once: %x", one.Bytes())
	}
}

func testCompressedSetEncoderOutOfOrder(t *testing.T) {
	tests := []struct {
		name string
		ids  []KSUID
	}{
		{
			name: "payload goes backwards within a timestamp",
			ids:  []KSUID{idAt(10, 0, 2), idAt(10, 0, 2), idAt(10, 0, 1)},
		},
		{
			name: "timestamp goes backwards across Add calls",
			ids:  []KSUID{idAt(20, 0, 9), idAt(19, 0, 9)},
		},
		{
			name: "old duplicate after later ids were committed",
			ids:  []KSUID{idAt(10, 0, 1), idAt(10, 0, 5), idAt(10, 0, 1)},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var buf bytes.Buffer
			e := NewCompressedSetEncoder(&buf)
			if err := e.Add(test.ids[:len(test.ids)-1]...); err != nil {
				t.Fatalf("valid prefix failed: %v", err)
			}
			err := e.Add(test.ids[len(test.ids)-1])
			if err != errCompressedSetNotSorted {
				t.Fatalf("expected not-sorted error, got %v", err)
			}

			// Sticky: the rejected id never reaches the writer and later
			// calls cannot append a suffix to the poisoned stream.
			after := buf.Len()
			if err := e.Add(test.ids[0]); err != errCompressedSetNotSorted {
				t.Fatalf("Add after ordering failure returned %v", err)
			}
			if err := e.Finish(); err != errCompressedSetNotSorted {
				t.Fatalf("Finish after ordering failure returned %v", err)
			}
			if buf.Len() != after {
				t.Fatalf("encoder wrote %d bytes after ordering failure", buf.Len()-after)
			}
		})
	}
}

func testCompressedSetEncoderFinish(t *testing.T) {
	// A stream ending inside a run: the trailing payloadRange is held back
	// until Finish.
	var buf closeCheckWriter
	e := NewCompressedSetEncoder(&buf)
	ids := []KSUID{idAt(10, 0, 7), idAt(10, 0, 8), idAt(10, 0, 9)}
	if err := e.Add(ids...); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	// Before Finish the range record is not on the wire yet: decoding the
	// buffer yields only the first id, and the output is not the final set.
	if prefix, clean := decodeSet(t, buf.Bytes(), len(ids)+1); clean && len(prefix) != 1 {
		t.Fatalf("open run was emitted before Finish: %d ids", len(prefix))
	}

	if err := e.Finish(); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	want := []byte(AppendCompressed(nil, ids...))
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("Finish did not flush the tail: %x != %x", buf.Bytes(), want)
	}
	got, clean := decodeSet(t, buf.Bytes(), len(ids)+1)
	if !clean || !reflect.DeepEqual(got, ids) {
		t.Fatal("flushed stream does not decode to the input")
	}

	// A second Finish neither rewrites the tail nor closes the writer.
	lenAfterFirst := buf.Len()
	if err := e.Finish(); err != nil {
		t.Fatalf("repeat Finish failed: %v", err)
	}
	if buf.Len() != lenAfterFirst {
		t.Fatal("repeat Finish wrote the tail a second time")
	}
	if buf.closed {
		t.Fatal("the encoder closed the underlying writer")
	}
}

func testCompressedSetEncoderSequence(t *testing.T) {
	var seq Sequence
	seq.Seed = New()

	ids := make([]KSUID, 200)
	for i := range ids {
		ids[i], _ = seq.Next()
	}
	want := []byte(AppendCompressed(nil, ids...))

	// Feed them one at a time from the sorted sequence source: the encoder
	// only ever keeps the pending-range counter.
	var buf bytes.Buffer
	e := NewCompressedSetEncoder(&buf)
	for _, id := range ids {
		if err := e.Add(id); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
	}
	if err := e.Finish(); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("sequence stream differs from AppendCompressed\n got: %x\nwant: %x", buf.Bytes(), want)
	}

	got, clean := decodeSet(t, buf.Bytes(), len(ids)+1)
	if !clean || !reflect.DeepEqual(got, ids) {
		t.Fatalf("decoded sequence mismatch (ok=%v, %d vs %d)", clean, len(got), len(ids))
	}
}
