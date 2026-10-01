// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"testing"
)

// Player is a leaderboard row. Score is the (non-unique) sortable value; ID
// is the unique primary key used to break score ties.
type Player struct {
	ID    string
	Score int
}

func leaderboardSchema() *DBSchema {
	return &DBSchema{
		Tables: map[string]*TableSchema{
			"player": {
				Name: "player",
				Indexes: map[string]*IndexSchema{
					"id": {
						Name:    "id",
						Unique:  true,
						Indexer: &StringFieldIndex{Field: "ID"},
					},
					"score": {
						Name:     "score",
						Unique:   false,
						Sortable: true,
						Indexer:  &IntFieldIndex{Field: "Score"},
					},
				},
			},
		},
	}
}

func mustCommit(t *testing.T, db *MemDB, fn func(*Txn)) {
	t.Helper()
	txn := db.Txn(true)
	fn(txn)
	txn.Commit()
}

// rankedPlayers is the independently computed reference ordering: ascending
// by (Score, ID).
func rankedPlayers(players []*Player) []*Player {
	out := append([]*Player(nil), players...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score < out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func TestRankSelect_BasicAndTies(t *testing.T) {
	db, err := NewMemDB(leaderboardSchema())
	if err != nil {
		t.Fatal(err)
	}

	// Deliberately include repeated scores so the primary-key tiebreak is
	// exercised.
	players := []*Player{
		{ID: "carol", Score: 30},
		{ID: "alice", Score: 30},
		{ID: "bob", Score: 10},
		{ID: "dave", Score: 50},
		{ID: "erin", Score: 30},
	}
	mustCommit(t, db, func(txn *Txn) {
		for _, p := range players {
			if err := txn.Insert("player", p); err != nil {
				t.Fatal(err)
			}
		}
	})

	want := rankedPlayers(players)
	txn := db.Txn(false)
	defer txn.Abort()

	if n, err := txn.Count("player", "score"); err != nil || n != len(players) {
		t.Fatalf("count = %d (%v), want %d", n, err, len(players))
	}

	// Ascending Select should match the reference ordering.
	for i, p := range want {
		raw, err := txn.Select("player", "score", Ascending, i+1)
		if err != nil {
			t.Fatal(err)
		}
		if raw.(*Player) != p {
			t.Fatalf("asc select(%d) = %v, want %v", i+1, raw.(*Player).ID, p.ID)
		}

		// RankObject must be the exact inverse.
		rank, found, err := txn.RankObject("player", "score", Ascending, p)
		if err != nil || !found || rank != i+1 {
			t.Fatalf("asc rank(%v) = %d found=%v err=%v, want %d", p.ID, rank, found, err, i+1)
		}

		// Descending is the mirror image.
		descRank, found, err := txn.RankObject("player", "score", Descending, p)
		if err != nil || !found || descRank != len(want)-i {
			t.Fatalf("desc rank(%v) = %d found=%v err=%v, want %d", p.ID, descRank, found, err, len(want)-i)
		}
		raw, err = txn.Select("player", "score", Descending, descRank)
		if err != nil || raw.(*Player) != p {
			t.Fatalf("desc select(%d) mismatch: %v err=%v", descRank, raw, err)
		}
	}

	// The 30-point group must come out ordered by ID: alice, carol, erin.
	got := []string{}
	for _, p := range want {
		if p.Score == 30 {
			got = append(got, p.ID)
		}
	}
	wantTies := []string{"alice", "carol", "erin"}
	if fmt.Sprint(got) != fmt.Sprint(wantTies) {
		t.Fatalf("tie order = %v, want %v", got, wantTies)
	}

	// Out of range and invalid ranks.
	if raw, err := txn.Select("player", "score", Ascending, 99); err != nil || raw != nil {
		t.Fatalf("out of range select = %v, %v", raw, err)
	}
	if raw, err := txn.Select("player", "score", Ascending, 0); err != nil || raw != nil {
		t.Fatalf("rank 0 select = %v, %v", raw, err)
	}
}

func TestRankSelect_UpdateAndDelete(t *testing.T) {
	db, _ := NewMemDB(leaderboardSchema())
	players := []*Player{
		{ID: "a", Score: 1},
		{ID: "b", Score: 2},
		{ID: "c", Score: 3},
		{ID: "d", Score: 2},
	}
	mustCommit(t, db, func(txn *Txn) {
		for _, p := range players {
			txn.Insert("player", p)
		}
	})

	mustCommit(t, db, func(txn *Txn) {
		// Move b from score 2 to score 5.
		if err := txn.Insert("player", &Player{ID: "b", Score: 5}); err != nil {
			t.Fatal(err)
		}
		// Delete c entirely.
		if err := txn.Delete("player", &Player{ID: "c", Score: 3}); err != nil {
			t.Fatal(err)
		}
	})

	remaining := []*Player{
		{ID: "a", Score: 1},
		{ID: "d", Score: 2},
		{ID: "b", Score: 5},
	}
	want := rankedPlayers(remaining)

	txn := db.Txn(false)
	defer txn.Abort()
	if n, _ := txn.Count("player", "score"); n != len(remaining) {
		t.Fatalf("count = %d, want %d", n, len(remaining))
	}
	for i, p := range want {
		raw, err := txn.Select("player", "score", Ascending, i+1)
		if err != nil || raw.(*Player).ID != p.ID {
			t.Fatalf("select(%d) = %v err=%v, want %v", i+1, raw, err, p.ID)
		}
		rank, found, err := txn.RankObject("player", "score", Ascending, p)
		if err != nil || !found || rank != i+1 {
			t.Fatalf("rank(%v) = %d found=%v err=%v", p.ID, rank, found, err)
		}
	}

	// The deleted player must no longer rank.
	if _, found, err := txn.RankObject("player", "score", Ascending, &Player{ID: "c", Score: 3}); err != nil || found {
		t.Fatalf("deleted player found=%v err=%v", found, err)
	}
}

func TestRankSelect_MVCCSkew(t *testing.T) {
	db, _ := NewMemDB(leaderboardSchema())

	const n = 1000
	initial := make([]*Player, n)
	mustCommit(t, db, func(txn *Txn) {
		for i := 0; i < n; i++ {
			initial[i] = &Player{ID: fmt.Sprintf("p%04d", i), Score: i}
			if err := txn.Insert("player", initial[i]); err != nil {
				t.Fatal(err)
			}
		}
	})

	// Open a long-lived read transaction and capture its expected ordering.
	old := db.Txn(false)
	defer old.Abort()

	oldSnapshot := make([]string, n)
	for i := 0; i < n; i++ {
		raw, err := old.Select("player", "score", Ascending, i+1)
		if err != nil {
			t.Fatal(err)
		}
		oldSnapshot[i] = raw.(*Player).ID
	}

	// Writers churn the table: delete half, change scores, add new players.
	mustCommit(t, db, func(txn *Txn) {
		for i := 0; i < n; i += 2 {
			if err := txn.Delete("player", initial[i]); err != nil {
				t.Fatal(err)
			}
		}
		for i := 1; i < n; i += 2 {
			if err := txn.Insert("player", &Player{ID: fmt.Sprintf("p%04d", i), Score: -i}); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < n; i++ {
			if err := txn.Insert("player", &Player{ID: fmt.Sprintf("q%04d", i), Score: 10000 + i}); err != nil {
				t.Fatal(err)
			}
		}
	})

	// The old transaction must still report exactly its snapshot.
	if count, _ := old.Count("player", "score"); count != n {
		t.Fatalf("old txn count drifted to %d, want %d", count, n)
	}
	for i, wantID := range oldSnapshot {
		raw, err := old.Select("player", "score", Ascending, i+1)
		if err != nil {
			t.Fatal(err)
		}
		if raw.(*Player).ID != wantID {
			t.Fatalf("old txn rank %d drifted to %v, want %v", i+1, raw.(*Player).ID, wantID)
		}
	}

	// A fresh transaction sees the new state size.
	fresh := db.Txn(false)
	defer fresh.Abort()
	wantCount := n - n/2 + n
	if count, _ := fresh.Count("player", "score"); count != wantCount {
		t.Fatalf("fresh count = %d, want %d", count, wantCount)
	}
}

func TestRankSelect_RandomizedConsistency(t *testing.T) {
	db, _ := NewMemDB(leaderboardSchema())
	rng := rand.New(rand.NewSource(99))

	live := map[string]*Player{}

	apply := func() {
		mustCommit(t, db, func(txn *Txn) {
			for op := 0; op < 200; op++ {
				id := fmt.Sprintf("id-%03d", rng.Intn(50))
				switch rng.Intn(3) {
				case 0, 1:
					p := &Player{ID: id, Score: rng.Intn(10)}
					if err := txn.Insert("player", p); err != nil {
						t.Fatal(err)
					}
					live[id] = p
				case 2:
					if p, ok := live[id]; ok {
						if err := txn.Delete("player", p); err != nil {
							t.Fatal(err)
						}
						delete(live, id)
					}
				}
			}
		})
	}

	for round := 0; round < 20; round++ {
		apply()

		players := make([]*Player, 0, len(live))
		for _, p := range live {
			players = append(players, p)
		}
		want := rankedPlayers(players)

		txn := db.Txn(false)
		if n, _ := txn.Count("player", "score"); n != len(want) {
			t.Fatalf("round %d: count = %d, want %d", round, n, len(want))
		}
		for i, p := range want {
			raw, err := txn.Select("player", "score", Ascending, i+1)
			if err != nil {
				t.Fatal(err)
			}
			got := raw.(*Player)
			if got.ID != p.ID || got.Score != p.Score {
				t.Fatalf("round %d select(%d) = %v, want %v", round, i+1, got, p)
			}
			rank, found, err := txn.RankObject("player", "score", Ascending, p)
			if err != nil || !found || rank != i+1 {
				t.Fatalf("round %d rank(%v) = %d found=%v err=%v, want %d", round, p.ID, rank, found, err, i+1)
			}
		}
		txn.Abort()
	}
}

func TestRankSelect_NotSortableAndUnique(t *testing.T) {
	schema := &DBSchema{
		Tables: map[string]*TableSchema{
			"t": {
				Name: "t",
				Indexes: map[string]*IndexSchema{
					"id":    {Name: "id", Unique: true, Indexer: &StringFieldIndex{Field: "ID"}},
					"score": {Name: "score", Indexer: &IntFieldIndex{Field: "Score"}},
				},
			},
		},
	}
	db, _ := NewMemDB(schema)
	txn := db.Txn(false)
	defer txn.Abort()

	if _, err := txn.Select("t", "score", Ascending, 1); err != ErrNotSortable {
		t.Fatalf("select err = %v, want ErrNotSortable", err)
	}
	if _, _, err := txn.RankObject("t", "score", Ascending, &Player{ID: "x", Score: 1}); err != ErrNotSortable {
		t.Fatalf("rank err = %v, want ErrNotSortable", err)
	}
}

func TestRankSelect_DeletePrefixKeepsOrderTreeConsistent(t *testing.T) {
	// "id" is both the primary key, a prefix index, and a sortable index here,
	// exercising the order-tree maintenance performed by DeletePrefix for the
	// prefix index itself (not just the other indexes).
	schema := &DBSchema{
		Tables: map[string]*TableSchema{
			"player": {
				Name: "player",
				Indexes: map[string]*IndexSchema{
					"id": {
						Name:     "id",
						Unique:   true,
						Sortable: true,
						Indexer:  &StringFieldIndex{Field: "ID"},
					},
					"score": {
						Name:     "score",
						Sortable: true,
						Indexer:  &IntFieldIndex{Field: "Score"},
					},
				},
			},
		},
	}
	db, _ := NewMemDB(schema)

	mustCommit(t, db, func(txn *Txn) {
		for _, p := range []*Player{
			{ID: "rm-1", Score: 1},
			{ID: "rm-2", Score: 2},
			{ID: "keep-1", Score: 3},
			{ID: "rm-3", Score: 4},
			{ID: "keep-2", Score: 5},
		} {
			if err := txn.Insert("player", p); err != nil {
				t.Fatal(err)
			}
		}
	})

	mustCommit(t, db, func(txn *Txn) {
		deleted, err := txn.DeletePrefix("player", "id_prefix", "rm-")
		if err != nil || !deleted {
			t.Fatalf("DeletePrefix = %v, %v", deleted, err)
		}
	})

	txn := db.Txn(false)
	defer txn.Abort()

	if n, _ := txn.Count("player", "id"); n != 2 {
		t.Fatalf("id order count = %d, want 2", n)
	}
	if n, _ := txn.Count("player", "score"); n != 2 {
		t.Fatalf("score order count = %d, want 2", n)
	}

	// Surviving rows must still rank consistently in both sortable indexes.
	raw, err := txn.Select("player", "id", Ascending, 1)
	if err != nil || raw.(*Player).ID != "keep-1" {
		t.Fatalf("id select(1) = %v, %v", raw, err)
	}
	raw, err = txn.Select("player", "score", Ascending, 1)
	if err != nil || raw.(*Player).ID != "keep-1" {
		t.Fatalf("score select(1) = %v, %v", raw, err)
	}

	for _, p := range []*Player{{ID: "rm-1", Score: 1}, {ID: "rm-2", Score: 2}, {ID: "rm-3", Score: 4}} {
		if _, found, err := txn.RankObject("player", "id", Ascending, p); err != nil || found {
			t.Fatalf("deleted %v still in id order: found=%v err=%v", p.ID, found, err)
		}
	}
}

func TestRank_UniqueIndexByArgs(t *testing.T) {
	schema := &DBSchema{
		Tables: map[string]*TableSchema{
			"player": {
				Name: "player",
				Indexes: map[string]*IndexSchema{
					"id": {
						Name:     "id",
						Unique:   true,
						Sortable: true,
						Indexer:  &StringFieldIndex{Field: "ID"},
					},
				},
			},
		},
	}
	db, _ := NewMemDB(schema)
	mustCommit(t, db, func(txn *Txn) {
		for _, id := range []string{"charlie", "alpha", "bravo"} {
			if err := txn.Insert("player", &Player{ID: id}); err != nil {
				t.Fatal(err)
			}
		}
	})

	txn := db.Txn(false)
	defer txn.Abort()

	rank, found, err := txn.Rank("player", "id", Ascending, "bravo")
	if err != nil || !found || rank != 2 {
		t.Fatalf("Rank(bravo) = %d found=%v err=%v, want 2", rank, found, err)
	}
	rank, found, err = txn.Rank("player", "id", Descending, "bravo")
	if err != nil || !found || rank != 2 {
		t.Fatalf("desc Rank(bravo) = %d found=%v err=%v, want 2", rank, found, err)
	}
	if _, found, err := txn.Rank("player", "id", Ascending, "missing"); err != nil || found {
		t.Fatalf("Rank(missing) found=%v err=%v", found, err)
	}
}

func TestRankSelect_ConcurrentReaderStable(t *testing.T) {
	db, _ := NewMemDB(leaderboardSchema())

	const n = 20000
	mustCommit(t, db, func(txn *Txn) {
		for i := 0; i < n; i++ {
			if err := txn.Insert("player", &Player{ID: fmt.Sprintf("p%05d", i), Score: i}); err != nil {
				t.Fatal(err)
			}
		}
	})

	// Hold a read transaction open while writers churn the database.
	old := db.Txn(false)
	defer old.Abort()

	target, err := old.Select("player", "score", Ascending, n/3+1)
	if err != nil {
		t.Fatal(err)
	}
	targetRank, found, err := old.RankObject("player", "score", Ascending, target)
	if err != nil || !found {
		t.Fatalf("initial rank failed: %v %v", found, err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for round := 0; round < 20; round++ {
			w := db.Txn(true)
			for i := 0; i < 1000; i++ {
				id := fmt.Sprintf("z%02d-%05d", round, i)
				if err := w.Insert("player", &Player{ID: id, Score: round*1000 + i}); err != nil {
					panic(err)
				}
			}
			if round == 0 {
				for i := 0; i < n; i += 7 {
					if err := w.Delete("player", &Player{ID: fmt.Sprintf("p%05d", i), Score: i}); err != nil {
						panic(err)
					}
				}
			}
			w.Commit()
		}
	}()

	// While writes are in flight the old snapshot must remain stable.
	for i := 0; i < 200; i++ {
		obj, err := old.Select("player", "score", Ascending, targetRank)
		if err != nil || obj.(*Player).ID != target.(*Player).ID {
			t.Fatalf("concurrent read drifted: %v err=%v want %v", obj, err, target.(*Player).ID)
		}
	}
	wg.Wait()
}

func TestRankSelect_DBSnapshotIsolation(t *testing.T) {
	db, _ := NewMemDB(leaderboardSchema())
	mustCommit(t, db, func(txn *Txn) {
		for _, p := range []*Player{
			{ID: "a", Score: 1},
			{ID: "b", Score: 2},
			{ID: "c", Score: 3},
		} {
			if err := txn.Insert("player", p); err != nil {
				t.Fatal(err)
			}
		}
	})

	snap := db.Snapshot()

	mustCommit(t, db, func(txn *Txn) {
		if err := txn.Insert("player", &Player{ID: "d", Score: 4}); err != nil {
			t.Fatal(err)
		}
	})

	stx := snap.Txn(false)
	defer stx.Abort()
	if n, _ := stx.Count("player", "score"); n != 3 {
		t.Fatalf("snapshot count = %d, want 3", n)
	}
	if raw, err := stx.Select("player", "score", Ascending, 3); err != nil || raw.(*Player).ID != "c" {
		t.Fatalf("snapshot select(3) = %v, %v, want c", raw, err)
	}
}

func TestRankSelect_IndependentSnapshotWriters(t *testing.T) {
	db, _ := NewMemDB(leaderboardSchema())
	mustCommit(t, db, func(txn *Txn) {
		txn.Insert("player", &Player{ID: "seed", Score: 0})
	})

	// Two snapshots share the same underlying root and PRNG state.
	snapA := db.Snapshot()
	snapB := db.Snapshot()

	mustCommit(t, snapA, func(txn *Txn) {
		for i := 0; i < 500; i++ {
			if err := txn.Insert("player", &Player{ID: fmt.Sprintf("a%04d", i), Score: i}); err != nil {
				t.Fatal(err)
			}
		}
	})
	mustCommit(t, snapB, func(txn *Txn) {
		for i := 0; i < 500; i++ {
			if err := txn.Insert("player", &Player{ID: fmt.Sprintf("b%04d", i), Score: i}); err != nil {
				t.Fatal(err)
			}
		}
	})

	// Each snapshot must be internally consistent and contain only its branch.
	for _, s := range []*MemDB{snapA, snapB} {
		read := s.Txn(false)
		count, _ := read.Count("player", "score")
		if count != 501 {
			t.Fatalf("count = %d, want 501", count)
		}
		for r := 1; r <= count; r++ {
			obj, err := read.Select("player", "score", Ascending, r)
			if err != nil || obj == nil {
				t.Fatalf("select(%d) failed: %v %v", r, obj, err)
			}
			rank, found, err := read.RankObject("player", "score", Ascending, obj)
			if err != nil || !found || rank != r {
				t.Fatalf("rank/select mismatch at %d: %d %v %v", r, rank, found, err)
			}
		}
		read.Abort()
	}
}
