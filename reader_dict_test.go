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

// lz4Seq is one literal/match sequence of an LZ4 block.
type lz4Seq struct {
	lits     []byte
	matchLen int // 0 for the final, literals only, sequence
	offset   uint16
}

// lz4Block encodes sequences into an LZ4 block.
func lz4Block(seqs ...lz4Seq) []byte {
	var b []byte
	appendLen := func(n int) {
		for n >= 255 {
			b = append(b, 255)
			n -= 255
		}
		b = append(b, byte(n))
	}
	for _, s := range seqs {
		ll, ml := len(s.lits), 0
		token := byte(0)
		if ll >= 15 {
			token |= 0xF0
		} else {
			token |= byte(ll) << 4
		}
		if s.matchLen > 0 {
			ml = s.matchLen - 4
			if ml >= 15 {
				token |= 0x0F
			} else {
				token |= byte(ml)
			}
		}
		b = append(b, token)
		if ll >= 15 {
			appendLen(ll - 15)
		}
		b = append(b, s.lits...)
		if s.matchLen > 0 {
			b = append(b, byte(s.offset), byte(s.offset>>8))
			if ml >= 15 {
				appendLen(ml - 15)
			}
		}
	}
	return b
}

// dictFrame builds an LZ4 frame carrying a dictionary ID around the given
// compressed blocks. A nil dictID builds a dictionary-less frame.
func dictFrame(dictID *uint32, independent bool, blocks [][]byte, content []byte) []byte {
	var buf bytes.Buffer
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], 0x184D2204)
	buf.Write(tmp[:])
	flg := byte(1 << 6) // version 1
	if independent {
		flg |= 1 << 5
	}
	flg |= 1 << 2                 // content checksum
	header := []byte{flg, 4 << 4} // 64Kb max block size
	if dictID != nil {
		header[0] |= 1 // dictionary ID present
		binary.LittleEndian.PutUint32(tmp[:], *dictID)
		header = append(header, tmp[:]...)
	}
	buf.Write(header)
	buf.WriteByte(byte(xxh32.ChecksumZero(header) >> 8))
	for _, block := range blocks {
		binary.LittleEndian.PutUint32(tmp[:], uint32(len(block)))
		buf.Write(tmp[:])
		buf.Write(block)
	}
	buf.Write([]byte{0, 0, 0, 0}) // end mark
	binary.LittleEndian.PutUint32(tmp[:], xxh32.ChecksumZero(content))
	buf.Write(tmp[:])
	return buf.Bytes()
}

