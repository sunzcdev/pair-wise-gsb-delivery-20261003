package semver

import (
	"fmt"
	"sort"
	"strings"
)

// Dependency is a single requirement on a package: its name plus the
// semver constraint an acceptable version must satisfy. The constraint is
// evaluated with this package's usual semantics (prereleases are excluded
// unless the constraint contains one, build metadata is ignored).
type Dependency struct {
	Name       string
	Constraint *Constraints
}

// NewDependency builds a Dependency from a constraint string such as
// "^1.2.0", ">= 1.0, < 2.0" or "*".
func NewDependency(name, constraint string) (Dependency, error) {
	c, err := NewConstraint(constraint)
	if err != nil {
		return Dependency{}, fmt.Errorf("dependency %q: %w", name, err)
	}
	return Dependency{Name: name, Constraint: c}, nil
}

// MustDependency is NewDependency that panics on error. It is convenient
// for static fixtures and tests.
func MustDependency(name, constraint string) Dependency {
	d, err := NewDependency(name, constraint)
	if err != nil {
		panic(err)
	}
	return d
}

// PackageVersion is one candidate version of a package together with the
// dependencies that selecting it would impose.
type PackageVersion struct {
	Version      *Version
	Dependencies []Dependency
}

// Repository is a pure in-memory view of the packages available to the
// resolver. It only ever contains the finite version lists supplied by the
// caller; the resolver never reaches out to the network for more
// candidates.
type Repository struct {
	packages map[string][]PackageVersion
}

// NewRepository returns an empty in-memory repository.
func NewRepository() *Repository {
	return &Repository{packages: make(map[string][]PackageVersion)}
}

// Add registers one version of a package along with the dependencies that
// version imposes. Versions are kept sorted highest first so the resolver
// always tries higher versions before lower ones.
func (r *Repository) Add(name, version string, deps ...Dependency) error {
	v, err := NewVersion(version)
	if err != nil {
		return fmt.Errorf("repository: %s@%s: %w", name, version, err)
	}
	r.packages[name] = append(r.packages[name], PackageVersion{Version: v, Dependencies: deps})
	vs := r.packages[name]
	sort.SliceStable(vs, func(i, j int) bool { return vs[i].Version.GreaterThan(vs[j].Version) })
	return nil
}

// SolveStatus describes how a Solve call ended.
type SolveStatus int

const (
	// StatusSolved means a globally consistent combination was found.
	StatusSolved SolveStatus = iota
	// StatusUnsatisfiable means the search space was fully explored and no
	// combination satisfies every constraint.
	StatusUnsatisfiable
	// StatusExhausted means the step budget ran out before the search
	// finished, so "no solution yet" must not be read as "no solution
	// exists".
	StatusExhausted
)

func (s SolveStatus) String() string {
	switch s {
	case StatusSolved:
		return "solved"
	case StatusUnsatisfiable:
		return "unsatisfiable"
	case StatusExhausted:
		return "budget exhausted"
	}
	return "unknown"
}

// ConflictReason explains one requirement that could not be satisfied.
type ConflictReason struct {
	// Package is the package the Constraint applies to.
	Package string
	// Constraint is the textual form of the unmet constraint.
	Constraint string
	// RequiredBy is the chain of "name@version" selections, starting at
	// "root", that led to this requirement. It lets a caller retrace
	// exactly which path imposed the constraint.
	RequiredBy []string
}

// Conflict describes why a package could not be assigned any version. When
// the failure happened deeper in the search, Cause holds the nested
// conflict that ultimately forced the backtrack.
type Conflict struct {
	Package string
	Reasons []ConflictReason
	Cause   *Conflict
}

func (c *Conflict) String() string {
	var b strings.Builder
	writeConflict(&b, c, 0)
	return b.String()
}

