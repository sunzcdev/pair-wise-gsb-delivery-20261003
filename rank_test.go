// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// rankPlayer is a leaderboard-style record used to exercise Rank and Select.
type rankPlayer struct {
	ID    string
	Score int
}

func testRankSchema() *DBSchema {
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
						Name:    "score",
						Unique:  false,
						Indexer: &IntFieldIndex{Field: "Score"},
					},
				},
			},
		},
	}
}

func testRankDB(t *testing.T) *MemDB {
	db, err := NewMemDB(testRankSchema())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	return db
}

// sortPlayers orders players the same way the non-unique "score" index does:
// by score, breaking ties with the primary key.
func sortPlayers(players []*rankPlayer) {
	sort.Slice(players, func(i, j int) bool {
		if players[i].Score != players[j].Score {
			return players[i].Score < players[j].Score
		}
		return players[i].ID < players[j].ID
	})
}

func makePlayers(n int, scoreFn func(i int) int) []*rankPlayer {
	players := make([]*rankPlayer, 0, n)
	for i := 0; i < n; i++ {
		players = append(players, &rankPlayer{
			ID:    fmt.Sprintf("player-%08d", i),
			Score: scoreFn(i),
		})
	}
	return players
}

func insertPlayers(t *testing.T, db *MemDB, players []*rankPlayer) {
	txn := db.Txn(true)
	for _, p := range players {
		if err := txn.Insert("player", p); err != nil {
			t.Fatalf("err: %v", err)
		}
	}
	txn.Commit()
}

