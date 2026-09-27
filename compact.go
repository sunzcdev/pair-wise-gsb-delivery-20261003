package bbolt

import (
	"bytes"
	"encoding/binary"
	"errors"

	"go.etcd.io/bbolt/internal/common"
)

// Compact will create a copy of the source DB and in the destination DB. This may
// reclaim space that the source database no longer has use for. txMaxSize can be
// used to limit the transactions size of this process and may trigger intermittent
// commits. A value of zero will ignore transaction sizes.
// TODO: merge with: https://github.com/etcd-io/etcd/blob/b7f0f52a16dbf83f18ca1d803f7892d750366a94/mvcc/backend/backend.go#L349
func Compact(dst, src *DB, txMaxSize int64) error {
	// commit regularly, or we'll run out of memory for large datasets if using one transaction.
	var size int64
	tx, err := dst.Begin(true)
	if err != nil {
		return err
	}
	defer func() {
		if tempErr := tx.Rollback(); tempErr != nil {
			err = tempErr
		}
	}()

	if err := walk(src, func(keys [][]byte, k, v []byte, seq uint64) error {
		// On each key/value, check if we have exceeded tx size.
		sz := int64(len(k) + len(v))
		if size+sz > txMaxSize && txMaxSize != 0 {
			// Commit previous transaction.
			if err := tx.Commit(); err != nil {
				return err
			}

			// Start new transaction.
			tx, err = dst.Begin(true)
			if err != nil {
				return err
			}
			size = 0
		}
		size += sz

		return putCompacted(tx, keys, k, v, seq)
	}); err != nil {
		return err
	}

	// Remove a stale progress marker in case dst is a leftover target of an
	// interrupted CompactResumable run.
	if err := deleteCompactProgress(tx); err != nil {
		return err
	}
	err = tx.Commit()

	return err
}

// CompactProgressBucketName is the reserved name of the bucket that
// CompactResumable creates in the destination DB to persist its progress.
// Its presence marks the DB as an unfinished compaction target (see
// CompactInterrupted); it is removed by the final commit of a completed
// compaction. Applications must not create a bucket with this name in a
// compaction destination DB.
const CompactProgressBucketName = "__bbolt_compact_progress__"

var (
	compactProgressTxidKey = []byte("src-txid")
	compactProgressPathKey = []byte("path")

	// errCompactSimulatedCrash is returned by compactResumable when the
	// crashAfterItems limit is reached. It is used by tests to simulate a
	// process crash in the middle of a compaction.
	errCompactSimulatedCrash = errors.New("simulated crash during compaction")
)

// CompactResumable copies the source DB into the destination DB like Compact,
// but persists its progress inside dst so that an interrupted compaction
// (e.g. a crashed or killed process) can be resumed by simply calling
// CompactResumable again with the same src and dst.
//
// The progress - the nested bucket path of the last copied key and the source
// transaction ID it depends on - is stored in a reserved bucket named
// CompactProgressBucketName and is committed atomically together with the
// copied data: a commit never advances the data without also advancing the
// recorded progress. The reserved bucket is removed by the final commit, so
// its presence in dst (see CompactInterrupted) identifies dst as an
// unfinished compaction target.
//
// The source DB must remain a stable snapshot for the whole duration of the
// compaction, including across process restarts. Within a single run the copy
// is performed under one read transaction, which does not block writers, but
// between runs the recorded source transaction ID is verified and
// ErrCompactSourceChanged is returned if the source has been modified in the
// meantime: resuming would otherwise splice two different points in time into
// one target DB. In that case dst must be discarded and the compaction
// restarted from an empty dst. To compact a live database, first take a
// consistent snapshot of it (e.g. with Tx.CopyFile) and compact the snapshot
// while no one writes to it.
//
// The destination DB must be empty or an unfinished target of a previous
// CompactResumable run on the same source.
func CompactResumable(dst, src *DB, txMaxSize int64) error {
	return compactResumable(dst, src, txMaxSize, 0)
}

// CompactInterrupted reports whether db is an unfinished compaction target
// left behind by an interrupted CompactResumable call.
func CompactInterrupted(db *DB) (bool, error) {
	var interrupted bool
	if err := db.View(func(tx *Tx) error {
		interrupted = tx.Bucket([]byte(CompactProgressBucketName)) != nil
		return nil
	}); err != nil {
		return false, err
	}
	return interrupted, nil
}

