package federation

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Metadata policy operators supported by this resolver, as defined by
// OpenID Federation 1.0 section 5.1.4.
const (
	PolicyValue      = "value"
	PolicyDefault    = "default"
	PolicyAdd        = "add"
	PolicyMerge      = "merge"
	PolicySubsetOf   = "subset_of"
	PolicySupersetOf = "superset_of"
	PolicyOneOf      = "one_of"
)

// SupportedPolicyOperators is the closed set of metadata policy operators the
// resolver understands. Every other operator encountered on a metadata
// policy in scope (openid_provider) is rejected with an error rather than
// being skipped: an unsupported operator is critical in the federation model
// and silently ignoring it would let an unvalidated policy pass.
var SupportedPolicyOperators = map[string]bool{
	PolicyValue:      true,
	PolicyDefault:    true,
	PolicyAdd:        true,
	PolicyMerge:      true,
	PolicySubsetOf:   true,
	PolicySupersetOf: true,
	PolicyOneOf:      true,
}

// Operators holds the operators applied to a single metadata parameter, keyed
// by operator name. Values are kept as raw JSON until application, when their
// types are checked against the parameter's current value.
type Operators map[string]json.RawMessage

// Policy maps a metadata parameter name (e.g. "scopes_supported") to the
// operators an entity statement declares for it.
type Policy map[string]Operators

func parsePolicy(raw json.RawMessage) (Policy, error) {
	var perParameter map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &perParameter); err != nil {
		return nil, fmt.Errorf("decoding metadata policy: %v", err)
	}

	policy := make(Policy, len(perParameter))
	for param, operators := range perParameter {
		if len(operators) == 0 {
			return nil, fmt.Errorf("parameter %q declares no operators", param)
		}
		op := make(Operators, len(operators))
		for name, value := range operators {
			if !SupportedPolicyOperators[name] {
				return nil, fmt.Errorf("parameter %q uses unsupported operator %q; supported operators: %s",
					param, name, joinOperatorNames())
			}
			if !validOperatorValue(name, value) {
				return nil, fmt.Errorf("parameter %q uses operator %q with a value of the wrong JSON type", param, name)
			}
			op[name] = value
		}
		// OpenID Federation 1.0 section 5.1.4: a metadata policy MUST NOT
		// contain more than one operator per parameter within a single
		// statement; conflicts across statements are handled by the rules of
		// combination.
		if len(op) > 1 {
			return nil, fmt.Errorf("parameter %q declares multiple operators in one statement (%s), which is forbidden",
				param, joinSortedOperatorNames(op))
		}
		policy[param] = op
	}
	return policy, nil
}

