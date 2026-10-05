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

package advisor

import (
	"fmt"
	"sort"
	"strings"

	casbin "github.com/casbin/casbin/v3"
	"github.com/casbin/casbin/v3/model"
)

// search holds the mutable state of one Suggest query. All trial grants are
// applied to sandbox, a detached copy of the caller's enforcer, and rolled
// back immediately after each candidate state is evaluated.
type search struct {
	sandbox    *casbin.Enforcer
	subject    string
	domain     string
	requests   [][]interface{}
	candidates []candidate
	budget     int

	evaluations  int
	exhausted    bool
	hasFreeRole  bool
	bestCost     int
	solutions    [][]string
	maxSolutions int
}

// run performs the branch-and-bound search.
func (s *search) run() (*Result, error) {
	s.bestCost = int(^uint(0) >> 1) // max int

	// Evaluate the root state: an already-valid policy is a hard
	// requirement, and an already-satisfied request set has the zero-cost
	// empty solution.
	ok, err := s.evaluate()
	if err != nil {
		return nil, fmt.Errorf("advisor: current policy is not constraint-valid: %w", err)
	}
	if s.exhausted {
		return s.result(), nil
	}
	if ok {
		s.record(0, nil)
		return s.result(), nil
	}

	s.dfs(0, 0, nil)
	return s.result(), nil
}

// dfs explores granting (or skipping) each candidate in order. On entry the
// sandbox carries exactly the grants listed in selected. Constraints are
// only checked at states that satisfy every target request: a rolePre
// violation, for example, is repairable by granting the prerequisite role
// later in the search, so intermediate violations must not be pruned.
func (s *search) dfs(index int, cost int, selected []string) {
	// A state strictly more expensive than the best known solution cannot
	// yield an optimum. Equal-cost states are still explored because other
	// zero-cost roles can form distinct equivalent solutions.
	if cost > s.bestCost {
		return
	}

	for i := index; i < len(s.candidates); i++ {
		cand := s.candidates[i]
		if cost+cand.cost > s.bestCost {
			continue
		}

		added, err := s.grant(cand.role)
		if err != nil {
			continue
		}
		if !added {
			continue
		}

		next := append(append([]string(nil), selected...), cand.role)
		s.evaluations++
		if s.evaluations > s.budget {
			s.exhausted = true
			s.revoke(cand.role)
			return
		}

		satisfied, valid, err := s.evaluateState()
		if err != nil {
			// Enforcement errors indicate a malformed request/model, not a
			// search outcome; treat this branch as unusable.
			s.revoke(cand.role)
			continue
		}

		if satisfied && valid {
			s.record(cost+cand.cost, next)
			// With positive costs, adding more roles cannot lower the cost,
			// so this branch is done. Zero-cost roles may still form
			// distinct equivalent solutions, so they are explored.
			if !s.hasFreeRole {
				s.revoke(cand.role)
				continue
			}
		}

		// Either the state is not a solution yet, or a zero-cost role could
		// still produce another equivalent solution: extend it.
		s.dfs(i+1, cost+cand.cost, next)
		s.revoke(cand.role)

		if s.exhausted {
			return
		}
	}
}

// evaluate inspects the current sandbox state before the search starts.
func (s *search) evaluate() (bool, error) {
	s.evaluations++
	if s.evaluations > s.budget {
		s.exhausted = true
		return false, nil
	}
	if err := s.sandbox.GetModel().ValidateConstraints(); err != nil {
		return false, err
	}
	return s.enforceAll()
}

// evaluateState reports whether the current sandbox state satisfies every
// request and, when it does, whether all model constraints hold.
func (s *search) evaluateState() (satisfied bool, valid bool, err error) {
	satisfied, err = s.enforceAll()
	if err != nil || !satisfied {
		return satisfied, false, err
	}
	if err := s.sandbox.GetModel().ValidateConstraints(); err != nil {
		return true, false, nil
	}
	return true, true, nil
}

// enforceAll reports whether every target request is allowed by the
// current sandbox state.
func (s *search) enforceAll() (bool, error) {
	for _, request := range s.requests {
		args := make([]interface{}, 0, len(request)+2)
		if s.domain != "" {
			args = append(args, s.subject, s.domain)
		} else {
			args = append(args, s.subject)
		}
		args = append(args, request...)

		allowed, err := s.sandbox.Enforce(args...)
		if err != nil {
			return false, err
		}
		if !allowed {
			return false, nil
		}
	}
	return true, nil
}

