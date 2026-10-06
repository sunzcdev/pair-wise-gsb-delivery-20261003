package semver

import (
	"strings"
	"testing"
)

func mustAdd(t *testing.T, r *Repository, name, version string, deps ...Dependency) {
	t.Helper()
	if err := r.Add(name, version, deps...); err != nil {
		t.Fatalf("add %s@%s: %v", name, version, err)
	}
}

// backtrackFixture builds the classic diamond where the greedy "highest
// version first" choice fails and only backtracking finds the solution:
//
//	root -> a, root -> b
//	a@2.0.0 -> c =1.0.0      (greedy pick for a)
//	a@1.0.0 -> c >=1.0.0
//	b@1.0.0 -> c >=2.0.0
//	c: 1.0.0, 2.0.0
//
// Greedy picks a@2.0.0, then b@1.0.0, and no c can satisfy both =1.0.0 and
// >=2.0.0. Backtracking to a@1.0.0 yields a@1.0.0, b@1.0.0, c@2.0.0.
func backtrackFixture(t *testing.T) *Repository {
	t.Helper()
	r := NewRepository()
	mustAdd(t, r, "a", "2.0.0", MustDependency("c", "=1.0.0"))
	mustAdd(t, r, "a", "1.0.0", MustDependency("c", ">=1.0.0"))
	mustAdd(t, r, "b", "1.0.0", MustDependency("c", ">=2.0.0"))
	mustAdd(t, r, "c", "1.0.0")
	mustAdd(t, r, "c", "2.0.0")
	return r
}

func backtrackRoots() []Dependency {
	return []Dependency{
		MustDependency("a", "*"),
		MustDependency("b", "*"),
	}
}

func TestSolveGreedyFailsBacktrackSucceeds(t *testing.T) {
	repo := backtrackFixture(t)

	res := Solve(repo, backtrackRoots(), SolveOptions{})
	if res.Status != StatusSolved {
		t.Fatalf("expected solved, got %s (conflict: %v)", res.Status, res.Conflict)
	}

	want := map[string]string{"a": "1.0.0", "b": "1.0.0", "c": "2.0.0"}
	for name, wantV := range want {
		v, ok := res.Selected[name]
		if !ok {
			t.Fatalf("package %q missing from solution", name)
		}
		if v.String() != wantV {
			t.Errorf("package %q: got %s, want %s", name, v, wantV)
		}
	}

	// A pure greedy run would need exactly 3 attempts (a, b, c). The
	// backtracking run must have tried (and undone) more candidates.
	if res.Steps <= 3 {
		t.Errorf("expected backtracking to cost more than 3 steps, got %d", res.Steps)
	}
}

func TestSolveCyclicUnsatisfiable(t *testing.T) {
	// a and b depend on each other, but the only available versions are
	// mutually incompatible: a@1.0.0 needs b >=2.0.0 while b@2.0.0 needs
	// a >=2.0.0, which does not exist.
	repo := NewRepository()
	mustAdd(t, repo, "a", "1.0.0", MustDependency("b", ">=2.0.0"))
	mustAdd(t, repo, "b", "2.0.0", MustDependency("a", ">=2.0.0"))

	res := Solve(repo, []Dependency{MustDependency("a", "*")}, SolveOptions{})
	if res.Status != StatusUnsatisfiable {
		t.Fatalf("expected unsatisfiable, got %s", res.Status)
	}
	if res.Conflict == nil {
		t.Fatal("expected a reviewable conflict")
	}

	// The conflict chain must let a caller retrace the cycle: b@2.0.0
	// demanded a >=2.0.0 while a@1.0.0 was selected.
	msg := res.Conflict.String()
	for _, want := range []string{`"b"`, `a >=2.0.0`, "root -> a@1.0.0 -> b@2.0.0"} {
		if !strings.Contains(msg, want) {
			t.Errorf("conflict report missing %q:\n%s", want, msg)
		}
	}
}

func TestSolveCyclicSolvable(t *testing.T) {
	// A compatible cycle must resolve: a@1.0.0 <-> b@1.0.0.
	repo := NewRepository()
	mustAdd(t, repo, "a", "1.0.0", MustDependency("b", "^1.0.0"))
	mustAdd(t, repo, "b", "1.0.0", MustDependency("a", "^1.0.0"))

	res := Solve(repo, []Dependency{MustDependency("a", "*")}, SolveOptions{})
	if res.Status != StatusSolved {
		t.Fatalf("expected solved, got %s (conflict: %v)", res.Status, res.Conflict)
	}
	if res.Selected["a"].String() != "1.0.0" || res.Selected["b"].String() != "1.0.0" {
		t.Errorf("unexpected selection: %v", res.Selected)
	}
}

