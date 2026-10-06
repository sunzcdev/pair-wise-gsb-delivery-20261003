package reedsolomon

import (
	"sort"
)

// RepairStrategy identifies which repair tier a plan uses.
type RepairStrategy int

const (
	// RepairNone means nothing is missing.
	RepairNone RepairStrategy = iota
	// RepairLocal means every missing shard is rebuilt inside its own local
	// group; shards from other groups and global parity shards are not read.
	RepairLocal
	// RepairGlobal means the local tier is insufficient, so a global
	// Reed-Solomon recovery (a k-independent-equation solve) is used.
	RepairGlobal
)

// String returns a short label for the repair strategy.
func (s RepairStrategy) String() string {
	switch s {
	case RepairLocal:
		return "local"
	case RepairGlobal:
		return "global"
	default:
		return "none"
	}
}

// LocalRepairStep describes one local-group XOR repair.
type LocalRepairStep struct {
	// Group is the local group index.
	Group int
	// Target is the physical shard index to rebuild (a data shard or the
	// group's local parity shard).
	Target int
	// Peers are the physical shard indices to read. They all belong to the
	// same group: the local parity plus the other data members when
	// rebuilding data, or all data members when rebuilding local parity.
	Peers []int
}

// RepairPlan tells the caller exactly which shards to fetch and how the
// missing shards will be reconstructed. It is derived purely from layout
// metadata and the set of present shards, so it can be produced before any
// shard bytes are read.
type RepairPlan struct {
	// Strategy is Local when local parity alone covers every loss,
	// otherwise Global.
	Strategy RepairStrategy

	// ReadShards are the physical shard indices the caller must fetch.
	// For a local plan these never leave the affected groups.
	ReadShards []int
	// ReadCount is len(ReadShards), exposed for quick I/O accounting and
	// savings comparisons against flat MDS reconstruction.
	ReadCount int
	// MDSReadCount is the number of shards a flat (k, g) MDS Reed-Solomon
	// layout must read to rebuild the same targets: always k, even for a
	// single loss. Compare with ReadCount to quantify local-repair savings.
	MDSReadCount int

	// Targets are the physical shard indices that will be reconstructed.
	Targets []int

	// LocalSteps lists the per-group XOR repairs (local tier).
	LocalSteps []LocalRepairStep

	// GlobalDecodeRows are the present shard indices forming the k
	// independent equations of the global solve, in the order the decoder
	// consumes them. Only set for the global tier.
	GlobalDecodeRows []int
	// DecodeData are the data shard indices produced by the global solve.
	DecodeData []int
	// Regenerate are parity shards recomputed for free from the completed
	// data after the global solve (no extra shard reads).
	Regenerate []int
}

// ReadsSaved returns MDSReadCount - ReadCount: the shard reads avoided by
// using this layout's local tier instead of flat MDS reconstruction.
func (p *RepairPlan) ReadsSaved() int { return p.MDSReadCount - p.ReadCount }

// PlanSingleShard returns the repair plan for losing exactly one physical
// shard while every other shard is available. This is the dominant object
// storage failure case and the best case for the local tier.
func (l *LRC) PlanSingleShard(missingIndex int) (*RepairPlan, error) {
	present := make([]bool, l.TotalShards())
	for i := range present {
		present[i] = true
	}
	if missingIndex < 0 || missingIndex >= len(present) {
		return nil, ErrTooFewShards
	}
	present[missingIndex] = false
	return l.PlanFromPresent(present)
}

// SingleShardReads reports only the number of shards a repair of one lost
// shard must fetch. It is a metadata-only, allocation-free convenience.
func (l *LRC) SingleShardReads(missingIndex int) (int, error) {
	plan, err := l.PlanSingleShard(missingIndex)
	if err != nil {
		return 0, err
	}
	return plan.ReadCount, nil
}

// presentFromShards reports which physical shard indices are present.
// A nil or zero-length entry means the shard is missing, matching the
// convention used by [Encoder.Reconstruct].
func (l *LRC) presentFromShards(shards [][]byte) ([]bool, error) {
	if len(shards) != l.TotalShards() {
		return nil, ErrTooFewShards
	}
	present := make([]bool, l.TotalShards())
	for i, s := range shards {
		present[i] = len(s) != 0
	}
	return present, nil
}

// missingIndices returns the missing physical shard indices in order.
func missingIndices(present []bool) []int {
	var missing []int
	for i, ok := range present {
		if !ok {
			missing = append(missing, i)
		}
	}
	return missing
}

// CanRecover reports whether the described shard set can be completely
// rebuilt, i.e. every missing shard is recoverable. It performs no shard
// reads and is safe to call before fetching anything.
func (l *LRC) CanRecover(present []bool) bool {
	plan, err := l.PlanFromPresent(present)
	return err == nil && plan != nil
}

