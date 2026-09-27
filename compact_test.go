package bbolt

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	berrors "go.etcd.io/bbolt/errors"
)

// compactTestEntry is one item (bucket or key/value pair) of a database,
// identified by its full nested key path.
type compactTestEntry struct {
	path     [][]byte
	value    []byte // nil for buckets
	isBucket bool
	seq      uint64
}

// collectCompactEntries returns all items of db in depth-first order.
func collectCompactEntries(t *testing.T, db *DB) []compactTestEntry {
	t.Helper()
	var entries []compactTestEntry
	err := db.View(func(tx *Tx) error {
		return walkFrom(tx, nil, func(keys [][]byte, k, v []byte, seq uint64) error {
			p := make([][]byte, 0, len(keys)+1)
			for _, e := range keys {
				p = append(p, append([]byte(nil), e...))
			}
			p = append(p, append([]byte(nil), k...))
			e := compactTestEntry{path: p, isBucket: v == nil}
			if v == nil {
				e.seq = seq
			} else {
				e.value = append([]byte(nil), v...)
			}
			entries = append(entries, e)
			return nil
		})
	})
	if err != nil {
		t.Fatalf("failed to collect entries: %v", err)
	}
	return entries
}

// compareCompactPaths compares two key paths in depth-first order: prefix
// paths (parents) sort before their children.
func compareCompactPaths(a, b [][]byte) int {
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
	}
	return 0
}

func requireSameEntries(t *testing.T, want, got []compactTestEntry) {
	t.Helper()
	sortCompactEntries := func(entries []compactTestEntry) {
		sort.Slice(entries, func(i, j int) bool {
			return compareCompactPaths(entries[i].path, entries[j].path) < 0
		})
	}
	sortCompactEntries(want)
	sortCompactEntries(got)
	if len(want) != len(got) {
		t.Fatalf("entry count mismatch: want %d, got %d", len(want), len(got))
	}
	for i := range want {
		w, g := want[i], got[i]
		if compareCompactPaths(w.path, g.path) != 0 {
			t.Fatalf("entry %d path mismatch: want %q, got %q", i, w.path, g.path)
		}
		if w.isBucket != g.isBucket {
			t.Fatalf("entry %q bucket flag mismatch", w.path)
		}
		if !bytes.Equal(w.value, g.value) {
			t.Fatalf("entry %q value mismatch: want %q, got %q", w.path, w.value, g.value)
		}
		if w.seq != g.seq {
			t.Fatalf("entry %q sequence mismatch: want %d, got %d", w.path, w.seq, g.seq)
		}
	}
}

// requireConsistent runs the bbolt consistency check on db.
func requireConsistent(t *testing.T, db *DB) {
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

// newCompactTestSource creates a source database with deeply nested buckets,
// empty buckets, key/value pairs and non-zero bucket sequences.
func newCompactTestSource(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path, 0600, nil)
	if err != nil {
		t.Fatalf("failed to open source db: %v", err)
	}
	err = db.Update(func(tx *Tx) error {
		// A flat bucket with a sequence.
		flat, err := tx.CreateBucket([]byte("flat"))
		if err != nil {
			return err
		}
		for i := 0; i < 10; i++ {
			if err := flat.Put([]byte(fmt.Sprintf("key-%02d", i)), []byte(fmt.Sprintf("value-%02d", i))); err != nil {
				return err
			}
		}
		if err := flat.SetSequence(42); err != nil {
			return err
		}

		// A deeply nested bucket chain with keys at every level.
		deep, err := tx.CreateBucket([]byte("deep"))
		if err != nil {
			return err
		}
		if err := deep.SetSequence(7); err != nil {
			return err
		}
		b := deep
		for level := 1; level <= 5; level++ {
			for i := 0; i < 5; i++ {
				k := []byte(fmt.Sprintf("k%d-%d", level, i))
				if err := b.Put(k, bytes.Repeat([]byte{byte(level)}, 16)); err != nil {
					return err
				}
			}
			nb, err := b.CreateBucket([]byte(fmt.Sprintf("level-%d", level)))
			if err != nil {
				return err
			}
			if err := nb.SetSequence(uint64(100 + level)); err != nil {
				return err
			}
			b = nb
		}

		// Empty buckets at various depths, including a nested empty bucket.
		if _, err := tx.CreateBucket([]byte("empty-root")); err != nil {
			return err
		}
		emptyParent, err := tx.CreateBucket([]byte("empty-parent"))
		if err != nil {
			return err
		}
		if _, err := emptyParent.CreateBucket([]byte("empty-child")); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to populate source db: %v", err)
	}
	return db
}

func openCompactTestDB(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path, 0600, nil)
	if err != nil {
		t.Fatalf("failed to open db %s: %v", path, err)
	}
	return db
}