func TestSolveBudgetExhaustedVsUnsatisfiable(t *testing.T) {
	// The backtracking fixture needs 7 attempts; a budget of 3 must report
	// "not fully searched" rather than "no solution".
	res := Solve(backtrackFixture(t), backtrackRoots(), SolveOptions{MaxSteps: 3})
	if res.Status != StatusExhausted {
		t.Fatalf("expected budget exhausted, got %s", res.Status)
	}

	// With enough budget the same input is provably solvable...
	res = Solve(backtrackFixture(t), backtrackRoots(), SolveOptions{MaxSteps: 100})
	if res.Status != StatusSolved {
		t.Fatalf("expected solved with ample budget, got %s", res.Status)
	}

	// ...and a genuinely impossible input is provably unsatisfiable even
	// with a huge budget, never confused with exhaustion.
	repo := NewRepository()
	mustAdd(t, repo, "a", "1.0.0", MustDependency("b", ">=2.0.0"))
	mustAdd(t, repo, "b", "1.0.0")
	res = Solve(repo, []Dependency{MustDependency("a", "*")}, SolveOptions{MaxSteps: 1000})
	if res.Status != StatusUnsatisfiable {
		t.Fatalf("expected unsatisfiable, got %s", res.Status)
	}
}

func TestSolvePrereleaseHandling(t *testing.T) {
	// Library semantics: a constraint without a prerelease part must not
	// match prerelease versions.
	repo := NewRepository()
	mustAdd(t, repo, "c", "2.0.0-beta.1")
	mustAdd(t, repo, "c", "1.5.0")

	res := Solve(repo, []Dependency{MustDependency("c", ">=1.0.0")}, SolveOptions{})
	if res.Status != StatusSolved || res.Selected["c"].String() != "1.5.0" {
		t.Fatalf("expected 1.5.0 (prerelease excluded), got %s %v", res.Status, res.Selected)
	}

	// Opting prereleases in with "-0" makes the higher prerelease win.
	res = Solve(repo, []Dependency{MustDependency("c", ">=1.0.0-0")}, SolveOptions{})
	if res.Status != StatusSolved || res.Selected["c"].String() != "2.0.0-beta.1" {
		t.Fatalf("expected 2.0.0-beta.1 (prerelease included), got %s %v", res.Status, res.Selected)
	}
}

func TestSolveBuildMetadataSemantics(t *testing.T) {
	// Library semantics ignore build metadata when comparing, so =1.2.0
	// is satisfied by 1.2.0+build.5 and the metadata survives in the
	// selected version.
	repo := NewRepository()
	mustAdd(t, repo, "a", "1.2.0+build.5")

	res := Solve(repo, []Dependency{MustDependency("a", "=1.2.0")}, SolveOptions{})
	if res.Status != StatusSolved {
		t.Fatalf("expected solved, got %s (conflict: %v)", res.Status, res.Conflict)
	}
	if got := res.Selected["a"].Metadata(); got != "build.5" {
		t.Errorf("expected metadata build.5, got %q", got)
	}
}

func TestSolveMultiplePathsConflictChain(t *testing.T) {
	// c is constrained through two different paths and no single version
	// satisfies both. The conflict must name both paths.
	repo := NewRepository()
	mustAdd(t, repo, "a", "2.0.0", MustDependency("c", "<2.0.0"))
	mustAdd(t, repo, "b", "1.0.0", MustDependency("c", ">=2.0.0"))
	mustAdd(t, repo, "c", "1.0.0")
	mustAdd(t, repo, "c", "2.0.0")

	res := Solve(repo, backtrackRoots(), SolveOptions{})
	if res.Status != StatusUnsatisfiable {
		t.Fatalf("expected unsatisfiable, got %s", res.Status)
	}
	if res.Conflict == nil || res.Conflict.Package != "c" {
		t.Fatalf("expected conflict on c, got %+v", res.Conflict)
	}
	if len(res.Conflict.Reasons) != 2 {
		t.Fatalf("expected 2 conflict reasons, got %d", len(res.Conflict.Reasons))
	}

	chains := map[string]bool{}
	for _, r := range res.Conflict.Reasons {
		chains[strings.Join(r.RequiredBy, " -> ")] = true
	}
	for _, want := range []string{"root -> a@2.0.0", "root -> b@1.0.0"} {
		if !chains[want] {
			t.Errorf("missing conflict chain %q in:\n%s", want, res.Conflict)
		}
	}
}

func TestSolveMissingPackage(t *testing.T) {
	res := Solve(NewRepository(), []Dependency{MustDependency("ghost", "^1.0.0")}, SolveOptions{})
	if res.Status != StatusUnsatisfiable {
		t.Fatalf("expected unsatisfiable, got %s", res.Status)
	}
	if !strings.Contains(res.Conflict.String(), "no versions available") {
		t.Errorf("expected 'no versions available' in conflict:\n%s", res.Conflict)
	}
}
