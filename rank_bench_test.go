// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import "testing"

func benchmarkLeaderboard(b *testing.B, n int) {
	db, err := NewMemDB(leaderboardSchema())
	if err != nil {
		b.Fatal(err)
	}

	txn := db.Txn(true)
	players := make([]*Player, n)
	for i := 0; i < n; i++ {
		// Scores repeat across a small range, producing dense tie groups that
		// exercise the primary-key tiebreak.
		p := &Player{ID: benchID(i), Score: i % 1000}
		players[i] = p
		if err := txn.Insert("player", p); err != nil {
			b.Fatal(err)
		}
	}
	txn.Commit()

	read := db.Txn(false)
	b.Cleanup(func() { read.Abort() })

	mid := n/2 + 1
	midPlayer := players[mid-1]

	// Scores repeat every 1000, so the middle player's rank is determined by
	// its (score, id). Compute the expected rank once up front.
	wantRank := 1
	for i := 0; i < n; i++ {
		if i == mid-1 {
			continue
		}
		if players[i].Score < midPlayer.Score ||
			(players[i].Score == midPlayer.Score && players[i].ID < midPlayer.ID) {
			wantRank++
		}
	}

	b.Run("select/middle", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			obj, err := read.Select("player", "score", Ascending, mid)
			if err != nil || obj == nil {
				b.Fatalf("select failed: %v %v", obj, err)
			}
		}
	})

	b.Run("select/last", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			obj, err := read.Select("player", "score", Descending, 1)
			if err != nil || obj == nil {
				b.Fatalf("select failed: %v %v", obj, err)
			}
		}
	})

	b.Run("rank/object", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			rank, found, err := read.RankObject("player", "score", Ascending, midPlayer)
			if err != nil || !found || rank != wantRank {
				b.Fatalf("rank failed: %d %v %v (want %d)", rank, found, err, wantRank)
			}
		}
	})

	// Baseline: the pre-existing approach of iterating from the start and
	// discarding every entry before the wanted rank. This is O(rank) work and
	// exists purely to contrast with the O(log n) Select above.
	b.Run("naive-iterate/middle", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			it, err := read.Get("player", "score")
			if err != nil {
				b.Fatal(err)
			}
			var obj interface{}
			for r := 0; r < mid; r++ {
				obj = it.Next()
			}
			if obj == nil {
				b.Fatal("naive select returned nil")
			}
		}
	})
}

func benchID(i int) string {
	const digits = "0123456789"
	buf := make([]byte, 8)
	for j := 7; j >= 0; j-- {
		buf[j] = digits[i%10]
		i /= 10
	}
	return "p" + string(buf)
}

func BenchmarkRankSelect_1k(b *testing.B)   { benchmarkLeaderboard(b, 1_000) }
func BenchmarkRankSelect_10k(b *testing.B)  { benchmarkLeaderboard(b, 10_000) }
func BenchmarkRankSelect_100k(b *testing.B) { benchmarkLeaderboard(b, 100_000) }

// The 500k and 1m cases are skipped in short mode so the default suite stays
// fast; run them explicitly with `-run BenchmarkRankSelect -bench .`.
func BenchmarkRankSelect_500k(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping large leaderboard in short mode")
	}
	benchmarkLeaderboard(b, 500_000)
}

func BenchmarkRankSelect_1m(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping large leaderboard in short mode")
	}
	benchmarkLeaderboard(b, 1_000_000)
}
