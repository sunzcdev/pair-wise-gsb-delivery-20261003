package bbolt

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

// compactTestItem is a single walked entry of a DB under test.
type compactTestItem struct {
	value  []byte // nil for buckets
	seq    uint64
	bucket bool
}

// createCompactTestSource builds a source DB with deeply nested buckets,
// key/value pairs at every level, empty buckets and non-zero sequences.
func createCompactTestSource(t *testing.T, path string) *DB {
	t.Helper()

	db, err := Open(path, 0600, nil)
	if err != nil {
		t.Fatalf("failed to open source db: %v", err)
	}

	err = db.Update(func(tx *Tx) error {
		// An empty top-level bucket with a sequence.
		emptyTop, err := tx.CreateBucket([]byte("empty-top"))
		if err != nil {
			return err
		}
		if err := emptyTop.SetSequence(99); err != nil {
			return err
		}

		root, err := tx.CreateBucket([]byte("root"))
		if err != nil {
			return err
		}
		if err := root.SetSequence(7); err != nil {
			return err
		}

		// A deep chain root/a/b/c/d/e with keys at every level.
		b := root
		for depth, name := range []string{"a", "b", "c", "d", "e"} {
			nb, err := b.CreateBucket([]byte(name))
			if err != nil {
				return err
			}
			if err := nb.SetSequence(uint64(10 + depth)); err != nil {
				return err
			}
			for i := 0; i < 20; i++ {
				key := []byte(fmt.Sprintf("%s-key-%02d", name, i))
				value := bytes.Repeat([]byte(name), 64)
				if err := nb.Put(key, value); err != nil {
					return err
				}
			}
			b = nb
		}

		// An empty bucket at the bottom of the deep chain.
		emptyDeep, err := b.CreateBucket([]byte("empty-deep"))
		if err != nil {
			return err
		}
		if err := emptyDeep.SetSequence(42); err != nil {
			return err
		}

		// A sibling branch sorting after the deep chain.
		sib, err := root.CreateBucket([]byte("z-sibling"))
		if err != nil {
			return err
		}
		for i := 0; i < 10; i++ {
			if err := sib.Put([]byte(fmt.Sprintf("sib-key-%02d", i)), []byte("sib-value")); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to populate source db: %v", err)
	}
	return db
}

// collectCompactItems walks db and returns all items keyed by their encoded
// nested bucket path. The compact progress marker bucket is skipped.
func collectCompactItems(t *testing.T, db *DB) map[string]compactTestItem {
	t.Helper()

	items := make(map[string]compactTestItem)
	err := db.View(func(tx *Tx) error {
		return walkTx(tx, nil, func(keys [][]byte, k, v []byte, seq uint64) error {
			if len(keys) == 0 && string(k) == CompactProgressBucketName {
				return nil
			}
			if len(keys) > 0 && string(keys[0]) == CompactProgressBucketName {
				return nil
			}
			path := appendKeyPath(keys, k)
			item := compactTestItem{seq: seq, bucket: v == nil}
			if v != nil {
				item.value = append([]byte(nil), v...)
			}
			items[string(encodeCompactPath(path))] = item
			return nil
		})
	})
	if err != nil {
		t.Fatalf("failed to collect items: %v", err)
	}
	return items
}

// readCompactProgressPath returns the resume path recorded in db, or nil if
// db carries no progress marker.
func readCompactProgressPath(t *testing.T, db *DB) [][]byte {
	t.Helper()

	var path [][]byte
	err := db.View(func(tx *Tx) error {
		b := tx.Bucket([]byte(CompactProgressBucketName))
		if b == nil {
			return nil
		}
		path = decodeCompactPath(b.Get(compactProgressPathKey))
		return nil
	})
	if err != nil {
		t.Fatalf("failed to read compact progress: %v", err)
	}
	return path
}

func checkConsistent(t *testing.T, db *DB) {
	t.Helper()

	if err := db.View(func(tx *Tx) error {
		for err := range tx.Check() {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("consistency check failed: %v", err)
	}
}

func TestCompactResumable(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	dstPath := filepath.Join(dir, "dst.db")

	src := createCompactTestSource(t, srcPath)
	defer func() { _ = src.Close() }()

	dst, err := Open(dstPath, 0600, nil)
	if err != nil {
		t.Fatalf("failed to open destination db: %v", err)
	}

	// Simulate a crash in the middle of the deep bucket chain.
	if err := compactResumable(dst, src, 4096, 75); !errors.Is(err, errCompactSimulatedCrash) {
		t.Fatalf("expected simulated crash, got: %v", err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("failed to close destination db: %v", err)
	}

	// Reopen to simulate a process restart.
	dst, err = Open(dstPath, 0600, nil)
	if err != nil {
		t.Fatalf("failed to reopen destination db: %v", err)
	}

	// The unfinished target must be clearly identifiable.
	interrupted, err := CompactInterrupted(dst)
	if err != nil {
		t.Fatalf("CompactInterrupted failed: %v", err)
	}
	if !interrupted {
		t.Fatal("expected destination db to be marked as an unfinished compaction target")
	}

	// The committed data and the committed progress must be consistent: the
	// destination must contain exactly the source items up to the recorded
	// resume path.
	resumePath := readCompactProgressPath(t, dst)
	if len(resumePath) == 0 {
		t.Fatal("expected a non-empty resume path after the crash")
	}
	srcItems := collectCompactItems(t, src)
	dstItems := collectCompactItems(t, dst)
	for encPath, item := range dstItems {
		path := decodeCompactPath([]byte(encPath))
		if compareKeyPaths(path, resumePath) > 0 {
			t.Fatalf("destination contains item past the recorded progress: %q", path)
		}
		want, ok := srcItems[encPath]
		if !ok {
			t.Fatalf("destination contains item not present in source: %q", path)
		}
		if !reflect.DeepEqual(want, item) {
			t.Fatalf("item mismatch at %q: got %+v, want %+v", path, item, want)
		}
	}
	for encPath := range srcItems {
		path := decodeCompactPath([]byte(encPath))
		if compareKeyPaths(path, resumePath) <= 0 {
			if _, ok := dstItems[encPath]; !ok {
				t.Fatalf("destination misses committed item: %q", path)
			}
		}
	}
	if len(dstItems) == len(srcItems) {
		t.Fatal("expected the crash to interrupt the compaction before completion")
	}

	// Resume the compaction; it must run to completion.
	if err := CompactResumable(dst, src, 4096); err != nil {
		t.Fatalf("failed to resume compaction: %v", err)
	}

	interrupted, err = CompactInterrupted(dst)
	if err != nil {
		t.Fatalf("CompactInterrupted failed: %v", err)
	}
	if interrupted {
		t.Fatal("destination db is still marked as an unfinished compaction target")
	}

	// The completed target must match the source exactly, including keys,
	// empty buckets and sequences.
	if got, want := collectCompactItems(t, dst), srcItems; !reflect.DeepEqual(got, want) {
		for encPath, wantItem := range want {
			gotItem, ok := got[encPath]
			if !ok {
				t.Errorf("missing item: %q", decodeCompactPath([]byte(encPath)))
			} else if !reflect.DeepEqual(gotItem, wantItem) {
				t.Errorf("item mismatch at %q: got %+v, want %+v", decodeCompactPath([]byte(encPath)), gotItem, wantItem)
			}
		}
		for encPath := range got {
			if _, ok := want[encPath]; !ok {
				t.Errorf("unexpected item: %q", decodeCompactPath([]byte(encPath)))
			}
		}
		t.Fatalf("compacted db does not match the source (%d items vs %d items)", len(got), len(want))
	}

	// The completed file must pass the consistency check.
	checkConsistent(t, dst)

	if err := dst.Close(); err != nil {
		t.Fatalf("failed to close destination db: %v", err)
	}
}

func TestCompactResumable_SourceChanged(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	dstPath := filepath.Join(dir, "dst.db")

	src := createCompactTestSource(t, srcPath)
	defer func() { _ = src.Close() }()

	dst, err := Open(dstPath, 0600, nil)
	if err != nil {
		t.Fatalf("failed to open destination db: %v", err)
	}
	defer func() { _ = dst.Close() }()

	// Interrupt the compaction in the middle of the deep bucket chain.
	if err := compactResumable(dst, src, 4096, 75); !errors.Is(err, errCompactSimulatedCrash) {
		t.Fatalf("expected simulated crash, got: %v", err)
	}

	// Modify the source before resuming.
	if err := src.Update(func(tx *Tx) error {
		return tx.Bucket([]byte("root")).Put([]byte("new-key"), []byte("new-value"))
	}); err != nil {
		t.Fatalf("failed to modify source db: %v", err)
	}

	// Resuming must be refused: the saved progress belongs to another source
	// version and resuming would splice two points in time into one target.
	if err := CompactResumable(dst, src, 4096); !errors.Is(err, ErrCompactSourceChanged) {
		t.Fatalf("expected ErrCompactSourceChanged, got: %v", err)
	}

	// The target must still identify as unfinished.
	interrupted, err := CompactInterrupted(dst)
	if err != nil {
		t.Fatalf("CompactInterrupted failed: %v", err)
	}
	if !interrupted {
		t.Fatal("destination db should still be marked as an unfinished compaction target")
	}
}

func TestCompactResumable_SingleRun(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	dstPath := filepath.Join(dir, "dst.db")

	src := createCompactTestSource(t, srcPath)
	defer func() { _ = src.Close() }()

	dst, err := Open(dstPath, 0600, nil)
	if err != nil {
		t.Fatalf("failed to open destination db: %v", err)
	}
	defer func() { _ = dst.Close() }()

	if err := CompactResumable(dst, src, 4096); err != nil {
		t.Fatalf("failed to compact: %v", err)
	}

	interrupted, err := CompactInterrupted(dst)
	if err != nil {
		t.Fatalf("CompactInterrupted failed: %v", err)
	}
	if interrupted {
		t.Fatal("destination db is marked as an unfinished compaction target")
	}

	if got, want := collectCompactItems(t, dst), collectCompactItems(t, src); !reflect.DeepEqual(got, want) {
		t.Fatalf("compacted db does not match the source (%d items vs %d items)", len(got), len(want))
	}

	checkConsistent(t, dst)
}
