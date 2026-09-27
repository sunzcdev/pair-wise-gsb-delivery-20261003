// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package casbin

import (
	"fmt"
	"sort"
	"strings"

	"github.com/casbin/casbin/v3/constant"
	"github.com/casbin/casbin/v3/effector"
)

// ObligationsTokenPrefix is the prefix of policy definition tokens that carry
// obligations, e.g. the token "obl_mask" in "p = sub, obj, act, eft, obl_mask"
// declares the obligation "mask".
const ObligationsTokenPrefix = "obl_"

// Obligations describes the duties the caller must fulfill when a request is
// allowed, e.g. "allow export, but mask the SSN column". The keys are the
// obligation names declared in the policy definition and the values are
// collected from the policy rules that actually took effect for the decision.
type Obligations map[string][]string

// ObligationsConflictStrategy decides how EnforceWithObligations reacts when
// the effective policy rules impose different values for the same obligation.
type ObligationsConflictStrategy int

const (
	// ObligationsConflictError makes EnforceWithObligations fail explicitly when
	// the effective rules impose conflicting values for the same obligation.
	// This is the default: conflicts are never silently overwritten.
	ObligationsConflictError ObligationsConflictStrategy = iota
	// ObligationsConflictUnion merges conflicting values into a de-duplicated
	// list instead of failing.
	ObligationsConflictUnion
)

// SetObligationsConflictStrategy sets how conflicting obligations imposed by
// different effective rules are handled by EnforceWithObligations.
func (e *Enforcer) SetObligationsConflictStrategy(strategy ObligationsConflictStrategy) {
	e.obligationsConflictStrategy = strategy
}

// EnforceWithObligations decides whether a request is allowed and, when it is,
// returns the obligations imposed by the policy rules that effectively
// contributed to the allow decision.
//
// Obligations are declared as extra policy definition tokens named
// "obl_<name>" (e.g. "p = sub, obj, act, eft, obl_mask, obl_watermark") and are
// returned as policy data for the caller to act on (mask a column, add a
// watermark, ...). They are collected only after the policy effect expression
// (allow/deny overrides, priority, ...) has determined which rules actually
// took effect: rules shadowed by a higher-priority deny or discarded by the
// effect expression never contribute obligations. When the effective rules
// impose conflicting values for the same obligation, the call fails unless a
// merge strategy has been configured with SetObligationsConflictStrategy.
func (e *Enforcer) EnforceWithObligations(rvals ...interface{}) (allowed bool, obligations Obligations, err error) {
	defer func() {
		if r := recover(); r != nil {
			allowed = false
			obligations = nil
			err = fmt.Errorf("panic: %v", r)
		}
	}()

	if !e.enabled {
		return true, nil, nil
	}

	res, err := e.evaluate("", true, rvals...)
	if err != nil {
		return false, nil, err
	}

	if res.effect != effector.Allow || !res.hasPolicy {
		return res.effect == effector.Allow, nil, nil
	}

	indexes, err := effectiveObligationIndexes(e.model["e"][res.eType].Value, res)
	if err != nil {
		return false, nil, err
	}

	policy := e.model["p"][res.pType].Policy
	rules := make([][]string, 0, len(indexes))
	for _, index := range indexes {
		if index < len(policy) {
			rules = append(rules, policy[index])
		}
	}

	obligations, err = e.mergeObligations(res.pType, rules)
	if err != nil {
		return false, nil, err
	}

	return true, obligations, nil
}

// effectiveObligationIndexes returns the indexes of the policy rules that
// effectively contributed to an allow decision under the given effect
// expression. It never simply concatenates every matching rule: the effect
// expression semantics decide which rules count.
func effectiveObligationIndexes(expr string, res *enforcementResult) ([]int, error) {
	switch expr {
	case constant.AllowOverrideEffect, constant.DenyOverrideEffect, constant.AllowAndDenyEffect:
		// The decision is allow, so no matched deny rule took effect; every
		// matched allow rule contributes its obligations.
		indexes := []int{}
		for i := range res.effects {
			if res.matches[i] != 0 && res.effects[i] == effector.Allow {
				indexes = append(indexes, i)
			}
		}
		return indexes, nil
	case constant.PriorityEffect, constant.SubjectPriorityEffect:
		// Only the single decisive (highest-priority) rule takes effect; rules
		// shadowed by it contribute nothing.
		if res.explainIndex >= 0 && res.explainIndex < len(res.effects) && res.effects[res.explainIndex] == effector.Allow {
			return []int{res.explainIndex}, nil
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("obligations are not supported for effect expression: %s", expr)
	}
}

// mergeObligations collects the obligations declared by the effective rules.
// Conflicting values for the same obligation are never silently overwritten:
// they either make the call fail or are merged according to the configured
// ObligationsConflictStrategy.
func (e *Enforcer) mergeObligations(pType string, rules [][]string) (Obligations, error) {
	prefix := pType + "_" + ObligationsTokenPrefix

	type obligationColumn struct {
		name  string
		index int
	}
	columns := []obligationColumn{}
	for i, token := range e.model["p"][pType].Tokens {
		if strings.HasPrefix(token, prefix) {
			columns = append(columns, obligationColumn{name: strings.TrimPrefix(token, prefix), index: i})
		}
	}
	// Iterate columns in a deterministic order so conflicts and merged values
	// are reported stably.
	sort.Slice(columns, func(i, j int) bool { return columns[i].name < columns[j].name })

	obligations := Obligations{}
	for _, rule := range rules {
		for _, column := range columns {
			value := rule[column.index]
			if value == "" {
				continue
			}

			values, ok := obligations[column.name]
			if !ok {
				obligations[column.name] = []string{value}
				continue
			}

			duplicate := false
			for _, existing := range values {
				if existing == value {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}

			if e.obligationsConflictStrategy != ObligationsConflictUnion {
				return nil, fmt.Errorf("conflicting obligations for %q: %q vs %q", column.name, values[0], value)
			}
			obligations[column.name] = append(values, value)
		}
	}

	return obligations, nil
}