// grant tentatively adds the subject->role grouping rule and its role link.
// added is false when the rule already exists. Constraint validation is
// intentionally left to evaluateState.
func (s *search) grant(role string) (added bool, err error) {
	m := s.sandbox.GetModel()
	rule := s.rule(role)

	exists, err := m.HasPolicy("g", "g", rule)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}

	if err := m.AddPolicy("g", "g", rule); err != nil {
		return false, err
	}
	if err := s.sandbox.BuildIncrementalRoleLinks(model.PolicyAdd, "g", [][]string{rule}); err != nil {
		_, _ = m.RemovePolicy("g", "g", rule)
		return false, err
	}
	return true, nil
}

// revoke removes a tentatively added grouping rule and its role link.
func (s *search) revoke(role string) {
	rule := s.rule(role)
	_ = s.sandbox.BuildIncrementalRoleLinks(model.PolicyRemove, "g", [][]string{rule})
	_, _ = s.sandbox.GetModel().RemovePolicy("g", "g", rule)
}

// rule builds the grouping policy rule for a candidate grant.
func (s *search) rule(role string) []string {
	if s.domain != "" {
		return []string{s.subject, role, s.domain}
	}
	return []string{s.subject, role}
}

// record keeps solution only when it reaches the best known cost.
func (s *search) record(cost int, roles []string) {
	if cost > s.bestCost {
		return
	}
	if cost < s.bestCost {
		s.bestCost = cost
		s.solutions = s.solutions[:0]
	}
	snapshot := append([]string(nil), roles...)
	sort.Strings(snapshot)
	s.solutions = append(s.solutions, snapshot)
}

// result converts the accumulated solutions into the public Result,
// removing solutions that contain another equally cheap solution and
// applying MaxSolutions.
func (s *search) result() *Result {
	solutions := minimalInclusion(s.solutions)

	advice := make([]Advice, 0, len(solutions))
	for _, roles := range solutions {
		cost := 0
		for _, role := range roles {
			cost += s.costOf(role)
		}
		advice = append(advice, Advice{
			Subject: s.subject,
			Domain:  s.domain,
			Roles:   roles,
			Cost:    cost,
		})
	}
	sort.Slice(advice, func(i, j int) bool {
		if advice[i].Cost != advice[j].Cost {
			return advice[i].Cost < advice[j].Cost
		}
		return strings.Join(advice[i].Roles, ",") < strings.Join(advice[j].Roles, ",")
	})
	if s.maxSolutions > 0 && len(advice) > s.maxSolutions {
		advice = advice[:s.maxSolutions]
	}

	status := StatusFound
	if s.exhausted {
		status = StatusBudgetExhausted
	} else if len(advice) == 0 {
		status = StatusUnsatisfiable
	}
	return &Result{Status: status, Advice: advice, Evaluations: s.evaluations}
}

func (s *search) costOf(role string) int {
	for _, cand := range s.candidates {
		if cand.role == role {
			return cand.cost
		}
	}
	return 0
}

// minimalInclusion drops any role set that is a strict superset of another
// role set. With non-negative costs this only matters for zero-cost roles.
func minimalInclusion(sets [][]string) [][]string {
	sort.Slice(sets, func(i, j int) bool {
		if len(sets[i]) != len(sets[j]) {
			return len(sets[i]) < len(sets[j])
		}
		return strings.Join(sets[i], ",") < strings.Join(sets[j], ",")
	})

	kept := make([][]string, 0, len(sets))
	for _, candidate := range sets {
		dominated := false
		for _, smaller := range kept {
			if isSubset(smaller, candidate) {
				dominated = true
				break
			}
		}
		if !dominated {
			kept = append(kept, candidate)
		}
	}
	return kept
}

func isSubset(smaller, larger []string) bool {
	if len(smaller) > len(larger) {
		return false
	}
	set := make(map[string]bool, len(larger))
	for _, role := range larger {
		set[role] = true
	}
	for _, role := range smaller {
		if !set[role] {
			return false
		}
	}
	return true
}
