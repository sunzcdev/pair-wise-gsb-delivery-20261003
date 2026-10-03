package arc

import "fmt"

// checkSealInput validates the existing ARC fields before adding a new set.
// Sealing on top of a malformed chain would only bury the problem, so the
// existing fields must carry valid, contiguous instance numbers.
func checkSealInput(c collectedFields) error {
	maxI := 0
	counts := map[string]map[int]int{}
	for kind, fields := range map[string][]*rawField{
		"aar": c.aar,
		"ams": c.ams,
		"as":  c.as,
	} {
		counts[kind] = map[int]int{}
		for _, f := range fields {
			if f.instance == 0 {
				return fmt.Errorf("%w: cannot seal on top of a field with a missing instance number", ErrMalformedChain)
			}
			counts[kind][f.instance]++
			if counts[kind][f.instance] > 1 {
				return fmt.Errorf("%w: duplicate %s with instance number %d", ErrMalformedChain, kindName(kind), f.instance)
			}
			if f.instance > maxI {
				maxI = f.instance
			}
		}
	}

	if maxI == 0 {
		return nil
	}

	// Every instance from 1 to maxI must have exactly one field of each
	// kind, and the three kinds must agree on the highest instance.
	for i := 1; i <= maxI; i++ {
		for kind, m := range counts {
			if m[i] != 1 {
				return fmt.Errorf("%w: instance %d does not have exactly one %s before sealing", ErrMalformedChain, i, kindName(kind))
			}
		}
	}

	return nil
}
