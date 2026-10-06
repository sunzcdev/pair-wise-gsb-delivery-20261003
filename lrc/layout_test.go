package lrc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"testing"
)

const smallShardSize = 256

// smallLayout is the layout used throughout the recovery enumeration:
// k=6 data, l=3 groups (2 data each), p=1 local parity per group, r=2 global
// parity; 11 shards total, storage overhead 5/11.
func smallLayout(t *testing.T) *Layout {
	t.Helper()
	ly, err := NewLayout(6, 3, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	return ly
}

func encodedShards(ly *Layout, rng *rand.Rand) [][]byte {
	shards := make([][]byte, ly.Shards())
	for i := 0; i < ly.DataShards(); i++ {
		shards[i] = make([]byte, smallShardSize)
		rng.Read(shards[i])
	}
	for i := ly.DataShards(); i < ly.Shards(); i++ {
		shards[i] = make([]byte, smallShardSize)
	}
	if err := ly.Encode(shards); err != nil {
		panic(err)
	}
	return shards
}

func cloneShards(in [][]byte) [][]byte {
	out := make([][]byte, len(in))
	for i, s := range in {
		if s != nil {
			out[i] = append([]byte(nil), s...)
		}
	}
	return out
}

func wipe(shards [][]byte, lost ...int) [][]byte {
	out := cloneShards(shards)
	for _, i := range lost {
		out[i] = nil
	}
	return out
}

func shardsEqual(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

func TestLayoutValidation(t *testing.T) {
	bad := [][4]int{
		{0, 1, 1, 1},
		{6, 0, 1, 2},
		{6, 3, 0, 2},
		{6, 3, 1, 0},
		{7, 3, 1, 2}, // k not divisible by groups
		{6, 7, 1, 2}, // more groups than data shards
	}
	for _, b := range bad {
		if _, err := NewLayout(b[0], b[1], b[2], b[3]); !errors.Is(err, ErrInvalidLayout) {
			t.Fatalf("NewLayout(%v) err=%v, want ErrInvalidLayout", b, err)
		}
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	ly := smallLayout(t)

	raw, err := json.Marshal(ly)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"lrc/v1","data_shards":6,"groups":3,"local_parity":1,"global_parity":2}`
	if string(raw) != want {
		t.Fatalf("metadata = %s, want %s", raw, want)
	}

	var ly2 Layout
	if err := json.Unmarshal(raw, &ly2); err != nil {
		t.Fatal(err)
	}
	if ly2.Params() != ly.Params() {
		t.Fatalf("round trip mismatch: %+v vs %+v", ly2.Params(), ly.Params())
	}

	// Foreign metadata must not be silently accepted as an LRC layout.
	mds := []byte(`{"type":"mds","data_shards":6,"parity":2}`)
	var ly3 Layout
	if err := json.Unmarshal(mds, &ly3); !errors.Is(err, ErrInvalidLayout) {
		t.Fatalf("foreign metadata err=%v, want ErrInvalidLayout", err)
	}
	if _, err := NewLayoutFromParams(Params{Type: "mds"}); !errors.Is(err, ErrInvalidLayout) {
		t.Fatalf("NewLayoutFromParams with foreign type err=%v, want ErrInvalidLayout", err)
	}
}

func TestShardIndices(t *testing.T) {
	ly := smallLayout(t)
	// data 0..5, local parity 6 (g0), 7 (g1), 8 (g2), global 9,10.
	if got := ly.LocalShardIndex(0, 2); got != 6 {
		t.Fatalf("local parity g0 index=%d, want 6", got)
	}
	if got := ly.LocalShardIndex(2, 2); got != 8 {
		t.Fatalf("local parity g2 index=%d, want 8", got)
	}
	if got := ly.GlobalParityIndex(1); got != 10 {
		t.Fatalf("global parity 1 index=%d, want 10", got)
	}
	if got := ly.DataIndexToGroup(5); got != 2 {
		t.Fatalf("data shard 5 group=%d, want 2", got)
	}
}

// lossSpec is one row of the human-readable recovery enumeration.
type lossSpec struct {
	lost    []int
	recover bool
	reads   int
	note    string
}

func labelIndices(ly *Layout, idx []int) string {
	var b bytes.Buffer
	for n, i := range idx {
		if n > 0 {
			b.WriteByte(',')
		}
		switch {
		case i < ly.DataShards():
			fmt.Fprintf(&b, "d%d", i)
		case i < ly.DataShards()+ly.Groups()*ly.LocalParity():
			fmt.Fprintf(&b, "L%d", (i-ly.DataShards())/ly.LocalParity())
		default:
			fmt.Fprintf(&b, "G%d", i-(ly.DataShards()+ly.Groups()*ly.LocalParity()))
		}
	}
	return b.String()
}

// TestSmallLayoutCombinations exhaustively enumerates every loss combination
// of the small (k=6, l=3, p=1, r=2) layout, plans+executes each one, and
// reports recoverable/unrecoverable patterns with their actual storage read
// counts. This is the authoritative table of what the layout can and cannot
// tolerate, and it is deliberately NOT the same guarantee as a flat (6,2) MDS
// stripe.
func TestSmallLayoutCombinations(t *testing.T) {
	ly := smallLayout(t)
	rng := rand.New(rand.NewSource(1))
	original := encodedShards(ly, rng)

	var unrecoverable []string
	enumerated := 0
	countsBySize := map[int][2]int{} // [recoverable, unrecoverable]
	// minReadsBySize records the smallest repair read count observed for
	// each recoverable loss count.
	minReadsBySize := map[int]int{}

	// Enumerate subsets of 11 shards as bitsets.
	for mask := 0; mask < 1<<ly.Shards(); mask++ {
		var lost []int
		for i := 0; i < ly.Shards(); i++ {
			if mask&(1<<i) != 0 {
				lost = append(lost, i)
			}
		}
		if len(lost) == 0 {
			continue
		}
		enumerated++

		damaged := wipe(original, lost...)
		plan, planErr := ly.RepairPlan(damaged)
		if planErr != nil {
			if !errors.Is(planErr, ErrUnrecoverable) {
				t.Fatalf("unexpected planner error for %v: %v", lost, planErr)
			}
			unrecoverable = append(unrecoverable, labelIndices(ly, lost))
			c := countsBySize[len(lost)]
			c[1]++
			countsBySize[len(lost)] = c
			continue
		}

		// Verify the read union matches the per-step read lists.
		union := map[int]bool{}
		for _, s := range plan.Steps {
			for _, r := range s.ReadShards {
				if union[r] {
					t.Fatalf("shard %d read twice across steps for %v", r, lost)
				}
				union[r] = true
			}
		}
		if len(union) != plan.ReadCount {
			t.Fatalf("ReadCount %d != union %d for %v", plan.ReadCount, len(union), lost)
		}

		if err := ly.Execute(damaged, plan); err != nil {
			t.Fatalf("execute failed for %v: %v", lost, err)
		}
		if !shardsEqual(original, damaged) {
			t.Fatalf("reconstructed data mismatch for %v", lost)
		}
		c := countsBySize[len(lost)]
		c[0]++
		countsBySize[len(lost)] = c
		if cur, ok := minReadsBySize[len(lost)]; !ok || plan.ReadCount < cur {
			minReadsBySize[len(lost)] = plan.ReadCount
		}
	}

	t.Logf("unrecoverable combinations: %d", len(unrecoverable))
	for _, s := range unrecoverable {
		t.Logf("  FAIL %s", s)
	}
	t.Logf("summary [losses] = (recoverable, unrecoverable):")
	for n := 1; n <= ly.Shards(); n++ {
		if c, ok := countsBySize[n]; ok {
			t.Logf("  %d losses: %4d recoverable / %4d unrecoverable (best repair = %d reads)",
				n, c[0], c[1], minReadsBySize[n])
		}
	}

	// Sanity totals: 2^11 - 1 non-empty subsets.
	if enumerated != (1<<ly.Shards())-1 {
		t.Fatalf("enumerated %d subsets, want %d", enumerated, (1<<ly.Shards())-1)
	}
	if c1 := countsBySize[1]; c1[1] != 0 {
		t.Fatalf("every single-shard loss must be recoverable, got %d failures", c1[1])
	}
	// Known boundary for (k=6,l=3,p=1,r=2): exactly six 3-shard patterns
	// fail: both data shards of one group plus one global parity shard.
	if c3 := countsBySize[3]; c3[0] != 159 || c3[1] != 6 {
		t.Fatalf("3-loss split = %+v, want {159,6}", c3)
	}
	if c2 := countsBySize[2]; c2[1] != 0 {
		t.Fatalf("every 2-shard loss must be recoverable, got %d failures", c2[1])
	}
}

// TestCuratedCombinations pins representative combinations and exact read
// counts, including the contrast with the flat (6,2) MDS layout.
func TestCuratedCombinations(t *testing.T) {
	ly := smallLayout(t)
	rng := rand.New(rand.NewSource(2))
	original := encodedShards(ly, rng)

	// d0,d1 group0; d2,d3 group1; d4,d5 group2; L0=6 L1=7 L2=8; G0=9 G1=10
	cases := []struct {
		name     string
		lost     []int
		wantOK   bool
		wantRd   int
		wantTier []Tier
	}{
		{"single data d0", []int{0}, true, 2, []Tier{TierLocal}},
		{"single local parity L1", []int{7}, true, 2, []Tier{TierLocal}},
		{"single global parity G0", []int{9}, true, 6, []Tier{TierGlobal}},
		{"two data same group (d0,d1)", []int{0, 1}, true, 6, []Tier{TierGlobal}},
		// d0 is locally repaired (d1,L0 read); regenerating lost G0 then
		// needs all 6 data shards, so d2..d5 are fetched too: 6 reads.
		{"data+global (d0,G0)", []int{0, 9}, true, 6, []Tier{TierLocal, TierGlobal}},
		{"data same group+global (d0,d1,G0)", []int{0, 1, 9}, false, 0, nil},
		{"two data different groups (d0,d2)", []int{0, 2}, true, 4, []Tier{TierLocal, TierLocal}},
		{"three groups one data each (d0,d2,d4)", []int{0, 2, 4}, true, 6, []Tier{TierLocal, TierLocal, TierLocal}},
		{"three groups one data each + global (d0,d2,d4,G0)", []int{0, 2, 4, 9}, true, 6, []Tier{TierLocal, TierLocal, TierLocal, TierGlobal}},
		{"two global parity (G0,G1)", []int{9, 10}, true, 6, []Tier{TierGlobal, TierGlobal}},
		{"two local parity (L0,L1)", []int{6, 7}, true, 4, []Tier{TierLocal, TierLocal}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			damaged := wipe(original, tc.lost...)
			plan, err := ly.RepairPlan(damaged)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("plan: %v", err)
				}
				if plan.ReadCount != tc.wantRd {
					t.Fatalf("reads=%d, want %d; read shards %v", plan.ReadCount, tc.wantRd, plan.ReadShards)
				}
				var tiers []Tier
				for _, s := range plan.Steps {
					tiers = append(tiers, s.Tier)
				}
				if fmt.Sprint(tiers) != fmt.Sprint(tc.wantTier) {
					t.Fatalf("tiers=%v, want %v", tiers, tc.wantTier)
				}
				if err := ly.Execute(damaged, plan); err != nil {
					t.Fatalf("execute: %v", err)
				}
				if !shardsEqual(original, damaged) {
					t.Fatal("reconstructed mismatch")
				}
			} else {
				if !errors.Is(err, ErrUnrecoverable) {
					t.Fatalf("err=%v, want ErrUnrecoverable", err)
				}
			}
		})
	}
}

// TestSingleShardRepairSavings demonstrates the repair-bandwidth saving on the
// common single data shard failure: the LRC layout reads only the two shards
// of the affected group, while a flat (6,2) MDS repair must read k=6 shards.
func TestSingleShardRepairSavings(t *testing.T) {
	ly := smallLayout(t)
	rng := rand.New(rand.NewSource(3))
	original := encodedShards(ly, rng)

	damaged := wipe(original, 0) // d0 lost
	plan, err := ly.RepairPlan(damaged)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ReadCount != ly.DataPerGroup() {
		t.Fatalf("LRC reads=%d, want %d", plan.ReadCount, ly.DataPerGroup())
	}
	if err := ly.Reconstruct(wipe(original, 0)); err != nil {
		t.Fatal(err)
	}

	mdsReads := ly.DataShards() // any single loss under flat (k,r) MDS costs k reads
	saved := mdsReads - plan.ReadCount
	t.Logf("single data shard loss: LRC reads %d shard(s), flat MDS reads %d shard(s); saved %d reads (%.1f%%)",
		plan.ReadCount, mdsReads, saved, 100*float64(saved)/float64(mdsReads))
	if saved != 4 {
		t.Fatalf("saved=%d, want 4", saved)
	}

	for _, r := range plan.ReadShards {
		if r >= ly.DataPerGroup() && r < ly.k {
			t.Fatalf("local repair read shard %d from another group", r)
		}
	}
}

func TestRepairTargets(t *testing.T) {
	ly := smallLayout(t)
	rng := rand.New(rand.NewSource(4))
	original := encodedShards(ly, rng)

	damaged := wipe(original, 0, 1)
	plan, err := ly.RepairTargets(damaged, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	if err := ly.Execute(damaged, plan); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(damaged[0], original[0]) {
		t.Fatal("requested d0 not restored")
	}
	if damaged[1] != nil {
		t.Fatal("non-requested d1 should stay missing")
	}

	damaged2 := wipe(original, 0, 2, 4)
	plan2, err := ly.RepairTargets(damaged2, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	if err := ly.Execute(damaged2, plan2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(damaged2[0], original[0]) {
		t.Fatal("requested d0 not restored via local repair")
	}

	// Both data shards of group 0 and both global parity shards lost:
	// neither tier can recover.
	damaged3 := wipe(original, 0, 1, 9, 10)
	if _, err := ly.RepairTargets(damaged3, []int{0}); !errors.Is(err, ErrUnrecoverable) {
		t.Fatalf("err=%v, want ErrUnrecoverable", err)
	}
}
