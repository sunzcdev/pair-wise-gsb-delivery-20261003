package buntdb

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// errFaultIO is the sentinel returned by the injected faulty filesystem.
var errFaultIO = errors.New("fault: injected I/O failure")

// faultFile wraps an AOFile and injects failures into the operations of the
// live database file only. Temporary shrink files use the same wrapper but
// have the live flag disabled.
type faultFile struct {
	AOFile
	fs   *faultFS
	live bool
}

func (f *faultFile) Write(p []byte) (int, error) {
	if f.live {
		if n := atomic.LoadInt32(&f.fs.writeFail); n > 0 {
			if atomic.CompareAndSwapInt32(&f.fs.writeFail, n, n-1) {
				short := f.fs.writeShort
				if short > len(p) {
					short = len(p)
				}
				if short > 0 {
					if _, err := f.AOFile.Write(p[:short]); err != nil {
						return 0, err
					}
				}
				return short, errFaultIO
			}
		}
	}
	return f.AOFile.Write(p)
}

func (f *faultFile) Sync() error {
	if f.live {
		for {
			n := atomic.LoadInt32(&f.fs.syncFail)
			if n <= 0 {
				break
			}
			if atomic.CompareAndSwapInt32(&f.fs.syncFail, n, n-1) {
				return errFaultIO
			}
		}
	}
	return f.AOFile.Sync()
}

// faultFS wraps the default AOFIO so individual operations can be made to
// fail, modelling a short append, a failed sync, or a failed shrink swap.
type faultFS struct {
	base AOFIO
	path string

	writeFail  int32 // number of live-file Writes that should fail
	writeShort int   // number of bytes the failing Write accepts
	syncFail   int32 // number of live-file Syncs that should fail

	mu         sync.Mutex
	renameFail bool
}

func (fs *faultFS) OpenFile(name string, flag int, perm os.FileMode) (AOFile, error) {
	f, err := fs.base.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return &faultFile{AOFile: f, fs: fs, live: name == fs.path}, nil
}

func (fs *faultFS) Create(name string) (AOFile, error) {
	f, err := fs.base.Create(name)
	if err != nil {
		return nil, err
	}
	return &faultFile{AOFile: f, fs: fs, live: false}, nil
}

func (fs *faultFS) Open(name string) (AOFile, error) {
	f, err := fs.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &faultFile{AOFile: f, fs: fs, live: false}, nil
}

func (fs *faultFS) Rename(oldpath, newpath string) error {
	fs.mu.Lock()
	fail := fs.renameFail
	fs.mu.Unlock()
	if fail {
		return errFaultIO
	}
	return fs.base.Rename(oldpath, newpath)
}

func (fs *faultFS) Remove(name string) error { return fs.base.Remove(name) }

