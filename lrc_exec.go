package reedsolomon

import "sort"

// ensureShard makes sure shards[idx] is a writable buffer of shardSize
// bytes, reusing available capacity.
func ensureShard(shards [][]byte, idx, shardSize int) {
	if len(shards[idx]) >= shardSize {
		shards[idx] = shards[idx][:shardSize]
		return
	}
	shards[idx] = make([]byte, shardSize)
}

// Repair reconstructs all missing shards according to the plan returned by
// [LRC.PlanRepair] or [LRC.PlanFromPresent].
//
// Only the shard indices listed in plan.ReadShards are accessed; all other
// present shards are left untouched and missing entries are allocated and
// filled in.
func (l *LRC) Repair(shards [][]byte, plan *RepairPlan) error {
	if plan == nil {
		return ErrUnrecoverableLRC
	}
	if len(shards) != l.TotalShards() {
		return ErrTooFewShards
	}
	switch plan.Strategy {
	case RepairNone:
		return nil
	case RepairLocal:
		return l.repairLocal(shards, plan)
	case RepairGlobal:
		return l.repairGlobal(shards, plan)
	default:
		return ErrUnrecoverableLRC
	}
}

// RepairShards plans a repair and immediately executes it. It returns the
// plan so callers can inspect which shards were read. If the stripe is not
// recoverable it returns [ErrUnrecoverableLRC] without modifying shards.
func (l *LRC) RepairShards(shards [][]byte) (*RepairPlan, error) {
	plan, err := l.PlanRepair(shards)
	if err != nil {
		return nil, err
	}
	if err := l.Repair(shards, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// repairLocal executes the per-group XOR steps. It reads no shard outside
// the affected groups.
func (l *LRC) repairLocal(shards [][]byte, plan *RepairPlan) error {
	size := 0
	for _, step := range plan.LocalSteps {
		for _, peer := range step.Peers {
			if len(shards[peer]) == 0 {
				return ErrTooFewShards
			}
			if size == 0 {
				size = len(shards[peer])
			}
		}
	}
	for _, step := range plan.LocalSteps {
		ensureShard(shards, step.Target, size)
		out := shards[step.Target][:size]
		for i := range out {
			out[i] = 0
		}
		for _, peer := range step.Peers {
			sliceXor(shards[peer][:size], out, l.o)
		}
	}
	return nil
}

// repairGlobal solves for the missing data shards from k independent
// equations, then regenerates missing parity shards from the completed data.
func (l *LRC) repairGlobal(shards [][]byte, plan *RepairPlan) error {
	k := l.params.DataShards
	if len(plan.GlobalDecodeRows) != k {
		return ErrUnrecoverableLRC
	}

	size := 0
	inputs := make([][]byte, k)
	for i, idx := range plan.GlobalDecodeRows {
		if len(shards[idx]) == 0 {
			return ErrTooFewShards
		}
		inputs[i] = shards[idx]
		if size == 0 {
			size = len(shards[idx])
		}
	}

	// Build the k x k matrix whose rows are the chosen equations and invert
	// it. Row d of the inverse expresses data shard d in the chosen basis.
	sub, err := newMatrix(k, k)
	if err != nil {
		return err
	}
	for i, idx := range plan.GlobalDecodeRows {
		copy(sub[i], l.codingRow(idx))
	}
	inv, err := gfInvert(sub)
	if err != nil {
		// Rank was already proven during planning, so this indicates the
		// plan and shard layout disagree.
		return ErrUnrecoverableLRC
	}

	dataTargets := append([]int(nil), plan.DecodeData...)
	sort.Ints(dataTargets)
	for _, d := range dataTargets {
		ensureShard(shards, d, size)
		out := shards[d][:size]
		for n := range out {
			out[n] = 0
		}
		// Row d of the inverse expresses data shard d as a combination of
		// the k chosen equation shards. The scalar slice kernels are used
		// directly so the chosen (possibly shuffled) basis maps 1:1 to
		// inputs, regardless of matrix generation assumptions.
		for c := 0; c < k; c++ {
			if coef := inv[d][c]; coef != 0 {
				galMulSliceXor(coef, inputs[c][:size], out, l.o)
			}
		}
	}

	// All data shards are now present; regenerate missing parity for free.
	for _, idx := range plan.Regenerate {
		kind, group, _ := l.params.ClassifyShard(idx)
		ensureShard(shards, idx, size)
		switch kind {
		case ShardLocalParity:
			members, _ := l.params.GroupMembers(group)
			out := shards[idx][:size]
			for i := range out {
				out[i] = 0
			}
			for _, d := range members {
				sliceXor(shards[d][:size], out, l.o)
			}
		case ShardGlobalParity:
			l.regenerateGlobalParity(shards, idx, size)
		}
	}
	return nil
}

// regenerateGlobalParity recomputes a single global parity shard from all
// present data shards.
func (l *LRC) regenerateGlobalParity(shards [][]byte, idx, size int) {
	k := l.params.DataShards
	j := idx - k - l.params.LocalGroups()
	inputs := shards[:k]
	out := [][]byte{shards[idx][:size]}
	row := [][]byte{l.globalRows[j]}
	if rs, ok := l.global.(*reedSolomon); ok {
		rs.codeSomeShards(row, inputs, out, size, true)
	}
}
