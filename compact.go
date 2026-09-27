package bbolt

import (
	"bytes"
	"encoding/binary"
	"fmt"

	berrors "go.etcd.io/bbolt/errors"
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

		// Create bucket on the root transaction if this is the first level.
		nk := len(keys)
		if nk == 0 {
			bkt, err := tx.CreateBucket(k)
			if err != nil {
				return err
			}
			if err := bkt.SetSequence(seq); err != nil {
				return err
			}
			return nil
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
			if err := bkt.SetSequence(seq); err != nil {
				return err
			}
			return nil
		}

		// Otherwise treat it as a key/value pair.
		return b.Put(k, v)
	}); err != nil {
		return err
	}
	err = tx.Commit()

	return err
}

// compactProgressBucket is the name of the reserved root bucket that
// CompactResumable maintains in the destination DB to persist its progress.
// Its presence marks the destination as an incomplete compaction result.
const compactProgressBucket = "__bbolt_compact_progress__"

const (
	compactProgressSrcTxIDKey = "srcTxID"
	compactProgressPathKey    = "path"
)

// compactWriteHook, when set, is invoked by CompactResumable after each item
// has been written to the destination transaction, receiving the number of
// items written so far. It is used by tests to simulate an interruption.
var compactWriteHook func(n int) error

// CompactResumable copies the source DB into the destination DB like Compact,
// but persists its progress (the nested bucket path of the last copied key
// and the txid of the source snapshot it is copying from) inside the
// destination DB itself, in the reserved root bucket
// "__bbolt_compact_progress__". The progress record is written in the same
// transaction as the copied data, so a committed destination transaction
// always carries a progress record that exactly matches its contents.
//
// If a previous run was interrupted (crash, kill, error), calling
// CompactResumable again with the same destination resumes the copy where it
// stopped instead of starting over. An incomplete destination is always
// recognizable by the presence of the progress bucket (see CompactInProgress)
// and must not be used for anything else; once the copy finishes, the
// progress bucket is removed in the final commit and the destination is a
// regular, fully consistent bbolt database.
//
// Stable source snapshot constraint: the copy is performed against the source
// snapshot observed by the first (interrupted) run, identified by the source
// txid recorded in the progress record. The source database must not be
// modified (no successful write transaction) between the first call and the
// completion of the copy. If the source txid has changed when resuming,
// CompactResumable refuses to splice two points in time into one destination
// and returns an error wrapping errors.ErrCompactSourceChanged; the
// destination must then be discarded and the compaction restarted from
// scratch. Within a single run the whole copy is performed under one source
// read transaction, so it always reflects a single consistent snapshot.
//
// The destination must be either empty or an incomplete destination of a
// previous CompactResumable run; anything else is rejected with
// errors.ErrCompactDstNotEmpty. The source must not contain a root bucket
// named "__bbolt_compact_progress__" (errors.ErrCompactReservedBucket).
func CompactResumable(dst, src *DB, txMaxSize int64) error {
	// Load the progress recorded by a previous interrupted run, if any.
	var recordedTxID uint64
	var resumePath [][]byte
	hasProgress := false
	if err := dst.View(func(tx *Tx) error {
		pb := tx.Bucket([]byte(compactProgressBucket))
		if pb == nil {
			// No progress record: only an empty destination is acceptable.
			if k, _ := tx.Cursor().First(); k != nil {
				return berrors.ErrCompactDstNotEmpty
			}
			return nil
		}
		hasProgress = true
		if v := pb.Get([]byte(compactProgressSrcTxIDKey)); len(v) == 8 {
			recordedTxID = binary.BigEndian.Uint64(v)
		}
		resumePath = decodeCompactPath(pb.Get([]byte(compactProgressPathKey)))
		return nil
	}); err != nil {
		return err
	}

	// The whole copy runs under a single source read transaction so that it
	// reflects one consistent snapshot of the source.
	return src.View(func(stx *Tx) error {
		if stx.Bucket([]byte(compactProgressBucket)) != nil {
			return berrors.ErrCompactReservedBucket
		}
		srcTxID := uint64(stx.ID())
		if hasProgress && recordedTxID != srcTxID {
			return fmt.Errorf("%w (recorded source txid %d, current txid %d)",
				berrors.ErrCompactSourceChanged, recordedTxID, srcTxID)
		}
		var txIDBytes [8]byte
		binary.BigEndian.PutUint64(txIDBytes[:], srcTxID)

		dtx, err := dst.Begin(true)
		if err != nil {
			return err
		}
		committed := false
		defer func() {
			if !committed {
				_ = dtx.Rollback()
			}
		}()

		// lastPath is the key path of the last item written to dtx.
		var lastPath [][]byte
		writeProgress := func() error {
			pb, err := dtx.CreateBucketIfNotExists([]byte(compactProgressBucket))
			if err != nil {
				return err
			}
			if err := pb.Put([]byte(compactProgressSrcTxIDKey), txIDBytes[:]); err != nil {
				return err
			}
			return pb.Put([]byte(compactProgressPathKey), encodeCompactPath(lastPath))
		}

		var size int64
		written := 0
		walkErr := walkFrom(stx, resumePath, func(keys [][]byte, k, v []byte, seq uint64) error {
			// On each key/value, check if we have exceeded tx size.
			sz := int64(len(k) + len(v))
			if size+sz > txMaxSize && txMaxSize != 0 {
				// Persist the progress in the same transaction as the
				// data it describes, then commit both atomically.
				if err := writeProgress(); err != nil {
					return err
				}
				if err := dtx.Commit(); err != nil {
					return err
				}

				// Start new transaction.
				if dtx, err = dst.Begin(true); err != nil {
					return err
				}
				size = 0
			}
			size += sz

			// Create bucket on the root transaction if this is the first level.
			nk := len(keys)
			if nk == 0 {
				bkt, err := dtx.CreateBucketIfNotExists(k)
				if err != nil {
					return err
				}
				if err := bkt.SetSequence(seq); err != nil {
					return err
				}
			} else {
				// Locate the parent bucket created earlier.
				b := dtx.Bucket(keys[0])
				for _, k := range keys[1:] {
					b = b.Bucket(k)
				}

				// Fill the entire page for best compaction.
				b.FillPercent = 1.0

				if v == nil {
					// There is no value: this is a bucket call.
					bkt, err := b.CreateBucketIfNotExists(k)
					if err != nil {
						return err
					}
					if err := bkt.SetSequence(seq); err != nil {
						return err
					}
				} else if err := b.Put(k, v); err != nil {
					// Otherwise treat it as a key/value pair.
					return err
				}
			}

			// Record the position of the copied item.
			p := make([][]byte, 0, len(keys)+1)
			p = append(p, keys...)
			lastPath = append(p, k)

			written++
			if compactWriteHook != nil {
				return compactWriteHook(written)
			}
			return nil
		})
		if walkErr != nil {
			return walkErr
		}

		// The copy is complete: remove the progress record so the
		// destination is no longer marked as incomplete, and commit.
		if err := dtx.DeleteBucket([]byte(compactProgressBucket)); err != nil && err != berrors.ErrBucketNotFound {
			return err
		}
		if err := dtx.Commit(); err != nil {
			return err
		}
		committed = true
		return nil
	})
}

