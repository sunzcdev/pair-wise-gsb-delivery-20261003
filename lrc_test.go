package reedsolomon_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/klauspost/reedsolomon"
)

// labels renders physical shard indices as short names: D0.., local Lg and
// global Gj.
func lrcLabels(p reedsolomon.LRCLayoutParams) []string {
	names := make([]string, p.TotalShards())
	for i := 0; i < p.DataShards; i++ {
		names[i] = fmt.Sprintf("D%d", i)
	}
	for g := range p.GroupSizes {
		idx, _ := p.LocalParityIndex(g)
		names[idx] = fmt.Sprintf("L%d", g)
	}
	for j := 0; j < p.GlobalParityShards; j++ {
		idx, _ := p.GlobalParityIndex(j)
		names[idx] = fmt.Sprintf("G%d", j)
	}
	return names
}

func makeLRCStripe(t *testing.T, codec *reedsolomon.LRC, shardSize int) [][]byte {
	t.Helper()
	shards := codec.AllocShards(shardSize)
	for i := 0; i < codec.DataShards(); i++ {
		fillRandom(shards[i])
	}
	if err := codec.Encode(shards); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return shards
}

func TestLRCParamsValidateAndPersist(t *testing.T) {
	p, err := reedsolomon.NewUniformLRC(12, 3, 2)
	if err != nil {
		t.Fatalf("NewUniformLRC: %v", err)
	}
	if got, want := p.String(), "LRC(12,[4 4 4],2)"; got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
	if got := p.TotalShards(); got != 17 {
		t.Fatalf("TotalShards = %d, want 17", got)
	}

	raw, err := reedsolomon.MarshalLRCLayoutParams(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	restored, err := reedsolomon.UnmarshalLRCLayoutParams(raw)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if restored.String() != p.String() {
		t.Fatalf("round trip mismatch: %q != %q", restored.String(), p.String())
	}

	bad := []struct {
		name string
		p    reedsolomon.LRCLayoutParams
	}{
		{"no groups", reedsolomon.LRCLayoutParams{Version: reedsolomon.LRCParamsVersion, Scheme: reedsolomon.LRCSchemeXorLocal, DataShards: 4, GlobalParityShards: 2}},
		{"sum mismatch", reedsolomon.LRCLayoutParams{Version: reedsolomon.LRCParamsVersion, Scheme: reedsolomon.LRCSchemeXorLocal, DataShards: 4, GroupSizes: []int{3, 2}, GlobalParityShards: 2}},
		{"zero global", reedsolomon.LRCLayoutParams{Version: reedsolomon.LRCParamsVersion, Scheme: reedsolomon.LRCSchemeXorLocal, DataShards: 4, GroupSizes: []int{2, 2}, GlobalParityShards: 0}},
		{"bad scheme", reedsolomon.LRCLayoutParams{Version: reedsolomon.LRCParamsVersion, Scheme: "weird", DataShards: 4, GroupSizes: []int{2, 2}, GlobalParityShards: 2}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := reedsolomon.NewLRCCodec(tc.p); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestLRCEncodeVerify(t *testing.T) {
	p, _ := reedsolomon.NewUniformLRC(6, 2, 2)
	codec, err := reedsolomon.NewLRCCodec(p)
	if err != nil {
		t.Fatalf("NewLRCCodec: %v", err)
	}
	shards := makeLRCStripe(t, codec, 256)

	ok, err := codec.Verify(shards)
	if err != nil || !ok {
		t.Fatalf("Verify = %v, %v", ok, err)
	}

	// Corrupt a byte and ensure verification detects it.
	localIdx, _ := p.LocalParityIndex(0)
	shards[localIdx][0] ^= 0xFF
	if ok, _ := codec.Verify(shards); ok {
		t.Fatal("Verify should detect a corrupted local parity")
	}
}

// repairAndCheck wipes the given indices, repairs and compares against the
// reference stripe.
func repairAndCheck(t *testing.T, codec *reedsolomon.LRC, ref [][]byte, kill []int) *reedsolomon.RepairPlan {
	t.Helper()
	shards := make([][]byte, len(ref))
	for i, s := range ref {
		shards[i] = append([]byte(nil), s...)
	}
	for _, idx := range kill {
		shards[idx] = nil
	}
	plan, err := codec.RepairShards(shards)
	if err != nil {
		t.Fatalf("repair %v: %v", kill, err)
	}
	for i := range ref {
		if !bytes.Equal(shards[i], ref[i]) {
			t.Fatalf("shard %d mismatch after repair %v", i, kill)
		}
	}
	return plan
}

func TestLRCSingleShardLocalRepair(t *testing.T) {
	p, _ := reedsolomon.NewUniformLRC(12, 3, 2)
	codec, err := reedsolomon.NewLRCCodec(p)
	if err != nil {
		t.Fatal(err)
	}
	ref := makeLRCStripe(t, codec, 1024)

	for i := 0; i < codec.TotalShards(); i++ {
		plan := repairAndCheck(t, codec, ref, []int{i})
		// A single data loss must be served from its own group only.
		if i < codec.DataShards() {
			if plan.Strategy != reedsolomon.RepairLocal {
				t.Fatalf("shard %d repaired via %s, want local", i, plan.Strategy)
			}
			if plan.ReadCount != 4 { // group size 4: local parity + 3 peers
				t.Fatalf("shard %d reads = %d, want 4", i, plan.ReadCount)
			}
			if plan.ReadsSaved() != 12-4 {
				t.Fatalf("shard %d saved = %d, want 8", i, plan.ReadsSaved())
			}
		}
	}
}

func TestLRCLocalIsolation(t *testing.T) {
	p, _ := reedsolomon.NewUniformLRC(12, 3, 2)
	codec, _ := reedsolomon.NewLRCCodec(p)
	ref := makeLRCStripe(t, codec, 512)

	plan := repairAndCheck(t, codec, ref, []int{5}) // D5 lives in group 1
	for _, idx := range plan.ReadShards {
		kind, group, _ := p.ClassifyShard(idx)
		if kind == reedsolomon.ShardData && group != 1 {
			t.Fatalf("local repair read data shard %d from group %d", idx, group)
		}
		if kind == reedsolomon.ShardGlobalParity {
			t.Fatalf("local repair read global parity %d", idx)
		}
	}
	if len(plan.ReadShards) != 4 {
		t.Fatalf("local read count = %d, want 4", len(plan.ReadShards))
	}
}

func TestLRCMultiLoss(t *testing.T) {
	p, _ := reedsolomon.NewUniformLRC(6, 2, 2) // LRC(6,[3,3],2), k=6
	codec, _ := reedsolomon.NewLRCCodec(p)
	ref := makeLRCStripe(t, codec, 256)

	type tc struct {
		name        string
		kill        []int
		wantStrat   reedsolomon.RepairStrategy
		wantRecover bool
	}
	g0, _ := p.LocalParityIndex(0) // 6
	g1, _ := p.LocalParityIndex(1) // 7
	cases := []tc{
		{"one per group, globals present, local", []int{0, 3}, reedsolomon.RepairLocal, true},
		{"two in one group, globals rescue", []int{0, 1}, reedsolomon.RepairGlobal, true},
		{"two in one group + its local, global rescue", []int{0, 1, g0}, reedsolomon.RepairGlobal, true},
		{"data + global, global rescue", []int{0, 8}, reedsolomon.RepairGlobal, true},
		{"only global parities lost", []int{8, 9}, reedsolomon.RepairGlobal, true},
		{"two in group + both globals (MDS gap)", []int{0, 1, 8, 9}, reedsolomon.RepairGlobal, false},
		{"three in one group + its local", []int{0, 1, 2, g0}, reedsolomon.RepairGlobal, false},
		{"one per group + both globals (locals rescue)", []int{0, 3, 8, 9}, reedsolomon.RepairGlobal, true},
		{"both locals + both globals", []int{g0, g1, 8, 9}, reedsolomon.RepairGlobal, true},
		{"three data in a group + both globals", []int{0, 1, 2, 8, 9}, reedsolomon.RepairGlobal, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			present := make([]bool, codec.TotalShards())
			for i := range present {
				present[i] = true
			}
			for _, idx := range c.kill {
				present[idx] = false
			}
			plan, err := codec.PlanFromPresent(present)
			recoverable := err == nil
			if recoverable != c.wantRecover {
				t.Fatalf("recoverable = %v (err=%v), want %v", recoverable, err, c.wantRecover)
			}
			if recoverable {
				if plan.Strategy != c.wantStrat {
					t.Fatalf("strategy = %s, want %s", plan.Strategy, c.wantStrat)
				}
				repairAndCheck(t, codec, ref, c.kill)
			}
		})
	}
}

func TestLRCLargeGroupGaps(t *testing.T) {
	// LRC(12,[4,4,4],2): groups of four plus two global parities. This
	// shows the non-MDS profile explicitly: some 3- and 4-loss patterns are
	// fatal even though many 4-loss patterns survive.
	p, _ := reedsolomon.NewUniformLRC(12, 3, 2)
	codec, _ := reedsolomon.NewLRCCodec(p)
	ref := makeLRCStripe(t, codec, 256)

	type tc struct {
		name string
		kill []int
		ok   bool
	}
	cases := []tc{
		// Any three arbitrary losses are recoverable (l+g = 5 redundancy).
		{"three data in one group", []int{0, 1, 2}, true},
		{"three data in one group + one global", []int{0, 1, 2, 13}, true},
		{"one data per group + both globals", []int{0, 4, 8, 13, 14}, true},
		// A whole group (4 members) taken out is a weight-4 information
		// set that none of the remaining equations can resolve.
		{"whole group lost", []int{0, 1, 2, 3}, false},
		{"three in group + its local", []int{0, 1, 2, 12}, false},
		// Two data lost in each of two groups leaves only the two global
		// equations to span four unknowns in those groups: rank deficient.
		{"two lost in each of two groups", []int{0, 1, 4, 5}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			present := make([]bool, codec.TotalShards())
			for i := range present {
				present[i] = true
			}
			for _, idx := range c.kill {
				present[idx] = false
			}
			got := codec.CanRecover(present)
			if got != c.ok {
				t.Fatalf("CanRecover = %v, want %v", got, c.ok)
			}
			if c.ok {
				repairAndCheck(t, codec, ref, c.kill)
			}
		})
	}
}

func TestLRCGlobalRepairLargeShards(t *testing.T) {
	// Two losses in one group force the global tier on a 1 MiB shard, which
	// exercises the vectorized galMulSliceXor combination path.
	p, _ := reedsolomon.NewUniformLRC(6, 2, 2)
	codec, _ := reedsolomon.NewLRCCodec(p)
	ref := makeLRCStripe(t, codec, 1<<20)

	plan := repairAndCheck(t, codec, ref, []int{0, 1})
	if plan.Strategy != reedsolomon.RepairGlobal {
		t.Fatalf("strategy = %s, want global", plan.Strategy)
	}
	if plan.ReadCount != 6 {
		t.Fatalf("reads = %d, want 6", plan.ReadCount)
	}

	// Three members of one group must be flagged unrecoverable, not silently
	// produced wrong.
	present := make([]bool, codec.TotalShards())
	for i := range present {
		present[i] = true
	}
	for _, idx := range []int{0, 1, 2} {
		present[idx] = false
	}
	if codec.CanRecover(present) {
		t.Fatal("three members of one group must be unrecoverable")
	}
}

// TestLRCSmallExhaustive enumerates every failure subset of a small
// LRC(6,[3,3],2) stripe and cross-checks the planner's recoverability
// decision against an actual repair attempt.
func TestLRCSmallExhaustive(t *testing.T) {
	p, _ := reedsolomon.NewUniformLRC(6, 2, 2)
	codec, _ := reedsolomon.NewLRCCodec(p)
	ref := makeLRCStripe(t, codec, 128)

	n := codec.TotalShards()
	recoverable, unrecoverable := 0, 0
	// bitmask over the 10 physical shards.
	for mask := 0; mask < 1<<n; mask++ {
		present := make([]bool, n)
		var kill []int
		for i := 0; i < n; i++ {
			if mask&(1<<i) == 0 {
				present[i] = true
			} else {
				kill = append(kill, i)
			}
		}
		plan, perr := codec.PlanFromPresent(present)
		plannedOK := perr == nil

		execOK := false
		if plannedOK {
			shards := make([][]byte, n)
			for i, s := range ref {
				shards[i] = append([]byte(nil), s...)
			}
			for _, idx := range kill {
				shards[idx] = nil
			}
			if rerr := codec.Repair(shards, plan); rerr == nil {
				execOK = bytes.Equal(bytes.Join(shards, nil), bytes.Join(ref, nil))
			}
		}
		if plannedOK != execOK {
			t.Fatalf("mask=%b plan=%v exec=%v disagreement", mask, plannedOK, execOK)
		}
		if execOK {
			recoverable++
		} else {
			unrecoverable++
		}
	}
	// mask==0 is the healthy stripe (trivially "recoverable"); there are
	// 2^10 subsets total.
	if recoverable+unrecoverable != 1<<n {
		t.Fatalf("enumerated %d subsets, want %d", recoverable+unrecoverable, 1<<n)
	}
	t.Logf("LRC(6,[3,3],2) failure subsets: recoverable=%d unrecoverable=%d total=%d",
		recoverable, unrecoverable, recoverable+unrecoverable)
}

func ExampleLRC_singleShardSavings() {
	// 12 data shards in three groups of four, 2 global parities: 17 shards.
	p, _ := reedsolomon.NewUniformLRC(12, 3, 2)
	codec, _ := reedsolomon.NewLRCCodec(p)

	const shardSize = 1 << 20 // 1 MiB shards
	shards := codec.AllocShards(shardSize)
	for i := 0; i < codec.DataShards(); i++ {
		fillRandom(shards[i])
	}
	_ = codec.Encode(shards)

	// Data shard D5 is lost.
	shards[5] = nil
	plan, _ := codec.RepairShards(shards)

	fmt.Printf("layout: %s (%d shards, overhead %.3fx)\n",
		p, p.TotalShards(), p.StorageOverhead())
	fmt.Printf("strategy: %s\n", plan.Strategy)
	fmt.Printf("LRC reads: %d shards (%d MiB)\n", plan.ReadCount, plan.ReadCount)
	fmt.Printf("flat MDS reads: %d shards (%d MiB)\n", plan.MDSReadCount, plan.MDSReadCount)
	fmt.Printf("saved: %d shard reads (%d MiB)\n", plan.ReadsSaved(), plan.ReadsSaved())
	// Output:
	// layout: LRC(12,[4 4 4],2) (17 shards, overhead 1.417x)
	// strategy: local
	// LRC reads: 4 shards (4 MiB)
	// flat MDS reads: 12 shards (12 MiB)
	// saved: 8 shard reads (8 MiB)
}