// lz4Frame compresses content into a regular LZ4 frame.
func lz4Frame(t *testing.T, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := lz4.NewWriter(&buf)
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestReaderDict(t *testing.T) {
	dict := []byte("the quick brown fox jumps over the lazy dog")
	dictID := uint32(0xDEADBEEF)

	cases := []struct {
		name        string
		independent bool
		blocks      [][]byte
		content     []byte
	}{
		{
			name:        "independent blocks",
			independent: true,
			blocks: [][]byte{
				// The whole dictionary, then a literal.
				lz4Block(lz4Seq{matchLen: 43, offset: 43}, lz4Seq{lits: []byte("!")}),
				// Only the dictionary tail: no history from the previous block.
				lz4Block(lz4Seq{matchLen: 8, offset: 8}, lz4Seq{lits: []byte("?")}),
			},
			content: []byte("the quick brown fox jumps over the lazy dog!lazy dog?"),
		},
		{
			name:        "linked blocks",
			independent: false,
			blocks: [][]byte{
				// Literals, then the dictionary tail.
				lz4Block(lz4Seq{lits: []byte("abc"), matchLen: 8, offset: 3 + 8}, lz4Seq{lits: []byte("!")}),
				// A match in the previous block: linked history, not the preset dictionary.
				lz4Block(lz4Seq{matchLen: 4, offset: 4}, lz4Seq{lits: []byte("?")}),
			},
			content: []byte("abclazy dog!dog!?"),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A dictionary-less frame follows the dictionary one: nothing
			// may leak from the first frame into the second.
			tail := []byte("frame without a dictionary")
			var stream []byte
			stream = append(stream, dictFrame(&dictID, c.independent, c.blocks, c.content)...)
			stream = append(stream, lz4Frame(t, tail)...)

			var releases int
			zr := lz4.NewReader(nil)
			err := zr.Apply(lz4.DictOption(func(id uint32) ([]byte, func(), error) {
				if id != dictID {
					t.Errorf("dictionary ID: got %#x, expected %#x", id, dictID)
				}
				return dict, func() { releases++ }, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			zr.Reset(bytes.NewReader(stream))

			out, err := io.ReadAll(zr)
			if err != nil {
				t.Fatal(err)
			}
			if want := append(append([]byte{}, c.content...), tail...); !bytes.Equal(out, want) {
				t.Errorf("got %q, expected %q", out, want)
			}
			if releases != 1 {
				t.Errorf("dictionary released %d times, expected 1", releases)
			}
			// The dictionary belongs to the caller: it must be untouched.
			if string(dict) != "the quick brown fox jumps over the lazy dog" {
				t.Errorf("dictionary modified: %q", dict)
			}
		})
	}
}

func TestReaderDictMissing(t *testing.T) {
	dict := []byte("the quick brown fox jumps over the lazy dog")
	dictID := uint32(0xDEADBEEF)
	block := lz4Block(lz4Seq{matchLen: 43, offset: 43}, lz4Seq{lits: []byte("!")})
	content := []byte("the quick brown fox jumps over the lazy dog!")
	frame := dictFrame(&dictID, true, [][]byte{block}, content)

	t.Run("no handler", func(t *testing.T) {
		zr := lz4.NewReader(bytes.NewReader(frame))
		_, err := io.ReadAll(zr)
		if !errors.Is(err, lz4.ErrMissingDictionary) {
			t.Errorf("got %v, expected %v", err, lz4.ErrMissingDictionary)
		}
	})

	t.Run("unknown ID", func(t *testing.T) {
		var releases int
		zr := lz4.NewReader(nil)
		err := zr.Apply(lz4.DictOption(func(id uint32) ([]byte, func(), error) {
			// Unknown dictionary: no dictionary, but something to release.
			return nil, func() { releases++ }, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		zr.Reset(bytes.NewReader(frame))
		_, err = io.ReadAll(zr)
		if !errors.Is(err, lz4.ErrMissingDictionary) {
			t.Errorf("got %v, expected %v", err, lz4.ErrMissingDictionary)
		}
		if releases != 1 {
			t.Errorf("dictionary released %d times, expected 1", releases)
		}
	})

	t.Run("handler failure", func(t *testing.T) {
		sentinel := errors.New("dictionary store unavailable")
		zr := lz4.NewReader(nil)
		err := zr.Apply(lz4.DictOption(func(id uint32) ([]byte, func(), error) {
			return nil, nil, sentinel
		}))
		if err != nil {
			t.Fatal(err)
		}
		zr.Reset(bytes.NewReader(frame))
		_, err = io.ReadAll(zr)
		if !errors.Is(err, sentinel) {
			t.Errorf("got %v, expected %v", err, sentinel)
		}
	})

	t.Run("reset releases", func(t *testing.T) {
		var releases int
		zr := lz4.NewReader(nil)
		err := zr.Apply(lz4.DictOption(func(id uint32) ([]byte, func(), error) {
			return dict, func() { releases++ }, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		zr.Reset(bytes.NewReader(frame))
		// Read only part of the frame, then reset: the dictionary is released.
		if _, err := zr.Read(make([]byte, 4)); err != nil {
			t.Fatal(err)
		}
		zr.Reset(bytes.NewReader(lz4Frame(t, []byte("plain"))))
		if releases != 1 {
			t.Errorf("dictionary released %d times, expected 1", releases)
		}
		if _, err := io.ReadAll(zr); err != nil {
			t.Fatal(err)
		}
		if releases != 1 {
			t.Errorf("dictionary released %d times, expected 1", releases)
		}
	})
}

func TestUncompressBlockWithDict(t *testing.T) {
	dict := []byte("the quick brown fox jumps over the lazy dog")
	block := lz4Block(lz4Seq{matchLen: 43, offset: 43}, lz4Seq{lits: []byte("!")})
	dst := make([]byte, 44)
	n, err := lz4.UncompressBlockWithDict(block, dst, dict)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(dst[:n]), "the quick brown fox jumps over the lazy dog!"; got != want {
		t.Errorf("got %q, expected %q", got, want)
	}
}
