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
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/casbin/casbin/v3/constant"
	"github.com/casbin/casbin/v3/effector"
	"github.com/casbin/govaluate"
)

// Decision is the three-valued result of Decide.
type Decision int

// Values for a Decide result.
const (
	// DecisionAllow means the request is granted with the attributes already present.
	DecisionAllow Decision = iota
	// DecisionDeny means the request is denied with the attributes already present.
	DecisionDeny
	// DecisionIndeterminate means the request cannot be decided yet: one or more
	// request attributes are missing and could still change the outcome.
	DecisionIndeterminate
)

// String returns the string representation of the decision.
func (d Decision) String() string {
	switch d {
	case DecisionAllow:
		return "allow"
	case DecisionDeny:
		return "deny"
	default:
		return "indeterminate"
	}
}

// missingAttrTracker wraps enforceParameters and records accesses to request
// attributes whose value is missing. A request attribute is considered missing
// when the corresponding request value is nil.
type missingAttrTracker struct {
	params  *enforceParameters
	missing map[string]bool
	touched map[string]bool
}

func newMissingAttrTracker(params *enforceParameters) *missingAttrTracker {
	missing := make(map[string]bool)
	for token, i := range params.rTokens {
		if i < len(params.rVals) && params.rVals[i] == nil {
			missing[token] = true
		}
	}
	return &missingAttrTracker{
		params:  params,
		missing: missing,
		touched: make(map[string]bool),
	}
}

// Get implements govaluate.Parameters.
func (t *missingAttrTracker) Get(name string) (interface{}, error) {
	if t.missing[name] {
		t.touched[name] = true
	}
	return t.params.Get(name)
}

func (t *missingAttrTracker) reset() {
	for token := range t.touched {
		delete(t.touched, token)
	}
}

// touchedAttributes returns the sorted names of the missing attributes that
// were accessed since the last reset.
func (t *missingAttrTracker) touchedAttributes() []string {
	attrs := make([]string, 0, len(t.touched))
	for token := range t.touched {
		attrs = append(attrs, token)
	}
	sort.Strings(attrs)
	return attrs
}

// decisionMerger merges per-rule three-valued outcomes into a final Decision,
// mirroring the semantics of effector.DefaultEffector. Rules are fed in policy
// order; merging short-circuits as soon as the outcome can no longer change, so
// missing attributes of rules that are irrelevant to the conclusion are never
// reported. A rule outcome is "known" when its matcher result does not depend
// on any missing attribute, otherwise it is unknown and its missing attributes
// are collected only if the rule's effect can still influence the final
// decision under the active effect expression:
//
//   - allow-override: a definite allow decides Allow immediately; unknown rules
//     with an allow effect keep the decision Indeterminate; otherwise Deny.
//   - deny-override: a definite deny decides Deny immediately; unknown rules
//     with a deny effect keep the decision Indeterminate; otherwise Allow.
//   - allow-and-deny: a definite deny decides Deny immediately (short-circuit);
//     unknown rules with an allow or deny effect keep the decision
//     Indeterminate; otherwise a definite allow decides Allow, else Deny.
//   - priority / subjectPriority: the first (highest priority) definite
//     allow/deny rule decides, unless an unknown allow/deny rule precedes it,
//     in which case the decision is Indeterminate; otherwise Deny.
type decisionMerger struct {
	expr      string
	decision  Decision
	done      bool
	sawAllow  bool
	needed    []string
	neededSet map[string]bool
}

func newDecisionMerger(expr string) *decisionMerger {
	return &decisionMerger{
		expr:      expr,
		neededSet: make(map[string]bool),
	}
}

func (m *decisionMerger) addNeeded(attrs []string) {
	for _, attr := range attrs {
		if !m.neededSet[attr] {
			m.neededSet[attr] = true
			m.needed = append(m.needed, attr)
		}
	}
}

// add feeds one rule outcome into the merger and reports whether the decision
// is final (no further rules need to be evaluated).
func (m *decisionMerger) add(known bool, matched bool, eft effector.Effect, missing []string) bool {
	switch m.expr {
	case constant.AllowOverrideEffect:
		if !known {
			if eft == effector.Allow {
				m.addNeeded(missing)
			}
			return false
		}
		if matched && eft == effector.Allow {
			m.decision = DecisionAllow
			m.done = true
		}
	case constant.DenyOverrideEffect:
		if !known {
			if eft == effector.Deny {
				m.addNeeded(missing)
			}
			return false
		}
		if matched && eft == effector.Deny {
			m.decision = DecisionDeny
			m.done = true
		}
	case constant.AllowAndDenyEffect:
		if !known {
			if eft == effector.Allow || eft == effector.Deny {
				m.addNeeded(missing)
			}
			return false
		}
		if matched && eft == effector.Deny {
			m.decision = DecisionDeny
			m.done = true
			return m.done
		}
		if matched && eft == effector.Allow {
			m.sawAllow = true
		}
	case constant.PriorityEffect, constant.SubjectPriorityEffect:
		if !known {
			if eft == effector.Allow || eft == effector.Deny {
				m.addNeeded(missing)
			}
			return false
		}
		if matched && (eft == effector.Allow || eft == effector.Deny) {
			switch {
			case len(m.needed) > 0:
				m.decision = DecisionIndeterminate
			case eft == effector.Allow:
				m.decision = DecisionAllow
			default:
				m.decision = DecisionDeny
			}
			m.done = true
		}
	}
	return m.done
}

