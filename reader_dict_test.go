package lz4_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/pierrec/lz4/v4"
	"github.com/pierrec/lz4/v4/internal/xxh32"
)

// These tests cover frames that carry a preset dictionary ID. The Reader
// resolves the ID through a DictionaryResolver and keeps the frame's
// preset dictionary distinct from the history accrued across linked blocks.

const (
	dictFlgV1    = 0x40 // descriptor version 01
	dictFlgIndep = 0x20
	dictFlgDict  = 0x01
	dictFlgCntCk = 0x04
	dictBD64K    = 4 << 4
)

// dictFrame builds an LZ4 frame around the given compressed blocks. When
// contentChecksum is set, it appends the XXH32 of the expected output.
func dictFrame(indep bool, dictID uint32, contentChecksum []byte, blocks ...[]byte) []byte {
	flg := byte(dictFlgV1 | dictFlgDict)
	if indep {
		flg |= dictFlgIndep
	}
	if contentChecksum != nil {
		flg |= dictFlgCntCk
	}
	desc := []byte{flg, dictBD64K}
	desc = binary.LittleEndian.AppendUint32(desc, dictID)
	desc = append(desc, byte(xxh32.ChecksumZero(desc)>>8)) // checksum over FLG,BD,DictID

	var b bytes.Buffer
	b.Write(binary.LittleEndian.AppendUint32(nil, 0x184D2204))
	b.Write(desc)
	for _, blk := range blocks {
		var hdr [4]byte
		binary.LittleEndian.PutUint32(hdr[:], uint32(len(blk)))
		b.Write(hdr[:])
		b.Write(blk)
	}
	b.Write([]byte{0, 0, 0, 0}) // end mark
	if contentChecksum != nil {
		b.Write(contentChecksum)
	}
	return b.Bytes()
}

// plainFrame builds an ordinary frame without a dictionary ID.
func plainFrame(indep bool, blocks ...[]byte) []byte {
	flg := byte(dictFlgV1)
	if indep {
		flg |= dictFlgIndep
	}
	desc := []byte{flg, dictBD64K}
	desc = append(desc, byte(xxh32.ChecksumZero(desc)>>8))

	var b bytes.Buffer
	b.Write(binary.LittleEndian.AppendUint32(nil, 0x184D2204))
	b.Write(desc)
	for _, blk := range blocks {
		var hdr [4]byte
		binary.LittleEndian.PutUint32(hdr[:], uint32(len(blk)))
		b.Write(hdr[:])
		b.Write(blk)
	}
	b.Write([]byte{0, 0, 0, 0})
	return b.Bytes()
}

// lz4Seq is one literal/match sequence of an LZ4 block. The final sequence
// carries only literals; its offset must be 0.
type lz4Seq struct {
	lit    []byte
	offset uint16
	match  int // match length including the 4-byte minimum
}

// lz4Block encodes sequences by hand.
func lz4Block(seqs ...lz4Seq) []byte {
	var b bytes.Buffer
	putLenExt := func(n int) {
		for n >= 0xFF {
			n -= 0xFF
			b.WriteByte(0xFF)
		}
		b.WriteByte(byte(n))
	}
	for i, s := range seqs {
		litLen := len(s.lit)
		last := i == len(seqs)-1
		var tok byte
		if litLen >= 0xF {
			tok |= 0xF0
		} else {
			tok |= byte(litLen) << 4
		}
		if !last {
			if m := s.match - 4; m >= 0xF {
				tok |= 0xF
			} else {
				tok |= byte(m)
			}
		}
		b.WriteByte(tok)
		if litLen >= 0xF {
			putLenExt(litLen - 0xF)
		}
		b.Write(s.lit)
		if last {
			continue
		}
		b.WriteByte(byte(s.offset))
		b.WriteByte(byte(s.offset >> 8))
		if m := s.match - 4; m >= 0xF {
			putLenExt(m - 0xF)
		}
	}
	return b.Bytes()
}

// dictStore is a resolver backend that also tracks resource behavior.
type dictStore struct {
	dicts    map[uint32][]byte
	calls    []uint32
	releases int
	fail     error
}

func (s *dictStore) resolver(id uint32) (*lz4.FrameDictionary, error) {
	s.calls = append(s.calls, id)
	if s.fail != nil {
		return nil, s.fail
	}
	d, ok := s.dicts[id]
	if !ok {
		return nil, nil
	}
	return &lz4.FrameDictionary{
		Data:    d,
		Release: func() { s.releases++ },
	}, nil
}

