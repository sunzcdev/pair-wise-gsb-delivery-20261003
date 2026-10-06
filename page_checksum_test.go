package bbolt_test

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"

	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
	"go.etcd.io/bbolt/internal/common"
)

// checksumOptions returns the options used by the page checksum tests.
// The page size is pinned so that tests can compute file offsets.
func checksumOptions(enabled bool) *bolt.Options {
	return &bolt.Options{
		PageSize:     pageSize,
		PageChecksum: enabled,
	}
}

// fillChecksumDB writes enough data to produce branch, leaf, overflow and
// freelist pages.
func fillChecksumDB(t *testing.T, db *bolt.DB) {
	t.Helper()

	// Small values: force the b-tree to split into branch + leaf pages.
	err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket([]byte("data"))
		if err != nil {
			return err
		}
		for i := 0; i < 2000; i++ {
			if err := b.Put([]byte(fmt.Sprintf("key-%06d", i)), []byte(fmt.Sprintf("value-%06d", i))); err != nil {
				return err
			}
		}
		// A large value which requires overflow pages.
		if err := b.Put([]byte("large"), make([]byte, 3*pageSize)); err != nil {
			return err
		}
		return nil
	})
	require.NoError(t, err)

	// Delete some keys and commit again so that the freelist is populated
	// and persisted.
	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("data"))
		for i := 0; i < 500; i++ {
			if err := b.Delete([]byte(fmt.Sprintf("key-%06d", i*2))); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
}

// checkErrors runs Tx.Check and returns all reported errors.
func checkErrors(t *testing.T, db *bolt.DB) []error {
	t.Helper()
	var errs []error
	err := db.View(func(tx *bolt.Tx) error {
		for chkErr := range tx.Check() {
			errs = append(errs, chkErr)
		}
		return nil
	})
	require.NoError(t, err)
	return errs
}

// findPageId returns the id of the first page of the given type
// ("leaf", "branch", "freelist", ...) with at least minOverflow overflow
// pages.
func findPageId(t *testing.T, db *bolt.DB, pageType string, minOverflow int) int {
	t.Helper()
	id := -1
	err := db.View(func(tx *bolt.Tx) error {
		for i := 0; i < int(tx.Size())/pageSize; i++ {
			info, err := tx.Page(i)
			if err != nil {
				return err
			}
			if info == nil || info.Type != pageType || info.OverflowCount < minOverflow {
				continue
			}
			id = i
			return nil
		}
		return nil
	})
	require.NoError(t, err)
	require.NotEqual(t, -1, id, fmt.Sprintf("no page of type %q found", pageType))
	return id
}

// flipByteInPage flips a single byte at the given offset inside a page of
// the database file.
func flipByteInPage(t *testing.T, path string, pageId int, offsetInPage int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()

	offset := int64(pageId*pageSize) + offsetInPage
	buf := make([]byte, 1)
	_, err = f.ReadAt(buf, offset)
	require.NoError(t, err)
	buf[0] ^= 0xff
	_, err = f.WriteAt(buf, offset)
	require.NoError(t, err)
	require.NoError(t, f.Sync())
}

// readMetaVersion reads the version field of the valid meta page with the
// highest transaction id.
func readMetaVersion(t *testing.T, path string) uint32 {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()

	var version uint32
	var maxTxid common.Txid
	for i := 0; i < 2; i++ {
		buf := make([]byte, pageSize)
		if _, err := f.ReadAt(buf, int64(i*pageSize)); err != nil {
			continue
		}
		m := (*common.Page)(unsafe.Pointer(&buf[0])).Meta()
		if m.Validate() == nil && m.Txid() >= maxTxid {
			maxTxid = m.Txid()
			version = m.Version()
		}
	}
	return version
}

// requireChecksumErr asserts that errs contains a page checksum error for
// the given page id.
func requireChecksumErr(t *testing.T, errs []error, pageId int) {
	t.Helper()
	for _, err := range errs {
		if errors.Is(err, berrors.ErrPageChecksum) && strings.Contains(err.Error(), fmt.Sprintf("page %d", pageId)) {
			return
		}
	}
	t.Fatalf("expected a page checksum error for page %d, got: %v", pageId, errs)
}

// Ensure the meta version validation accepts both the default format and
// the page checksum format, and rejects anything else.
func TestPageChecksum_MetaVersionValidation(t *testing.T) {
	newMeta := func(v uint32) *common.Meta {
		m := &common.Meta{}
		m.SetMagic(common.Magic)
		m.SetVersion(v)
		m.SetChecksum(m.Sum64())
		return m
	}

	require.NoError(t, newMeta(common.Version).Validate())
	require.False(t, newMeta(common.Version).SupportsPageChecksum())

	require.NoError(t, newMeta(common.VersionPageChecksum).Validate())
	require.True(t, newMeta(common.VersionPageChecksum).SupportsPageChecksum())

	require.ErrorIs(t, newMeta(99).Validate(), berrors.ErrVersionMismatch)

	// Older binaries only accept common.Version, so they reject
	// checksum-enabled databases with a clear ErrVersionMismatch error.
	require.NotEqual(t, common.Version, common.VersionPageChecksum)
}

