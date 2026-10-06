package lrc

import (
	"errors"
	"math/rand"
	"testing"
)

// oracleRepairable independently decides whether a full repair of the lost
// shards exists:
//  1. Groups with <= p losses are locally fixed.
//  2. Afterwards, every still-missing data shard and every missing global
//     parity shard is one unknown in the flat (k,r) stripe; that succeeds iff
//     their total is <= r.
//  3. Lost local parity is regenerated from the (by then complete) group data.
func oracleRepairable(ly *Layout, lost map[int]bool) bool {
	unresolvedData := 0
	for g := 0; g < ly.l; g++ {
		n := 0
		dataLost := 0
		for s := 0; s < ly.dataPerGroup+ly.p; s++ {
			idx := ly.LocalShardIndex(g, s)
			if lost[idx] {
				n++
				if s < ly.dataPerGroup {
					dataLost++
				}
			}
		}
		if n > ly.p {
			unresolvedData += dataLost
		}
	}
	globalLost := 0
	for j := 0; j < ly.r; j++ {
		if lost[ly.GlobalParityIndex(j)] {
			globalLost++
		}
	}
	return unresolvedData+globalLost <= ly.r
}

func TestRandomizedLayouts(t *testing.T) {
	configs := [][4]int{
		{8, 4, 2, 3},  // 2 data/group, 2 local, 3 global
		{12, 4, 2, 3}, // 3 data/group
		{9, 3, 1, 3},  // 3 data/group
	}
	rng := rand.New(rand.NewSource(42))

	for _, cfg := range configs {
		ly, err := NewLayout(cfg[0], cfg[1], cfg[2], cfg[3])
		if err != nil {
			t.Fatal(err)
		}
		original := encodedShards(ly, rng)

		for iter := 0; iter < 400; iter++ {
			lost := map[int]bool{}
			// Bias towards small loss counts, but occasionally try many.
			n := 1 + rng.Intn(ly.Shards())
			for len(lost) < n {
				lost[rng.Intn(ly.Shards())] = true
			}
			list := make([]int, 0, n)
			for i := range lost {
				list = append(list, i)
			}

			damaged := wipe(original, list...)
			plan, planErr := ly.RepairPlan(damaged)
			wantOK := oracleRepairable(ly, lost)

			if wantOK {
				if planErr != nil {
					t.Fatalf("cfg%v lost=%v oracle=ok plan=%v", cfg, list, planErr)
				}
				if plan.ReadCount > ly.DataShards()+ly.GlobalParity() {
					t.Fatalf("cfg%v lost=%v read %d shards, implausible", cfg, list, plan.ReadCount)
				}
				if err := ly.Execute(damaged, plan); err != nil {
					t.Fatalf("cfg%v lost=%v execute=%v", cfg, list, err)
				}
				if !shardsEqual(original, damaged) {
					t.Fatalf("cfg%v lost=%v data mismatch", cfg, list)
				}
				// A single data shard loss must always be a local-only
				// repair that reads exactly dataPerGroup shards.
				if len(list) == 1 && list[0] < ly.k &&
					(plan.ReadCount != ly.DataPerGroup() || plan.Steps[0].Tier != TierLocal) {
					t.Fatalf("cfg%v single data loss plan=%+v, want local %d reads",
						cfg, plan.Steps, ly.DataPerGroup())
				}
			} else {
				if !errors.Is(planErr, ErrUnrecoverable) {
					t.Fatalf("cfg%v lost=%v oracle=fail plan=%v", cfg, list, planErr)
				}
			}
		}
	}
}