func newReader(frame []byte, store *dictStore, opts ...lz4.Option) *lz4.Reader {
	zr := lz4.NewReader(bytes.NewReader(frame))
	all := append([]lz4.Option{lz4.DictionaryResolverOption(store.resolver)}, opts...)
	if err := zr.Apply(all...); err != nil {
		panic(err)
	}
	return zr
}

// TestReaderDictionaryWriteTo exercises the WriteTo fast path, which has
// its own frame-end and error handling.
func TestReaderDictionaryWriteTo(t *testing.T) {
	dict := []byte("abcdefghijklmnopqrstuvwxyz")
	const id = 0xABCD

	indep := lz4Block(
		lz4Seq{lit: []byte("AB"), offset: 6, match: 4},
		lz4Seq{lit: []byte("!!")},
	)
	linked := lz4Block(
		lz4Seq{lit: []byte("ABCD"), offset: 8, match: 4},
		lz4Seq{lit: []byte("1")},
	)

	for _, tc := range []struct {
		name  string
		frame []byte
		want  []byte
	}{
		{"independent", dictFrame(true, id, nil, indep, indep), []byte("ABwxyz!!ABwxyz!!")},
		{"linked", dictFrame(false, id, nil, linked), []byte("ABCDwxyz1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &dictStore{dicts: map[uint32][]byte{id: dict}}
			zr := newReader(tc.frame, store)
			var got bytes.Buffer
			if _, err := zr.WriteTo(&got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), tc.want) {
				t.Fatalf("got %q; want %q", got.Bytes(), tc.want)
			}
			if store.releases != 1 {
				t.Fatalf("releases=%d", store.releases)
			}
		})
	}
}

// TestReaderDictionaryIndependent reads a multi-block independent frame
// that references a preset dictionary from every block.
func TestReaderDictionaryIndependent(t *testing.T) {
	// 26 distinct bytes: no two 4-byte runs coincide within the dictionary.
	dict := []byte("abcdefghijklmnopqrstuvwxyz")
	const id = 0x11223344

	// Two dict references within one block: lit "A" -> uvwx, lit "B" -> wxyz.
	block := lz4Block(
		lz4Seq{lit: []byte("A"), offset: 7, match: 4},
		lz4Seq{lit: []byte("B"), offset: 10, match: 4},
		lz4Seq{lit: []byte(".")},
	)
	wantBlock := []byte("AuvwxBwxyz.")
	frame := dictFrame(true, id, nil, block, block, block)
	wantAll := bytes.Repeat(wantBlock, 3)

	store := &dictStore{dicts: map[uint32][]byte{id: dict}}
	out, err := io.ReadAll(newReader(frame, store))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, wantAll) {
		t.Fatalf("got %q; want %q", out, wantAll)
	}

	// The resolver is consulted exactly once for the frame and the
	// dictionary is released once at the normal frame end.
	if len(store.calls) != 1 || store.calls[0] != id {
		t.Fatalf("resolver calls = %v", store.calls)
	}
	if store.releases != 1 {
		t.Fatalf("releases = %d; want 1", store.releases)
	}
}

// TestReaderDictionaryConcurrencyDropped makes sure requesting concurrency
// for a dictionary frame is silently downgraded to serial decoding rather
// than failing or racing on the shared dictionary.
func TestReaderDictionaryConcurrencyDropped(t *testing.T) {
	dict := []byte("abcdefghijklmnopqrstuvwxyz")
	const id = 1
	block := lz4Block(lz4Seq{lit: []byte("AB"), offset: 6, match: 4}, lz4Seq{lit: []byte("!")})
	frame := dictFrame(true, id, nil, block, block)

	store := &dictStore{dicts: map[uint32][]byte{id: dict}}
	zr := newReader(frame, store, lz4.ConcurrencyOption(4))
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if want := bytes.Repeat([]byte("ABwxyz!"), 2); !bytes.Equal(out, want) {
		t.Fatalf("got %q; want %q", out, want)
	}
}

