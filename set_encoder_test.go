package ksuid

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"
)

func TestCompressedSetEncoder(t *testing.T) {
	tests := []struct {
		scenario string
		function func(*testing.T)
	}{
		{
			scenario: "empty input produces no output",
			function: testEncoderEmpty,
		},
		{
			scenario: "output is identical to AppendCompressed",
			function: testEncoderMatchesAppendCompressed,
		},
		{
			scenario: "all input subsets decode to the sorted deduplicated input",
			function: testEncoderExhaustiveSubsets,
		},
		{
			scenario: "duplicate ids are ignored",
			function: testEncoderDuplicates,
		},
		{
			scenario: "out-of-order ids are rejected when consumed",
			function: testEncoderOutOfOrder,
		},
		{
			scenario: "add after finish fails",
			function: testEncoderAddAfterFinish,
		},
		{
			scenario: "finish is idempotent",
			function: testEncoderFinishIdempotent,
		},
		{
			scenario: "write failures are sticky at every write boundary",
			function: testEncoderStickyFailure,
		},
		{
			scenario: "short writes are reported as io.ErrShortWrite",
			function: testEncoderShortWrite,
		},
		{
			scenario: "the encoder does not close the writer",
			function: testEncoderDoesNotCloseWriter,
		},
	}

	for _, test := range tests {
		t.Run(test.scenario, test.function)
	}
}

func makeEncoderTestID(timestamp uint32, high, low uint64) KSUID {
	return makeUint128(high, low).ksuid(timestamp)
}

// encoderTestPool returns a sorted pool of ids exercising every code path
// of the encoder: runs of consecutive payloads, gaps of various sizes
// (including 64 and 128 bit deltas), and timestamp changes.
func encoderTestPool() []KSUID {
	return []KSUID{
		makeEncoderTestID(100, 0, 0),
		makeEncoderTestID(100, 0, 1),
		makeEncoderTestID(100, 0, 2),
		makeEncoderTestID(100, 0, 3),
		makeEncoderTestID(100, 0, 5),
		makeEncoderTestID(100, 0, 6),
		makeEncoderTestID(100, 0, 300),
		makeEncoderTestID(100, 0, (1<<32)+7),
		makeEncoderTestID(100, 1, 0),
		makeEncoderTestID(100, 1, 1),
		makeEncoderTestID(101, 0, 0),
		makeEncoderTestID(200, 0, 42),
	}
}

