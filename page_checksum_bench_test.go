package bbolt_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	bolt "go.etcd.io/bbolt"
)

// BenchmarkPageChecksumWrite measures the write cost of sealing a checksum
// trailer per committed block, comparing the legacy format with the
// checksumming format.
func BenchmarkPageChecksumWrite(b *testing.B) {
	for _, checksum := range []bool{false, true} {
		name := "legacy"
		if checksum {
			name = "checksum"
		}
		b.Run(name, func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "db")
			db, err := bolt.Open(path, 0600, &bolt.Options{PageChecksum: checksum})
			require.NoError(b, err)
			b.Cleanup(func() { _ = db.Close() })

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				err := db.Update(func(tx *bolt.Tx) error {
					bk, err := tx.CreateBucketIfNotExists([]byte("bench"))
					if err != nil {
						return err
					}
					return bk.Put([]byte(fmt.Sprintf("k%08d", i)), make([]byte, 128))
				})
				require.NoError(b, err)
			}
		})
	}
}

// BenchmarkPageChecksumRead measures the read cost of verifying the checksum
// trailer of every disk-backed block on access.
func BenchmarkPageChecksumRead(b *testing.B) {
	for _, checksum := range []bool{false, true} {
		name := "legacy"
		if checksum {
			name = "checksum"
		}
		b.Run(name, func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "db")
			db, err := bolt.Open(path, 0600, &bolt.Options{PageChecksum: checksum})
			require.NoError(b, err)

			const keys = 2000
			require.NoError(b, db.Update(func(tx *bolt.Tx) error {
				bk, err := tx.CreateBucket([]byte("bench"))
				if err != nil {
					return err
				}
				for i := 0; i < keys; i++ {
					if err := bk.Put([]byte(fmt.Sprintf("k%08d", i)), make([]byte, 128)); err != nil {
						return err
					}
				}
				return nil
			}))
			require.NoError(b, db.Close())

			db, err = bolt.Open(path, 0600, nil)
			require.NoError(b, err)
			b.Cleanup(func() { _ = db.Close() })

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				require.NoError(b, db.View(func(tx *bolt.Tx) error {
					bk := tx.Bucket([]byte("bench"))
					for j := 0; j < keys; j++ {
						if v := bk.Get([]byte(fmt.Sprintf("k%08d", j))); v == nil {
							return fmt.Errorf("missing key %d", j)
						}
					}
					return nil
				}))
			}
		})
	}
}
