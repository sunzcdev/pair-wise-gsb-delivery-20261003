package lrc

import (
	"fmt"
	"sort"
)

// Tier identifies which coding level a repair step uses.
type Tier int

const (
	// TierLocal reconstructs using only shards of one group.
	TierLocal Tier = iota + 1
	// TierGlobal reconstructs using the flat (k, r) global stripe.
	TierGlobal
)

func (t Tier) String() string {
	switch t {
	case TierLocal:
		return "local"
	case TierGlobal:
		return "global"
	default:
		return "unknown"
	}
}

// Kind identifies the role of a repair step.
type Kind int

const (
	// KindReconstruct solves for missing shards from other shards of the
	// same stripe (requires reads).
	KindReconstruct Kind = iota + 1
	// KindRegenerate recomputes parity shards from data shards. Its read
	// set lists only the data shards that were not already fetched by an
	// earlier step.
	KindRegenerate
)

func (k Kind) String() string {
	switch k {
	case KindReconstruct:
		return "reconstruct"
	case KindRegenerate:
		return "regenerate"
	default:
		return "unknown"
	}
}

// Step is one stage of a repair plan.
//
// ReadShards lists the flat shard indices the caller must fetch from storage
// for THIS step. Indices reconstructed or fetched by an earlier step are never
// repeated, so the union of all steps' ReadShards is the total repair read
// set (Plan.ReadShards).
type Step struct {
	Tier Tier
	Kind Kind
	// Group is the affected group for TierLocal steps; -1 for TierGlobal.
	Group int
	// Missing are the flat shard indices this step restores.
	Missing []int
	// ReadShards are the flat shard indices the caller must read from
	// storage for this step.
	ReadShards []int
}

// Plan tells a caller exactly which shards to read and in what order to
// repair the missing shards.
type Plan struct {
	// Steps are ordered: local reconstruction first, then global
	// reconstruction, then local parity regeneration, then global parity
	// regeneration.
	Steps []Step
	// ReadShards is the deduplicated union of every step's ReadShards.
	ReadShards []int
	// ReadCount is len(ReadShards): the number of shards fetched from
	// storage for the whole repair.
	ReadCount int
}

// RepairPlan computes a repair plan for every nil/zero-length shard.
//
// Every locally sufficient group (at most p shards of that group missing) is
// repaired from its own shards only; no other group appears in its read set.
// Only after all locally sufficient groups are fixed does the plan evaluate
// the global stripe for the groups that cannot repair themselves. If neither
// tier covers the losses, ErrUnrecoverable is returned.
func (ly *Layout) RepairPlan(shards [][]byte) (*Plan, error) {
	if len(shards) != ly.Shards() {
		return nil, fmt.Errorf("%w: expected %d shards, got %d", ErrInvalidLayout, ly.Shards(), len(shards))
	}
	return ly.plan(missingSet(shards), nil)
}

// RepairTargets computes a repair plan for a chosen subset of missing shards.
// Shards not listed in targets are treated as permanently unavailable while
// planning and are not restored. This is useful when the caller only needs
// the object data back, not every lost parity shard.
func (ly *Layout) RepairTargets(shards [][]byte, targets []int) (*Plan, error) {
	if len(shards) != ly.Shards() {
		return nil, fmt.Errorf("%w: expected %d shards, got %d", ErrInvalidLayout, ly.Shards(), len(shards))
	}
	for _, t := range targets {
		if t < 0 || t >= ly.Shards() {
			return nil, fmt.Errorf("%w: target shard %d out of range [0,%d)", ErrInvalidLayout, t, ly.Shards())
		}
	}
	absent := missingSet(shards)
	wanted := make(map[int]bool, len(targets))
	for _, t := range targets {
		if absent[t] {
			wanted[t] = true
		}
	}
	return ly.plan(absent, wanted)
}

func missingSet(shards [][]byte) map[int]bool {
	m := make(map[int]bool)
	for i := range shards {
		if len(shards[i]) == 0 {
			m[i] = true
		}
	}
	return m
}