// TestReaderDictionaryIndependentSmallBuffer exercises the path where each
// block is uncompressed into the Reader's own buffer and copied out in
// pieces, instead of being decoded directly into the caller's buffer.
func TestReaderDictionaryIndependentSmallBuffer(t *testing.T) {
	dict := []byte("abcdefghijklmnopqrstuvwxyz")
	const id = 42
	block := lz4Block(
		lz4Seq{lit: []byte("AB"), offset: 6, match: 4},
		lz4Seq{lit: []byte("!!")},
	)
	wantBlock := []byte("ABwxyz!!")
	frame := dictFrame(true, id, nil, block, block)

	store := &dictStore{dicts: map[uint32][]byte{id: dict}}
	zr := newReader(frame, store)
	var got bytes.Buffer
	buf := make([]byte, 3)
	for {
		n, err := zr.Read(buf)
		got.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if want := bytes.Repeat(wantBlock, 2); !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("got %q; want %q", got.Bytes(), want)
	}
	if store.releases != 1 {
		t.Fatalf("releases = %d", store.releases)
	}
}

// TestReaderDictionaryLinked reads linked blocks: the preset dictionary is
// available to every block, while decoded blocks accrue as history that
// later blocks can reference.
func TestReaderDictionaryLinked(t *testing.T) {
	dict := []byte("abcdefghijklmnopqrstuvwxyz")
	const id = 7

	// Block 1 uses only the preset dictionary.
	b1 := lz4Block(
		lz4Seq{lit: []byte("ABCD"), offset: 8, match: 4}, // di=4: dict[26+4-8:22]="wxyz"
		lz4Seq{lit: []byte("1")},
	)
	// History is now dict + "ABCDwxyz1".
	// Block 2 references the first block ("ABCD") rather than the dict.
	b2 := lz4Block(
		lz4Seq{lit: []byte("Z"), offset: 10, match: 4}, // di=1: history[35+1-10:26]="ABCD"
		lz4Seq{lit: []byte("2")},
	)
	// Block 3 references data that only exists because block 2 accrued
	// ("ZABCD"), proving linked history chains beyond the dictionary.
	b3 := lz4Block(
		lz4Seq{lit: []byte("Q"), offset: 16, match: 4}, // di=1: history[41+1-16:26]="ABCD" from block 2
		lz4Seq{lit: []byte("3")},
	)

	want := []byte("ABCDwxyz1")
	want = append(want, []byte("ZABCD2")...)
	want = append(want, []byte("QABCD3")...)

	frame := dictFrame(false, id, nil, b1, b2, b3)
	store := &dictStore{dicts: map[uint32][]byte{id: dict}}
	out, err := io.ReadAll(newReader(frame, store))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, want) {
		t.Fatalf("got %q; want %q", out, want)
	}
	if len(store.calls) != 1 || store.releases != 1 {
		t.Fatalf("calls=%v releases=%d", store.calls, store.releases)
	}
}

// TestReaderDictionaryLinkedThenPlain reads a dictionary-backed linked
// frame followed by a frame without a dictionary ID, concatenated in one
// stream. The second frame must not see the first frame's dictionary or any
// of its accrued history.
func TestReaderDictionaryLinkedThenPlain(t *testing.T) {
	dict := []byte("abcdefghijklmnopqrstuvwxyz")
	const id = 99

	dictB1 := lz4Block(
		lz4Seq{lit: []byte("ABCD"), offset: 8, match: 4},
		lz4Seq{lit: []byte("1")},
	)
	// Second linked block copies "ABCD" from the first block's history.
	dictB2 := lz4Block(
		lz4Seq{lit: []byte("Z"), offset: 10, match: 4},
		lz4Seq{lit: []byte("2")},
	)
	dictWant := []byte("ABCDwxyz1ZABCD2")
	dictFrameBytes := dictFrame(false, id, nil, dictB1, dictB2)

	// A linked plain frame. If any history leaked, its second block would
	// decode against foreign bytes. With a clean history it references only
	// its own first block.
	plainB1 := lz4Block(
		lz4Seq{lit: []byte("MNOP")},
	)
	plainB2 := lz4Block(
		lz4Seq{lit: []byte("Q"), offset: 5, match: 4}, // di=1 -> history[0:]="MNOP"
		lz4Seq{lit: []byte("!")},
	)
	plainWant := []byte("MNOPQMNOP!")
	plainFrameBytes := plainFrame(false, plainB1, plainB2)

	store := &dictStore{dicts: map[uint32][]byte{id: dict}}
	zr := newReader(append(dictFrameBytes, plainFrameBytes...), store)
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if want := append(append([]byte{}, dictWant...), plainWant...); !bytes.Equal(out, want) {
		t.Fatalf("got %q; want %q", out, want)
	}
	// Resolver called only for the first frame; released as it ended.
	if len(store.calls) != 1 || store.calls[0] != id {
		t.Fatalf("calls=%v", store.calls)
	}
	if store.releases != 1 {
		t.Fatalf("releases=%d", store.releases)
	}
}

