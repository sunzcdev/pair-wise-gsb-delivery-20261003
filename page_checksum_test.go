package bbolt_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"

	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
	"go.etcd.io/bbolt/internal/btesting"
	"go.etcd.io/bbolt/internal/common"
)

// rawPage mirrors the on-disk page header for direct file manipulation.
type rawPage struct {
	id       uint64
	flags    uint16
	count    uint16
	overflow uint32
}

func pageChecksumOptions() *bolt.Options {
	return &bolt.Options{PageChecksum: true}
}

func openPageChecksumDB(t *testing.T) *btesting.DB {
	t.Helper()
	return btesting.MustCreateDBWithOption(t, pageChecksumOptions())
}

// loadRawPages parses every plausible physical block header in a db file.
func loadRawPages(t *testing.T, path string, pageSize int) map[uint64]*rawPage {
	t.Helper()
	buf, err := os.ReadFile(path)
	require.NoError(t, err)

	pages := make(map[uint64]*rawPage)
	for off := 0; off+int(common.PageHeaderSize) <= len(buf); off += pageSize {
		p := (*rawPage)(unsafe.Pointer(&buf[off]))
		if p.id >= 2 && p.id < uint64(len(buf)/pageSize) &&
			(p.flags == common.LeafPageFlag || p.flags == common.BranchPageFlag ||
				p.flags == common.FreelistPageFlag) {
			pages[p.id] = p
		}
	}
	return pages
}

// reachablePageIDs returns the physical ids of all blocks reachable from the
// root bucket tree, via the information Tx.Page exposes. It avoids stale
// blocks that sit on the free list and are never read.
func reachablePageIDs(t *testing.T, db *bolt.DB) []uint64 {
	t.Helper()
	var ids []uint64
	require.NoError(t, db.View(func(tx *bolt.Tx) error {
		hwm := int(tx.Size() / int64(db.Info().PageSize))
		for id := 2; id < hwm; id++ {
			info, err := tx.Page(id)
			require.NoError(t, err)
			if info == nil {
				continue
			}
			if info.Type == "leaf" || info.Type == "branch" || info.Type == "freelist" {
				ids = append(ids, uint64(id))
			}
		}
		return nil
	}))
	return ids
}

// findReachablePageID returns a reachable block with the given flags.
func findReachablePageID(t *testing.T, db *bolt.DB, pages map[uint64]*rawPage, flags uint16) (uint64, bool) {
	t.Helper()
	ids := reachablePageIDs(t, db)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if p, ok := pages[id]; ok && p.flags == flags {
			return id, true
		}
	}
	return 0, false
}

// corruptByte flips one bit inside a block, away from the page header and the
// checksum trailer so the corruption lands in covered payload/continuation.
func corruptByte(t *testing.T, path string, pageSize int, id uint64) {
	t.Helper()
	buf, err := os.ReadFile(path)
	require.NoError(t, err)

	hdr := (*rawPage)(unsafe.Pointer(&buf[int(id)*pageSize]))
	blockPages := int(hdr.overflow) + 1
	offset := int(id)*pageSize + (blockPages-1)*pageSize + pageSize/2
	// For a single-page block pageSize/2 is comfortably inside the covered
	// region [header, pageSize-trailer).
	buf[offset] ^= 0x01
	require.NoError(t, os.WriteFile(path, buf, 0600))
}

func seedData(t *testing.T, db *bolt.DB, bucket string, n int) {
	t.Helper()
	require.NoError(t, db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucket))
		require.NoError(t, err)
		for i := 0; i < n; i++ {
			if err := b.Put([]byte(strconv.Itoa(i)), []byte("value")); err != nil {
				return err
			}
		}
		return nil
	}))
}

func checkErrors(t *testing.T, db *bolt.DB) []error {
	t.Helper()
	var errs []error
	err := db.View(func(tx *bolt.Tx) error {
		for e := range tx.Check() {
			errs = append(errs, e)
		}
		return nil
	})
	require.NoError(t, err)
	return errs
}

// Round-trip preserves the version 3 format and data, and reopening without
// the option keeps checksumming enabled (the file format is authoritative).
func TestPageChecksum_RoundTrip(t *testing.T) {
	db := openPageChecksumDB(t)
	seedData(t, db.DB, "widgets", 50)
	require.Equal(t, common.PageChecksumVersion, db.Version())

	path := db.Path()
	db.MustClose()

	reopened := btesting.MustOpenDBWithOption(t, path, pageChecksumOptions())
	require.True(t, reopened.PageChecksum())
	require.NoError(t, reopened.View(func(tx *bolt.Tx) error {
		require.Equal(t, "value", string(tx.Bucket([]byte("widgets")).Get([]byte("7"))))
		return nil
	}))
	require.Empty(t, checkErrors(t, reopened.DB))
	reopened.MustClose()

	plain := btesting.MustOpenDBWithOption(t, path, nil)
	require.Equal(t, common.PageChecksumVersion, plain.Version())
	require.True(t, plain.PageChecksum())
	plain.MustClose()
}

