package buntdb

import (
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"testing"
)

// collectIndex returns all key/value pairs in index order.
func collectIndex(t *testing.T, db *DB, name string) [][2]string {
	t.Helper()
	var pairs [][2]string
	err := db.View(func(tx *Tx) error {
		return tx.Ascend(name, func(key, val string) bool {
			pairs = append(pairs, [2]string{key, val})
			return true
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return pairs
}

func fillKeys(t *testing.T, db *DB, n int) {
	t.Helper()
	err := db.Update(func(tx *Tx) error {
		for i := 0; i < n; i++ {
			_, _, err := tx.Set(fmt.Sprintf("key:%06d", i),
				strconv.Itoa(i), nil)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCreateIndexAsyncBasic(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const n = 10000
	fillKeys(t, db, n)

	b, err := db.CreateIndexAsync("vals", "key:*", nil, IndexString)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Wait(); err != nil {
		t.Fatal(err)
	}
	scanned, total := b.Progress()
	if scanned != n || total != n {
		t.Fatalf("expected progress %d/%d, got %d/%d", n, n, scanned, total)
	}
	pairs := collectIndex(t, db, "vals")
	if len(pairs) != n {
		t.Fatalf("expected %d pairs, got %d", n, len(pairs))
	}
	// values are ordered lexicographically by IndexString.
	for i := 1; i < len(pairs); i++ {
		if pairs[i-1][1] > pairs[i][1] {
			t.Fatalf("pairs out of order at %d: %v > %v",
				i, pairs[i-1], pairs[i])
		}
	}
	// identical to a synchronous rebuild.
	if err := db.Update(func(tx *Tx) error {
		if err := tx.DropIndex("vals"); err != nil {
			return err
		}
		return tx.CreateIndex("vals", "key:*", IndexString)
	}); err != nil {
		t.Fatal(err)
	}
	want := collectIndex(t, db, "vals")
	if len(want) != len(pairs) {
		t.Fatalf("length mismatch: got %d, want %d", len(pairs), len(want))
	}
	for i := range want {
		if pairs[i] != want[i] {
			t.Fatalf("mismatch at %d: got %v, want %v",
				i, pairs[i], want[i])
		}
	}
	names, err := db.Indexes()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "vals" {
		t.Fatalf("expected [vals], got %v", names)
	}
}

func TestCreateIndexAsyncConcurrentWrites(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const n = 100000
	fillKeys(t, db, n)

	b, err := db.CreateIndexAsync("vals", "key:*", nil, IndexString)
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)

	// writer 1: random sets and deletes over the whole keyspace,
	// including keys that do not exist yet.
	wg.Add(1)
	go func() {
		defer wg.Done()
		rnd := rand.New(rand.NewSource(1))
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			key := fmt.Sprintf("key:%06d", rnd.Intn(n+1000))
			val := strconv.Itoa(rnd.Intn(1000000))
			var err error
			if i%5 == 4 {
				err = db.Update(func(tx *Tx) error {
					if _, err := tx.Delete(key); err == ErrNotFound {
						return nil
					} else {
						return err
					}
				})
			} else {
				err = db.Update(func(tx *Tx) error {
					_, _, err := tx.Set(key, val, nil)
					return err
				})
			}
			if err != nil {
				errs <- err
				return
			}
		}
	}()

	// writer 2: repeatedly replaces the values of the lowest keys,
	// which the scan passes first.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			key := fmt.Sprintf("key:%06d", i%128)
			err := db.Update(func(tx *Tx) error {
				_, _, err := tx.Set(key, strconv.Itoa(i), nil)
				return err
			})
			if err != nil {
				errs <- err
				return
			}
		}
	}()

	// let the writers run concurrently with the scan for a while,
	// then stop them and wait for the build to catch up and publish.
	for {
		scanned, total := b.Progress()
		if total != n {
			t.Fatalf("expected total %d, got %d", n, total)
		}
		if scanned >= n/2 {
			break
		}
		select {
		case <-b.Done():
			goto done
		default:
		}
	}
done:
	close(stop)
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
	if err := b.Wait(); err != nil {
		t.Fatal(err)
	}

	// the resulting index must be identical to a synchronous rebuild
	// performed after the writes stopped.
	got := collectIndex(t, db, "vals")
	if err := db.Update(func(tx *Tx) error {
		if err := tx.DropIndex("vals"); err != nil {
			return err
		}
		return tx.CreateIndex("vals", "key:*", IndexString)
	}); err != nil {
		t.Fatal(err)
	}
	want := collectIndex(t, db, "vals")
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mismatch at %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestCreateIndexAsyncNotVisibleDuringBuild(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const n = 200000
	fillKeys(t, db, n)

	b, err := db.CreateIndexAsync("vals", "key:*", nil, IndexString)
	if err != nil {
		t.Fatal(err)
	}
	// the index must not be visible to queries while it is building.
	if err := db.View(func(tx *Tx) error {
		return tx.Ascend("vals", func(key, val string) bool { return true })
	}); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	names, err := db.Indexes()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Fatalf("expected no indexes, got %v", names)
	}
	if err := b.Wait(); err != nil {
		t.Fatal(err)
	}
	// now it must be visible.
	names, err = db.Indexes()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "vals" {
		t.Fatalf("expected [vals], got %v", names)
	}
}

func TestCreateIndexAsyncCancel(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const n = 200000
	fillKeys(t, db, n)

	// an unrelated index that must survive the cancellation.
	if err := db.CreateIndex("other", "key:*", IndexString); err != nil {
		t.Fatal(err)
	}

	b, err := db.CreateIndexAsync("vals", "key:*", nil, IndexString)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Cancel(); err != nil {
		t.Fatal(err)
	}
	if err := b.Wait(); err != ErrIndexBuildCanceled {
		t.Fatalf("expected ErrIndexBuildCanceled, got %v", err)
	}
	// the canceled index must not be visible.
	names, err := db.Indexes()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "other" {
		t.Fatalf("expected [other], got %v", names)
	}
	// the other index must be intact.
	if pairs := collectIndex(t, db, "other"); len(pairs) != n {
		t.Fatalf("expected %d pairs, got %d", n, len(pairs))
	}
	// the name must have been released.
	if err := db.CreateIndex("vals", "key:*", IndexString); err != nil {
		t.Fatal(err)
	}
	if pairs := collectIndex(t, db, "vals"); len(pairs) != n {
		t.Fatalf("expected %d pairs, got %d", n, len(pairs))
	}
}

func TestCreateIndexAsyncOverflow(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const n = 200000
	fillKeys(t, db, n)

	if err := db.CreateIndex("other", "key:*", IndexString); err != nil {
		t.Fatal(err)
	}

	b, err := db.CreateIndexAsync("vals", "key:*",
		&BackgroundIndexOptions{MaxPendingChanges: 16}, IndexString)
	if err != nil {
		t.Fatal(err)
	}
	// change more distinct keys than the pending capacity while the
	// scan is still running.
	err = db.Update(func(tx *Tx) error {
		for i := 0; i < 1000; i++ {
			if _, _, err := tx.Set(fmt.Sprintf("key:%06d", i),
				"changed", nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Wait(); err != ErrIndexBuildOverflow {
		t.Fatalf("expected ErrIndexBuildOverflow, got %v", err)
	}
	// the failed build must not have published anything, and the
	// other index must be intact.
	names, err := db.Indexes()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "other" {
		t.Fatalf("expected [other], got %v", names)
	}
	if pairs := collectIndex(t, db, "other"); len(pairs) != n {
		t.Fatalf("expected %d pairs, got %d", n, len(pairs))
	}
	// the name must have been released.
	if err := db.CreateIndex("vals", "key:*", IndexString); err != nil {
		t.Fatal(err)
	}
}

func TestCreateIndexAsyncPublishConflict(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const n = 200000
	fillKeys(t, db, n)

	b, err := db.CreateIndexAsync("vals", "key:*", nil, IndexString)
	if err != nil {
		t.Fatal(err)
	}
	// a synchronous index with the same name wins the race.
	if err := db.CreateIndex("vals", "key:*", IndexString); err != nil {
		t.Fatal(err)
	}
	if err := b.Wait(); err != ErrIndexExists {
		t.Fatalf("expected ErrIndexExists, got %v", err)
	}
	// the synchronously created index must be intact.
	if pairs := collectIndex(t, db, "vals"); len(pairs) != n {
		t.Fatalf("expected %d pairs, got %d", n, len(pairs))
	}
}

func TestCreateIndexAsyncExists(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.CreateIndex("vals", "key:*", IndexString); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateIndexAsync("vals", "key:*", nil,
		IndexString); err != ErrIndexExists {
		t.Fatalf("expected ErrIndexExists, got %v", err)
	}
	b, err := db.CreateIndexAsync("other", "key:*", nil, IndexString)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Cancel()
	if _, err := db.CreateIndexAsync("other", "key:*", nil,
		IndexString); err != ErrIndexExists {
		t.Fatalf("expected ErrIndexExists, got %v", err)
	}
}

func TestCreateIndexAsyncDeleteAll(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const n = 200000
	fillKeys(t, db, n)

	b, err := db.CreateIndexAsync("vals", "key:*", nil, IndexString)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *Tx) error {
		return tx.DeleteAll()
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Wait(); err != ErrIndexBuildReset {
		t.Fatalf("expected ErrIndexBuildReset, got %v", err)
	}
}

func TestCreateSpatialIndexAsync(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const n = 50000
	if err := db.Update(func(tx *Tx) error {
		for i := 0; i < n; i++ {
			_, _, err := tx.Set(fmt.Sprintf("key:%06d", i),
				Point(float64(i), float64(i%100)), nil)
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	b, err := db.CreateSpatialIndexAsync("points", "key:*", nil, IndexRect)
	if err != nil {
		t.Fatal(err)
	}

	// mutate some points while the build is running.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rnd := rand.New(rand.NewSource(2))
		for {
			select {
			case <-stop:
				return
			default:
			}
			i := rnd.Intn(n)
			_ = db.Update(func(tx *Tx) error {
				_, _, err := tx.Set(fmt.Sprintf("key:%06d", i),
					Point(float64(i), 50), nil)
				return err
			})
		}
	}()
	<-b.Done()
	close(stop)
	wg.Wait()
	if err := b.Wait(); err != nil {
		t.Fatal(err)
	}

	count := func(name string) int {
		var c int
		if err := db.View(func(tx *Tx) error {
			return tx.Intersects(name, Rect([]float64{0, 0},
				[]float64{50000, 100}), func(key, val string) bool {
				c++
				return true
			})
		}); err != nil {
			t.Fatal(err)
		}
		return c
	}
	got := count("points")
	if err := db.Update(func(tx *Tx) error {
		if err := tx.DropIndex("points"); err != nil {
			return err
		}
		return tx.CreateSpatialIndex("points", "key:*", IndexRect)
	}); err != nil {
		t.Fatal(err)
	}
	if want := count("points"); got != want {
		t.Fatalf("expected %d points, got %d", want, got)
	}
}