// Ensure a database created with page checksums round-trips data and
// passes the consistency check, across reopens.
func TestPageChecksum_CreateAndReopen(t *testing.T) {
	path := tempfile()
	defer os.RemoveAll(path)

	db, err := bolt.Open(path, 0600, checksumOptions(true))
	require.NoError(t, err)
	fillChecksumDB(t, db)

	// The database must use the new format version.
	require.Equal(t, uint32(common.VersionPageChecksum), readMetaVersion(t, path))

	// All pages (leaf, branch, overflow, freelist) must verify.
	require.Empty(t, checkErrors(t, db))
	require.NoError(t, db.Close())

	// Reopen and verify the data and the checksums again.
	db, err = bolt.Open(path, 0600, checksumOptions(false))
	require.NoError(t, err)
	defer db.Close()

	require.Empty(t, checkErrors(t, db))
	require.NoError(t, db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("data"))
		require.NotNil(t, b)
		require.Equal(t, []byte("value-000001"), b.Get([]byte("key-000001")))
		require.Equal(t, 3*pageSize, len(b.Get([]byte("large"))))
		return nil
	}))
}

// Ensure a bit flip in a leaf page is detected and reported with the page
// id and the error type.
func TestPageChecksum_BitFlipLeafPage(t *testing.T) {
	path := tempfile()
	defer os.RemoveAll(path)

	db, err := bolt.Open(path, 0600, checksumOptions(true))
	require.NoError(t, err)
	fillChecksumDB(t, db)
	leafId := findPageId(t, db, "leaf", 0)
	require.NoError(t, db.Close())

	// Flip one byte near the end of the page payload (before the trailer).
	flipByteInPage(t, path, leafId, pageSize-16)

	db, err = bolt.Open(path, 0600, checksumOptions(true))
	require.NoError(t, err)
	defer db.Close()

	requireChecksumErr(t, checkErrors(t, db), leafId)
}

// Ensure a bit flip in an overflow page is detected via the checksum of
// its head page.
func TestPageChecksum_BitFlipOverflowPage(t *testing.T) {
	path := tempfile()
	defer os.RemoveAll(path)

	db, err := bolt.Open(path, 0600, checksumOptions(true))
	require.NoError(t, err)
	fillChecksumDB(t, db)
	leafId := findPageId(t, db, "leaf", 1)
	require.NoError(t, db.Close())

	// Flip one byte inside the first overflow page of the leaf page run.
	flipByteInPage(t, path, leafId+1, 100)

	db, err = bolt.Open(path, 0600, checksumOptions(true))
	require.NoError(t, err)
	defer db.Close()

	requireChecksumErr(t, checkErrors(t, db), leafId)
}

// Ensure a bit flip in the freelist page is detected. The database must
// still open: the freelist is rebuilt by scanning instead of trusting the
// corrupted page.
func TestPageChecksum_BitFlipFreelistPage(t *testing.T) {
	path := tempfile()
	defer os.RemoveAll(path)

	db, err := bolt.Open(path, 0600, checksumOptions(true))
	require.NoError(t, err)
	fillChecksumDB(t, db)
	freelistId := findPageId(t, db, "freelist", 0)
	require.NoError(t, db.Close())

	flipByteInPage(t, path, freelistId, pageSize-16)

	// The database opens fine because the freelist is rebuilt by scanning.
	db, err = bolt.Open(path, 0600, checksumOptions(true))
	require.NoError(t, err)
	defer db.Close()

	requireChecksumErr(t, checkErrors(t, db), freelistId)

	// The database is still writable.
	require.NoError(t, db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("data")).Put([]byte("after-corruption"), []byte("ok"))
	}))
}

// Ensure databases created without page checksums (format version 2) keep
// working unchanged and are never upgraded in place.
func TestPageChecksum_OldVersionCompat(t *testing.T) {
	path := tempfile()
	defer os.RemoveAll(path)

	db, err := bolt.Open(path, 0600, checksumOptions(false))
	require.NoError(t, err)
	fillChecksumDB(t, db)
	require.Equal(t, uint32(common.Version), readMetaVersion(t, path))
	require.NoError(t, db.Close())

	// Reopen without the option: everything works, no checksum verification
	// happens and commits keep the old format version.
	db, err = bolt.Open(path, 0600, checksumOptions(false))
	require.NoError(t, err)
	require.Empty(t, checkErrors(t, db))
	require.NoError(t, db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("data")).Put([]byte("new-key"), []byte("new-value"))
	}))
	require.Equal(t, uint32(common.Version), readMetaVersion(t, path))
	require.NoError(t, db.Close())

	// Requesting page checksums on a database created without them fails
	// with a clear error instead of silently running unprotected.
	_, err = bolt.Open(path, 0600, checksumOptions(true))
	require.ErrorIs(t, err, berrors.ErrPageChecksumNotEnabled)
}