func writeConflict(b *strings.Builder, c *Conflict, depth int) {
	indent := strings.Repeat("  ", depth)
	fmt.Fprintf(b, "%sno version of %q satisfies all requirements:\n", indent, c.Package)
	for _, r := range c.Reasons {
		fmt.Fprintf(b, "%s  - %s %s required by %s\n",
			indent, r.Package, r.Constraint, strings.Join(r.RequiredBy, " -> "))
	}
	if c.Cause != nil {
		fmt.Fprintf(b, "%s  because:\n", indent)
		writeConflict(b, c.Cause, depth+2)
	}
}

// Result is the outcome of a Solve call.
type Result struct {
	Status SolveStatus
	// Selected maps package name to the chosen version. It is only
	// populated when Status is StatusSolved.
	Selected map[string]*Version
	// Conflict carries the reviewable conflict chain when Status is
	// StatusUnsatisfiable.
	Conflict *Conflict
	// Steps is the number of candidate assignments attempted.
	Steps int
}

// SolveOptions tunes a Solve call.
type SolveOptions struct {
	// MaxSteps caps the number of candidate assignments attempted. When
	// the budget is spent the result status is StatusExhausted. A value
	// <= 0 means no limit.
	MaxSteps int
}

// Solve searches the in-memory repository for a combination of versions
// that satisfies the root dependencies and every transitive constraint.
//
// Packages are considered in ascending name order and, within a package,
// higher versions are tried before lower ones. When a choice leads to a
// conflict the solver backtracks and tries the next lower version, so a
// greedy-first choice that fails does not prevent a solution from being
// found. Cyclic dependencies are handled naturally: a dependency edge back
// to an already selected package is checked against the selection instead
// of being resolved again.
func Solve(repo *Repository, roots []Dependency, opts SolveOptions) *Result {
	s := &solver{
		repo:     repo,
		budget:   opts.MaxSteps,
		assigned: make(map[string]*Version),
		reqs:     make(map[string][]requirement),
	}
	for _, d := range roots {
		s.reqs[d.Name] = append(s.reqs[d.Name], requirement{dep: d})
	}

	conflict, exhausted := s.search()
	res := &Result{Steps: s.steps}
	switch {
	case exhausted:
		res.Status = StatusExhausted
	case conflict != nil:
		res.Status = StatusUnsatisfiable
		res.Conflict = conflict
	default:
		res.Status = StatusSolved
		res.Selected = make(map[string]*Version, len(s.assigned))
		for name, v := range s.assigned {
			res.Selected[name] = v
		}
	}
	return res
}

// requirement is a constraint on a package plus the selection chain that
// introduced it. A nil source means the requirement came from the caller
// (a root dependency).
type requirement struct {
	dep    Dependency
	source *sourceNode
}

// sourceNode records which selected package introduced a requirement, so
// conflict reports can show the full chain back to the root.
type sourceNode struct {
	sel    string // "name@version"
	parent *sourceNode
}

