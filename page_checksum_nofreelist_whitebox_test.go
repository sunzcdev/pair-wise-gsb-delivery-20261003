package bbolt

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.etcd.io/bbolt/internal/common"
)

// With NoFreelistSync the freelist is reconstructed on open by scanning every
// data page; those pages must still validate their trailers.
func TestPageChecksum_NoFreelistSync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, err := Open(path, 0600, &Options{
		PageChecksum:   true,
		NoFreelistSync: true,
	})
	require.NoError(t, err)

	for i := 0; i < 50; i++ {
		require.NoError(t, db.Update(func(tx *Tx) error {
			b, err := tx.CreateBucketIfNotExists([]byte("b"))
			require.NoError(t, err)
			return b.Put([]byte(itob(i)), []byte("v"))
		}))
	}
	require.NoError(t, db.Close())

	reopened, err := Open(path, 0600, &Options{NoFreelistSync: true})
	require.NoError(t, err)
	require.True(t, reopened.PageChecksum())
	require.Equal(t, common.PageChecksumVersion, reopened.Version())
	require.NoError(t, reopened.View(func(tx *Tx) error {
		require.Equal(t, "v", string(tx.Bucket([]byte("b")).Get([]byte(itob(7)))))
		return nil
	}))
	require.NoError(t, reopened.Close())
}

func itob(i int) []byte {
	return []byte{byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)}
}
