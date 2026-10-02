package table

import "slices"

// AddPathDiversityMode determines how the Add-Path send-max slots for a
// destination are distributed among the candidate paths.
type AddPathDiversityMode int

const (
	// AddPathDiversityNone disables diversity-aware selection. The first
	// send-max candidates in best-path order are selected.
	AddPathDiversityNone AddPathDiversityMode = iota
	// AddPathDiversityNexthop prefers candidates with distinct next-hops.
	AddPathDiversityNexthop
	// AddPathDiversityNeighborAS prefers candidates received from distinct
	// neighboring (leftmost AS_PATH) autonomous systems.
	AddPathDiversityNeighborAS
)

// AddPathDiversityFill controls how the remaining send-max slots are handled
// when the number of distinct diversity groups is smaller than send-max.
type AddPathDiversityFill int

const (
	// AddPathDiversityFillBest fills the remaining slots with the best
	// remaining candidates, even if their diversity key is already covered.
	AddPathDiversityFillBest AddPathDiversityFill = iota
	// AddPathDiversityFillStrict leaves the remaining slots empty instead of
	// selecting candidates whose diversity key is already covered.
	AddPathDiversityFillStrict
)

// addPathDiversityKey extracts the diversity key of a path for the given
// mode. The key is computed from the path as stored in the RIB, before any
// outbound attribute rewriting (e.g. next-hop-self) takes place.
func addPathDiversityKey(p *Path, mode AddPathDiversityMode) any {
	switch mode {
	case AddPathDiversityNexthop:
		return p.GetNexthop()
	case AddPathDiversityNeighborAS:
		if asPath := p.GetAsPath(); asPath != nil {
			for _, seg := range asPath.Value {
				if asList := seg.GetAS(); len(asList) > 0 {
					return asList[0]
				}
			}
		}
		// Locally originated paths carry no AS_PATH; group them by the
		// source AS so they are not treated as distinct neighbors.
		if info := p.GetSource(); info != nil {
			return info.AS
		}
		return uint32(0)
	}
	return struct{}{}
}

// SelectAddPaths picks the indices of the candidates to advertise for a
// single destination under the Add-Path send-max limit. candidates must be
// sorted in best-path order (as in destination.knownPathList).
//
// With a diversity mode other than AddPathDiversityNone, the first candidate
// of each distinct diversity key is preferred over a better-ranked candidate
// whose key is already covered. When the distinct groups do not exhaust the
// send-max limit, the fill mode decides whether the remaining slots are
// backfilled with the best remaining candidates.
//
// The selection is deterministic: recomputing it over an unchanged candidate
// list always yields the same indices, so repeated recomputation neither
// changes the advertised set nor flaps path identifiers.
func SelectAddPaths(candidates []*Path, sendMax int, mode AddPathDiversityMode, fill AddPathDiversityFill) []int {
	if sendMax <= 0 || len(candidates) == 0 {
		return nil
	}
	if mode == AddPathDiversityNone {
		n := min(sendMax, len(candidates))
		selected := make([]int, n)
		for i := range n {
			selected[i] = i
		}
		return selected
	}
	selected := make([]int, 0, min(sendMax, len(candidates)))
	chosen := make([]bool, len(candidates))
	seen := make(map[any]struct{}, len(candidates))
	for i, p := range candidates {
		if len(selected) == sendMax {
			return selected
		}
		key := addPathDiversityKey(p, mode)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		chosen[i] = true
		selected = append(selected, i)
	}
	if fill == AddPathDiversityFillBest {
		for i := range candidates {
			if len(selected) == sendMax {
				break
			}
			if chosen[i] {
				continue
			}
			selected = append(selected, i)
		}
	}
	slices.Sort(selected)
	return selected
}