func (n *sourceNode) chain() []string {
	var out []string
	for cur := n; cur != nil; cur = cur.parent {
		out = append(out, cur.sel)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return append([]string{"root"}, out...)
}

type solver struct {
	repo     *Repository
	budget   int
	steps    int
	assigned map[string]*Version
	reqs     map[string][]requirement
}

// search returns (nil, false) when the current partial assignment extends
// to a full solution, (conflict, false) when the remaining space is proven
// unsatisfiable, and (_, true) when the step budget ran out.
func (s *solver) search() (*Conflict, bool) {
	name, ok := s.nextPackage()
	if !ok {
		return nil, false
	}

	candidates := s.repo.packages[name]
	conflict := &Conflict{Package: name}
	if len(candidates) == 0 {
		for _, r := range s.reqs[name] {
			conflict.Reasons = append(conflict.Reasons, ConflictReason{
				Package:    name,
				Constraint: r.dep.Constraint.String() + " (no versions available)",
				RequiredBy: chainOf(r.source),
			})
		}
		return conflict, false
	}

	var deepest *Conflict
	for _, cand := range candidates {
		if s.budget > 0 && s.steps >= s.budget {
			return conflict, true
		}
		s.steps++

		if reasons, bad := s.checkRequirements(name, cand.Version); bad {
			conflict.Reasons = append(conflict.Reasons, reasons...)
			continue
		}
		if reasons, bad := s.checkAssignedDeps(name, cand); bad {
			conflict.Reasons = append(conflict.Reasons, reasons...)
			continue
		}

		undo := s.apply(name, cand)
		child, exhausted := s.search()
		if exhausted {
			undo()
			return conflict, true
		}
		if child == nil {
			return nil, false
		}
		deepest = child
		undo()
	}
	if deepest != nil && len(conflict.Reasons) == 0 {
		return deepest, false
	}
	conflict.Cause = deepest
	return conflict, false
}

// nextPackage returns the lexicographically smallest package name that has
// requirements but no assignment yet.
func (s *solver) nextPackage() (string, bool) {
	best := ""
	found := false
	for name := range s.reqs {
		if _, done := s.assigned[name]; done {
			continue
		}
		if !found || name < best {
			best, found = name, true
		}
	}
	return best, found
}

// checkRequirements verifies a candidate against every constraint
// accumulated for its package, including constraints arriving from
// multiple dependency paths.
func (s *solver) checkRequirements(name string, v *Version) ([]ConflictReason, bool) {
	var reasons []ConflictReason
	for _, r := range s.reqs[name] {
		if !r.dep.Constraint.Check(v) {
			reasons = append(reasons, ConflictReason{
				Package:    name,
				Constraint: fmt.Sprintf("%s (rejected %s)", r.dep.Constraint.String(), v.String()),
				RequiredBy: chainOf(r.source),
			})
		}
	}
	return reasons, len(reasons) > 0
}

// checkAssignedDeps verifies the dependencies of a candidate against
// packages that are already selected. This is where dependency cycles
// back to an assigned package are validated.
func (s *solver) checkAssignedDeps(name string, cand PackageVersion) ([]ConflictReason, bool) {
	var reasons []ConflictReason
	node := s.nodeFor(name, cand.Version)
	for _, d := range cand.Dependencies {
		if v, done := s.assigned[d.Name]; done && !d.Constraint.Check(v) {
			reasons = append(reasons, ConflictReason{
				Package:    d.Name,
				Constraint: fmt.Sprintf("%s (already selected %s)", d.Constraint.String(), v.String()),
				RequiredBy: node.chain(),
			})
		}
	}
	return reasons, len(reasons) > 0
}

// apply tentatively selects a candidate and records its dependencies as
// new requirements. The returned function undoes the change.
func (s *solver) apply(name string, cand PackageVersion) func() {
	s.assigned[name] = cand.Version
	node := s.nodeFor(name, cand.Version)

	appended := make(map[string]int)
	for _, d := range cand.Dependencies {
		if _, done := s.assigned[d.Name]; done {
			continue // already validated by checkAssignedDeps
		}
		s.reqs[d.Name] = append(s.reqs[d.Name], requirement{dep: d, source: node})
		appended[d.Name]++
	}

	return func() {
		delete(s.assigned, name)
		for depName, n := range appended {
			s.reqs[depName] = s.reqs[depName][:len(s.reqs[depName])-n]
		}
	}
}

// nodeFor builds the source node for a selection, chained through the
// requirement that first introduced the package so conflict reports show
// the whole path from the root.
func (s *solver) nodeFor(name string, v *Version) *sourceNode {
	node := &sourceNode{sel: name + "@" + v.String()}
	if rs := s.reqs[name]; len(rs) > 0 {
		node.parent = rs[0].source
	}
	return node
}

func chainOf(n *sourceNode) []string {
	if n == nil {
		return []string{"root"}
	}
	return n.chain()
}
