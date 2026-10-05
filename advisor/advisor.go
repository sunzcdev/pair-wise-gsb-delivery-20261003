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

// Package advisor computes role-grant recommendations that make a set of
// denied requests pass. It only produces advice: it never mutates the
// enforcer used for the query, it never removes deny policies, and it only
// grants roles taken from the caller-provided candidate set.
package advisor

import (
	"fmt"
	"sort"

	casbin "github.com/casbin/casbin/v3"
)

// Status tells how the Suggest call terminated.
type Status int

const (
	// StatusFound means the search completed and at least one minimum-cost
	// solution is returned. An empty solution is possible when the subject
	// can already perform every requested action.
	StatusFound Status = iota
	// StatusUnsatisfiable means the search space was fully explored and no
	// combination of candidate roles can satisfy the requests without
	// either leaving a deny policy in effect or breaking a constraint.
	StatusUnsatisfiable
	// StatusBudgetExhausted means the evaluation budget ran out before the
	// search could prove unsatisfiability or enumerate every optimum. Any
	// advice returned with this status is best-effort and is not guaranteed
	// to be minimal or complete.
	StatusBudgetExhausted
)

// String renders a Status.
func (s Status) String() string {
	switch s {
	case StatusFound:
		return "found"
	case StatusUnsatisfiable:
		return "unsatisfiable"
	case StatusBudgetExhausted:
		return "budget exhausted"
	default:
		return "unknown"
	}
}

// Advice is a single proposed grant for the subject.
type Advice struct {
	// Subject is the user the roles should be granted to.
	Subject string
	// Domain is the optional domain for the grant. It is empty for models
	// without domains.
	Domain string
	// Roles are the roles to add via AddRoleForUser (or
	// AddRoleForUserInDomain when Domain is non-empty).
	Roles []string
	// Cost is the sum of the per-role costs.
	Cost int
}

// Options configures a Suggest call.
type Options struct {
	// Subject is the user the advice is computed for.
	Subject string
	// Domain optionally scopes every grant and every request to one
	// domain. Leave empty for models without domains.
	Domain string
	// Requests are the (obj, act, ...) tuples the subject must be allowed
	// to perform after the proposed grants. The subject and domain are
	// prepended automatically, so each tuple must not contain them.
	Requests [][]interface{}
	// CandidateRoles are the only roles the advisor is allowed to grant.
	CandidateRoles []string
	// RoleCost gives the non-negative cost of granting each candidate
	// role. Every candidate role must have an entry.
	RoleCost map[string]int
	// Budget bounds the number of candidate states the search is allowed
	// to evaluate. Zero selects the package default.
	Budget int
	// MaxSolutions caps how many minimum-cost solutions are returned.
	// Zero returns all minimum-cost solutions.
	MaxSolutions int
}

// Result is the outcome of Suggest.
type Result struct {
	// Status reports how the search terminated.
	Status Status
	// Advice holds the minimum-cost solutions when Status is StatusFound;
	// it holds best-effort solutions when Status is StatusBudgetExhausted
	// and some were found before the budget ran out.
	Advice []Advice
	// Evaluations is the number of candidate states actually evaluated.
	Evaluations int
}

// DefaultBudget is used when Options.Budget is not set.
const DefaultBudget = 10000

// candidate is a role the search may grant.
type candidate struct {
	role string
	cost int
}

// Suggest computes the minimum-cost set of roles that makes every request in
// opts.Requests pass for opts.Subject.
//
// The query is read-only with respect to e: all trial grants happen on a
// detached copy, deny policies are never removed, and roles outside
// opts.CandidateRoles are never granted. A budget-exhausted result is kept
// distinct from a proven-unsatisfiable result.
func Suggest(e *casbin.Enforcer, opts Options) (*Result, error) {
	if e == nil {
		return nil, fmt.Errorf("advisor: nil enforcer")
	}
	if opts.Subject == "" {
		return nil, fmt.Errorf("advisor: Options.Subject is required")
	}
	if len(opts.Requests) == 0 {
		return nil, fmt.Errorf("advisor: at least one request is required")
	}
	if len(opts.CandidateRoles) == 0 {
		return nil, fmt.Errorf("advisor: at least one candidate role is required")
	}
	if opts.RoleCost == nil {
		return nil, fmt.Errorf("advisor: Options.RoleCost is required")
	}

	candidates := make([]candidate, 0, len(opts.CandidateRoles))
	seen := make(map[string]bool, len(opts.CandidateRoles))
	for _, role := range opts.CandidateRoles {
		if role == "" {
			return nil, fmt.Errorf("advisor: candidate role names must not be empty")
		}
		if seen[role] {
			return nil, fmt.Errorf("advisor: duplicate candidate role %q", role)
		}
		seen[role] = true
		cost, ok := opts.RoleCost[role]
		if !ok {
			return nil, fmt.Errorf("advisor: no cost provided for candidate role %q", role)
		}
		if cost < 0 {
			return nil, fmt.Errorf("advisor: cost of role %q must be non-negative", role)
		}
		candidates = append(candidates, candidate{role: role, cost: cost})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].cost != candidates[j].cost {
			return candidates[i].cost < candidates[j].cost
		}
		return candidates[i].role < candidates[j].role
	})

	budget := opts.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}

	sandbox, err := cloneEnforcer(e)
	if err != nil {
		return nil, fmt.Errorf("advisor: %w", err)
	}

	// Roles the subject already holds (directly or through inheritance)
	// never need to be paid for or granted again.
	rm := sandbox.GetRoleManager()
	filtered := candidates[:0]
	for _, cand := range candidates {
		has, err := rm.HasLink(opts.Subject, cand.role, domainArgs(opts.Domain)...)
		if err != nil {
			return nil, fmt.Errorf("advisor: role lookup failed: %w", err)
		}
		if !has {
			filtered = append(filtered, cand)
		}
	}
	candidates = filtered

	search := &search{
		sandbox:      sandbox,
		subject:      opts.Subject,
		domain:       opts.Domain,
		requests:     opts.Requests,
		candidates:   candidates,
		budget:       budget,
		maxSolutions: opts.MaxSolutions,
	}
	for _, cand := range candidates {
		if cand.cost == 0 {
			search.hasFreeRole = true
			break
		}
	}

	return search.run()
}

func domainArgs(domain string) []string {
	if domain == "" {
		return nil
	}
	return []string{domain}
}
