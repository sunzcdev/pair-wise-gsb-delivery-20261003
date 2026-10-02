package buntdb

import (
	"errors"
	"os"
	"testing"
	"time"
)

// errInjected is the synthetic storage error returned by the injected I/O
// layer. It stands in for an out-of-space or unrecoverable disk error.
var errInjected = errors.New("injected storage failure")

// faultFile wraps the live AOF file so that a single Write or Sync can be
// sabotaged. All other operations, including reads, pass straight through to
// the underlying operating-system file, which keeps the test honest about
// what actually lands on disk.
type faultFile struct {
	*os.File
	write func(real *os.File, p []byte) (int, error)
	sync  func() error
}

func (f *faultFile) Write(p []byte) (int, error) {
	if f.write != nil {
		return f.write(f.File, p)
	}
	return f.File.Write(p)
}

func (f *faultFile) Sync() error {
	if f.sync != nil {
		return f.sync()
	}
	return f.File.Sync()
}

// installFaults arranges for the live AOF file, and any future live file
// opened through Recover or the Shrink reopen, to carry the supplied hooks.
func installFaults(db *DB, write func(real *os.File, p []byte) (int, error), sync func() error) {
	arm := func(f persistFile) persistFile {
		if of, ok := f.(*os.File); ok {
			return &faultFile{File: of, write: write, sync: sync}
		}
		return f
	}
	db.file = arm(db.file)
	prev := openFile
	openFile = func(path string, flag int, perm os.FileMode, role fileRole) (persistFile, error) {
		f, err := prev(path, flag, perm, role)
		if err != nil {
			return nil, err
		}
		return arm(f), nil
	}
}

// resetFaults restores the default file opener. Wrappers already attached to
// an open handle are left in place because their hooks are nil after they
// fire once.
func resetFaults() {
	openFile = func(path string, flag int, perm os.FileMode, role fileRole) (persistFile, error) {
		return os.OpenFile(path, flag, perm)
	}
}

func openFaultDB(t *testing.T, policy SyncPolicy) *DB {
	t.Helper()
	path := "fault.db"
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetConfig(Config{
		SyncPolicy:           policy,
		AutoShrinkPercentage: 100,
		AutoShrinkMinSize:    32 * 1024 * 1024,
		AutoShrinkDisabled:   true,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		resetFaults()
		renameFileHook = nil
		_ = os.RemoveAll(path)
		_ = os.RemoveAll(path + ".tmp")
	})
	return db
}

// closeOnCleanup closes whatever handle the variable currently points at when
// the test ends, so it also covers a handle replaced by a reopen. It is
// registered after the file-removal cleanup and therefore runs first.
func closeOnCleanup(t *testing.T, db **DB) {
	t.Helper()
	t.Cleanup(func() {
		if *db != nil {
			_ = (*db).Close()
		}
	})
}

// TestFaultShortWrite sabotages an append so that only part of the record
// reaches the file and the rest is reported as a storage error.
func TestFaultShortWrite(t *testing.T) {
	db := openFaultDB(t, Always)
	closeOnCleanup(t, &db)

	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("key1", "value1", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Accept the first half of the record, then fail on the remainder.
	fired := false
	installFaults(db, func(real *os.File, p []byte) (int, error) {
		if fired {
			return real.Write(p)
		}
		fired = true
		// Half the bytes genuinely land on the file before the failure.
		if _, werr := real.Write(p[:len(p)/2]); werr != nil {
			return 0, werr
		}
		return len(p) / 2, errInjected
	}, nil)

	err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("key2", "value2", nil)
		return err
	})
	if err != ErrDatabaseReadOnly {
		t.Fatalf("expected ErrDatabaseReadOnly, got %v", err)
	}
	if !errors.Is(db.PersistError(), errInjected) {
		t.Fatalf("expected injected cause, got %v", db.PersistError())
	}

	// Reads keep working and never observe the half-applied transaction.
	if v, err := getOK(db, "key1"); err != nil || v != "value1" {
		t.Fatalf("key1 = %q, %v", v, err)
	}
	if _, err := getOK(db, "key2"); err != ErrNotFound {
		t.Fatalf("expected key2 to be rolled back, got %v", err)
	}

	// Writes remain refused while degraded.
	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("key3", "value3", nil)
		return err
	}); err != ErrDatabaseReadOnly {
		t.Fatalf("expected writes to stay rejected, got %v", err)
	}

	// Recovery must establish the real boundary rather than keep appending to
	// the corrupt tail.
	if err := db.Recover(); err != nil {
		t.Fatal(err)
	}
	if db.PersistError() != nil {
		t.Fatalf("expected healthy after recover, got %v", db.PersistError())
	}
	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("key3", "value3", nil)
		return err
	}); err != nil {
		t.Fatalf("writes should be re-enabled, got %v", err)
	}

	// Close and reopen from plain Open: the partial tail must be gone and
	// only the confirmed records survive.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = nil
	db = reopenFaultDB(t, "fault.db")
	if v, err := getOK(db, "key1"); err != nil || v != "value1" {
		t.Fatalf("after reopen key1 = %q, %v", v, err)
	}
	if _, err := getOK(db, "key2"); err != ErrNotFound {
		t.Fatalf("after reopen key2 should be absent, got %v", err)
	}
	if v, err := getOK(db, "key3"); err != nil || v != "value3" {
		t.Fatalf("after reopen key3 = %q, %v", v, err)
	}
}