// TestReaderDictionaryMissing covers the ways a dictionary lookup can fail.
func TestReaderDictionaryMissing(t *testing.T) {
	dict := []byte("abcdefghijklmnopqrstuvwxyz")
	const id = 5
	block := lz4Block(lz4Seq{lit: []byte("AB"), offset: 6, match: 4}, lz4Seq{lit: []byte("!")})
	frame := dictFrame(true, id, nil, block)

	t.Run("noResolver", func(t *testing.T) {
		zr := lz4.NewReader(bytes.NewReader(frame))
		if _, err := io.ReadAll(zr); !errors.Is(err, lz4.ErrDictionaryNotFound) {
			t.Fatalf("got %v; want ErrDictionaryNotFound", err)
		}
	})

	t.Run("unknownID", func(t *testing.T) {
		store := &dictStore{dicts: map[uint32][]byte{}}
		if _, err := io.ReadAll(newReader(frame, store)); !errors.Is(err, lz4.ErrDictionaryNotFound) {
			t.Fatalf("got %v; want ErrDictionaryNotFound", err)
		}
		if len(store.calls) != 1 || store.calls[0] != id {
			t.Fatalf("calls=%v", store.calls)
		}
		if store.releases != 0 {
			t.Fatalf("releases=%d; want 0", store.releases)
		}
	})

	t.Run("resolverError", func(t *testing.T) {
		myErr := errors.New("storage unavailable")
		store := &dictStore{fail: myErr}
		_, err := io.ReadAll(newReader(frame, store))
		if !errors.Is(err, myErr) {
			t.Fatalf("got %v; want %v", err, myErr)
		}
		if store.releases != 0 {
			t.Fatalf("releases=%d; want 0", store.releases)
		}
	})

	// The ID is never compared against the dictionary contents.
	t.Run("idNotChecksum", func(t *testing.T) {
		store := &dictStore{dicts: map[uint32][]byte{id: dict}}
		out, err := io.ReadAll(newReader(frame, store))
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != "ABwxyz!" {
			t.Fatalf("got %q", out)
		}
	})
}