// groupState summarizes losses inside one local group.
type groupState struct {
	missingData  []int // data shard indices missing in this group
	localMissing bool  // whether the group's local parity is missing
}

// PlanRepair builds a [RepairPlan] from a shard vector. Presence is the only
// property inspected (nil or zero-length means missing), so this performs no
// data reads.
func (l *LRC) PlanRepair(shards [][]byte) (*RepairPlan, error) {
	present, err := l.presentFromShards(shards)
	if err != nil {
		return nil, err
	}
	return l.PlanFromPresent(present)
}

// PlanFromPresent builds a [RepairPlan] from a presence bitmap of length
// TotalShards. This lets a metadata/index service compute the read set
// without holding shard contents.
func (l *LRC) PlanFromPresent(present []bool) (*RepairPlan, error) {
	if len(present) != l.TotalShards() {
		return nil, ErrTooFewShards
	}
	missing := missingIndices(present)
	k := l.params.DataShards
	plan := &RepairPlan{Strategy: RepairNone, MDSReadCount: k}
	if len(missing) == 0 {
		return plan, nil
	}
	plan.Targets = missing

	ng := l.params.LocalGroups()

	// Tally losses per group.
	states := make([]groupState, ng)
	globalMissing := 0
	for _, idx := range missing {
		kind, group, _ := l.params.ClassifyShard(idx)
		switch kind {
		case ShardData:
			states[group].missingData = append(states[group].missingData, idx)
		case ShardLocalParity:
			states[group].localMissing = true
		case ShardGlobalParity:
			globalMissing++
		}
	}

	// Tier 1: pure local repair. Every affected group must have exactly one
	// loss among its members+local parity, and all global parity shards must
	// be present because the local tier cannot rebuild them.
	if globalMissing == 0 {
		steps, reads, ok := l.planLocal(states, present)
		if ok {
			plan.Strategy = RepairLocal
			plan.LocalSteps = steps
			plan.ReadShards = reads
			plan.ReadCount = len(reads)
			return plan, nil
		}
	}

	// Tier 2: global recovery. Select k independent equations from the
	// present data, global-parity and local-parity shards.
	chosen, err := l.selectIndependent(present)
	if err != nil {
		return nil, err
	}
	if len(chosen) != k {
		return nil, ErrUnrecoverableLRC
	}

	plan.Strategy = RepairGlobal
	plan.GlobalDecodeRows = chosen

	readSet := make(map[int]struct{}, k)
	for _, idx := range chosen {
		readSet[idx] = struct{}{}
	}

	for _, idx := range missing {
		if idx < k {
			plan.DecodeData = append(plan.DecodeData, idx)
		} else {
			// Parity shards are regenerated from the completed data and do
			// not add any reads.
			plan.Regenerate = append(plan.Regenerate, idx)
		}
	}
	for idx := range readSet {
		plan.ReadShards = append(plan.ReadShards, idx)
	}
	sort.Ints(plan.ReadShards)
	plan.ReadCount = len(plan.ReadShards)
	return plan, nil
}

// planLocal attempts to construct a purely local repair. It returns the
// steps, the read set and ok=false as soon as one group is not locally
// repairable on its own.
func (l *LRC) planLocal(states []groupState, present []bool) ([]LocalRepairStep, []int, bool) {
	var steps []LocalRepairStep
	readSet := make(map[int]struct{})
	for g, st := range states {
		losses := len(st.missingData)
		if st.localMissing {
			losses++
		}
		if losses == 0 {
			continue // untouched group: never read
		}
		if losses != 1 {
			return nil, nil, false // two losses in one group defeat a single XOR parity
		}
		members, _ := l.params.GroupMembers(g)
		localIdx, _ := l.params.LocalParityIndex(g)
		step := LocalRepairStep{Group: g}
		if st.localMissing {
			// Rebuild local parity = XOR of all members.
			step.Target = localIdx
			for _, d := range members {
				if !present[d] {
					return nil, nil, false
				}
				step.Peers = append(step.Peers, d)
				readSet[d] = struct{}{}
			}
		} else {
			// Rebuild the single missing data shard = local parity XOR peers.
			target := st.missingData[0]
			step.Target = target
			if !present[localIdx] {
				return nil, nil, false
			}
			step.Peers = append(step.Peers, localIdx)
			readSet[localIdx] = struct{}{}
			for _, d := range members {
				if d == target {
					continue
				}
				if !present[d] {
					return nil, nil, false
				}
				step.Peers = append(step.Peers, d)
				readSet[d] = struct{}{}
			}
		}
		sort.Ints(step.Peers)
		steps = append(steps, step)
	}
	reads := make([]int, 0, len(readSet))
	for idx := range readSet {
		reads = append(reads, idx)
	}
	sort.Ints(reads)
	return steps, reads, true
}