// CompactInProgress reports whether db is the incomplete destination of an
// interrupted CompactResumable run. Such a database must only be used as the
// destination of a resumed CompactResumable call, or be discarded.
func CompactInProgress(db *DB) (bool, error) {
	inProgress := false
	err := db.View(func(tx *Tx) error {
		inProgress = tx.Bucket([]byte(compactProgressBucket)) != nil
		return nil
	})
	return inProgress, err
}

// encodeCompactPath encodes a nested bucket key path into a flat byte slice.
func encodeCompactPath(path [][]byte) []byte {
	var n int
	for _, p := range path {
		n += 4 + len(p)
	}
	buf := make([]byte, 0, n)
	var lb [4]byte
	for _, p := range path {
		binary.BigEndian.PutUint32(lb[:], uint32(len(p)))
		buf = append(buf, lb[:]...)
		buf = append(buf, p...)
	}
	return buf
}

// decodeCompactPath decodes a key path encoded by encodeCompactPath.
func decodeCompactPath(data []byte) [][]byte {
	var path [][]byte
	for len(data) >= 4 {
		n := binary.BigEndian.Uint32(data)
		data = data[4:]
		if uint64(n) > uint64(len(data)) {
			return nil
		}
		p := make([]byte, n)
		copy(p, data[:n])
		path = append(path, p)
		data = data[n:]
	}
	return path
}

// walkFunc is the type of the function called for keys (buckets and "normal"
// values) discovered by Walk. keys is the list of keys to descend to the bucket
// owning the discovered key/value pair k/v.
type walkFunc func(keys [][]byte, k, v []byte, seq uint64) error

// walk walks recursively the bolt database db, calling walkFn for each key it finds.
func walk(db *DB, walkFn walkFunc) error {
	return db.View(func(tx *Tx) error {
		return tx.ForEach(func(name []byte, b *Bucket) error {
			return walkBucket(b, nil, name, nil, b.Sequence(), walkFn)
		})
	})
}

func walkBucket(b *Bucket, keypath [][]byte, k, v []byte, seq uint64, fn walkFunc) error {
	// Execute callback.
	if err := fn(keypath, k, v, seq); err != nil {
		return err
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
			return walkBucket(bkt, keypath, k, nil, bkt.Sequence(), fn)
		}
		return walkBucket(b, keypath, k, v, b.Sequence(), fn)
	})
}

// walkFrom walks the database like walk, but skips every item whose key path
// is less than or equal to skipPath in depth-first order (the order in which
// walk emits items). A nil or empty skipPath walks the entire database.
// It is used to resume an interrupted CompactResumable run.
func walkFrom(tx *Tx, skipPath [][]byte, walkFn walkFunc) error {
	return walkLevel(tx.Cursor(), tx.Bucket, nil, 0, skipPath, walkFn)
}

// walkLevel iterates over the items of one bucket level (the root level when
// called by walkFrom), skipping all items at or before skip. childBucket
// resolves the bucket stored under a key at this level, keypath is the path
// of the bucket being iterated and parentSeq its sequence number.
func walkLevel(c *Cursor, childBucket func(k []byte) *Bucket, keypath [][]byte, parentSeq uint64, skip [][]byte, fn walkFunc) error {
	var k, v []byte
	if len(skip) > 0 {
		// Position at the last copied item of this level; it and
		// everything before it has already been copied.
		k, v = c.Seek(skip[0])
	} else {
		k, v = c.First()
	}
	for ; k != nil; k, v = c.Next() {
		// Only the first item of this level can match the skip path,
		// because keys are visited in sorted order.
		skipping := len(skip) > 0 && bytes.Equal(k, skip[0])
		if v == nil {
			bkt := childBucket(k)
			if !skipping {
				if err := fn(keypath, k, nil, bkt.Sequence()); err != nil {
					return err
				}
			}
			var childSkip [][]byte
			if skipping {
				childSkip = skip[1:]
			}
			if err := walkLevel(bkt.Cursor(), bkt.Bucket, append(keypath, k), bkt.Sequence(), childSkip, fn); err != nil {
				return err
			}
			continue
		}
		if skipping {
			continue
		}
		if err := fn(keypath, k, v, parentSeq); err != nil {
			return err
		}
	}
	return nil
}