// TestReaderDictionaryReleaseOnError verifies the resolver's release runs
// when a later block fails to decode.
func TestReaderDictionaryReleaseOnError(t *testing.T) {
	const id = 5
	// Offset 1 is legal; an offset of 0 is corrupt.
	good := lz4Block(lz4Seq{lit: []byte("AB"), offset: 6, match: 4}, lz4Seq{lit: []byte("!")})
	bad := []byte{0x20, 'X', 'Y', 0x00, 0x00, 0x00}
	frame := dictFrame(true, id, nil, good, bad)

	released := 0
	zr := lz4.NewReader(bytes.NewReader(frame))
	if err := zr.Apply(lz4.DictionaryResolverOption(func(id uint32) (*lz4.FrameDictionary, error) {
		return &lz4.FrameDictionary{
			Data:    []byte("abcdefghijklmnopqrstuvwxyz"),
			Release: func() { released++ },
		}, nil
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(zr); err == nil {
		t.Fatal("expected a decode error")
	}
	if released != 1 {
		t.Fatalf("released=%d; want 1", released)
	}
}

// TestReaderDictionaryResetRelease verifies that Reset abandons and releases
// the current frame's dictionary.
func TestReaderDictionaryResetRelease(t *testing.T) {
	dict := []byte("abcdefghijklmnopqrstuvwxyz")
	const id = 11
	// Truncated frame: header with a dict ID but no blocks.
	frame := dictFrame(true, id, nil)[:4+7]

	released := 0
	zr := lz4.NewReader(bytes.NewReader(frame))
	if err := zr.Apply(lz4.DictionaryResolverOption(func(uint32) (*lz4.FrameDictionary, error) {
		return &lz4.FrameDictionary{Data: dict, Release: func() { released++ }}, nil
	})); err != nil {
		t.Fatal(err)
	}
	// First read fails on the truncated frame; release must already have run
	// through the error path, and Reset must not release a second time.
	_, _ = io.ReadAll(zr)
	zr.Reset(bytes.NewReader(encode(t, []byte("clean frame"))))
	out, err := io.ReadAll(zr)
	if err != nil || string(out) != "clean frame" {
		t.Fatalf("after reset: %q %v", out, err)
	}
	if released != 1 {
		t.Fatalf("released=%d; want 1", released)
	}
}

// TestReaderDictionaryWindowTrimsOnceHistoryGrows exercises the linked-block
// sliding window once preset dictionary plus decoded blocks exceed 128KiB,
// matching the reference decoder's history management.
func TestReaderDictionaryWindowTrimsOnceHistoryGrows(t *testing.T) {
	seed := []byte("PAT!")

	// 70KiB dictionary; only the final 64KiB is usable. The seed sits at the
	// first reachable position of that window (its second byte).
	dict := make([]byte, 70*1024)
	for i := range dict {
		dict[i] = 'x'
	}
	copy(dict[70*1024-64*1024+1:], seed)

	// Block 1: seed from the dictionary, replicate to 50KiB.
	b1, w1 := emitRunsBlock(seed, 65535, 50*1024, []byte("end1"))
	// Block 2: seed from the tail of block 1 (offset 12 skips "end1").
	b2, w2 := emitRunsBlock(seed, 12, 50*1024, []byte("end2"))
	// Block 3 seeds from the recent linked history (block 2 output).
	b3, w3 := emitRunsBlock(seed, 8, 10*1024, []byte("end3"))

	want := append(append(append([]byte{}, w1...), w2...), w3...)
	frame := dictFrame(false, 1234, nil, b1, b2, b3)

	store := &dictStore{dicts: map[uint32][]byte{1234: dict}}
	out, err := io.ReadAll(newReader(frame, store))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, want) {
		t.Fatalf("decoded %d bytes; want %d", len(out), len(want))
	}
	if store.releases != 1 {
		t.Fatalf("releases=%d", store.releases)
	}
}

// emitRunsBlock encodes a block that copies seedLen bytes from history at
// firstOffset, self-replicates the seed up to totalLen, then emits tail.
func emitRunsBlock(seed []byte, firstOffset, totalLen int, tail []byte) ([]byte, []byte) {
	var b bytes.Buffer
	put := func(n int) {
		for n >= 0xFF {
			n -= 0xFF
			b.WriteByte(0xFF)
		}
		b.WriteByte(byte(n))
	}
	seedLen := len(seed)
	// First match from history, match-4 encoded in the token's low nibble.
	b.WriteByte(0)
	b.WriteByte(byte(firstOffset))
	b.WriteByte(byte(firstOffset >> 8))

	out := append([]byte{}, seed...)
	for len(out)+seedLen <= totalLen {
		b.WriteByte(0x00) // zero literals, match length 4
		b.WriteByte(byte(seedLen))
		b.WriteByte(byte(seedLen >> 8))
		out = append(out, seed...)
	}
	if litLen := len(tail); litLen >= 0xF {
		b.WriteByte(0xF0)
		put(litLen - 0xF)
	} else {
		b.WriteByte(byte(litLen) << 4)
	}
	b.Write(tail)
	out = append(out, tail...)
	return b.Bytes(), out
}

// TestReaderDictionaryContentChecksum makes sure the content checksum still
// guards a dictionary frame; the ID does not replace it.
func TestReaderDictionaryContentChecksum(t *testing.T) {
	dict := []byte("abcdefghijklmnopqrstuvwxyz")
	const id = 8
	block := lz4Block(lz4Seq{lit: []byte("AB"), offset: 6, match: 4}, lz4Seq{lit: []byte("!")})
	want := []byte("ABwxyz!")
	sum := binary.LittleEndian.AppendUint32(nil, xxh32.ChecksumZero(want))

	frame := dictFrame(true, id, sum, block)
	store := &dictStore{dicts: map[uint32][]byte{id: dict}}
	out, err := io.ReadAll(newReader(frame, store))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, want) {
		t.Fatalf("got %q", out)
	}

	// Flip the checksum byte; the frame must be rejected.
	bad := append([]byte{}, frame...)
	bad[len(bad)-1] ^= 1
	if _, err := io.ReadAll(newReader(bad, &dictStore{dicts: map[uint32][]byte{id: dict}})); err == nil {
		t.Fatal("expected checksum error")
	}
}

// TestUncompressBlockWithDictAvailable confirms the block-level API remains.
func TestUncompressBlockWithDictAvailable(t *testing.T) {
	n, err := lz4.UncompressBlockWithDict(
		[]byte{0x20, 'A', 'B', 0x06, 0x00, 0x20, '!', '!'},
		make([]byte, 64),
		[]byte("abcdefghijklmnopqrstuvwxyz"),
	)
	if err != nil || n != len("ABwxyz!!") {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
