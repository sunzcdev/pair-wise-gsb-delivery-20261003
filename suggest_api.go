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
	"sort"
)

const (
	// DefaultSuggestMaxNodes is the default search budget (number of role
	// combinations evaluated) used when SuggestOptions.MaxNodes is 0.
	DefaultSuggestMaxNodes = 10000
	// DefaultSuggestMaxSolutions is the default cap on the number of optimal
	// solutions returned when SuggestOptions.MaxSolutions is 0.
	DefaultSuggestMaxSolutions = 100
)

// RoleCandidate describes a role that SuggestRoles is allowed to grant to the
// subject, together with the cost of granting it.
type RoleCandidate struct {
	Role string
	Cost int
}

// SuggestOptions tunes a SuggestRoles query.
type SuggestOptions struct {
	// MutexPairs lists pairs of roles that must never be effective at the same
	// time (separation of duties). A combination is rejected if both roles of a
	// pair are held by the subject after inheritance is taken into account.
	MutexPairs [][2]string
	// MaxNodes bounds how many role combinations are evaluated.
	// 0 means DefaultSuggestMaxNodes, a negative value means no limit.
	MaxNodes int
	// MaxSolutions caps how many optimal solutions are returned.
	// 0 means DefaultSuggestMaxSolutions, a negative value means no limit.
	MaxSolutions int
}

// SuggestStatus is the outcome kind of a SuggestRoles query.
type SuggestStatus int

const (
	// SuggestFound means at least one satisfying combination was found.
	SuggestFound SuggestStatus = iota
	// SuggestUnsatisfiable means the whole search space was explored and no
	// combination of the candidate roles satisfies all requests.
	SuggestUnsatisfiable
	// SuggestBudgetExhausted means the search budget ran out before the search
	// space was fully explored and nothing satisfying had been found yet.
	SuggestBudgetExhausted
)

func (s SuggestStatus) String() string {
	switch s {
	case SuggestFound:
		return "found"
	case SuggestUnsatisfiable:
		return "unsatisfiable"
	case SuggestBudgetExhausted:
		return "budget_exhausted"
	}
	return "unknown"
}

// RoleSuggestion is the result of a SuggestRoles query. It is advice only:
// SuggestRoles never modifies the enforcer it is called on.
type RoleSuggestion struct {
	Status SuggestStatus
	// Solutions holds the minimal-cost role combinations (only when Status is
	// SuggestFound). Several entries mean equally cheap alternatives exist.
	Solutions [][]string
	// Cost is the total cost of every returned solution.
	Cost int
	// Complete is false when the search budget was exhausted, in which case
	// the solutions are the best found so far and may not be minimal.
	Complete bool
	// NodesExplored reports how many combinations were evaluated.
	NodesExplored int
}