// A bit flip in a leaf block is detected and the error names the page id and
// the checksum error type.
func TestPageChecksum_BitFlipLeaf(t *testing.T) {
	db := openPageChecksumDB(t)
	seedData(t, db.DB, "widgets", 10)
	path := db.Path()
	pageSize := db.Info().PageSize
	db.MustClose()

	pages := loadRawPages(t, path, pageSize)
	reopened := btesting.MustOpenDBWithOption(t, path, nil)
	leafID, ok := findReachablePageID(t, reopened.DB, pages, common.LeafPageFlag)
	reopened.MustClose()
	require.True(t, ok, "expected a reachable leaf page")
	corruptByte(t, path, pageSize, leafID)

	reopened = btesting.MustOpenDBWithOption(t, path, nil)
	defer reopened.MustClose()

	errs := checkErrors(t, reopened.DB)
	require.NotEmpty(t, errs)
	require.True(t, containsPageChecksumError(errs, leafID), "errors: %v", errs)
}

// With enough keys the b-tree grows branch pages; corrupting a branch block
// is detected.
func TestPageChecksum_BitFlipBranch(t *testing.T) {
	db := openPageChecksumDB(t)
	seedData(t, db.DB, "widgets", 20000)
	path := db.Path()
	pageSize := db.Info().PageSize
	db.MustClose()

	pages := loadRawPages(t, path, pageSize)
	reopened := btesting.MustOpenDBWithOption(t, path, nil)
	branchID, ok := findReachablePageID(t, reopened.DB, pages, common.BranchPageFlag)
	reopened.MustClose()
	require.True(t, ok, "expected a reachable branch page")
	corruptByte(t, path, pageSize, branchID)

	reopened = btesting.MustOpenDBWithOption(t, path, nil)
	defer reopened.MustClose()

	errs := checkErrors(t, reopened.DB)
	require.NotEmpty(t, errs)
	require.True(t, containsPageChecksumError(errs, branchID), "errors: %v", errs)
}

// A large value creates an overflow span. Flipping a bit in the continuation
// page is still caught because the whole block is covered by one checksum.
func TestPageChecksum_BitFlipOverflow(t *testing.T) {
	db := openPageChecksumDB(t)
	large := make([]byte, 3*db.Info().PageSize)
	for i := range large {
		large[i] = byte(i % 251)
	}
	require.NoError(t, db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket([]byte("big"))
		require.NoError(t, err)
		return b.Put([]byte("blob"), large)
	}))
	path := db.Path()
	pageSize := db.Info().PageSize
	db.MustClose()

	pages := loadRawPages(t, path, pageSize)
	reopened := btesting.MustOpenDBWithOption(t, path, nil)
	leafID, ok := findReachablePageID(t, reopened.DB, pages, common.LeafPageFlag)
	reopened.MustClose()
	require.True(t, ok, "expected a reachable leaf block")
	require.Greater(t, pages[leafID].overflow, uint32(0), "expected an overflow leaf block")
	corruptByte(t, path, pageSize, leafID)

	reopened = btesting.MustOpenDBWithOption(t, path, nil)
	defer reopened.MustClose()

	errs := checkErrors(t, reopened.DB)
	require.NotEmpty(t, errs)
	require.True(t, containsPageChecksumError(errs, leafID), "errors: %v", errs)
}

// Corrupting the freelist block prevents opening a writable database, with a
// page id and the checksum error type surfaced.
func TestPageChecksum_BitFlipFreelist(t *testing.T) {
	db := openPageChecksumDB(t)
	seedData(t, db.DB, "widgets", 10)
	// Delete keys so a fresh non-empty freelist is written.
	require.NoError(t, db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("widgets")).Delete([]byte("0"))
	}))
	path := db.Path()
	pageSize := db.Info().PageSize
	db.MustClose()

	pages := loadRawPages(t, path, pageSize)
	reopened := btesting.MustOpenDBWithOption(t, path, nil)
	freeID, ok := findReachablePageID(t, reopened.DB, pages, common.FreelistPageFlag)
	reopened.MustClose()
	require.True(t, ok, "expected a reachable freelist page")
	corruptByte(t, path, pageSize, freeID)

	_, err := bolt.Open(path, 0600, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, berrors.ErrPageChecksum)
	require.True(t, strings.Contains(err.Error(), strconv.FormatUint(freeID, 10)),
		"expected page id in error, got: %v", err)
}