func openFaultDB(t *testing.T, path string, policy SyncPolicy, fs *faultFS) *DB {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	db, err := OpenWithConfig(path, Config{
		SyncPolicy:         policy,
		AutoShrinkDisabled: true,
		AOFIO:              fs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func reopenPlain(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func assertGet(t *testing.T, db *DB, key, want string) {
	t.Helper()
	err := db.View(func(tx *Tx) error {
		got, err := tx.Get(key)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("key %q: want %q, got %q", key, want, got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertMissing(t *testing.T, db *DB, key string) {
	t.Helper()
	err := db.View(func(tx *Tx) error {
		_, gerr := tx.Get(key)
		if gerr != ErrNotFound {
			return fmt.Errorf("key %q: want ErrNotFound, got %v", key, gerr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// TestFaultShortWrite appends a record but only part of the bytes reach the
// file. The caller must receive the wrapped I/O error with ErrDiskFault, the
// database must be read-only afterwards, and the in-memory change must have
// been rolled back. Recovery truncates the torn tail back to the boundary of
// the last complete command before writes are allowed again, and a plain
// reopen produces exactly the same contents.
func TestFaultShortWrite(t *testing.T) {
	path := "fault_short.db"
	defer os.RemoveAll(path)
	fs := &faultFS{base: osAOFIO{}, path: path}
	db := openFaultDB(t, path, EverySecond, fs)

	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("good", "1", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	goodSize := fileSize(t, path)

	// The faulty write accepts only 3 bytes of the record, leaving a torn
	// command at the tail.
	atomic.StoreInt32(&fs.writeFail, 1)
	fs.writeShort = 3
	err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("bad", "2", nil)
		return err
	})
	if !errors.Is(err, ErrDiskFault) {
		t.Fatalf("want ErrDiskFault, got %v", err)
	}
	if !errors.Is(err, errFaultIO) {
		t.Fatalf("want wrapped cause errFaultIO, got %v", err)
	}
	if db.DiskFault() == nil {
		t.Fatal("DiskFault should be reported")
	}
	// reads still work, and the failed change was rolled back in memory.
	assertGet(t, db, "good", "1")
	assertMissing(t, db, "bad")
	// further writes are rejected while in the fault state.
	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("nope", "x", nil)
		return err
	}); !errors.Is(err, ErrDiskFault) {
		t.Fatalf("want ErrDiskFault on follow-up write, got %v", err)
	}
	// the torn tail is still on disk at this point.
	if sz := fileSize(t, path); sz != goodSize+3 {
		t.Fatalf("want torn file size %d, got %d", goodSize+3, sz)
	}

	// recovery must reconfirm the durable boundary instead of just clearing
	// the error flag.
	if err := db.Recover(); err != nil {
		t.Fatal(err)
	}
	if db.DiskFault() != nil {
		t.Fatal("DiskFault should be cleared after Recover")
	}
	if sz := fileSize(t, path); sz != goodSize {
		t.Fatalf("want truncated file size %d, got %d", goodSize, sz)
	}
	assertMissing(t, db, "bad")
	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("later", "3", nil)
		return err
	}); err != nil {
		t.Fatalf("writes should work after Recover: %v", err)
	}
	assertGet(t, db, "later", "3")

	// close and reopen with a plain Open and verify the on-disk contents
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = reopenPlain(t, path)
	defer db.Close()
	assertGet(t, db, "good", "1")
	assertGet(t, db, "later", "3")
	assertMissing(t, db, "bad")
}

// TestFaultSyncFailure covers the case where the whole record reaches the
// kernel but Sync fails. Durability is unconfirmed, so the database enters
// the read-only state; the change is kept in memory because it may actually
// be on disk. Recover keeps the fully written record and reopens for writes.
func TestFaultSyncFailure(t *testing.T) {
	path := "fault_sync.db"
	defer os.RemoveAll(path)
	fs := &faultFS{base: osAOFIO{}, path: path}
	db := openFaultDB(t, path, Always, fs)

	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("good", "1", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt32(&fs.syncFail, 1)
	err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("maybe", "2", nil)
		return err
	})
	if !errors.Is(err, ErrDiskFault) || !errors.Is(err, errFaultIO) {
		t.Fatalf("want ErrDiskFault wrapping errFaultIO, got %v", err)
	}
	if db.DiskFault() == nil {
		t.Fatal("DiskFault should be reported")
	}
	// the complete record survived to the file even though fsync failed.
	assertGet(t, db, "maybe", "2")

	// Reconfirming the boundary finds an intact record; nothing is
	// truncated and writes resume.
	if err := db.Recover(); err != nil {
		t.Fatal(err)
	}
	if db.DiskFault() != nil {
		t.Fatal("DiskFault should be cleared after Recover")
	}
	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("later", "3", nil)
		return err
	}); err != nil {
		t.Fatalf("writes should work after Recover: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = reopenPlain(t, path)
	defer db.Close()
	assertGet(t, db, "good", "1")
	assertGet(t, db, "maybe", "2")
	assertGet(t, db, "later", "3")
}

// TestFaultShrinkSwap fails the rename that swaps the compacted file in.
// The original AOF must remain intact, the database must report a disk
// fault and reject writes, and Recover must reconfirm the boundary on the
// untouched file before allowing writes again.
func TestFaultShrinkSwap(t *testing.T) {
	path := "fault_shrink.db"
	defer os.RemoveAll(path)
	defer os.RemoveAll(path + ".tmp")
	fs := &faultFS{base: osAOFIO{}, path: path}
	db := openFaultDB(t, path, EverySecond, fs)

	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("key%d", i)
		if err := db.Update(func(tx *Tx) error {
			_, _, err := tx.Set(key, fmt.Sprintf("val%d", i), nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	beforeSize := fileSize(t, path)

	fs.mu.Lock()
	fs.renameFail = true
	fs.mu.Unlock()
	err := db.Shrink()
	fs.mu.Lock()
	fs.renameFail = false
	fs.mu.Unlock()
	if !errors.Is(err, ErrDiskFault) || !errors.Is(err, errFaultIO) {
		t.Fatalf("want ErrDiskFault wrapping errFaultIO, got %v", err)
	}
	if db.DiskFault() == nil {
		t.Fatal("DiskFault should be reported")
	}
	// the original file is untouched; the compacted tmp is cleaned up.
	if sz := fileSize(t, path); sz != beforeSize {
		t.Fatalf("original AOF must remain, want size %d, got %d", beforeSize, sz)
	}
	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("nope", "x", nil)
		return err
	}); !errors.Is(err, ErrDiskFault) {
		t.Fatalf("want ErrDiskFault on write during fault, got %v", err)
	}

	// Recover reopens/replays the original file and clears the fault.
	if err := db.Recover(); err != nil {
		t.Fatal(err)
	}
	if db.DiskFault() != nil {
		t.Fatal("DiskFault should be cleared after Recover")
	}
	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("after", "9", nil)
		return err
	}); err != nil {
		t.Fatalf("writes should work after Recover: %v", err)
	}
	for i := 0; i < 20; i++ {
		assertGet(t, db, fmt.Sprintf("key%d", i), fmt.Sprintf("val%d", i))
	}
	assertGet(t, db, "after", "9")

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = reopenPlain(t, path)
	defer db.Close()
	for i := 0; i < 20; i++ {
		assertGet(t, db, fmt.Sprintf("key%d", i), fmt.Sprintf("val%d", i))
	}
	assertGet(t, db, "after", "9")
}

// TestFaultRecoveryCannotBeFaked verifies that recovery is a boundary
// confirmation, not a flag reset: the torn tail is actually truncated before
// writes resume and a plain reopen sees the same recovered content.
func TestFaultRecoveryCannotBeFaked(t *testing.T) {
	path := "fault_recover.db"
	defer os.RemoveAll(path)
	fs := &faultFS{base: osAOFIO{}, path: path}
	db := openFaultDB(t, path, EverySecond, fs)

	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("good", "1", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt32(&fs.writeFail, 1)
	fs.writeShort = 2
	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("bad", "2", nil)
		return err
	}); !errors.Is(err, ErrDiskFault) {
		t.Fatalf("want ErrDiskFault, got %v", err)
	}
	if err := db.Recover(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = reopenPlain(t, path)
	defer db.Close()
	assertGet(t, db, "good", "1")
	assertMissing(t, db, "bad")
}

// TestFaultBackgroundSyncFailure makes the background (EverySecond) sync
// fail. The database must enter the disk-fault state on its own, even though
// the committing Update returned nil.
func TestFaultBackgroundSyncFailure(t *testing.T) {
	path := "fault_bgsync.db"
	defer os.RemoveAll(path)
	fs := &faultFS{base: osAOFIO{}, path: path}
	db := openFaultDB(t, path, EverySecond, fs)

	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("good", "1", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// the next background tick will hit the failing sync.
	atomic.StoreInt32(&fs.syncFail, 1)
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if db.DiskFault() != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if db.DiskFault() == nil {
		t.Fatal("background sync failure should force the disk-fault state")
	}
	if !errors.Is(db.DiskFault(), ErrDiskFault) ||
		!errors.Is(db.DiskFault(), errFaultIO) {
		t.Fatalf("fault should wrap ErrDiskFault and errFaultIO, got %v", db.DiskFault())
	}
	// a direct writable Begin reports the fault too.
	if _, err := db.Begin(true); !errors.Is(err, ErrDiskFault) {
		t.Fatalf("Begin(true) want ErrDiskFault, got %v", err)
	}
	// reads stay available.
	assertGet(t, db, "good", "1")
	if err := db.Recover(); err != nil {
		t.Fatal(err)
	}
	if db.DiskFault() != nil {
		t.Fatal("Recover should clear the fault")
	}
	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("ok", "2", nil)
		return err
	}); err != nil {
		t.Fatalf("writes should resume: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = reopenPlain(t, path)
	defer db.Close()
	assertGet(t, db, "good", "1")
	assertGet(t, db, "ok", "2")
}