// SuggestRoles answers "what is the cheapest set of roles we still need to
// grant to subject so that all requests are allowed". It only picks roles from
// candidates, never removes or weakens any existing (deny) policy, and never
// modifies the receiver enforcer: every combination is evaluated on a private
// copy of the model, and each returned solution is re-verified on a fresh
// independent copy before being reported.
//
// Requests are full request tuples (e.g. {sub, obj, act}) whose subject is
// subject. Roles already held by the subject are treated as free and are never
// part of a solution.
func (e *Enforcer) SuggestRoles(subject string, requests [][]interface{}, candidates []RoleCandidate, opts *SuggestOptions) (*RoleSuggestion, error) {
	if subject == "" {
		return nil, errors.New("subject must not be empty")
	}
	options := SuggestOptions{}
	if opts != nil {
		options = *opts
	}
	maxNodes := options.MaxNodes
	if maxNodes == 0 {
		maxNodes = DefaultSuggestMaxNodes
	}
	maxSolutions := options.MaxSolutions
	if maxSolutions == 0 {
		maxSolutions = DefaultSuggestMaxSolutions
	}

	cands, err := normalizeCandidates(candidates)
	if err != nil {
		return nil, err
	}

	// Work on a private copy so the receiver's policy is never touched.
	scratch, err := e.scratchCopy()
	if err != nil {
		return nil, err
	}

	// Roles the subject already holds are always effective and cost nothing.
	held, err := scratch.GetImplicitRolesForUser(subject)
	if err != nil {
		return nil, err
	}
	heldSet := map[string]bool{subject: true}
	for _, role := range held {
		heldSet[role] = true
	}

	// Pre-compute the transitive role closure of every candidate so that
	// mutual-exclusion checks account for inherited roles.
	closures := make([]map[string]bool, len(cands))
	for i, cand := range cands {
		inherited, err := scratch.GetImplicitRolesForUser(cand.Role)
		if err != nil {
			return nil, err
		}
		closure := map[string]bool{cand.Role: true}
		for _, role := range inherited {
			closure[role] = true
		}
		closures[i] = closure
	}

	search := &roleSearch{
		scratch:      scratch,
		subject:      subject,
		requests:     requests,
		cands:        cands,
		closures:     closures,
		heldSet:      heldSet,
		mutexPairs:   options.MutexPairs,
		maxNodes:     maxNodes,
		maxSolutions: maxSolutions,
		bestCost:     -1,
	}
	if err := search.run(); err != nil {
		return nil, err
	}

	res := &RoleSuggestion{
		Complete:      !search.exhausted,
		NodesExplored: search.nodes,
	}
	switch {
	case len(search.solutions) > 0:
		res.Status = SuggestFound
		res.Cost = search.bestCost
		res.Solutions = search.solutions
	case search.exhausted:
		res.Status = SuggestBudgetExhausted
	default:
		res.Status = SuggestUnsatisfiable
	}

	// Re-verify every reported solution on a fresh independent copy of the
	// policy before handing it out.
	for _, solution := range res.Solutions {
		if err := e.verifySolution(subject, requests, solution, options.MutexPairs); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// scratchCopy builds a new enforcer from a deep copy of the receiver's model.
func (e *Enforcer) scratchCopy() (*Enforcer, error) {
	scratch, err := NewEnforcer(e.GetModel().Copy())
	if err != nil {
		return nil, err
	}
	// Reuse custom matcher functions registered on the original enforcer.
	scratch.fm = e.fm
	// The copied model already carries the policy, but with no adapter
	// LoadPolicy (which builds role links) never runs, so build them here.
	if err := scratch.BuildRoleLinks(); err != nil {
		return nil, err
	}
	return scratch, nil
}

// verifySolution applies a suggested combination to a fresh copy of the policy
// and checks that all requests become allowed and no mutual-exclusion pair is
// violated.
func (e *Enforcer) verifySolution(subject string, requests [][]interface{}, roles []string, mutexPairs [][2]string) error {
	scratch, err := e.scratchCopy()
	if err != nil {
		return err
	}
	for _, role := range roles {
		if _, err := scratch.AddGroupingPolicy(subject, role); err != nil {
			return err
		}
	}
	effective, err := scratch.GetImplicitRolesForUser(subject)
	if err != nil {
		return err
	}
	effectiveSet := map[string]bool{subject: true}
	for _, role := range effective {
		effectiveSet[role] = true
	}
	for _, pair := range mutexPairs {
		if effectiveSet[pair[0]] && effectiveSet[pair[1]] {
			return fmt.Errorf("suggestion %v violates mutual exclusion of %q and %q", roles, pair[0], pair[1])
		}
	}
	for _, req := range requests {
		ok, err := scratch.Enforce(req...)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("suggestion %v does not satisfy request %v", roles, req)
		}
	}
	return nil
}

func normalizeCandidates(candidates []RoleCandidate) ([]RoleCandidate, error) {
	seen := make(map[string]bool, len(candidates))
	cands := make([]RoleCandidate, 0, len(candidates))
	for _, cand := range candidates {
		if cand.Role == "" {
			return nil, errors.New("candidate role must not be empty")
		}
		if cand.Cost < 0 {
			return nil, fmt.Errorf("candidate role %q has negative cost", cand.Role)
		}
		if seen[cand.Role] {
			return nil, fmt.Errorf("duplicate candidate role %q", cand.Role)
		}
		seen[cand.Role] = true
		cands = append(cands, cand)
	}
	// Explore cheap roles first so good solutions are found early and the
	// branch-and-bound pruning becomes effective.
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Cost != cands[j].Cost {
			return cands[i].Cost < cands[j].Cost
		}
		return cands[i].Role < cands[j].Role
	})
	return cands, nil
}

