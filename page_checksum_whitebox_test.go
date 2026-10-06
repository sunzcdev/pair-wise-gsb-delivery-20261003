package bbolt

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	berrors "go.etcd.io/bbolt/errors"
	"go.etcd.io/bbolt/internal/common"
)

func newPageChecksumDB(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db")
	db, err := Open(path, 0600, &Options{PageChecksum: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

// A new database created with the option is format version 3 and reports the
// checksumming mode.
func TestPageChecksum_NewDBFormat(t *testing.T) {
	db, _ := newPageChecksumDB(t)
	require.Equal(t, common.PageChecksumVersion, db.Version())
	require.True(t, db.PageChecksum())
}

// A legacy database created without the option stays on version 2 and reports
// checksumming disabled.
func TestPageChecksum_LegacyDBFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, err := Open(path, 0600, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	require.Equal(t, common.Version, db.Version())
	require.False(t, db.PageChecksum())
}

// Requesting checksumming on an existing legacy file is rejected; there is no
// in-place upgrade.
func TestPageChecksum_RejectUpgradeLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, err := Open(path, 0600, nil)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = Open(path, 0600, &Options{PageChecksum: true})
	require.ErrorIs(t, err, berrors.ErrPageChecksumNotSupported)
}

// Dirty blocks built inside a write transaction are unsealed while the
// transaction is in progress: reading them must not consult a (garbage)
// trailer. VerifyDirtyPageChecksums seals them in place, exactly like the
// write path does right before writing to disk.
func TestPageChecksum_UncommittedDirtyPages(t *testing.T) {
	db, _ := newPageChecksumDB(t)

	tx, err := db.Begin(true)
	require.NoError(t, err)

	bucket, err := tx.CreateBucket([]byte("widgets"))
	require.NoError(t, err)
	require.NoError(t, bucket.Put([]byte("foo"), []byte("bar")))

	// The freshly allocated leaf block is dirty and unsealed: its trailer is
	// zeroed buffer memory, yet reading through the transaction still works
	// because dirty pages bypass disk verification.
	require.NoError(t, bucket.ForEach(func(k, v []byte) error {
		require.Equal(t, "foo", string(k))
		require.Equal(t, "bar", string(v))
		return nil
	}))

	// Sealing the uncommitted buffers validates them without touching disk.
	require.NoError(t, tx.VerifyDirtyPageChecksums())
	// Sealing is idempotent.
	require.NoError(t, tx.VerifyDirtyPageChecksums())

	// Commit the sealed buffers; the write path seals them again and writes
	// them to disk.
	require.NoError(t, tx.Commit())

	// After commit the same blocks are disk-backed and go through the trailer
	// verification path.
	require.NoError(t, db.Update(func(tx *Tx) error {
		b := tx.Bucket([]byte("widgets"))
		require.NotNil(t, b)
		return b.Put([]byte("baz"), []byte("qux"))
	}))
	require.NoError(t, db.View(func(tx *Tx) error {
		require.Equal(t, "bar", string(tx.Bucket([]byte("widgets")).Get([]byte("foo"))))
		require.Equal(t, "qux", string(tx.Bucket([]byte("widgets")).Get([]byte("baz"))))
		return nil
	}))
}

// The payload capacity of a block in a checksumming database is exactly one
// trailer short; legacy blocks keep their full physical capacity.
func TestPageChecksum_PagesNeededForPayload(t *testing.T) {
	ps := os.Getpagesize()

	legacy, err := Open(filepath.Join(t.TempDir(), "legacy"), 0600, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = legacy.Close() })
	require.Equal(t, ps, legacy.pageSize)
	require.Equal(t, 1, legacy.pagesNeededForPayload(ps))

	checksummed, _ := newPageChecksumDB(t)
	// A full physical page of payload needs 2 pages because 8 bytes are
	// reserved for the trailer.
	require.Equal(t, 2, checksummed.pagesNeededForPayload(ps))
	require.Equal(t, 1, checksummed.pagesNeededForPayload(ps-common.PageTrailerSize))
	require.Equal(t, ps-common.PageTrailerSize, checksummed.usableBlockSize(0))
	require.Equal(t, 2*ps-common.PageTrailerSize, checksummed.usableBlockSize(1))
}