func joinSortedOperatorNames(op Operators) string {
	names := make([]string, 0, len(op))
	for name := range op {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func joinOperatorNames() string {
	names := make([]string, 0, len(SupportedPolicyOperators))
	for name := range SupportedPolicyOperators {
		names = append(names, name)
	}
	sort.Strings(names)
	result := ""
	for i, name := range names {
		if i > 0 {
			result += ", "
		}
		result += name
	}
	return result
}

// composePoliciesWithOrigins merges policies declared from trust anchor
// toward the leaf using the rules of combination from OpenID Federation 1.0
// section 5.1.4. Conflicts are errors, never silent wins. It returns the
// composed policy and, per parameter, the issuer that fixed the decisive
// operator.
func composePoliciesWithOrigins(steps []PolicyContribution) (Policy, map[string]string, error) {
	composed := Policy{}
	origins := map[string]string{}
	for _, step := range steps {
		for param, incoming := range step.Policy {
			merged, decisive, err := composeParameter(composed[param], incoming)
			if err != nil {
				return nil, nil, fmt.Errorf("operator conflict for %q at %q: %v", param, step.Issuer, err)
			}
			composed[param] = merged
			if decisive != "" {
				origins[param] = step.Issuer
			}
		}
	}
	return composed, origins, nil
}

func composeParameter(previous, incoming Operators) (result Operators, decisiveOperator string, err error) {
	result = Operators{}
	for name, value := range previous {
		result[name] = value
	}

	for name, value := range incoming {
		switch name {
		case PolicyValue:
			// A downstream value must still satisfy every constraint the
			// superiors already composed: it has to belong to a subset_of or
			// one_of set, and satisfy any superset_of requirement.
			if allowed, ok := result[PolicySubsetOf]; ok && !listContains(allowed, value) {
				return nil, "", fmt.Errorf("fixed value %s not in subset_of %s", compact(value), compact(allowed))
			}
			if allowed, ok := result[PolicyOneOf]; ok && !listContains(allowed, value) {
				return nil, "", fmt.Errorf("fixed value %s not in one_of %s", compact(value), compact(allowed))
			}
			if required, ok := result[PolicySupersetOf]; ok {
				elements, err := decodeStringList(value)
				if err != nil {
					return nil, "", fmt.Errorf("value %s fails superset_of %s: value is not a JSON string array", compact(value), compact(required))
				}
				wanted, _ := decodeStringList(required)
				for _, item := range wanted {
					if !stringListContains(elements, item) {
						return nil, "", fmt.Errorf("fixed value %s fails superset_of %s", compact(value), compact(required))
					}
				}
			}
			// value overrides any default a superior proposed.
			if existing, ok := result[PolicyAdd]; ok {
				added, _ := decodeStringList(existing)
				elements, err := decodeStringList(value)
				if err != nil {
					return nil, "", fmt.Errorf("value %s cannot replace add %s: value is not a JSON string array", compact(value), compact(existing))
				}
				for _, item := range added {
					if !stringListContains(elements, item) {
						return nil, "", fmt.Errorf("fixed value %s does not contain added element %q", compact(value), item)
					}
				}
			}
			if existing, ok := result[PolicyMerge]; ok {
				currentObj, err := decodeObject(value)
				if err != nil {
					return nil, "", fmt.Errorf("value %s cannot replace merge %s: value is not a JSON object", compact(value), compact(existing))
				}
				merged, _ := decodeObject(existing)
				for key, mergedValue := range merged {
					if got, ok := currentObj[key]; !ok || !jsonEqual(got, mergedValue) {
						return nil, "", fmt.Errorf("fixed value for merged key %q does not match %s", key, compact(mergedValue))
					}
				}
			}
			result = Operators{PolicyValue: value}
			decisiveOperator = PolicyValue

		case PolicyDefault:
			if _, ok := result[PolicyValue]; ok {
				continue
			}
			if existing, ok := result[PolicyDefault]; ok && !jsonEqual(existing, value) {
				return nil, "", fmt.Errorf("conflicting default operators (%s vs %s)", compact(existing), compact(value))
			}
			result[PolicyDefault] = value
			decisiveOperator = PolicyDefault

		case PolicyAdd:
			if _, ok := result[PolicyValue]; ok {
				continue
			}
			added, _ := decodeStringList(value)
			if subset, ok := result[PolicySubsetOf]; ok {
				allowed, _ := decodeStringList(subset)
				kept := make([]string, 0)
				for _, item := range added {
					if stringListContains(allowed, item) {
						kept = append(kept, item)
					}
				}
				if len(kept) == 0 {
					return nil, "", fmt.Errorf("add %s has no element within subset_of %s", compact(value), compact(subset))
				}
				value = encodeStringList(kept)
			}
			if oneOf, ok := result[PolicyOneOf]; ok {
				allowed, _ := decodeStringList(oneOf)
				kept := make([]string, 0)
				for _, item := range added {
					if stringListContains(allowed, item) {
						kept = append(kept, item)
					}
				}
				if len(kept) == 0 {
					return nil, "", fmt.Errorf("add %s has no element within one_of %s", compact(value), compact(oneOf))
				}
				value = encodeStringList(kept)
			}
			result[PolicyAdd] = appendStringList(result[PolicyAdd], value)
			decisiveOperator = PolicyAdd

		case PolicyMerge:
			if _, ok := result[PolicyValue]; ok {
				continue
			}
			if _, ok := result[PolicyAdd]; ok {
				return nil, "", fmt.Errorf("merge cannot be combined with add")
			}
			result[PolicyMerge] = mergeJSONObjects(result[PolicyMerge], value)
			decisiveOperator = PolicyMerge

		case PolicySubsetOf:
			if existing, ok := result[PolicyValue]; ok {
				if !listContains(value, existing) {
					return nil, "", fmt.Errorf("fixed value %s not in subset_of %s", compact(existing), compact(value))
				}
				delete(result, PolicySubsetOf)
				continue
			}
			if existing, ok := result[PolicyDefault]; ok {
				if !listContains(value, existing) {
					return nil, "", fmt.Errorf("default %s not in subset_of %s", compact(existing), compact(value))
				}
				delete(result, PolicySubsetOf)
				continue
			}
			if existing, ok := result[PolicySubsetOf]; ok {
				value = intersectStringLists(existing, value)
			}
			result[PolicySubsetOf] = value
			decisiveOperator = PolicySubsetOf

		case PolicySupersetOf:
			if existing, ok := result[PolicyValue]; ok {
				if !listContains(value, existing) {
					return nil, "", fmt.Errorf("fixed value %s fails superset_of %s", compact(existing), compact(value))
				}
				delete(result, PolicySupersetOf)
				continue
			}
			if existing, ok := result[PolicyDefault]; ok {
				if !listContains(value, existing) {
					return nil, "", fmt.Errorf("default %s fails superset_of %s", compact(existing), compact(value))
				}
				delete(result, PolicySupersetOf)
				continue
			}
			if existing, ok := result[PolicySubsetOf]; ok {
				intersection := intersectStringLists(existing, value)
				if len(intersection) == 0 {
					return nil, "", fmt.Errorf("superset_of %s and subset_of %s have no common element", compact(value), compact(existing))
				}
				delete(result, PolicySubsetOf)
				result[PolicyOneOf] = intersection
				decisiveOperator = PolicyOneOf
				continue
			}
			if existing, ok := result[PolicySupersetOf]; ok {
				result[PolicySupersetOf] = unionStringLists(existing, value)
			} else {
				result[PolicySupersetOf] = value
			}
			decisiveOperator = PolicySupersetOf

		case PolicyOneOf:
			if existing, ok := result[PolicyValue]; ok {
				if !listContains(value, existing) {
					return nil, "", fmt.Errorf("fixed value %s not in one_of %s", compact(existing), compact(value))
				}
				delete(result, PolicyOneOf)
				continue
			}
			if existing, ok := result[PolicyDefault]; ok {
				if !listContains(value, existing) {
					return nil, "", fmt.Errorf("default %s not in one_of %s", compact(existing), compact(value))
				}
				delete(result, PolicyOneOf)
				continue
			}
			if _, ok := result[PolicySupersetOf]; ok {
				return nil, "", fmt.Errorf("one_of cannot be combined with superset_of")
			}
			if existing, ok := result[PolicySubsetOf]; ok {
				value = intersectStringLists(existing, value)
			}
			if existing, ok := result[PolicyOneOf]; ok {
				value = intersectStringLists(existing, value)
			}
			if len(value) == 0 {
				return nil, "", fmt.Errorf("one_of narrowed to an empty set")
			}
			result[PolicyOneOf] = value
			decisiveOperator = PolicyOneOf
		}
	}
	return result, decisiveOperator, nil
}

func validOperatorValue(name string, raw json.RawMessage) bool {
	switch name {
	case PolicyValue, PolicyDefault:
		// Any JSON value.
		return true
	case PolicyOneOf:
		var values []json.RawMessage
		return json.Unmarshal(raw, &values) == nil && len(values) > 0
	case PolicyAdd, PolicySubsetOf, PolicySupersetOf:
		var values []string
		return json.Unmarshal(raw, &values) == nil
	case PolicyMerge:
		var obj map[string]json.RawMessage
		return json.Unmarshal(raw, &obj) == nil
	}
	return false
}
