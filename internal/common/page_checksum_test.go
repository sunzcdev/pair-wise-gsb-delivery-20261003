package common

import (
	"testing"
	"unsafe"
)

// Ensure that a checksum trailer is correctly stamped and verified, and
// that any bit flip inside the page run (payload or trailer) is detected.
func TestPageChecksum_WriteAndVerify(t *testing.T) {
	pageSize := 4096
	buf := make([]byte, pageSize*3)
	p := (*Page)(unsafe.Pointer(&buf[0]))
	p.SetId(7)
	p.SetFlags(LeafPageFlag)
	p.SetOverflow(2)

	if run := p.PageRunSize(pageSize); run != 3*pageSize {
		t.Fatalf("unexpected page run size: %d", run)
	}

	// Fill the payload area with non-zero data.
	for i := int(PageHeaderSize); i < len(buf)-PageChecksumSize; i++ {
		buf[i] = byte(i)
	}

	p.WriteChecksum(pageSize)
	if !p.VerifyChecksum(pageSize) {
		t.Fatal("expected checksum verification to pass")
	}

	stored := p.Checksum(pageSize)
	if stored == 0 {
		t.Fatal("expected non-zero checksum")
	}

	// Bit flip in the payload of the head page.
	buf[100] ^= 0xff
	if p.VerifyChecksum(pageSize) {
		t.Fatal("expected checksum verification to fail after bit flip in head page")
	}
	buf[100] ^= 0xff

	// Bit flip in an overflow page.
	buf[pageSize+100] ^= 0xff
	if p.VerifyChecksum(pageSize) {
		t.Fatal("expected checksum verification to fail after bit flip in overflow page")
	}
	buf[pageSize+100] ^= 0xff

	// Bit flip in the trailer itself.
	buf[len(buf)-1] ^= 0xff
	if p.VerifyChecksum(pageSize) {
		t.Fatal("expected checksum verification to fail after bit flip in trailer")
	}
	buf[len(buf)-1] ^= 0xff

	if !p.VerifyChecksum(pageSize) {
		t.Fatal("expected checksum verification to pass after restoring all bytes")
	}
	if got := p.Checksum(pageSize); got != stored {
		t.Fatalf("checksum changed after restore: %x != %x", got, stored)
	}
}

// Ensure the checksum trailer never overlaps the page header.
func TestPageChecksum_TrailerDoesNotOverlapHeader(t *testing.T) {
	pageSize := 4096
	buf := make([]byte, pageSize)
	p := (*Page)(unsafe.Pointer(&buf[0]))
	p.SetId(3)
	p.SetFlags(BranchPageFlag)

	p.WriteChecksum(pageSize)

	// The header fields must be untouched by the checksum computation.
	if p.Id() != 3 || !p.IsBranchPage() || p.Count() != 0 || p.Overflow() != 0 {
		t.Fatalf("page header clobbered by checksum: %s", p)
	}
}