func TestTxn_RankSelect(t *testing.T) {
	db := testRankDB(t)

	// Use heavily duplicated scores (only 10 distinct values) to verify
	// that ties are broken by primary key into a stable order.
	players := makePlayers(1000, func(i int) int { return i % 10 })
	insertPlayers(t, db, players)

	sorted := make([]*rankPlayer, len(players))
	copy(sorted, players)
	sortPlayers(sorted)

	txn := db.Txn(false)
	defer txn.Abort()

	// Every object must rank at its sorted position, and Select must
	// invert Rank exactly.
	for want, p := range sorted {
		got, err := txn.Rank("player", "score", p)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got != want {
			t.Fatalf("bad rank for %#v: got %d, want %d", p, got, want)
		}

		obj, err := txn.Select("player", "score", want)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if obj != p {
			t.Fatalf("bad select at %d: got %#v, want %#v", want, obj, p)
		}
	}

	// Boundary checks
	first, err := txn.Select("player", "score", 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if first != sorted[0] {
		t.Fatalf("bad first: %#v", first)
	}
	last, err := txn.Select("player", "score", len(sorted)-1)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if last != sorted[len(sorted)-1] {
		t.Fatalf("bad last: %#v", last)
	}
}

func TestTxn_RankSelect_UniqueIndex(t *testing.T) {
	db := testRankDB(t)
	players := makePlayers(100, func(i int) int { return i })
	insertPlayers(t, db, players)

	sorted := make([]*rankPlayer, len(players))
	copy(sorted, players)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	txn := db.Txn(false)
	defer txn.Abort()

	for want, p := range sorted {
		got, err := txn.Rank("player", "id", p)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got != want {
			t.Fatalf("bad rank for %#v: got %d, want %d", p, got, want)
		}
		obj, err := txn.Select("player", "id", want)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if obj != p {
			t.Fatalf("bad select at %d: %#v", want, obj)
		}
	}
}

func TestTxn_RankSelect_SnapshotIsolation(t *testing.T) {
	db := testRankDB(t)
	players := makePlayers(1000, func(i int) int { return i * 2 })
	insertPlayers(t, db, players)

	// Pin a read transaction and record what it sees.
	old := db.Txn(false)
	defer old.Abort()

	type observation struct {
		rank int
		obj  interface{}
	}
	before := make(map[string]observation)
	for _, p := range players {
		rank, err := old.Rank("player", "score", p)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		obj, err := old.Select("player", "score", rank)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		before[p.ID] = observation{rank, obj}
	}

	// Continuously insert, delete and re-score while the read transaction
	// is still open.
	for round := 0; round < 10; round++ {
		txn := db.Txn(true)
		// Insert new players interleaved into the existing order.
		for i := 0; i < 50; i++ {
			p := &rankPlayer{
				ID:    fmt.Sprintf("extra-%04d-%04d", round, i),
				Score: i*37 + round,
			}
			if err := txn.Insert("player", p); err != nil {
				t.Fatalf("err: %v", err)
			}
		}
		// Delete some existing players.
		for i := 0; i < 50; i++ {
			if err := txn.Delete("player", players[(round*50+i)%len(players)]); err != nil {
				t.Fatalf("err: %v", err)
			}
		}
		// Update scores of other existing players.
		for i := 0; i < 50; i++ {
			victim := players[(round*50+i+500)%len(players)]
			updated := &rankPlayer{ID: victim.ID, Score: victim.Score + 10000}
			if err := txn.Insert("player", updated); err != nil {
				t.Fatalf("err: %v", err)
			}
		}
		txn.Commit()
	}

	// The pinned transaction must observe exactly the same ranks and
	// entries as before the writes: no drift.
	for _, p := range players {
		rank, err := old.Rank("player", "score", p)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		obj, err := old.Select("player", "score", rank)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		want := before[p.ID]
		if rank != want.rank || obj != want.obj {
			t.Fatalf("snapshot drift for %s: got (%d, %#v), want (%d, %#v)",
				p.ID, rank, obj, want.rank, want.obj)
		}
	}

	// A fresh transaction sees the new state, and Rank/Select still agree.
	fresh := db.Txn(false)
	defer fresh.Abort()
	count := 0
	iter, err := fresh.Get("player", "score")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	for obj := iter.Next(); obj != nil; obj = iter.Next() {
		p := obj.(*rankPlayer)
		rank, err := fresh.Rank("player", "score", p)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if rank != count {
			t.Fatalf("bad rank after writes for %#v: got %d, want %d", p, rank, count)
		}
		sel, err := fresh.Select("player", "score", rank)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if sel != p {
			t.Fatalf("bad select after writes at %d: %#v", rank, sel)
		}
		count++
	}
}

func TestTxn_Rank_MissingObject(t *testing.T) {
	db := testRankDB(t)
	players := makePlayers(100, func(i int) int { return i * 10 })
	insertPlayers(t, db, players)

	txn := db.Txn(false)
	defer txn.Abort()

	// An object that was never inserted gets the rank it would occupy.
	ghost := &rankPlayer{ID: "player-00000500", Score: 505}
	rank, err := txn.Rank("player", "score", ghost)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Scores 0..500 step 10 => 51 entries sort before score 505.
	if rank != 51 {
		t.Fatalf("bad rank for missing object: got %d, want 51", rank)
	}
}

func TestTxn_RankSelect_Errors(t *testing.T) {
	db := testRankDB(t)
	players := makePlayers(10, func(i int) int { return i })
	insertPlayers(t, db, players)

	txn := db.Txn(false)
	defer txn.Abort()

	if _, err := txn.Rank("nope", "score", players[0]); err == nil {
		t.Fatalf("expected error for invalid table")
	}
	if _, err := txn.Rank("player", "nope", players[0]); err == nil {
		t.Fatalf("expected error for invalid index")
	}
	if _, err := txn.Select("nope", "score", 0); err == nil {
		t.Fatalf("expected error for invalid table")
	}
	if _, err := txn.Select("player", "nope", 0); err == nil {
		t.Fatalf("expected error for invalid index")
	}
	if _, err := txn.Select("player", "score", -1); err != ErrNotFound {
		t.Fatalf("bad: %v", err)
	}
	if _, err := txn.Select("player", "score", len(players)); err != ErrNotFound {
		t.Fatalf("bad: %v", err)
	}
}

func benchmarkRankDB(b *testing.B, n int) (*MemDB, []*rankPlayer) {
	db, err := NewMemDB(testRankSchema())
	if err != nil {
		b.Fatalf("err: %v", err)
	}
	rng := rand.New(rand.NewSource(42))
	players := makePlayers(n, func(i int) int { return rng.Intn(n) })
	txn := db.Txn(true)
	for _, p := range players {
		if err := txn.Insert("player", p); err != nil {
			b.Fatalf("err: %v", err)
		}
	}
	txn.Commit()
	return db, players
}

func BenchmarkTxn_Select(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000, 1000000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			db, players := benchmarkRankDB(b, n)
			txn := db.Txn(false)
			defer txn.Abort()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rank := i % len(players)
				if _, err := txn.Select("player", "score", rank); err != nil {
					b.Fatalf("err: %v", err)
				}
			}
		})
	}
}

func BenchmarkTxn_Rank(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000, 1000000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			db, players := benchmarkRankDB(b, n)
			txn := db.Txn(false)
			defer txn.Abort()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := txn.Rank("player", "score", players[i%len(players)]); err != nil {
					b.Fatalf("err: %v", err)
				}
			}
		})
	}
}
