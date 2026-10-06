package lrc

import (
	"fmt"
)

// Reconstruct fills every nil/zero-length shard that the layout can repair.
// It computes the repair plan and executes it: locally repairable groups are
// rebuilt from their own shards only, remaining losses are covered by the
// global stripe, and lost parity shards are regenerated afterwards.
//
// If the loss pattern is not recoverable under this LRC layout,
// ErrUnrecoverable is returned and shards are left unmodified.
func (ly *Layout) Reconstruct(shards [][]byte) error {
	plan, err := ly.RepairPlan(shards)
	if err != nil {
		return err
	}
	return ly.Execute(shards, plan)
}

// Execute runs a previously computed Plan against the shards. A caller that
// wants to fetch shards from remote storage itself can inspect
// plan.ReadShards (or each step's ReadShards), fill those slices in, and then
// call Execute.
func (ly *Layout) Execute(shards [][]byte, plan *Plan) error {
	if len(shards) != ly.Shards() {
		return fmt.Errorf("%w: expected %d shards, got %d", ErrInvalidLayout, ly.Shards(), len(shards))
	}

	shardsize := 0
	for _, s := range shards {
		if len(s) > 0 {
			shardsize = len(s)
			break
		}
	}
	if shardsize == 0 {
		return fmt.Errorf("%w: cannot reconstruct without any reference shard", ErrInvalidLayout)
	}

	for stepIdx, step := range plan.Steps {
		for _, idx := range step.ReadShards {
			if len(shards[idx]) != shardsize {
				return fmt.Errorf("%w: step %d requires shard %d, present size %d, expected %d",
					ErrInvalidLayout, stepIdx, idx, len(shards[idx]), shardsize)
			}
		}

		switch {
		case step.Tier == TierLocal && step.Kind == KindReconstruct:
			g := step.Group
			view := make([][]byte, ly.dataPerGroup+ly.p)
			for s := 0; s < ly.dataPerGroup+ly.p; s++ {
				idx := ly.LocalShardIndex(g, s)
				view[s] = shards[idx]
			}
			required := make([]bool, ly.dataPerGroup+ly.p)
			for _, idx := range step.Missing {
				if idx < ly.k {
					required[idx-(g*ly.dataPerGroup)] = true
				} else {
					required[ly.dataPerGroup+(idx-(ly.k+g*ly.p))] = true
				}
			}
			if err := ly.groups[g].ReconstructSome(view, required); err != nil {
				return fmt.Errorf("local reconstruct group %d: %w", g, err)
			}
			for s := 0; s < ly.dataPerGroup+ly.p; s++ {
				idx := ly.LocalShardIndex(g, s)
				shards[idx] = view[s]
			}

		case step.Tier == TierGlobal && step.Kind == KindReconstruct:
			view := make([][]byte, ly.k+ly.r)
			copy(view, shards[:ly.k])
			copy(view[ly.k:], shards[ly.k+ly.localTotal:])
			required := make([]bool, ly.k+ly.r)
			for _, idx := range step.Missing {
				if idx < ly.k {
					required[idx] = true
				} else {
					required[ly.k+(idx-(ly.k+ly.localTotal))] = true
				}
			}
			if err := ly.global.ReconstructSome(view, required); err != nil {
				return fmt.Errorf("global reconstruct: %w", err)
			}
			copy(shards[:ly.k], view[:ly.k])
			for j := 0; j < ly.r; j++ {
				shards[ly.GlobalParityIndex(j)] = view[ly.k+j]
			}

		case step.Tier == TierLocal && step.Kind == KindRegenerate:
			g := step.Group
			view := make([][]byte, ly.dataPerGroup+ly.p)
			for s := 0; s < ly.dataPerGroup; s++ {
				view[s] = shards[ly.LocalShardIndex(g, s)]
			}
			for j := 0; j < ly.p; j++ {
				idx := ly.LocalShardIndex(g, ly.dataPerGroup+j)
				if len(shards[idx]) == shardsize {
					view[ly.dataPerGroup+j] = shards[idx]
				} else {
					view[ly.dataPerGroup+j] = make([]byte, shardsize)
				}
			}
			if err := ly.groups[g].Encode(view); err != nil {
				return fmt.Errorf("local regenerate group %d: %w", g, err)
			}
			for j := 0; j < ly.p; j++ {
				idx := ly.LocalShardIndex(g, ly.dataPerGroup+j)
				shards[idx] = view[ly.dataPerGroup+j]
			}

		case step.Tier == TierGlobal && step.Kind == KindRegenerate:
			view := make([][]byte, ly.k+ly.r)
			copy(view, shards[:ly.k])
			for j := 0; j < ly.r; j++ {
				idx := ly.GlobalParityIndex(j)
				if len(shards[idx]) == shardsize {
					view[ly.k+j] = shards[idx]
				} else {
					view[ly.k+j] = make([]byte, shardsize)
				}
			}
			if err := ly.global.Encode(view); err != nil {
				return fmt.Errorf("global regenerate: %w", err)
			}
			for j := 0; j < ly.r; j++ {
				shards[ly.GlobalParityIndex(j)] = view[ly.k+j]
			}

		default:
			return fmt.Errorf("%w: unknown repair step tier=%s kind=%s", ErrInvalidLayout, step.Tier, step.Kind)
		}
	}
	return nil
}
