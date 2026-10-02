package federation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// ClaimOrigin explains how one parameter of the resolved metadata got its
// final value.
type ClaimOrigin struct {
	// Parameter is the openid_provider metadata parameter name.
	Parameter string
	// Final is the JSON value present in the resolved metadata.
	Final json.RawMessage
	// Source is the identifier of the entity that supplied the final value:
	// the leaf entity ID for unchanged leaf values, or the issuer of the
	// decisive policy operator.
	Source string
	// Operator names the decisive operator ("value", "default", "add",
	// "merge", "subset_of", ...). Empty when the value is unchanged leaf
	// metadata.
	Operator string
}

// applyPolicy applies the composed policy to leaf metadata and returns the
// resulting openid_provider metadata together with per-parameter origins.
// Parameters introduced by "value"/"default" operators are included even if
// the leaf did not publish them; parameters the leaf published that now
// violate a constraint are hard errors.
func applyPolicy(leafEntityID string, metadata json.RawMessage, policy Policy, operatorIssuer map[string]string) (json.RawMessage, []ClaimOrigin, error) {
	result := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(metadata)) > 0 {
		if err := json.Unmarshal(metadata, &result); err != nil {
			return nil, nil, fmt.Errorf("decoding leaf openid_provider metadata: %v", err)
		}
	}

	origins := []ClaimOrigin{}

	params := make([]string, 0, len(policy))
	for param := range policy {
		params = append(params, param)
	}
	sort.Strings(params)

	for _, param := range params {
		operators := policy[param]
		raw, present := result[param]

		switch {
		case hasOp(operators, PolicyValue):
			result[param] = operators[PolicyValue]
			origins = append(origins, claimOrigin(param, operators[PolicyValue], operatorIssuer, PolicyValue))
		case present:
			next, touched, err := applyToExisting(param, raw, operators)
			if err != nil {
				return nil, nil, err
			}
			result[param] = next
			if touched {
				origins = append(origins, claimOrigin(param, next, operatorIssuer, lastMutatingOperator(operators)))
			}
		case hasOp(operators, PolicyDefault):
			result[param] = operators[PolicyDefault]
			origins = append(origins, claimOrigin(param, operators[PolicyDefault], operatorIssuer, PolicyDefault))
		default:
			// add/merge/subset_of/superset_of/one_of with neither a leaf
			// value nor a default do not create a parameter.
		}
	}

	for param, raw := range result {
		if _, touched := policy[param]; !touched {
			origins = append(origins, ClaimOrigin{Parameter: param, Final: cloneRaw(raw), Source: leafEntityID})
		}
	}

	out, err := json.Marshal(result)
	if err != nil {
		return nil, nil, err
	}
	return out, origins, nil
}

func claimOrigin(param string, final json.RawMessage, operatorIssuer map[string]string, operator string) ClaimOrigin {
	return ClaimOrigin{
		Parameter: param,
		Final:     cloneRaw(final),
		Source:    operatorIssuer[param],
		Operator:  operator,
	}
}

// lastMutatingOperator returns the name of the operator that mutated the
// parameter when it was applied to an existing leaf value (add or merge).
// Constraint-only operators (subset_of/superset_of/one_of) do not change the
// value, so they leave the origin at the leaf.
func lastMutatingOperator(operators Operators) string {
	if hasOp(operators, PolicyAdd) {
		return PolicyAdd
	}
	if hasOp(operators, PolicyMerge) {
		return PolicyMerge
	}
	return ""
}