// roleSearch is a branch-and-bound search over the subsets of candidate roles,
// minimizing (total cost, number of roles).
type roleSearch struct {
	scratch      *Enforcer
	subject      string
	requests     [][]interface{}
	cands        []RoleCandidate
	closures     []map[string]bool
	heldSet      map[string]bool
	mutexPairs   [][2]string
	maxNodes     int
	maxSolutions int

	nodes     int
	exhausted bool
	bestCost  int
	bestSize  int
	solutions [][]string
	err       error
}

func (s *roleSearch) run() error {
	effective := make(map[string]bool, len(s.heldSet))
	for role := range s.heldSet {
		effective[role] = true
	}
	s.dfs(0, nil, effective, 0)
	return s.err
}

func (s *roleSearch) dfs(idx int, chosen []int, effective map[string]bool, cost int) {
	if s.exhausted || s.err != nil {
		return
	}

	// Mutual exclusion is monotone: if the current set already violates it,
	// every superset does too, so the whole subtree can be skipped for free.
	if s.violatesMutex(effective) {
		return
	}

	if s.maxNodes >= 0 && s.nodes >= s.maxNodes {
		s.exhausted = true
		return
	}
	s.nodes++

	if s.satisfies(chosen) {
		s.record(chosen, cost)
	}

	for i := idx; i < len(s.cands); i++ {
		nextCost := cost + s.cands[i].Cost
		// Candidates are sorted by cost, so once one cannot improve on the
		// best known solution, none of the following ones can either.
		if s.bestCost >= 0 && nextCost > s.bestCost {
			break
		}
		if s.bestCost >= 0 && nextCost == s.bestCost && len(chosen)+1 > s.bestSize {
			continue
		}
		nextEffective := make(map[string]bool, len(effective)+len(s.closures[i]))
		for role := range effective {
			nextEffective[role] = true
		}
		for role := range s.closures[i] {
			nextEffective[role] = true
		}
		s.dfs(i+1, append(chosen, i), nextEffective, nextCost)
		if s.exhausted || s.err != nil {
			return
		}
	}
}

func (s *roleSearch) violatesMutex(effective map[string]bool) bool {
	for _, pair := range s.mutexPairs {
		if effective[pair[0]] && effective[pair[1]] {
			return true
		}
	}
	return false
}

// satisfies grants the chosen roles on the scratch enforcer, enforces every
// request, and then undoes the grants. Deny policies stay in place, so a
// request blocked by a deny rule can never be satisfied by adding roles.
func (s *roleSearch) satisfies(chosen []int) bool {
	granted := make([][]string, 0, len(chosen))
	for _, i := range chosen {
		ok, err := s.scratch.AddGroupingPolicy(s.subject, s.cands[i].Role)
		if err != nil {
			s.err = err
			return false
		}
		if ok {
			granted = append(granted, []string{s.subject, s.cands[i].Role})
		}
	}
	defer func() {
		if len(granted) > 0 {
			if _, err := s.scratch.RemoveGroupingPolicies(granted); err != nil && s.err == nil {
				s.err = err
			}
		}
	}()

	for _, req := range s.requests {
		ok, err := s.scratch.Enforce(req...)
		if err != nil {
			s.err = err
			return false
		}
		if !ok {
			return false
		}
	}
	return true
}

func (s *roleSearch) record(chosen []int, cost int) {
	size := len(chosen)
	if s.bestCost < 0 || cost < s.bestCost || (cost == s.bestCost && size < s.bestSize) {
		s.bestCost = cost
		s.bestSize = size
		s.solutions = [][]string{s.roleNames(chosen)}
		return
	}
	if cost == s.bestCost && size == s.bestSize && (s.maxSolutions < 0 || len(s.solutions) < s.maxSolutions) {
		s.solutions = append(s.solutions, s.roleNames(chosen))
	}
}

func (s *roleSearch) roleNames(chosen []int) []string {
	names := make([]string, len(chosen))
	for i, idx := range chosen {
		names[i] = s.cands[idx].Role
	}
	sort.Strings(names)
	return names
}
