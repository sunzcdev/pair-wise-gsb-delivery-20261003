package federation

import (
	"fmt"
	"reflect"
	"slices"
)

// Metadata policy operators understood by this resolver. Entity statements
// carrying any other operator are rejected: silently ignoring an unknown
// operator would weaken constraints a superior entity meant to enforce.
const (
	OpValue      = "value"       // fixed value, overrides any subordinate policy and metadata
	OpAdd        = "add"         // values added to the metadata claim
	OpDefault    = "default"     // value used when the metadata claim is absent
	OpEssential  = "essential"   // if true, the metadata claim must be present
	OpSubsetOf   = "subset_of"   // metadata claim values must be a subset of these
	OpSupersetOf = "superset_of" // metadata claim values must be a superset of these
	OpOneOf      = "one_of"      // metadata claim value must be one of these
)

// SupportedOperators lists every metadata policy operator this resolver
// accepts, in no particular order.
var SupportedOperators = []string{
	OpValue, OpAdd, OpDefault, OpEssential, OpSubsetOf, OpSupersetOf, OpOneOf,
}

var supportedOperatorSet = func() map[string]bool {
	m := make(map[string]bool, len(SupportedOperators))
	for _, op := range SupportedOperators {
		m[op] = true
	}
	return m
}()

// ClaimPolicy maps a metadata policy operator to its operand, e.g.
// {"subset_of": ["openid", "profile"]}.
type ClaimPolicy map[string]any

// validatePolicyOperators rejects policies using operators outside the
// supported set.
func validatePolicyOperators(policy map[string]map[string]ClaimPolicy) error {
	for entityType, claims := range policy {
		for claim, cp := range claims {
			for op := range cp {
				if !supportedOperatorSet[op] {
					return fmt.Errorf("federation: unsupported metadata policy operator %q for claim %q in %q", op, claim, entityType)
				}
			}
		}
	}
	return nil
}

// combinePolicies merges a superior's metadata policy with the policy of the
// entity directly below it. The result is at least as restrictive as the
// superior's policy.
func combinePolicies(superior, subordinate map[string]ClaimPolicy) map[string]ClaimPolicy {
	if len(superior) == 0 {
		return subordinate
	}
	if len(subordinate) == 0 {
		return superior
	}
	out := make(map[string]ClaimPolicy, len(superior)+len(subordinate))
	for claim, cp := range subordinate {
		out[claim] = cp
	}
	for claim, sup := range superior {
		if sub, ok := out[claim]; ok {
			out[claim] = combineClaimPolicies(sup, sub)
		} else {
			out[claim] = sup
		}
	}
	return out
}

func combineClaimPolicies(superior, subordinate ClaimPolicy) ClaimPolicy {
	out := ClaimPolicy{}
	// A superior's fixed value and default cannot be relaxed from below.
	if v, ok := superior[OpValue]; ok {
		out[OpValue] = v
	} else if v, ok := subordinate[OpValue]; ok {
		out[OpValue] = v
	}
	if v, ok := superior[OpDefault]; ok {
		out[OpDefault] = v
	} else if v, ok := subordinate[OpDefault]; ok {
		out[OpDefault] = v
	}
	// add accumulates down the chain.
	if sup, ok := superior[OpAdd]; ok {
		out[OpAdd] = asSet(sup)
	}
	if sub, ok := subordinate[OpAdd]; ok {
		out[OpAdd] = unionSets(asSet(out[OpAdd]), asSet(sub))
	}
	// Set constraints combine by intersection: the result must satisfy both.
	for _, op := range []string{OpSubsetOf, OpSupersetOf, OpOneOf} {
		sup, supOK := superior[op]
		sub, subOK := subordinate[op]
		switch {
		case supOK && subOK:
			out[op] = intersectSets(asSet(sup), asSet(sub))
		case supOK:
			out[op] = sup
		case subOK:
			out[op] = sub
		}
	}
	// A claim required anywhere in the chain stays required.
	if truthy(superior[OpEssential]) || truthy(subordinate[OpEssential]) {
		if _, supOK := superior[OpEssential]; supOK {
			out[OpEssential] = superior[OpEssential]
		} else {
			out[OpEssential] = subordinate[OpEssential]
		}
	}
	return out
}

// applyMetadataPolicy applies a combined metadata policy to self-asserted
// metadata and returns the resulting trusted metadata. A metadata document
// that violates the policy is rejected outright.
func applyMetadataPolicy(metadata map[string]any, policy map[string]ClaimPolicy) (map[string]any, error) {
	out := make(map[string]any, len(metadata)+len(policy))
	for k, v := range metadata {
		out[k] = v
	}
	for claim, cp := range policy {
		if v, ok := cp[OpValue]; ok {
			out[claim] = v
		}
		if add, ok := cp[OpAdd]; ok {
			if existing, present := out[claim]; present {
				out[claim] = unionSets(asSet(existing), asSet(add))
			} else {
				out[claim] = asSet(add)
			}
		}
		value, present := out[claim]
		if subset, ok := cp[OpSubsetOf]; ok && present {
			if !isSubset(asSet(value), asSet(subset)) {
				return nil, fmt.Errorf("federation: metadata claim %q value %v is not a subset of policy %v", claim, value, subset)
			}
		}
		if superset, ok := cp[OpSupersetOf]; ok && present {
			if !isSubset(asSet(superset), asSet(value)) {
				return nil, fmt.Errorf("federation: metadata claim %q value %v is not a superset of policy %v", claim, value, superset)
			}
		}
		if oneOf, ok := cp[OpOneOf]; ok && present {
			if !containsValue(asSet(oneOf), value) {
				return nil, fmt.Errorf("federation: metadata claim %q value %v is not one of %v", claim, value, oneOf)
			}
		}
		if def, ok := cp[OpDefault]; ok && !present {
			out[claim] = def
			value, present = def, true
		}
		if truthy(cp[OpEssential]) && !present {
			return nil, fmt.Errorf("federation: essential metadata claim %q is missing", claim)
		}
	}
	return out, nil
}

// asSet normalizes a JSON value to a list. Arrays are kept as-is, scalars
// become single-element sets.
func asSet(v any) []any {
	if v == nil {
		return nil
	}
	if list, ok := v.([]any); ok {
		return list
	}
	return []any{v}
}

func containsValue(set []any, v any) bool {
	return slices.ContainsFunc(set, func(e any) bool { return reflect.DeepEqual(e, v) })
}

func unionSets(a, b []any) []any {
	out := slices.Clone(a)
	for _, v := range b {
		if !containsValue(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func intersectSets(a, b []any) []any {
	var out []any
	for _, v := range a {
		if containsValue(b, v) {
			out = append(out, v)
		}
	}
	return out
}

func isSubset(a, b []any) bool {
	for _, v := range a {
		if !containsValue(b, v) {
			return false
		}
	}
	return true
}

func truthy(v any) bool {
	b, ok := v.(bool)
	return ok && b
}