func applyToExisting(param string, current json.RawMessage, operators Operators) (json.RawMessage, bool, error) {
	switch {
	case hasOp(operators, PolicyAdd):
		values, err := decodeStringList(current)
		if err != nil {
			return nil, false, fmt.Errorf("operator add for %q requires a JSON array of strings: %v", param, err)
		}
		added, err := decodeStringList(operators[PolicyAdd])
		if err != nil {
			return nil, false, err
		}
		for _, v := range added {
			if !stringListContains(values, v) {
				values = append(values, v)
			}
		}
		return encodeStringList(values), true, nil

	case hasOp(operators, PolicyMerge):
		currentObj, err := decodeObject(current)
		if err != nil {
			return nil, false, fmt.Errorf("operator merge for %q requires a JSON object: %v", param, err)
		}
		merged, err := decodeObject(operators[PolicyMerge])
		if err != nil {
			return nil, false, err
		}
		for k, v := range merged {
			currentObj[k] = v
		}
		out, err := json.Marshal(currentObj)
		return out, true, err

	case hasOp(operators, PolicySubsetOf):
		values, err := decodeStringList(current)
		if err != nil {
			return nil, false, fmt.Errorf("operator subset_of for %q requires a JSON array of strings: %v", param, err)
		}
		allowed, _ := decodeStringList(operators[PolicySubsetOf])
		for _, v := range values {
			if !stringListContains(allowed, v) {
				return nil, false, fmt.Errorf("leaf value %q for %q is not within subset_of %s", v, param, compact(operators[PolicySubsetOf]))
			}
		}
		return current, false, nil

	case hasOp(operators, PolicyOneOf):
		if !listContains(operators[PolicyOneOf], current) {
			return nil, false, fmt.Errorf("leaf value %s for %q is not in one_of %s", compact(current), param, compact(operators[PolicyOneOf]))
		}
		return current, false, nil

	case hasOp(operators, PolicySupersetOf):
		values, err := decodeStringList(current)
		if err != nil {
			return nil, false, fmt.Errorf("operator superset_of for %q requires a JSON array of strings: %v", param, err)
		}
		required, _ := decodeStringList(operators[PolicySupersetOf])
		for _, v := range required {
			if !stringListContains(values, v) {
				return nil, false, fmt.Errorf("leaf value for %q misses required superset_of element %q", param, v)
			}
		}
		return current, false, nil
	}
	return current, false, nil
}

func hasOp(operators Operators, name string) bool {
	_, ok := operators[name]
	return ok
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}

func jsonEqual(a, b json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b))
}

func compact(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(out)
}

func decodeStringList(raw json.RawMessage) ([]string, error) {
	var values []string
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	return values, nil
}

func encodeStringList(values []string) json.RawMessage {
	out, _ := json.Marshal(values)
	return out
}

func decodeObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

func stringListContains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// listContains reports whether the JSON array raw contains the JSON element
// (comparison is by compact JSON encoding).
func listContains(list, element json.RawMessage) bool {
	var values []json.RawMessage
	if err := json.Unmarshal(list, &values); err != nil {
		return false
	}
	for _, v := range values {
		if jsonEqual(v, element) {
			return true
		}
	}
	return false
}

func appendStringList(list, addition json.RawMessage) json.RawMessage {
	values, _ := decodeStringList(list)
	added, _ := decodeStringList(addition)
	for _, v := range added {
		if !stringListContains(values, v) {
			values = append(values, v)
		}
	}
	return encodeStringList(values)
}

func intersectStringLists(a, b json.RawMessage) json.RawMessage {
	left, _ := decodeStringList(a)
	right, _ := decodeStringList(b)
	out := make([]string, 0)
	seen := map[string]bool{}
	for _, v := range left {
		if stringListContains(right, v) && !seen[v] {
			out = append(out, v)
			seen[v] = true
		}
	}
	return encodeStringList(out)
}

func unionStringLists(a, b json.RawMessage) json.RawMessage {
	left, _ := decodeStringList(a)
	right, _ := decodeStringList(b)
	out := make([]string, 0, len(left)+len(right))
	for _, v := range append(append([]string{}, left...), right...) {
		if !stringListContains(out, v) {
			out = append(out, v)
		}
	}
	return encodeStringList(out)
}

func mergeJSONObjects(a, b json.RawMessage) json.RawMessage {
	left, err := decodeObject(a)
	if err != nil || left == nil {
		left = map[string]json.RawMessage{}
	}
	right, err := decodeObject(b)
	if err != nil {
		right = map[string]json.RawMessage{}
	}
	for k, v := range right {
		left[k] = v
	}
	out, _ := json.Marshal(left)
	return out
}