// Ensure StrictMode (which runs Tx.Check after writing dirty pages but
// before writing the meta) works together with page checksums: pages
// constructed by the write transaction are stamped at commit time and
// verify cleanly.
func TestPageChecksum_StrictMode(t *testing.T) {
	path := tempfile()
	defer os.RemoveAll(path)

	db, err := bolt.Open(path, 0600, checksumOptions(true))
	require.NoError(t, err)
	db.StrictMode = true
	defer db.Close()

	fillChecksumDB(t, db)
}

// Ensure copies and compactions of checksum-enabled databases keep
// checksums intact.
func TestPageChecksum_CopyAndCompact(t *testing.T) {
	path := tempfile()
	defer os.RemoveAll(path)

	db, err := bolt.Open(path, 0600, checksumOptions(true))
	require.NoError(t, err)
	fillChecksumDB(t, db)

	// Tx.CopyFile copies the raw file, checksum trailers included.
	copyPath := tempfile()
	defer os.RemoveAll(copyPath)
	require.NoError(t, db.View(func(tx *bolt.Tx) error {
		return tx.CopyFile(copyPath, 0600)
	}))

	copyDB, err := bolt.Open(copyPath, 0600, checksumOptions(true))
	require.NoError(t, err)
	require.Empty(t, checkErrors(t, copyDB))
	require.Equal(t, uint32(common.VersionPageChecksum), readMetaVersion(t, copyPath))
	require.NoError(t, copyDB.Close())

	// Compact into a checksum-enabled destination.
	compactPath := tempfile()
	defer os.RemoveAll(compactPath)
	compactDB, err := bolt.Open(compactPath, 0600, checksumOptions(true))
	require.NoError(t, err)
	require.NoError(t, bolt.Compact(compactDB, db, 0))
	require.Empty(t, checkErrors(t, compactDB))
	require.NoError(t, compactDB.Close())

	require.NoError(t, db.Close())
}

// BenchmarkPageChecksum_Put measures the write overhead of page checksums.
func BenchmarkPageChecksum_Put(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		b.Run(fmt.Sprintf("checksum=%v", enabled), func(b *testing.B) {
			path := tempfile()
			defer os.RemoveAll(path)

			db, err := bolt.Open(path, 0600, &bolt.Options{PageSize: pageSize, PageChecksum: enabled, NoSync: true})
			require.NoError(b, err)
			defer db.Close()

			require.NoError(b, db.Update(func(tx *bolt.Tx) error {
				_, err := tx.CreateBucket([]byte("bench"))
				return err
			}))

			value := make([]byte, 100)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				require.NoError(b, db.Update(func(tx *bolt.Tx) error {
					bkt := tx.Bucket([]byte("bench"))
					for j := 0; j < 100; j++ {
						if err := bkt.Put([]byte(fmt.Sprintf("key-%010d", i*100+j)), value); err != nil {
							return err
						}
					}
					return nil
				}))
			}
			b.StopTimer()
			b.ReportMetric(float64(b.Elapsed())/float64(b.N*100), "ns/put")
		})
	}
}

// BenchmarkPageChecksum_Check measures the read/verification overhead of
// page checksums when running Tx.Check.
func BenchmarkPageChecksum_Check(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		b.Run(fmt.Sprintf("checksum=%v", enabled), func(b *testing.B) {
			path := tempfile()
			defer os.RemoveAll(path)

			db, err := bolt.Open(path, 0600, &bolt.Options{PageSize: pageSize, PageChecksum: enabled, NoSync: true})
			require.NoError(b, err)
			defer db.Close()

			require.NoError(b, db.Update(func(tx *bolt.Tx) error {
				bkt, err := tx.CreateBucket([]byte("bench"))
				if err != nil {
					return err
				}
				value := make([]byte, 100)
				for i := 0; i < 20000; i++ {
					if err := bkt.Put([]byte(fmt.Sprintf("key-%010d", i)), value); err != nil {
						return err
					}
				}
				return nil
			}))

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				require.NoError(b, db.View(func(tx *bolt.Tx) error {
					for chkErr := range tx.Check() {
						return chkErr
					}
					return nil
				}))
			}
		})
	}
}