// compactResumable implements CompactResumable. crashAfterItems is used by
// tests to simulate a crash: after copying that many items the current
// transaction is rolled back and errCompactSimulatedCrash is returned.
func compactResumable(dst, src *DB, txMaxSize int64, crashAfterItems int) error {
	// Hold a single read transaction on the source for the whole run so that
	// everything copied in this run belongs to the same source snapshot.
	return src.View(func(srcTx *Tx) error {
		srcTxid := srcTx.meta.Txid()

		resumePath, err := loadCompactProgress(dst, srcTxid)
		if err != nil {
			return err
		}

		// commit regularly, or we'll run out of memory for large datasets if using one transaction.
		var size int64
		var copied int
		var lastPath [][]byte // path of the last item written in the current tx

		tx, err := dst.Begin(true)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()

		// Mark dst as an unfinished compaction target and record the source
		// version the progress depends on. The marker is committed together
		// with the copied data and removed by the final commit.
		if err := writeCompactProgress(tx, srcTxid, resumePath); err != nil {
			return err
		}

		if err := walkTx(srcTx, resumePath, func(keys [][]byte, k, v []byte, seq uint64) error {
			// On each key/value, check if we have exceeded tx size.
			sz := int64(len(k) + len(v))
			if size+sz > txMaxSize && txMaxSize != 0 {
				// Persist the progress in the same transaction as the data
				// it describes, so that a commit always advances both
				// atomically.
				if lastPath != nil {
					if err := writeCompactProgress(tx, srcTxid, lastPath); err != nil {
						return err
					}
				}

				// Commit previous transaction.
				if err := tx.Commit(); err != nil {
					return err
				}

				// Start new transaction.
				tx, err = dst.Begin(true)
				if err != nil {
					return err
				}
				size = 0
				lastPath = nil
			}
			size += sz

			if err := putCompacted(tx, keys, k, v, seq); err != nil {
				return err
			}
			lastPath = appendKeyPath(keys, k)

			copied++
			if crashAfterItems > 0 && copied >= crashAfterItems {
				return errCompactSimulatedCrash
			}
			return nil
		}); err != nil {
			return err
		}

		// Final commit: drop the progress marker so that the finished target
		// no longer identifies as an unfinished compaction.
		if err := deleteCompactProgress(tx); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// putCompacted writes a single walked item (a bucket or a key/value pair)
// into dst's current transaction.
func putCompacted(tx *Tx, keys [][]byte, k, v []byte, seq uint64) error {
	// Create bucket on the root transaction if this is the first level.
	nk := len(keys)
	if nk == 0 {
		bkt, err := tx.CreateBucket(k)
		if err != nil {
			return err
		}
		return bkt.SetSequence(seq)
	}

	// Create buckets on subsequent levels, if necessary.
	b := tx.Bucket(keys[0])
	if nk > 1 {
		for _, k := range keys[1:] {
			b = b.Bucket(k)
		}
	}

	// Fill the entire page for best compaction.
	b.FillPercent = 1.0

	// If there is no value then this is a bucket call.
	if v == nil {
		bkt, err := b.CreateBucket(k)
		if err != nil {
			return err
		}
		return bkt.SetSequence(seq)
	}

	// Otherwise treat it as a key/value pair.
	return b.Put(k, v)
}

// loadCompactProgress reads the progress record stored in dst by a previous
// CompactResumable run. It returns the path of the last copied key, or nil if
// dst carries no usable progress. It returns ErrCompactSourceChanged if the
// recorded source transaction ID does not match srcTxid.
func loadCompactProgress(dst *DB, srcTxid common.Txid) ([][]byte, error) {
	var resumePath [][]byte
	if err := dst.View(func(tx *Tx) error {
		b := tx.Bucket([]byte(CompactProgressBucketName))
		if b == nil {
			return nil
		}
		var storedTxid common.Txid
		if v := b.Get(compactProgressTxidKey); len(v) == 8 {
			storedTxid = common.Txid(binary.BigEndian.Uint64(v))
		}
		if storedTxid != srcTxid {
			return ErrCompactSourceChanged
		}
		resumePath = decodeCompactPath(b.Get(compactProgressPathKey))
		return nil
	}); err != nil {
		return nil, err
	}
	return resumePath, nil
}

// writeCompactProgress records the source transaction ID and the path of the
// last copied key in dst's current transaction.
func writeCompactProgress(tx *Tx, srcTxid common.Txid, path [][]byte) error {
	b, err := tx.CreateBucketIfNotExists([]byte(CompactProgressBucketName))
	if err != nil {
		return err
	}
	var txidBuf [8]byte
	binary.BigEndian.PutUint64(txidBuf[:], uint64(srcTxid))
	if err := b.Put(compactProgressTxidKey, txidBuf[:]); err != nil {
		return err
	}
	return b.Put(compactProgressPathKey, encodeCompactPath(path))
}

// deleteCompactProgress removes the progress marker from dst's current
// transaction, if present.
func deleteCompactProgress(tx *Tx) error {
	if tx.Bucket([]byte(CompactProgressBucketName)) == nil {
		return nil
	}
	return tx.DeleteBucket([]byte(CompactProgressBucketName))
}

// encodeCompactPath encodes a nested bucket path into a byte slice.
func encodeCompactPath(path [][]byte) []byte {
	var buf []byte
	buf = binary.AppendUvarint(buf, uint64(len(path)))
	for _, seg := range path {
		buf = binary.AppendUvarint(buf, uint64(len(seg)))
		buf = append(buf, seg...)
	}
	return buf
}

// decodeCompactPath decodes a path encoded by encodeCompactPath. It returns
// nil for empty or malformed input.
func decodeCompactPath(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	n, m := binary.Uvarint(data)
	if m <= 0 {
		return nil
	}
	data = data[m:]
	var path [][]byte
	for i := uint64(0); i < n; i++ {
		l, s := binary.Uvarint(data)
		if s <= 0 || uint64(len(data)-s) < l {
			return nil
		}
		seg := make([]byte, l)
		copy(seg, data[s:s+int(l)])
		path = append(path, seg)
		data = data[s+int(l):]
	}
	return path
}

// appendKeyPath returns a deep copy of keypath with k appended.
func appendKeyPath(keypath [][]byte, k []byte) [][]byte {
	path := make([][]byte, 0, len(keypath)+1)
	for _, key := range keypath {
		path = append(path, append([]byte(nil), key...))
	}
	return append(path, append([]byte(nil), k...))
}

// compareKeyPaths compares two nested bucket paths segment by segment. A path
// that is a prefix of another one sorts first. This matches the depth-first
// pre-order used by walkTx, so a path can be used as a resume position.
func compareKeyPaths(a, b [][]byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := bytes.Compare(a[i], b[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	default:
		return 0
	}
}

// walkFunc is the type of the function called for keys (buckets and "normal"
// values) discovered by Walk. keys is the list of keys to descend to the bucket
// owning the discovered key/value pair k/v.
type walkFunc func(keys [][]byte, k, v []byte, seq uint64) error

// walk walks recursively the bolt database db, calling walkFn for each key it finds.
func walk(db *DB, walkFn walkFunc) error {
	return db.View(func(tx *Tx) error {
		return walkTx(tx, nil, walkFn)
	})
}

// walkTx walks recursively all buckets in tx, calling walkFn for each key
// whose full path sorts strictly after resumePath. A nil resumePath walks
// from the beginning. Keys at or before resumePath are skipped, but buckets
// among them are still descended into, because their children may sort after
// resumePath.
func walkTx(tx *Tx, resumePath [][]byte, walkFn walkFunc) error {
	return tx.ForEach(func(name []byte, b *Bucket) error {
		return walkBucket(b, nil, name, nil, b.Sequence(), resumePath, walkFn)
	})
}

func walkBucket(b *Bucket, keypath [][]byte, k, v []byte, seq uint64, resumePath [][]byte, fn walkFunc) error {
	// Execute callback, unless this key was already copied in a previous run.
	if len(resumePath) == 0 ||
		compareKeyPaths(append(keypath[:len(keypath):len(keypath)], k), resumePath) > 0 {
		if err := fn(keypath, k, v, seq); err != nil {
			return err
		}
	}

	// If this is not a bucket then stop.
	if v != nil {
		return nil
	}

	// Iterate over each child key/value.
	keypath = append(keypath, k)
	return b.ForEach(func(k, v []byte) error {
		if v == nil {
			bkt := b.Bucket(k)
			return walkBucket(bkt, keypath, k, nil, bkt.Sequence(), resumePath, fn)
		}
		return walkBucket(b, keypath, k, v, b.Sequence(), resumePath, fn)
	})
}
