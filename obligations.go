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
	"strings"

	"github.com/casbin/casbin/v3/effector"
)

// Obligations is the set of duties attached to an allow decision, e.g.
// "mask=ssn" (export allowed, but must be desensitized) or "watermark=owner"
// (download allowed, but must be watermarked). Obligations are policy data
// returned to the caller, which is responsible for fulfilling them.
type Obligations map[string]string

// ObligationConflictError is returned when effective allow rules attach
// mutually exclusive values to the same obligation field and no
// ObligationsMerger is configured to reconcile them.
type ObligationConflictError struct {
	Field  string
	Values []string
}

// Error implements the error interface.
func (e *ObligationConflictError) Error() string {
	return fmt.Sprintf("obligation conflict on field %q: values %v", e.Field, e.Values)
}

// ObligationsMerger reconciles conflicting values proposed by different
// effective rules for the same obligation field. values holds the distinct
// proposed values in policy order.
type ObligationsMerger func(field string, values []string) (string, error)

// JoinObligationsMerger returns an ObligationsMerger that joins conflicting
// values with sep.
func JoinObligationsMerger(sep string) ObligationsMerger {
	return func(field string, values []string) (string, error) {
		return strings.Join(values, sep), nil
	}
}

// SetObligationsMerger configures how conflicting obligation values are
// merged. The default (nil) is to fail with an *ObligationConflictError
// instead of silently overwriting one value with another.
func (e *Enforcer) SetObligationsMerger(merger ObligationsMerger) {
	e.obligationsMerger = merger
}

// EnforceWithObligations decides whether a "subject" can access a "object"
// with the operation "action", and additionally returns the obligations
// attached to the effective allow rule(s) as policy data. A deny decision
// never carries obligations: rules overridden by a deny or a higher-priority
// rule do not contribute.
func (e *Enforcer) EnforceWithObligations(rvals ...interface{}) (bool, Obligations, error) {
	obligations := Obligations{}
	result, err := e.enforce("", nil, &obligations, rvals...)
	if err != nil {
		return false, nil, err
	}
	return result, obligations, nil
}

// collectObligations gathers the obligations of the rules that are effective
// for an allow decision: with a priority effect only the winning rule
// contributes, otherwise every matched allow rule contributes. Conflicting
// values for the same field are reconciled by the configured ObligationsMerger
// or fail with an *ObligationConflictError.
func (e *Enforcer) collectObligations(pType string, pTokens map[string]int, priorityEffect bool, explainIndex int, policyEffects []effector.Effect, matcherResults []float64) (Obligations, error) {
	obligations := Obligations{}
	obligationsIndex, ok := pTokens[pType+"_obligations"]
	if !ok {
		return obligations, nil
	}

	policy := e.model["p"][pType].Policy
	ruleIndexes := make([]int, 0, len(policy))
	if priorityEffect {
		if explainIndex >= 0 && explainIndex < len(policy) {
			ruleIndexes = append(ruleIndexes, explainIndex)
		}
	} else {
		for i := range policy {
			if matcherResults[i] != 0 && policyEffects[i] == effector.Allow {
				ruleIndexes = append(ruleIndexes, i)
			}
		}
	}

	fields := make([]string, 0)
	fieldValues := make(map[string][]string)
	for _, ruleIndex := range ruleIndexes {
		pairs, err := parseObligations(policy[ruleIndex][obligationsIndex])
		if err != nil {
			return nil, fmt.Errorf("invalid obligations in policy rule %v: %w", policy[ruleIndex], err)
		}
		for _, pair := range pairs {
			field, value := pair[0], pair[1]
			values, seen := fieldValues[field]
			if !seen {
				fields = append(fields, field)
			}
			duplicate := false
			for _, v := range values {
				if v == value {
					duplicate = true
					break
				}
			}
			if !duplicate {
				fieldValues[field] = append(values, value)
			}
		}
	}

	for _, field := range fields {
		values := fieldValues[field]
		if len(values) == 1 {
			obligations[field] = values[0]
			continue
		}
		if e.obligationsMerger == nil {
			return nil, &ObligationConflictError{Field: field, Values: values}
		}
		merged, err := e.obligationsMerger(field, values)
		if err != nil {
			return nil, err
		}
		obligations[field] = merged
	}
	return obligations, nil
}

// parseObligations parses a policy obligations column of the form
// "mask=ssn; watermark=owner". An empty or "-" column means no obligations.
func parseObligations(raw string) ([][2]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "-" {
		return nil, nil
	}
	var pairs [][2]string
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		sep := strings.Index(part, "=")
		if sep <= 0 {
			return nil, fmt.Errorf("invalid obligation %q, expected key=value", part)
		}
		pairs = append(pairs, [2]string{strings.TrimSpace(part[:sep]), strings.TrimSpace(part[sep+1:])})
	}
	return pairs, nil
}