func encodeIDs(ids []KSUID) ([]byte, error) {
	buf := &bytes.Buffer{}
	enc := NewCompressedSetEncoder(buf)

	for _, id := range ids {
		if err := enc.Add(id); err != nil {
			return nil, err
		}
	}
	if err := enc.Finish(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeIDs(set []byte) []KSUID {
	ids := []KSUID{}
	for it := CompressedSet(set).Iter(); it.Next(); {
		ids = append(ids, it.KSUID)
	}
	return ids
}

func testEncoderEmpty(t *testing.T) {
	buf := &bytes.Buffer{}
	enc := NewCompressedSetEncoder(buf)

	if err := enc.Finish(); err != nil {
		t.Fatalf("Finish on empty encoder: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("empty encoder produced %d bytes", buf.Len())
	}
	if err := enc.Finish(); err != nil {
		t.Fatalf("second Finish on empty encoder: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("second Finish produced %d bytes", buf.Len())
	}

	// An empty encoder never writes, so even a failing writer cannot make
	// its lifecycle fail.
	w := &failAfterWriter{limit: 0, err: errBoom}
	if err := NewCompressedSetEncoder(w).Finish(); err != nil {
		t.Errorf("Finish on empty encoder with failing writer: %v", err)
	}
	if w.invocations != 0 {
		t.Errorf("empty encoder performed %d writes", w.invocations)
	}
}

func testEncoderMatchesAppendCompressed(t *testing.T) {
	now := time.Now()

	generators := map[string]func() []KSUID{
		"sparse": func() []KSUID {
			times := [100]time.Time{}
			for i := range times {
				times[i] = now.Add(time.Duration(i) * 2 * time.Second)
			}
			ids := make([]KSUID, 1000)
			for i := range ids {
				ids[i], _ = NewRandomWithTime(times[i%len(times)])
			}
			return ids
		},
		"packed": func() []KSUID {
			sequences := [10]Sequence{}
			for i := range sequences {
				sequences[i] = Sequence{Seed: New()}
			}
			ids := make([]KSUID, 1000)
			for i := range ids {
				ids[i], _ = sequences[i%len(sequences)].Next()
			}
			return ids
		},
		"mixed": func() []KSUID {
			times := [20]time.Time{}
			for i := range times {
				times[i] = now.Add(time.Duration(i) * 2 * time.Second)
			}
			sequences := [200]Sequence{}
			for i := range sequences {
				seed, _ := NewRandomWithTime(times[i%len(times)])
				sequences[i] = Sequence{Seed: seed}
			}
			ids := make([]KSUID, 1000)
			for i := range ids {
				ids[i], _ = sequences[i%len(sequences)].Next()
			}
			return ids
		},
		"single id repeated": func() []KSUID {
			id := New()
			ids := make([]KSUID, 40)
			for i := range ids {
				ids[i] = id
			}
			return ids
		},
	}

	for name, generate := range generators {
		ids := generate()

		// AppendCompressed sorts ids in place, leaving them in the order
		// expected by the encoder.
		expected := AppendCompressed(nil, ids...)

		encoded, err := encodeIDs(ids)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(encoded, expected) {
			t.Errorf("%s: encoder output differs from AppendCompressed", name)
		}
	}
}

func testEncoderExhaustiveSubsets(t *testing.T) {
	pool := encoderTestPool()

	for mask := 0; mask < (1 << uint(len(pool))); mask++ {
		var ids []KSUID
		for i, id := range pool {
			if mask&(1<<uint(i)) != 0 {
				ids = append(ids, id)
			}
		}

		expected := AppendCompressed(nil, ids...)

		encoded, err := encodeIDs(ids)
		if err != nil {
			t.Fatalf("mask %d: %v", mask, err)
		}
		if !bytes.Equal(encoded, expected) {
			t.Fatalf("mask %d: encoder output differs from AppendCompressed", mask)
		}

		decoded := decodeIDs(encoded)
		if len(decoded) != len(ids) {
			t.Fatalf("mask %d: decoded %d ids, expected %d", mask, len(decoded), len(ids))
		}
		for i := range ids {
			if decoded[i] != ids[i] {
				t.Fatalf("mask %d: decoded id at index %d is %s, expected %s",
					mask, i, decoded[i], ids[i])
			}
		}
	}
}

func testEncoderDuplicates(t *testing.T) {
	pool := encoderTestPool()

	ids := make([]KSUID, 0, 3*len(pool))
	for _, id := range pool {
		ids = append(ids, id, id, id)
	}

	expected := AppendCompressed(nil, ids...)

	encoded, err := encodeIDs(ids)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, expected) {
		t.Error("encoder output differs from AppendCompressed")
	}
	if decoded := decodeIDs(encoded); len(decoded) != len(pool) {
		t.Errorf("decoded %d ids, expected %d deduplicated ids", len(decoded), len(pool))
	}
}

func testEncoderOutOfOrder(t *testing.T) {
	id1 := makeEncoderTestID(100, 0, 1)
	id2 := makeEncoderTestID(100, 0, 2)
	id3 := makeEncoderTestID(200, 0, 0)

	buf := &bytes.Buffer{}
	enc := NewCompressedSetEncoder(buf)

	if err := enc.Add(id2); err != nil {
		t.Fatal(err)
	}
	if err := enc.Add(id1); err != ErrOutOfOrder {
		t.Errorf("expected ErrOutOfOrder, got %v", err)
	}
	if err := enc.Add(makeEncoderTestID(99, 0, 0)); err != ErrOutOfOrder {
		t.Errorf("expected ErrOutOfOrder, got %v", err)
	}
	// The ordering error is not sticky: the encoder state was left
	// unchanged and valid ids may still be added.
	if err := enc.Add(id3); err != nil {
		t.Fatal(err)
	}
	if err := enc.Finish(); err != nil {
		t.Fatal(err)
	}

	decoded := decodeIDs(buf.Bytes())
	if len(decoded) != 2 || decoded[0] != id2 || decoded[1] != id3 {
		t.Errorf("unexpected decoded ids: %v", decoded)
	}
}

func testEncoderAddAfterFinish(t *testing.T) {
	enc := NewCompressedSetEncoder(&bytes.Buffer{})

	if err := enc.Add(New()); err != nil {
		t.Fatal(err)
	}
	if err := enc.Finish(); err != nil {
		t.Fatal(err)
	}
	if err := enc.Add(New()); err != ErrEncoderClosed {
		t.Errorf("expected ErrEncoderClosed, got %v", err)
	}
	if err := enc.Finish(); err != nil {
		t.Errorf("Finish after Finish: %v", err)
	}
}

func testEncoderFinishIdempotent(t *testing.T) {
	// End with a run of consecutive payloads so that Finish has pending
	// state to flush.
	ids := []KSUID{
		makeEncoderTestID(100, 0, 0),
		makeEncoderTestID(100, 0, 1),
		makeEncoderTestID(100, 0, 2),
	}

	buf := &bytes.Buffer{}
	enc := NewCompressedSetEncoder(buf)

	for _, id := range ids {
		if err := enc.Add(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Finish(); err != nil {
		t.Fatal(err)
	}

	snapshot := append([]byte(nil), buf.Bytes()...)

	for i := 0; i != 3; i++ {
		if err := enc.Finish(); err != nil {
			t.Fatalf("repeated Finish: %v", err)
		}
	}
	if !bytes.Equal(buf.Bytes(), snapshot) {
		t.Error("repeated Finish wrote to the underlying writer")
	}
}

var errBoom = errors.New("boom")

// failAfterWriter allows limit successful writes, then fails with err. It
// counts all invocations so tests can verify that no write is attempted
// after the first failure.
type failAfterWriter struct {
	invocations int
	limit       int
	err         error
}

func (w *failAfterWriter) Write(b []byte) (int, error) {
	w.invocations++
	if w.invocations > w.limit {
		return 0, w.err
	}
	return len(b), nil
}

func testEncoderStickyFailure(t *testing.T) {
	pool := encoderTestPool()

	// Count the number of writes performed by a full encoding.
	counter := &failAfterWriter{limit: 1 << 30}
	if err := encodeIDsTo(counter, pool); err != nil {
		t.Fatal(err)
	}
	totalWrites := counter.invocations

	for limit := 0; limit < totalWrites; limit++ {
		w := &failAfterWriter{limit: limit, err: errBoom}
		enc := NewCompressedSetEncoder(w)

		errs := []error{}
		for _, id := range pool {
			if err := enc.Add(id); err != nil {
				errs = append(errs, err)
			}
		}
		errs = append(errs, enc.Finish())
		errs = append(errs, enc.Finish())

		if len(errs) == 0 {
			t.Errorf("limit %d: expected an error", limit)
			continue
		}
		if errs[0] != errBoom {
			t.Errorf("limit %d: expected %v, got %v", limit, errBoom, errs[0])
		}
		for _, err := range errs[1:] {
			if err != errs[0] {
				t.Errorf("limit %d: failure is not sticky: got %v after %v",
					limit, err, errs[0])
			}
		}
		if w.invocations != limit+1 {
			t.Errorf("limit %d: encoder kept writing after the failure (%d invocations)",
				limit, w.invocations)
		}
	}
}

func encodeIDsTo(w io.Writer, ids []KSUID) error {
	enc := NewCompressedSetEncoder(w)
	for _, id := range ids {
		if err := enc.Add(id); err != nil {
			return err
		}
	}
	return enc.Finish()
}

type shortWriter struct {
	invocations int
}

func (w *shortWriter) Write(b []byte) (int, error) {
	w.invocations++
	return len(b) - 1, nil
}

func testEncoderShortWrite(t *testing.T) {
	w := &shortWriter{}
	enc := NewCompressedSetEncoder(w)

	if err := enc.Add(New()); err != io.ErrShortWrite {
		t.Errorf("expected io.ErrShortWrite, got %v", err)
	}
	if err := enc.Add(New()); err != io.ErrShortWrite {
		t.Errorf("expected sticky io.ErrShortWrite, got %v", err)
	}
	if err := enc.Finish(); err != io.ErrShortWrite {
		t.Errorf("expected sticky io.ErrShortWrite, got %v", err)
	}
	if w.invocations != 1 {
		t.Errorf("encoder kept writing after the short write (%d invocations)",
			w.invocations)
	}
}

type closeTracker struct {
	io.Writer
	closed bool
}

func (w *closeTracker) Close() error {
	w.closed = true
	return nil
}

func testEncoderDoesNotCloseWriter(t *testing.T) {
	w := &closeTracker{Writer: &bytes.Buffer{}}
	enc := NewCompressedSetEncoder(w)

	if err := enc.Add(New()); err != nil {
		t.Fatal(err)
	}
	if err := enc.Finish(); err != nil {
		t.Fatal(err)
	}
	if w.closed {
		t.Error("the encoder closed the underlying writer")
	}
}
