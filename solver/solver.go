// Package solver provides a pure in-memory, backtracking dependency solver.
//
// The solver works against a fixed Catalog: every package exposes only the
// finite list of versions handed to it by the caller. It never fetches
// additional candidates or reaches the network.
//
// Selection is deterministic: packages are decided in package-name order and,
// within a package, the highest version that satisfies every requirement seen
// so far is tried first. When a choice leads to a dead end the solver
// backtracks and tries the next candidate, which lets it find solutions that a
// purely greedy pass would miss.
//
// Version comparison and constraint evaluation reuse the host semver library,
// so build metadata is ignored for precedence and prerelease handling follows
// the library semantics (see semver.Constraints.IncludePrerelease).
package solver

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// Requirement is a semver constraint placed on a package.
type Requirement struct {
	// Package is the target package name.
	Package string

	// Constraint is a semver constraint string as understood by
	// semver.NewConstraint (for example "^1.2.0", ">=1.0.0 <2.0.0", "=1.2.3").
	Constraint string

	// Source identifies who imposes the requirement. The empty string denotes
	// a root requirement. Otherwise it is the "package@version" of the
	// selected version that declares the dependency.
	Source string
}

// Dependency is a single outgoing edge declared by a package version.
type Dependency struct {
	// Package is the required package name.
	Package string

	// Constraint is the semver constraint string applied to Package.
	Constraint string
}

// Candidate is one available version of a package.
type Candidate struct {
	// Version is the semantic version string. It is returned verbatim in the
	// Result, so original spelling (for example a "v" prefix or build
	// metadata) is preserved.
	Version string

	// Deps are the requirements this candidate declares. Cycles are allowed.
	Deps []Dependency
}

// Catalog is the immutable, finite universe of package versions.
type Catalog map[string][]Candidate

// Options tunes a Solve run.
type Options struct {
	// IncludePrerelease is passed to every parsed constraint. When false (the
	// default) prerelease candidates only match a constraint group that itself
	// mentions a prerelease.
	IncludePrerelease bool

	// MaxTrials bounds the number of candidate versions the solver is allowed
	// to attempt. Zero means unlimited. When the budget is consumed before the
	// search is exhausted the result status is Exhausted; a fully explored
	// search that finds no answer is Unsatisfiable.
	MaxTrials int

	// GreedyOnly disables backtracking. The first candidate that satisfies the
	// currently visible requirements is committed; if it later proves fatal
	// that failure is reported without trying lower versions. It exists for
	// tests and diagnostics.
	GreedyOnly bool
}

// Status classifies the outcome of a Solve run.
type Status int

const (
	// Solved means a globally consistent version assignment was found.
	Solved Status = iota
	// Unsatisfiable means the complete candidate space was searched and no
	// consistent assignment exists.
	Unsatisfiable
	// Exhausted means the trial budget ran out before the search space was
	// fully explored; the presence or absence of a solution is still unknown.
	Exhausted
)

func (s Status) String() string {
	switch s {
	case Solved:
		return "solved"
	case Unsatisfiable:
		return "unsatisfiable"
	case Exhausted:
		return "exhausted"
	default:
		return fmt.Sprintf("status(%d)", int(s))
	}
}

// Conflict is a reviewable explanation of why a package could not be
// assigned. It forms a tree: each entry in Tried describes a rejected
// candidate and, recursively, why trying that candidate failed.
type Conflict struct {
	// Package is the package that had no viable candidate.
	Package string

	// Requirements are all constraints imposed on Package at the failure
	// point, tagged with their source.
	Requirements []Requirement

	// Tried lists the candidates that were attempted, highest version first,
	// with the reason each one failed.
	Tried []RejectedCandidate

	// Reason summarizes the immediate cause when there are no candidates to
	// try.
	Reason string
}

// RejectedCandidate pairs a tried version with the reason it failed.
type RejectedCandidate struct {
	// Version is the candidate version string.
	Version string

	// RejectedBy is the requirement the candidate does not satisfy. It is nil
	// when the candidate met every requirement but choosing it led to the
	// downstream conflict in Because.
	RejectedBy *Requirement

	// Because is the downstream conflict produced after choosing this
	// candidate. It is nil when RejectedBy is set.
	Because *Conflict
}

// Error renders a human-readable, indented conflict chain.
func (c *Conflict) Error() string {
	return c.render(0)
}

