package common

import (
	"errors"
	"strings"
	"testing"
	"unsafe"

	berrors "go.etcd.io/bbolt/errors"
)

func newBlock(t *testing.T, pageSize, pages int, id Pgid, flags uint16) []byte {
	t.Helper()
	buf := make([]byte, pageSize*pages)
	p := (*Page)(unsafe.Pointer(&buf[0]))
	p.SetId(id)
	p.SetFlags(flags)
	p.SetOverflow(uint32(pages - 1))
	return buf
}

func TestPageChecksum_SealAndVerify(t *testing.T) {
	const ps = 4096
	buf := newBlock(t, ps, 1, 5, LeafPageFlag)
	p := LoadPage(buf)

	// Write payload inside the covered region, including near the trailer.
	for i := PageHeaderSize; i < ps-PageTrailerSize; i++ {
		buf[i] = byte(i % 251)
	}
	SealPageChecksum(p, ps)
	if err := VerifyPageChecksum(p, ps); err != nil {
		t.Fatalf("expected valid checksum: %v", err)
	}

	// A bit flip in the payload is detected and the error carries the page id
	// and error type.
	buf[ps/2] ^= 0x01
	err := VerifyPageChecksum(p, ps)
	if !errors.Is(err, berrors.ErrPageChecksum) {
		t.Fatalf("expected ErrPageChecksum, got: %v", err)
	}
	if !strings.Contains(err.Error(), "page 5") {
		t.Fatalf("expected page id in error, got: %v", err)
	}

	// Flipping a bit inside an overflow continuation also fails.
	big := newBlock(t, ps, 3, 7, LeafPageFlag)
	bigPage := LoadPage(big)
	SealPageChecksum(bigPage, ps)
	if err := VerifyPageChecksum(bigPage, ps); err != nil {
		t.Fatalf("expected valid checksum: %v", err)
	}
	big[2*ps+100] ^= 0x01
	if err := VerifyPageChecksum(bigPage, ps); err == nil {
		t.Fatal("expected overflow corruption to be detected")
	}
}

func TestPageChecksum_MetaVersionValidation(t *testing.T) {
	mk := func(version uint32) *Meta {
		m := &Meta{}
		m.SetMagic(Magic)
		m.SetVersion(version)
		m.SetPageSize(4096)
		m.SetChecksum(m.Sum64())
		return m
	}

	// Current binary accepts both the legacy (2) and checksumming (3) format.
	if err := mk(Version).Validate(); err != nil {
		t.Fatalf("legacy version must validate: %v", err)
	}
	if err := mk(PageChecksumVersion).Validate(); err != nil {
		t.Fatalf("checksum version must validate: %v", err)
	}

	// An older binary that only knows up to version 2 must reject version 3.
	if err := mk(PageChecksumVersion).ValidateUpTo(Version); !errors.Is(err, berrors.ErrVersionMismatch) {
		t.Fatalf("expected ErrVersionMismatch for new format on old binary, got: %v", err)
	}

	if !mk(PageChecksumVersion).PageChecksumEnabled() {
		t.Fatal("version 3 must report checksumming enabled")
	}
	if mk(Version).PageChecksumEnabled() {
		t.Fatal("version 2 must report checksumming disabled")
	}
}
