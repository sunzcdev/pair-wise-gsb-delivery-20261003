package table

import (
	"fmt"

	"github.com/osrg/gobgp/v4/pkg/config/oc"
)

// AddPathDiversityKey returns the diversity key of a RIB path for the given
// selection diversity mode. Paths sharing a key are considered to come from
// the same source for add-paths diversity selection. The key is derived from
// the pre-policy RIB path, so it is not affected by outbound attribute
// rewriting (e.g. next-hop-self).
func AddPathDiversityKey(mode oc.AddPathsSelectionDiversityType, path *Path) string {
	switch mode {
	case oc.ADD_PATHS_SELECTION_DIVERSITY_NEXTHOP:
		return "nh:" + path.GetNexthop().String()
	case oc.ADD_PATHS_SELECTION_DIVERSITY_NEIGHBOR_AS:
		if asPath := path.GetAsPath(); asPath != nil && len(asPath.Value) > 0 {
			if asList := asPath.Value[0].GetAS(); len(asList) > 0 {
				return fmt.Sprintf("as:%d", asList[0])
			}
		}
		// Locally originated paths carry no AS_PATH yet; group them
		// together under a dedicated key.
		return "as:local"
	}
	return ""
}

// SelectAddPathsWithDiversity picks the indices of up to sendMax candidates
// to advertise for a single NLRI. Candidates must be ordered from best to
// worst (the order of the destination's known path list) and must already be
// filtered by the export policy: a path rejected by policy must never appear
// in candidates, so diversity selection can never override an explicit
// policy denial.
//
// The selection first picks the best candidate of each distinct diversity
// key. If slots remain, the backfill mode decides whether they are filled
// with the best remaining candidates (ADD_PATHS_SELECTION_BACKFILL_BEST_PATH)
// or left unused (ADD_PATHS_SELECTION_BACKFILL_NONE).
//
// The result is deterministic for a given input, so recomputing the
// selection over an unchanged candidate set always yields the same paths and
// neither Path IDs nor advertisements flap.
func SelectAddPathsWithDiversity(candidates []*Path, mode oc.AddPathsSelectionDiversityType, sendMax uint8, backfill oc.AddPathsSelectionBackfillType) []int {
	if sendMax == 0 || len(candidates) == 0 {
		return nil
	}
	limit := int(sendMax)
	selected := make([]int, 0, limit)
	isSelected := make([]bool, len(candidates))

	// Diversity pass: take the best candidate of each distinct key.
	seen := make(map[string]struct{}, len(candidates))
	for i, p := range candidates {
		if len(selected) >= limit {
			break
		}
		key := AddPathDiversityKey(mode, p)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		selected = append(selected, i)
		isSelected[i] = true
	}

	// Backfill pass: fill the remaining slots with the best remaining
	// candidates, regardless of their diversity key.
	if backfill != oc.ADD_PATHS_SELECTION_BACKFILL_NONE {
		for i := range candidates {
			if len(selected) >= limit {
				break
			}
			if isSelected[i] {
				continue
			}
			selected = append(selected, i)
			isSelected[i] = true
		}
	}
	return selected
}