func (c *Conflict) render(depth int) string {
	pad := strings.Repeat("  ", depth)
	var b strings.Builder
	fmt.Fprintf(&b, "%spackage %q has no usable version", pad, c.Package)
	switch {
	case len(c.Requirements) > 0:
		b.WriteString("; requirements:\n")
		for _, r := range c.Requirements {
			if r.Source == "" {
				fmt.Fprintf(&b, "%s  - root requires %s %s\n", pad, r.Package, r.Constraint)
			} else {
				fmt.Fprintf(&b, "%s  - %s requires %s %s\n", pad, r.Source, r.Package, r.Constraint)
			}
		}
		if c.Reason != "" {
			fmt.Fprintf(&b, "%s  %s\n", pad, c.Reason)
		}
	case c.Reason != "":
		fmt.Fprintf(&b, ": %s\n", c.Reason)
	}
	for _, t := range c.Tried {
		if t.RejectedBy != nil {
			fmt.Fprintf(&b, "%s  - %s rejected by %s (from %s)\n",
				pad, t.Version, t.RejectedBy.Constraint,
				sourceLabel(t.RejectedBy.Source))
			continue
		}
		fmt.Fprintf(&b, "%s  - %s leads to:\n%s\n", pad, t.Version, t.Because.render(depth+2))
	}
	return strings.TrimRight(b.String(), "\n")
}

func sourceLabel(source string) string {
	if source == "" {
		return "root"
	}
	return source
}

// Result is the outcome of a Solve run.
type Result struct {
	// Status is Solved, Unsatisfiable, or Exhausted.
	Status Status

	// Selection maps every decided package name to the selected original
	// version string. It is non-nil even for failed runs and then holds the
	// last partial assignment, which is useful when Status is Exhausted.
	Selection map[string]string

	// Conflict is the failure explanation. It is non-nil exactly when Status
	// is Unsatisfiable or Exhausted.
	Conflict *Conflict

	// Trials is how many candidate versions were actually attempted.
	Trials int

	// BudgetExhausted is true when the configured MaxTrials was consumed
	// before every remaining candidate could be attempted. In that case
	// Status is Exhausted and the search has not proven unsatisfiability.
	BudgetExhausted bool
}

type engine struct {
	catalog  Catalog
	opts     Options
	sorted   map[string][]Candidate
	versions map[string]map[string]*semver.Version
	parsed   map[string]*semver.Constraints
	trials   int
}

// Solve finds a globally consistent selection for roots within catalog.
// It is pure and in-memory: candidates come exclusively from catalog.
func Solve(roots []Requirement, catalog Catalog, opts Options) (*Result, error) {
	e := &engine{
		catalog:  catalog,
		opts:     opts,
		sorted:   make(map[string][]Candidate),
		versions: make(map[string]map[string]*semver.Version),
		parsed:   make(map[string]*semver.Constraints),
	}

	initial := &state{
		chosen:  make(map[string]string),
		reqs:    make(map[string][]Requirement),
		pending: make(map[string]struct{}),
	}
	for _, r := range roots {
		if r.Package == "" {
			return nil, fmt.Errorf("solver: root requirement has empty package name")
		}
		if _, err := e.constraint(r.Constraint); err != nil {
			return nil, fmt.Errorf("solver: root requirement on %q has invalid constraint %q: %w",
				r.Package, r.Constraint, err)
		}
		e.addRequirement(initial, r)
	}
	if err := e.validateCatalog(); err != nil {
		return nil, err
	}

	conflict, winner, frontier, exhausted := e.dfs(initial)
	if winner != nil {
		return &Result{
			Status:    Solved,
			Selection: cloneSelection(winner.chosen),
			Trials:    e.trials,
		}, nil
	}
	status := Unsatisfiable
	partial := initial.chosen
	if frontier != nil {
		partial = frontier.chosen
	}
	if exhausted {
		status = Exhausted
	}
	return &Result{
		Status:          status,
		Selection:       cloneSelection(partial),
		Conflict:        conflict,
		Trials:          e.trials,
		BudgetExhausted: exhausted,
	}, nil
}