// TestCompactResumable_Full verifies that an uninterrupted resumable
// compaction copies all keys, empty buckets and sequences, leaves no
// progress marker behind and produces a consistent database.
func TestCompactResumable_Full(t *testing.T) {
	dir := t.TempDir()
	src := newCompactTestSource(t, filepath.Join(dir, "src.db"))
	defer src.Close()
	dst := openCompactTestDB(t, filepath.Join(dir, "dst.db"))
	defer dst.Close()

	if err := CompactResumable(dst, src, 128); err != nil {
		t.Fatalf("CompactResumable failed: %v", err)
	}

	inProgress, err := CompactInProgress(dst)
	if err != nil {
		t.Fatalf("CompactInProgress failed: %v", err)
	}
	if inProgress {
		t.Fatal("destination still marked as incomplete after a finished compaction")
	}

	requireSameEntries(t, collectCompactEntries(t, src), collectCompactEntries(t, dst))
	requireConsistent(t, dst)
}

// TestCompactResumable_Resume simulates a crash in the middle of a deep
// bucket, then verifies that the committed destination data matches the
// recorded progress exactly, and that a resumed run completes the copy.
func TestCompactResumable_Resume(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	dstPath := filepath.Join(dir, "dst.db")
	src := newCompactTestSource(t, srcPath)
	dst := openCompactTestDB(t, dstPath)
	defer dst.Close()
	defer src.Close()

	// Interrupt the copy after 20 items, in the middle of the deep bucket.
	errAbort := errors.New("simulated crash")
	compactWriteHook = func(n int) error {
		if n >= 20 {
			return errAbort
		}
		return nil
	}
	defer func() { compactWriteHook = nil }()

	if err := CompactResumable(dst, src, 128); !errors.Is(err, errAbort) {
		t.Fatalf("expected abort error, got: %v", err)
	}
	compactWriteHook = nil

	// The destination must be recognizable as incomplete.
	inProgress, err := CompactInProgress(dst)
	if err != nil {
		t.Fatalf("CompactInProgress failed: %v", err)
	}
	if !inProgress {
		t.Fatal("interrupted destination is not marked as incomplete")
	}

	// The recorded progress must match the committed destination data
	// exactly: the destination must contain precisely the source items up
	// to and including the recorded position.
	srcEntries := collectCompactEntries(t, src)
	dstEntries := collectCompactEntries(t, dst)
	var progressPath [][]byte
	var recordedTxID uint64
	if err := dst.View(func(tx *Tx) error {
		pb := tx.Bucket([]byte(compactProgressBucket))
		if pb == nil {
			return errors.New("progress bucket missing")
		}
		progressPath = decodeCompactPath(pb.Get([]byte(compactProgressPathKey)))
		v := pb.Get([]byte(compactProgressSrcTxIDKey))
		if len(v) != 8 {
			return errors.New("recorded source txid missing")
		}
		recordedTxID = uint64(v[0])<<56 | uint64(v[1])<<48 | uint64(v[2])<<40 | uint64(v[3])<<32 |
			uint64(v[4])<<24 | uint64(v[5])<<16 | uint64(v[6])<<8 | uint64(v[7])
		return nil
	}); err != nil {
		t.Fatalf("failed to read progress: %v", err)
	}
	if err := src.View(func(tx *Tx) error {
		if uint64(tx.ID()) != recordedTxID {
			return fmt.Errorf("recorded txid %d does not match source txid %d", recordedTxID, tx.ID())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Strip the progress bookkeeping bucket (and its contents) from the
	// destination view.
	var dataEntries []compactTestEntry
	for _, e := range dstEntries {
		if len(e.path) >= 1 && string(e.path[0]) == compactProgressBucket {
			continue
		}
		dataEntries = append(dataEntries, e)
	}
	var wantPrefix []compactTestEntry
	for _, e := range srcEntries {
		if compareCompactPaths(e.path, progressPath) > 0 {
			break
		}
		wantPrefix = append(wantPrefix, e)
	}
	if len(wantPrefix) == 0 {
		t.Fatal("expected at least one committed item before the abort")
	}
	requireSameEntries(t, wantPrefix, dataEntries)

	// Simulate a process restart: close and reopen both databases.
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	src = openCompactTestDB(t, srcPath)
	dst = openCompactTestDB(t, dstPath)
	defer src.Close()
	defer dst.Close()

	// Resume the compaction; it must complete the copy.
	if err := CompactResumable(dst, src, 128); err != nil {
		t.Fatalf("resumed CompactResumable failed: %v", err)
	}

	inProgress, err = CompactInProgress(dst)
	if err != nil {
		t.Fatalf("CompactInProgress failed: %v", err)
	}
	if inProgress {
		t.Fatal("destination still marked as incomplete after the resumed run finished")
	}

	requireSameEntries(t, collectCompactEntries(t, src), collectCompactEntries(t, dst))
	requireConsistent(t, dst)
}

// TestCompactResumable_SourceChanged verifies that resuming against a
// modified source database is rejected instead of splicing two points in
// time into one destination.
func TestCompactResumable_SourceChanged(t *testing.T) {
	dir := t.TempDir()
	src := newCompactTestSource(t, filepath.Join(dir, "src.db"))
	defer src.Close()
	dst := openCompactTestDB(t, filepath.Join(dir, "dst.db"))
	defer dst.Close()

	errAbort := errors.New("simulated crash")
	compactWriteHook = func(n int) error {
		if n >= 10 {
			return errAbort
		}
		return nil
	}
	if err := CompactResumable(dst, src, 128); !errors.Is(err, errAbort) {
		t.Fatalf("expected abort error, got: %v", err)
	}
	compactWriteHook = nil

	// Modify the source after the interruption.
	if err := src.Update(func(tx *Tx) error {
		return tx.Bucket([]byte("flat")).Put([]byte("new-key"), []byte("new-value"))
	}); err != nil {
		t.Fatalf("failed to modify source: %v", err)
	}

	err := CompactResumable(dst, src, 128)
	if !errors.Is(err, berrors.ErrCompactSourceChanged) {
		t.Fatalf("expected ErrCompactSourceChanged, got: %v", err)
	}

	// The destination must still be marked as incomplete.
	inProgress, ierr := CompactInProgress(dst)
	if ierr != nil {
		t.Fatalf("CompactInProgress failed: %v", ierr)
	}
	if !inProgress {
		t.Fatal("destination lost its incomplete marker after the rejected resume")
	}
}

// TestCompactResumable_DstNotEmpty verifies that a non-empty destination
// without a progress record is rejected.
func TestCompactResumable_DstNotEmpty(t *testing.T) {
	dir := t.TempDir()
	src := newCompactTestSource(t, filepath.Join(dir, "src.db"))
	defer src.Close()
	dst := openCompactTestDB(t, filepath.Join(dir, "dst.db"))
	defer dst.Close()

	if err := dst.Update(func(tx *Tx) error {
		_, err := tx.CreateBucket([]byte("foreign"))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if err := CompactResumable(dst, src, 128); !errors.Is(err, berrors.ErrCompactDstNotEmpty) {
		t.Fatalf("expected ErrCompactDstNotEmpty, got: %v", err)
	}
}

// TestCompactResumable_ResumeIdempotent verifies that a crash after the last
// data commit (progress record left behind) resumes into a finished copy.
func TestCompactResumable_ResumeIdempotent(t *testing.T) {
	dir := t.TempDir()
	src := newCompactTestSource(t, filepath.Join(dir, "src.db"))
	defer src.Close()
	dst := openCompactTestDB(t, filepath.Join(dir, "dst.db"))
	defer dst.Close()

	// Abort exactly when every item has been written but the final
	// progress-removing commit has not happened yet.
	total := len(collectCompactEntries(t, src))
	errAbort := errors.New("simulated crash")
	remaining := total
	compactWriteHook = func(n int) error {
		remaining--
		if remaining == 0 {
			return errAbort
		}
		return nil
	}
	if err := CompactResumable(dst, src, 128); !errors.Is(err, errAbort) {
		t.Fatalf("expected abort error, got: %v", err)
	}
	compactWriteHook = nil

	if err := CompactResumable(dst, src, 128); err != nil {
		t.Fatalf("resumed CompactResumable failed: %v", err)
	}
	requireSameEntries(t, collectCompactEntries(t, src), collectCompactEntries(t, dst))
	requireConsistent(t, dst)
}