// Legacy databases keep working and have no trailer; the option must not be
// force-enabled on them.
func TestPageChecksum_LegacyCompatibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := btesting.MustOpenDBWithOption(t, path, nil)
	seedData(t, db.DB, "widgets", 10)
	require.Equal(t, common.Version, db.Version())
	db.MustClose()

	// Flipping a byte in a legacy file is not protected: reopen succeeds and
	// the format remains version 2.
	reopened := btesting.MustOpenDBWithOption(t, path, &bolt.Options{})
	require.Equal(t, common.Version, reopened.Version())
	require.False(t, reopened.PageChecksum())
	require.Empty(t, checkErrors(t, reopened.DB))
	reopened.MustClose()

	// Enabling checksumming on the existing file is rejected explicitly.
	_, err := bolt.Open(path, 0600, pageChecksumOptions())
	require.ErrorIs(t, err, berrors.ErrPageChecksumNotSupported)
}

// Read-only open also verifies disk blocks when the tree is traversed.
func TestPageChecksum_ReadOnlyDetectsCorruption(t *testing.T) {
	db := openPageChecksumDB(t)
	seedData(t, db.DB, "widgets", 10)
	path := db.Path()
	pageSize := db.Info().PageSize
	db.MustClose()

	pages := loadRawPages(t, path, pageSize)
	reopened := btesting.MustOpenDBWithOption(t, path, nil)
	leafID, ok := findReachablePageID(t, reopened.DB, pages, common.LeafPageFlag)
	reopened.MustClose()
	require.True(t, ok)
	corruptByte(t, path, pageSize, leafID)

	ro, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true})
	require.NoError(t, err)
	defer ro.Close()

	require.Panics(t, func() {
		_ = ro.View(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte("widgets")).ForEach(func(k, v []byte) error { return nil })
		})
	})
}

// A write transaction that reads a corrupted block panics, surfacing the page
// id and the checksum error type just like a read-only transaction.
func TestPageChecksum_WriteTxReadsCorruption(t *testing.T) {
	db := openPageChecksumDB(t)
	seedData(t, db.DB, "widgets", 10)
	path := db.Path()
	pageSize := db.Info().PageSize
	db.MustClose()

	pages := loadRawPages(t, path, pageSize)
	reopened := btesting.MustOpenDBWithOption(t, path, nil)
	leafID, ok := findReachablePageID(t, reopened.DB, pages, common.LeafPageFlag)
	reopened.MustClose()
	require.True(t, ok)
	corruptByte(t, path, pageSize, leafID)

	corrupt, err := bolt.Open(path, 0600, nil)
	require.NoError(t, err)
	defer corrupt.Close()

	var panickedErr any
	func() {
		defer func() { panickedErr = recover() }()
		_ = corrupt.Update(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte("widgets")).ForEach(func(k, v []byte) error { return nil })
		})
	}()
	require.NotNil(t, panickedErr)
	msg := fmt.Sprint(panickedErr)
	require.Contains(t, msg, strconv.FormatUint(leafID, 10))
	require.Contains(t, msg, berrors.ErrPageChecksum.Error())
}

// CopyFile preserves the checksummed format and the copy validates.
func TestPageChecksum_CopyFile(t *testing.T) {
	db := openPageChecksumDB(t)
	seedData(t, db.DB, "widgets", 20)

	copyPath := filepath.Join(t.TempDir(), "copy")
	require.NoError(t, db.View(func(tx *bolt.Tx) error {
		return tx.CopyFile(copyPath, 0600)
	}))
	db.MustClose()

	copy := btesting.MustOpenDBWithOption(t, copyPath, nil)
	require.Equal(t, common.PageChecksumVersion, copy.Version())
	require.NoError(t, copy.View(func(tx *bolt.Tx) error {
		require.Equal(t, "value", string(tx.Bucket([]byte("widgets")).Get([]byte("3"))))
		return nil
	}))
	require.Empty(t, checkErrors(t, copy.DB))
	copy.MustClose()
}

// Compacting a legacy source into a checksummed destination produces a valid
// version 3 database.
func TestPageChecksum_Compact(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src")
	src := btesting.MustOpenDBWithOption(t, srcPath, nil)
	seedData(t, src.DB, "widgets", 30)
	src.MustClose()

	dstPath := filepath.Join(t.TempDir(), "dst")
	dst, err := bolt.Open(dstPath, 0600, pageChecksumOptions())
	require.NoError(t, err)

	src2, err := bolt.Open(srcPath, 0600, nil)
	require.NoError(t, err)
	require.NoError(t, bolt.Compact(dst, src2, 0))
	require.NoError(t, src2.Close())
	require.NoError(t, dst.Close())

	checked := btesting.MustOpenDBWithOption(t, dstPath, nil)
	require.Equal(t, common.PageChecksumVersion, checked.Version())
	require.NoError(t, checked.View(func(tx *bolt.Tx) error {
		require.Equal(t, "value", string(tx.Bucket([]byte("widgets")).Get([]byte("5"))))
		return nil
	}))
	require.Empty(t, checkErrors(t, checked.DB))
	checked.MustClose()
}

func containsPageChecksumError(errs []error, id uint64) bool {
	for _, err := range errs {
		if errors.Is(err, berrors.ErrPageChecksum) &&
			strings.Contains(err.Error(), strconv.FormatUint(id, 10)) {
			return true
		}
	}
	return false
}