func cloneSelection(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// state is the search frontier. It is copied before every mutation so
// competing backtracking branches stay isolated.
type state struct {
	// chosen maps decided package name to the original candidate version.
	chosen map[string]string
	// reqs maps package name to the accumulated requirements on it.
	reqs map[string][]Requirement
	// pending holds package names that are required but not yet chosen.
	pending map[string]struct{}
}

func (s *state) clone() *state {
	n := &state{
		chosen:  make(map[string]string, len(s.chosen)),
		reqs:    make(map[string][]Requirement, len(s.reqs)),
		pending: make(map[string]struct{}, len(s.pending)),
	}
	for k, v := range s.chosen {
		n.chosen[k] = v
	}
	for k, v := range s.reqs {
		n.reqs[k] = append([]Requirement(nil), v...)
	}
	for k := range s.pending {
		n.pending[k] = struct{}{}
	}
	return n
}

// validateCatalog parses every version and dependency constraint up front so
// malformed inputs surface as errors instead of being tangled into a search.
func (e *engine) validateCatalog() error {
	pkgs := make([]string, 0, len(e.catalog))
	for pkg := range e.catalog {
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)
	for _, pkg := range pkgs {
		list, err := e.candidates(pkg)
		if err != nil {
			return err
		}
		for _, cand := range list {
			for _, dep := range cand.Deps {
				if dep.Package == "" {
					return fmt.Errorf("solver: %s@%s declares dependency with empty package name", pkg, cand.Version)
				}
				if _, err := e.constraint(dep.Constraint); err != nil {
					return fmt.Errorf("solver: %s@%s has invalid constraint %q on %s: %w",
						pkg, cand.Version, dep.Constraint, dep.Package, err)
				}
			}
		}
	}
	return nil
}

// addRequirement records one requirement and marks the target pending unless
// it is already chosen. Identical requirements are de-duplicated.
func (e *engine) addRequirement(st *state, r Requirement) {
	for _, existing := range st.reqs[r.Package] {
		if existing == r {
			return
		}
	}
	st.reqs[r.Package] = append(st.reqs[r.Package], r)
	if _, ok := st.chosen[r.Package]; !ok {
		st.pending[r.Package] = struct{}{}
	}
}

func (e *engine) constraint(raw string) (*semver.Constraints, error) {
	if c, ok := e.parsed[raw]; ok {
		return c, nil
	}
	c, err := semver.NewConstraint(raw)
	if err != nil {
		return nil, err
	}
	c.IncludePrerelease = e.opts.IncludePrerelease
	e.parsed[raw] = c
	return c, nil
}

func (e *engine) version(pkg, raw string) (*semver.Version, error) {
	if m, ok := e.versions[pkg]; ok {
		if v, ok := m[raw]; ok {
			return v, nil
		}
	}
	v, err := semver.NewVersion(raw)
	if err != nil {
		return nil, fmt.Errorf("solver: catalog package %q has invalid version %q: %w", pkg, raw, err)
	}
	if e.versions[pkg] == nil {
		e.versions[pkg] = make(map[string]*semver.Version)
	}
	e.versions[pkg][raw] = v
	return v, nil
}

// candidates returns a package's versions sorted from highest to lowest
// precedence. Input order is preserved for equal precedence (for example equal
// core versions that differ only by build metadata).
func (e *engine) candidates(pkg string) ([]Candidate, error) {
	if list, ok := e.sorted[pkg]; ok {
		return list, nil
	}
	list := append([]Candidate(nil), e.catalog[pkg]...)
	type parsed struct {
		c Candidate
		v *semver.Version
		i int
	}
	parsedList := make([]parsed, len(list))
	for i, c := range list {
		v, err := e.version(pkg, c.Version)
		if err != nil {
			return nil, err
		}
		parsedList[i] = parsed{c: c, v: v, i: i}
	}
	sort.SliceStable(parsedList, func(i, j int) bool {
		if cmp := parsedList[i].v.Compare(parsedList[j].v); cmp != 0 {
			return cmp > 0
		}
		return parsedList[i].i < parsedList[j].i
	})
	out := make([]Candidate, len(parsedList))
	for i, p := range parsedList {
		out[i] = p.c
	}
	e.sorted[pkg] = out
	return out, nil
}

func nextPackage(st *state) string {
	first := ""
	for pkg := range st.pending {
		if first == "" || pkg < first {
			first = pkg
		}
	}
	return first
}

func (e *engine) satisfied(r Requirement, v *semver.Version) bool {
	c, err := e.constraint(r.Constraint)
	if err != nil {
		return false
	}
	return c.Check(v)
}

// mustVersion resolves a catalog version during search. All catalog versions
// are parsed by validateCatalog before searching, so this cannot fail.
func (e *engine) mustVersion(pkg, raw string) *semver.Version {
	v, err := e.version(pkg, raw)
	if err != nil {
		panic(err)
	}
	return v
}

// dfs searches one decision node. The values are:
//   - conflict: the full reason when this node fails;
//   - winner: the completed state when a solution was found;
//   - frontier: the deepest reached state when the budget was exhausted;
//   - exhausted: true if the budget ran out while this subtree was open.
func (e *engine) dfs(st *state) (*Conflict, *state, *state, bool) {
	if len(st.pending) == 0 {
		return nil, st, nil, false
	}
	pkg := nextPackage(st)
	reqs := st.reqs[pkg]

	list, err := e.candidates(pkg)
	if err != nil {
		// Unreachable after validateCatalog, but keep the signature defensive.
		return &Conflict{Package: pkg, Reason: err.Error()}, nil, nil, false
	}

	tree := &Conflict{Package: pkg, Requirements: append([]Requirement(nil), reqs...)}

	for i := range list {
		cand := list[i]
		v := e.mustVersion(pkg, cand.Version)

		var blocker *Requirement
		for ri := range reqs {
			if !e.satisfied(reqs[ri], v) {
				r := reqs[ri]
				blocker = &r
				break
			}
		}
		if blocker != nil {
			tree.Tried = append(tree.Tried, RejectedCandidate{
				Version:    cand.Version,
				RejectedBy: blocker,
			})
			if e.opts.GreedyOnly {
				return tree, nil, nil, false
			}
			continue
		}

		if e.opts.MaxTrials > 0 && e.trials >= e.opts.MaxTrials {
			tree.Reason = fmt.Sprintf(
				"trial budget (%d) exhausted before %s@%s could be attempted",
				e.opts.MaxTrials, pkg, cand.Version)
			return tree, nil, st, true
		}
		e.trials++

		child := st.clone()
		delete(child.pending, pkg)
		child.chosen[pkg] = cand.Version

		source := pkg + "@" + cand.Version
		for _, dep := range cand.Deps {
			r := Requirement{Package: dep.Package, Constraint: dep.Constraint, Source: source}
			e.addRequirement(child, r)
		}
		// A new requirement may land on a package that an earlier decision on
		// this branch already selected. Decisions are never mutated in place
		// (that would orphan dependencies of the old version); instead the
		// branch fails and backtracking revisits the earlier decision. This is
		// also what makes dependency cycles terminate.
		if bad := e.checkChosen(child); bad != nil {
			tree.Tried = append(tree.Tried, RejectedCandidate{
				Version: cand.Version,
				Because: bad,
			})
			if e.opts.GreedyOnly {
				return tree, nil, nil, false
			}
			continue
		}

		sub, winner, subFrontier, exhausted := e.dfs(child)
		if winner != nil {
			return nil, winner, nil, false
		}
		tree.Tried = append(tree.Tried, RejectedCandidate{
			Version: cand.Version,
			Because: sub,
		})
		if exhausted {
			return sub, nil, subFrontier, true
		}
		if e.opts.GreedyOnly {
			return tree, nil, nil, false
		}
	}

	if len(list) == 0 {
		if len(e.catalog[pkg]) == 0 {
			tree.Reason = "no versions are listed for this package in the catalog"
		}
	}
	return tree, nil, nil, false
}

// checkChosen verifies that every already selected package still satisfies all
// requirements accumulated on the current branch. It returns a Conflict for
// the first violation (package-name order for determinism), or nil.
func (e *engine) checkChosen(st *state) *Conflict {
	names := make([]string, 0, len(st.chosen))
	for name := range st.chosen {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := e.mustVersion(name, st.chosen[name])
		reqs := st.reqs[name]
		for _, r := range reqs {
			if e.satisfied(r, v) {
				continue
			}
			return &Conflict{
				Package:      name,
				Requirements: append([]Requirement(nil), reqs...),
				Reason: fmt.Sprintf(
					"selected version %s is inconsistent with %s required by %s",
					st.chosen[name], r.Constraint, sourceLabel(r.Source)),
			}
		}
	}
	return nil
}