// TestFaultSyncFailure lets the whole record reach the kernel but makes the
// durability Sync fail. Durability cannot be assumed, so the database must
// still enter the restricted state.
func TestFaultSyncFailure(t *testing.T) {
	db := openFaultDB(t, Always)
	closeOnCleanup(t, &db)

	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("alpha", "one", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	fired := false
	installFaults(db, nil, func() error {
		if fired {
			return nil
		}
		fired = true
		return errInjected
	})

	err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("beta", "two", nil)
		return err
	})
	if err != ErrDatabaseReadOnly {
		t.Fatalf("expected ErrDatabaseReadOnly, got %v", err)
	}
	if !errors.Is(db.PersistError(), errInjected) {
		t.Fatalf("expected injected cause, got %v", db.PersistError())
	}

	// Reads remain available while the file is in question.
	if v, err := getOK(db, "alpha"); err != nil || v != "one" {
		t.Fatalf("alpha = %q, %v", v, err)
	}
	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("gamma", "three", nil)
		return err
	}); err != ErrDatabaseReadOnly {
		t.Fatalf("expected writes to stay rejected, got %v", err)
	}

	// Reopen a fresh handle, reparse to the confirmed boundary, and sync.
	if err := db.Recover(); err != nil {
		t.Fatal(err)
	}
	// The complete beta record is part of the file's confirmed contents: it
	// reached the kernel before Sync failed, so reparsing keeps it. It would
	// instead be lost here if a crash had prevented the page cache from ever
	// reaching storage, and the reopened database would simply not contain it.
	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("gamma", "three", nil)
		return err
	}); err != nil {
		t.Fatalf("writes should be re-enabled, got %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = nil
	db = reopenFaultDB(t, "fault.db")
	if v, err := getOK(db, "alpha"); err != nil || v != "one" {
		t.Fatalf("after reopen alpha = %q, %v", v, err)
	}
	if v, err := getOK(db, "beta"); err != nil || v != "two" {
		t.Fatalf("after reopen beta = %q, %v", v, err)
	}
	if v, err := getOK(db, "gamma"); err != nil || v != "three" {
		t.Fatalf("after reopen gamma = %q, %v", v, err)
	}
}

// TestFaultShrinkSwap drives failures at the Shrink file-switch step and
// proves the database degrades instead of panicking, then recovers by
// reopening whichever file actually exists and confirming its boundary.
func TestFaultShrinkSwap(t *testing.T) {
	cases := []struct {
		name   string
		failAt func()
	}{
		{
			name: "rename",
			failAt: func() {
				renameFileHook = func(src, dest string) error {
					renameFileHook = nil
					return errInjected
				}
			},
		},
		{
			name: "reopen",
			failAt: func() {
				prev := openFile
				openFile = func(path string, flag int, perm os.FileMode, role fileRole) (persistFile, error) {
					if role == roleReopen {
						openFile = prev
						return nil, errInjected
					}
					return prev(path, flag, perm, role)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetFaults()
			renameFileHook = nil
			db := openFaultDB(t, Always)
			closeOnCleanup(t, &db)
			for i := 0; i < 5; i++ {
				key := string(rune('a' + i))
				if err := db.Update(func(tx *Tx) error {
					_, _, err := tx.Set(key, "v", nil)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}

			shrinkErr := make(chan error, 1)
			tc.failAt()
			go func() { shrinkErr <- db.Shrink() }()

			select {
			case err := <-shrinkErr:
				if err != ErrDatabaseReadOnly {
					t.Fatalf("expected ErrDatabaseReadOnly, got %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("shrink did not return after the injected swap failure")
			}
			if !errors.Is(db.PersistError(), errInjected) {
				t.Fatalf("expected injected cause, got %v", db.PersistError())
			}

			// Reads keep serving the pre-shrink dataset.
			if v, err := getOK(db, "a"); err != nil || v != "v" {
				t.Fatalf("a = %q, %v", v, err)
			}

			if err := db.Recover(); err != nil {
				t.Fatal(err)
			}
			if db.PersistError() != nil {
				t.Fatalf("expected healthy after recover, got %v", db.PersistError())
			}
			if err := db.Update(func(tx *Tx) error {
				_, _, err := tx.Set("z", "v", nil)
				return err
			}); err != nil {
				t.Fatalf("writes should be re-enabled, got %v", err)
			}

			// No temporary file may be left behind.
			if _, err := os.Stat("fault.db.tmp"); !os.IsNotExist(err) {
				t.Fatalf("expected tmp file removed, stat err = %v", err)
			}

			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db = nil
			db = reopenFaultDB(t, "fault.db")
			for i := 0; i < 5; i++ {
				key := string(rune('a' + i))
				if v, err := getOK(db, key); err != nil || v != "v" {
					t.Fatalf("after reopen %s = %q, %v", key, v, err)
				}
			}
			if v, err := getOK(db, "z"); err != nil || v != "v" {
				t.Fatalf("after reopen z = %q, %v", v, err)
			}
		})
	}
}

func getOK(db *DB, key string) (string, error) {
	var val string
	err := db.View(func(tx *Tx) error {
		var err error
		val, err = tx.Get(key)
		return err
	})
	return val, err
}

func reopenFaultDB(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// TestRecoverFailsWhileStorageBroken ensures Recover does not clear the
// restricted state until the fresh handle, boundary reparse, and Sync all
// succeed. While the storage still fails, writes must remain refused.
func TestRecoverFailsWhileStorageBroken(t *testing.T) {
	db := openFaultDB(t, Always)
	closeOnCleanup(t, &db)

	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("k", "v", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	fired := false
	installFaults(db, func(real *os.File, p []byte) (int, error) {
		if fired {
			return real.Write(p)
		}
		fired = true
		if _, werr := real.Write(p[:len(p)/2]); werr != nil {
			return 0, werr
		}
		return len(p) / 2, errInjected
	}, nil)

	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("x", "y", nil)
		return err
	}); err != ErrDatabaseReadOnly {
		t.Fatalf("expected degraded, got %v", err)
	}

	// Recovery attempts while the next Sync still fails must be rejected and
	// leave the database degraded.
	def := func(path string, flag int, perm os.FileMode, role fileRole) (persistFile, error) {
		return os.OpenFile(path, flag, perm)
	}
	openFile = func(path string, flag int, perm os.FileMode, role fileRole) (persistFile, error) {
		f, err := def(path, flag, perm, role)
		if err != nil {
			return nil, err
		}
		if role == roleMain {
			return &faultFile{File: f.(*os.File),
				sync: func() error { return errInjected },
			}, nil
		}
		return f, nil
	}
	if err := db.Recover(); !errors.Is(err, errInjected) {
		t.Fatalf("expected Recover to fail with injected error, got %v", err)
	}
	if db.PersistError() == nil {
		t.Fatal("database should stay degraded after an unsuccessful Recover")
	}
	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("y", "z", nil)
		return err
	}); err != ErrDatabaseReadOnly {
		t.Fatalf("writes must remain refused, got %v", err)
	}

	// With healthy storage restored, Recover confirms the boundary and opens
	// the database for writes again.
	openFile = def
	if err := db.Recover(); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("y", "z", nil)
		return err
	}); err != nil {
		t.Fatalf("writes should resume, got %v", err)
	}
}

// TestFaultShortWriteReopenOnly proves that a plain close and Open (with no
// in-process Recover) also re-establishes the usable boundary: the partial
// tail is detected and truncated during loading.
func TestFaultShortWriteReopenOnly(t *testing.T) {
	db := openFaultDB(t, Always)
	closeOnCleanup(t, &db)

	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("good", "data", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	fired := false
	installFaults(db, func(real *os.File, p []byte) (int, error) {
		if fired {
			return real.Write(p)
		}
		fired = true
		if _, werr := real.Write(p[:len(p)/2]); werr != nil {
			return 0, werr
		}
		return len(p) / 2, errInjected
	}, nil)

	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("bad", "tail", nil)
		return err
	}); err != ErrDatabaseReadOnly {
		t.Fatalf("expected ErrDatabaseReadOnly, got %v", err)
	}

	// Close while degraded and reopen with an untouched public Open. The
	// partial record is discarded and only complete data is visible.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = nil
	db = reopenFaultDB(t, "fault.db")
	if v, err := getOK(db, "good"); err != nil || v != "data" {
		t.Fatalf("good = %q, %v", v, err)
	}
	if _, err := getOK(db, "bad"); err != ErrNotFound {
		t.Fatalf("bad should be truncated away, got %v", err)
	}
	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("new", "write", nil)
		return err
	}); err != nil {
		t.Fatalf("freshly opened database should accept writes, got %v", err)
	}
}
