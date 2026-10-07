package lz4stream

import (
	"strings"
	"testing"

	"github.com/pierrec/lz4/v4/internal/xxh32"
)

// TestFrameDescriptorDictionaryID verifies parsing of the dictionary ID
// field, alone and combined with a content size.
func TestFrameDescriptorDictionaryID(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flg   byte
		bd    byte
		extra []byte
		id    uint32
		size  uint64
	}{
		{"dictOnly", 0x41, 0x40, []byte{0x78, 0x56, 0x34, 0x12}, 0x12345678, 0},
		{"dictAndSize", 0x49, 0x40, []byte{
			1, 0, 0, 0, 0, 0, 0, 0, // content size 1
			0xef, 0xbe, 0xad, 0xde, // dictionary ID
		}, 0xdeadbeef, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := append([]byte{tc.flg, tc.bd}, tc.extra...)
			desc := append(append([]byte{}, fields...), byte(xxh32.ChecksumZero(fields)>>8))
			f := NewFrame()
			f.Magic = frameMagic
			var fd FrameDescriptor
			if err := fd.initR(f, strings.NewReader(string(desc))); err != nil {
				t.Fatal(err)
			}
			if !fd.Flags.DictionaryID() {
				t.Fatal("DictionaryID flag not set")
			}
			if fd.DictionaryID != tc.id {
				t.Fatalf("id=%#x; want %#x", fd.DictionaryID, tc.id)
			}
			if tc.size != 0 && fd.ContentSize != tc.size {
				t.Fatalf("size=%d; want %d", fd.ContentSize, tc.size)
			}
		})
	}
}