// plan computes the repair steps. absent lists all currently unavailable
// shards. If wanted is nil every absent shard is restored; otherwise only the
// wanted ones are, and parity regeneration is skipped.
func (ly *Layout) plan(absent map[int]bool, wanted map[int]bool) (*Plan, error) {
	repairAll := wanted == nil
	if repairAll {
		wanted = absent
	}

	var steps []Step
	fetched := map[int]bool{} // shards the caller must read from storage
	// inMemory tracks shards available to the executor after the steps
	// planned so far: fetched by an earlier step or produced by a previous
	// reconstruction. Surviving shards that no step has fetched are NOT in
	// this set, because the whole point of the plan is to avoid reading
	// them.
	inMemory := map[int]bool{}
	// fixed tracks absent shards restored by an earlier step.
	fixed := map[int]bool{}

	want := func(i int) bool { return repairAll || wanted[i] }

	groupDataLost := make([][]int, ly.l)
	groupLocalLost := make([][]int, ly.l)
	groupTotalLost := make([]int, ly.l)
	for idx := range absent {
		switch {
		case idx < ly.k:
			g := idx / ly.dataPerGroup
			groupDataLost[g] = append(groupDataLost[g], idx)
			groupTotalLost[g]++
		case idx < ly.k+ly.localTotal:
			g := (idx - ly.k) / ly.p
			groupLocalLost[g] = append(groupLocalLost[g], idx)
			groupTotalLost[g]++
		}
	}

	addStep := func(tier Tier, kind Kind, group int, missing, reads []int) {
		steps = append(steps, Step{
			Tier:       tier,
			Kind:       kind,
			Group:      group,
			Missing:    sortedCopy(missing),
			ReadShards: sortedCopy(reads),
		})
		for _, idx := range reads {
			fetched[idx] = true
			inMemory[idx] = true
		}
		for _, idx := range missing {
			fixed[idx] = true
			inMemory[idx] = true
		}
	}

	// Tier 1: every group with at most p losses repairs itself using only
	// its own shards. Other groups are never read.
	for g := 0; g < ly.l; g++ {
		if groupTotalLost[g] == 0 || groupTotalLost[g] > ly.p {
			continue
		}
		var lost []int
		lost = append(lost, groupDataLost[g]...)
		lost = append(lost, groupLocalLost[g]...)
		if !repairAll {
			lost = keepWanted(lost, wanted)
			if len(lost) == 0 {
				continue
			}
		}

		// A group RS solve needs exactly dataPerGroup known shards. Prefer
		// surviving data shards, then surviving local parity.
		var reads []int
		for s := 0; s < ly.dataPerGroup; s++ {
			idx := ly.LocalShardIndex(g, s)
			if !absent[idx] {
				reads = append(reads, idx)
			}
		}
		for j := 0; j < ly.p && len(reads) < ly.dataPerGroup; j++ {
			idx := ly.LocalShardIndex(g, ly.dataPerGroup+j)
			if !absent[idx] {
				reads = append(reads, idx)
			}
		}
		if len(reads) < ly.dataPerGroup {
			return nil, fmt.Errorf("%w: group %d: internal planner error, %d available shards, need %d",
				ErrUnrecoverable, g, len(reads), ly.dataPerGroup)
		}
		addStep(TierLocal, KindReconstruct, g, lost, reads)
	}

	// Tier 2: data shards still missing, plus missing global parity, must
	// be covered by the flat (k, r) stripe. In target mode, data shards the
	// caller does not want are still unknowns that consume parity equations.
	var unresolvedData []int
	unknownData := 0
	for i := 0; i < ly.k; i++ {
		if absent[i] && !fixed[i] {
			unknownData++
			if want(i) {
				unresolvedData = append(unresolvedData, i)
			}
		}
	}
	var globalLost []int
	for j := 0; j < ly.r; j++ {
		idx := ly.GlobalParityIndex(j)
		if absent[idx] {
			globalLost = append(globalLost, idx)
		}
	}

	if len(unresolvedData) > 0 {
		// Flat MDS stripe: data losses and global parity losses share the
		// r parity equations. Recoverable iff their sum is <= r.
		if unknownData+len(globalLost) > ly.r {
			return nil, fmt.Errorf("%w: global stripe has %d data and %d global parity losses, r=%d",
				ErrUnrecoverable, unknownData, len(globalLost), ly.r)
		}

		// The RS decoder needs k known data-equivalents total. Every
		// surviving global parity shard can stand in for one missing data
		// shard. Data shards fetched or locally reconstructed in earlier
		// steps are already in memory and cost no extra read.
		var reads []int
		inMemoryKnown := 0
		survivingData := 0
		for i := 0; i < ly.k; i++ {
			if inMemory[i] {
				inMemoryKnown++
			} else if !absent[i] {
				survivingData++
			}
		}
		survivingGlobal := 0
		for j := 0; j < ly.r; j++ {
			idx := ly.GlobalParityIndex(j)
			if !absent[idx] {
				survivingGlobal++
			}
		}

		// Rank check: known data + surviving global parity must reach k.
		if inMemoryKnown+survivingData+survivingGlobal < ly.k {
			return nil, fmt.Errorf("%w: global stripe: internal planner error, %d data-equivalents < %d",
				ErrUnrecoverable, inMemoryKnown+survivingData+survivingGlobal, ly.k)
		}

		// Fetch surviving data first (cheaper/equivalent), then as many
		// surviving global parity shards as needed to close the gap.
		needed := ly.k - inMemoryKnown
		for i := 0; i < ly.k && needed > 0; i++ {
			if !absent[i] && !inMemory[i] {
				reads = append(reads, i)
				needed--
			}
		}
		for j := 0; j < ly.r && needed > 0; j++ {
			idx := ly.GlobalParityIndex(j)
			if !absent[idx] && !inMemory[idx] {
				reads = append(reads, idx)
				needed--
			}
		}
		if needed > 0 {
			return nil, fmt.Errorf("%w: global stripe: internal planner error, still short %d shards",
				ErrUnrecoverable, needed)
		}
		addStep(TierGlobal, KindReconstruct, -1, unresolvedData, reads)
	}

	if repairAll {
		// Regenerate local parity for groups whose local parity was lost.
		for g := 0; g < ly.l; g++ {
			for _, idx := range groupLocalLost[g] {
				if fixed[idx] {
					continue
				}
				var reads []int
				for s := 0; s < ly.dataPerGroup; s++ {
					di := ly.LocalShardIndex(g, s)
					if absent[di] && !fixed[di] {
						return nil, fmt.Errorf("%w: local parity shard %d cannot be regenerated: data shard %d unresolved",
							ErrUnrecoverable, idx, di)
					}
					if !inMemory[di] {
						reads = append(reads, di)
					}
				}
				addStep(TierLocal, KindRegenerate, g, []int{idx}, reads)
			}
		}

		// Regenerate global parity. All k data shards must be in memory by
		// now (present, locally reconstructed, or globally reconstructed);
		// read any that were never fetched. Missing global parity after a
		// global data solve is restored here as well.
		for _, idx := range globalLost {
			var reads []int
			for i := 0; i < ly.k; i++ {
				if !inMemory[i] {
					if absent[i] && !fixed[i] {
						return nil, fmt.Errorf("%w: global parity shard %d cannot be regenerated: data shard %d unresolved",
							ErrUnrecoverable, idx, i)
					}
					reads = append(reads, i)
				}
			}
			addStep(TierGlobal, KindRegenerate, -1, []int{idx}, reads)
		}
	} else {
		// Target mode: regenerate only the requested global parity shards.
		for _, idx := range keepWanted(sortedCopy(globalLost), wanted) {
			var reads []int
			for i := 0; i < ly.k; i++ {
				if !inMemory[i] {
					if absent[i] && !fixed[i] {
						return nil, fmt.Errorf("%w: global parity shard %d cannot be regenerated: data shard %d unresolved",
							ErrUnrecoverable, idx, i)
					}
					reads = append(reads, i)
				}
			}
			addStep(TierGlobal, KindRegenerate, -1, []int{idx}, reads)
		}
	}

	// Final coverage check for target-only repairs.
	for idx := range wanted {
		if absent[idx] && !fixed[idx] {
			return nil, fmt.Errorf("%w: requested shard %d cannot be restored", ErrUnrecoverable, idx)
		}
	}

	readList := make([]int, 0, len(fetched))
	for idx := range fetched {
		readList = append(readList, idx)
	}
	sort.Ints(readList)
	return &Plan{
		Steps:      steps,
		ReadShards: readList,
		ReadCount:  len(readList),
	}, nil
}

func keepWanted(in []int, wanted map[int]bool) []int {
	out := in[:0]
	for _, v := range in {
		if wanted[v] {
			out = append(out, v)
		}
	}
	return out
}

func sortedCopy(in []int) []int {
	out := append([]int(nil), in...)
	sort.Ints(out)
	return out
}