// result returns the final decision and the attributes still needed to reach a
// definite conclusion. A final Allow or Deny never reports needed attributes.
func (m *decisionMerger) result() (Decision, []string, error) {
	if m.done {
		if m.decision == DecisionIndeterminate {
			return m.decision, m.needed, nil
		}
		return m.decision, nil, nil
	}

	if len(m.needed) > 0 {
		return DecisionIndeterminate, m.needed, nil
	}

	switch m.expr {
	case constant.AllowOverrideEffect:
		return DecisionDeny, nil, nil
	case constant.DenyOverrideEffect:
		return DecisionAllow, nil, nil
	case constant.AllowAndDenyEffect:
		if m.sawAllow {
			return DecisionAllow, nil, nil
		}
		return DecisionDeny, nil, nil
	case constant.PriorityEffect, constant.SubjectPriorityEffect:
		return DecisionDeny, nil, nil
	default:
		return DecisionDeny, nil, errors.New("unsupported effect")
	}
}

// Decide decides whether a "subject" can access a "object" with the operation
// "action" like Enforce does, but request attributes that are not available yet
// may be passed as nil. Instead of treating a missing attribute as a false
// condition, Decide then returns DecisionIndeterminate together with the names
// of the request attributes (e.g. "r_sub") that still need to be fetched to
// reach a definite conclusion. Attributes that cannot influence the conclusion
// are not reported. When no attribute is missing, the returned decision always
// agrees with Enforce.
func (e *Enforcer) Decide(rvals ...interface{}) (Decision, []string, error) {
	return e.decide("", rvals...)
}

// DecideWithMatcher is Decide with a custom matcher, like EnforceWithMatcher is
// to Enforce.
func (e *Enforcer) DecideWithMatcher(matcher string, rvals ...interface{}) (Decision, []string, error) {
	return e.decide(matcher, rvals...)
}

// decide is the three-valued counterpart of enforce, see Decide.
func (e *Enforcer) decide(matcher string, rvals ...interface{}) (decision Decision, needed []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()

	if !e.enabled {
		return DecisionAllow, nil, nil
	}

	var tracker *missingAttrTracker
	prep, err := e.prepareEnforcement(matcher, func(parameters *enforceParameters) govaluate.Parameters {
		tracker = newMissingAttrTracker(parameters)
		return tracker
	}, rvals)
	if err != nil {
		return DecisionDeny, nil, err
	}

	merger := newDecisionMerger(e.model["e"][prep.eType].Value)

	if policyLen := len(e.model["p"][prep.pType].Policy); policyLen != 0 && strings.Contains(prep.expString, prep.pType+"_") {
		for _, pvals := range e.model["p"][prep.pType].Policy {
			if len(e.model["p"][prep.pType].Tokens) != len(pvals) {
				return DecisionDeny, nil, fmt.Errorf(
					"invalid policy size: expected %d, got %d, pvals: %v",
					len(e.model["p"][prep.pType].Tokens),
					len(pvals),
					pvals)
			}

			prep.parameters.pVals = pvals
			tracker.reset()

			known, matched, err := evalRule(prep, tracker)
			if err != nil {
				return DecisionDeny, nil, err
			}

			eft := effector.Allow
			if j, ok := prep.parameters.pTokens[prep.pType+"_eft"]; ok {
				switch pvals[j] {
				case "allow":
					eft = effector.Allow
				case "deny":
					eft = effector.Deny
				default:
					eft = effector.Indeterminate
				}
			}

			if merger.add(known, matched, eft, tracker.touchedAttributes()) {
				break
			}
		}
	} else {
		if prep.hasEval && len(e.model["p"][prep.pType].Policy) == 0 {
			return DecisionDeny, nil, errors.New("please make sure rule exists in policy when using eval() in matcher")
		}

		prep.parameters.pVals = make([]string, len(prep.parameters.pTokens))
		tracker.reset()

		known, matched, err := evalRule(prep, tracker)
		if err != nil {
			return DecisionDeny, nil, err
		}
		if !known {
			// a policy-less matcher decides everything on its own, so any
			// missing attribute it depends on makes the request undecidable.
			return DecisionIndeterminate, tracker.touchedAttributes(), nil
		}

		// mirror enforce: a policy-less matcher acts as a single rule whose
		// effect is allow when the matcher holds, indeterminate otherwise.
		eft := effector.Indeterminate
		if matched {
			eft = effector.Allow
		}
		merger.add(known, true, eft, tracker.touchedAttributes())
	}

	return merger.result()
}

// evalRule evaluates the matcher for the current policy rule and reports
// whether the result is known (independent of missing attributes) and matched.
// Evaluation errors caused by missing attributes yield an unknown outcome;
// any other error is returned.
func evalRule(prep *preparedEnforcement, tracker *missingAttrTracker) (known bool, matched bool, err error) {
	result, evalErr := prep.expression.Eval(prep.evalParams)
	if evalErr != nil {
		if len(tracker.touched) > 0 {
			return false, false, nil
		}
		return false, false, evalErr
	}

	switch result := result.(type) {
	case bool:
		matched = result
	case float64:
		matched = result != 0
	default:
		return false, false, errors.New("matcher result should be bool, int or float")
	}

	// A result produced from a missing attribute cannot be trusted either way.
	known = len(tracker.touched) == 0
	return known, matched, nil
}