// codingRow returns the k-coefficient row (over the data variables) that a
// present physical shard represents in the global linear system.
func (l *LRC) codingRow(idx int) []byte {
	k := l.params.DataShards
	kind, group, _ := l.params.ClassifyShard(idx)
	row := make([]byte, k)
	switch kind {
	case ShardData:
		row[idx] = 1
	case ShardLocalParity:
		members, _ := l.params.GroupMembers(group)
		for _, d := range members {
			row[d] = 1
		}
	case ShardGlobalParity:
		j := idx - k - l.params.LocalGroups()
		copy(row, l.globalRows[j])
	}
	return row
}

// selectIndependent greedily picks k shard indices whose coding rows are
// linearly independent. It prefers data shards (unit rows), then global
// parity, and only uses local-parity equations when needed, so the read set
// stays as close to a plain RS recovery as possible.
func (l *LRC) selectIndependent(present []bool) ([]int, error) {
	k := l.params.DataShards

	var ordered []int
	for idx, ok := range present {
		if ok && idx < k {
			ordered = append(ordered, idx) // data first
		}
	}
	globalStart := k + l.params.LocalGroups()
	for idx, ok := range present {
		if ok && idx >= globalStart {
			ordered = append(ordered, idx) // global parity second
		}
	}
	for idx, ok := range present {
		if ok && idx >= k && idx < globalStart {
			ordered = append(ordered, idx) // local parity last
		}
	}

	basis := make([][]byte, 0, k) // reduced rows (pivot form) actually retained
	chosen := make([]int, 0, k)

	for _, idx := range ordered {
		if len(chosen) == k {
			break
		}
		reduced, grows := reduceRow(l.codingRow(idx), basis)
		if grows {
			basis = append(basis, reduced)
			chosen = append(chosen, idx)
		}
	}
	return chosen, nil
}

// reduceRow reduces row against the basis (whose rows have distinct leading
// pivots) over GF(2^8). It returns the reduced row and whether it is
// non-zero, i.e. whether the original row grew the basis rank.
func reduceRow(row []byte, basis [][]byte) ([]byte, bool) {
	work := append([]byte(nil), row...)
	for _, b := range basis {
		pivot := leadingPivot(b)
		if work[pivot] != 0 {
			// Basis rows are pivot-normalized (b[pivot] == 1), so this
			// XOR exactly cancels the pivot coefficient of work.
			scale := work[pivot]
			for c := range work {
				work[c] ^= galMultiply(scale, b[c])
			}
		}
	}
	if pivot := leadingPivot(work); pivot >= 0 {
		// Normalize the retained row so its pivot coefficient is 1.
		scale := galOneOver(work[pivot])
		for c := range work {
			work[c] = galMultiply(work[c], scale)
		}
		return work, true
	}
	return work, false
}

// leadingPivot returns the column index of the first non-zero coefficient,
// or -1 for the all-zero row.
func leadingPivot(row []byte) int {
	for c, v := range row {
		if v != 0 {
			return c
		}
	}
	return -1
}

// gfInvert returns the inverse of a square matrix over GF(2^8) using
// Gauss-Jordan elimination with explicit pivot selection. Unlike
// matrix.Invert, it does not assume the pivots already lie on the diagonal,
// which is required for a global solve built from an arbitrarily ordered
// basis (data, global, local rows).
func gfInvert(m matrix) (matrix, error) {
	n := len(m)
	aug, _ := newMatrix(n, 2*n)
	for r := 0; r < n; r++ {
		copy(aug[r][:n], m[r])
		aug[r][n+r] = 1
	}
	for col := 0; col < n; col++ {
		pivot := -1
		for r := col; r < n; r++ {
			if aug[r][col] != 0 {
				pivot = r
				break
			}
		}
		if pivot < 0 {
			return nil, errSingular
		}
		aug[col], aug[pivot] = aug[pivot], aug[col]

		scale := galOneOver(aug[col][col])
		for c := col; c < 2*n; c++ {
			aug[col][c] = galMultiply(aug[col][c], scale)
		}
		for r := 0; r < n; r++ {
			if r == col || aug[r][col] == 0 {
				continue
			}
			factor := aug[r][col]
			for c := col; c < 2*n; c++ {
				aug[r][c] ^= galMultiply(factor, aug[col][c])
			}
		}
	}
	out, _ := newMatrix(n, n)
	for r := 0; r < n; r++ {
		copy(out[r], aug[r][n:])
	}
	return out, nil
}
