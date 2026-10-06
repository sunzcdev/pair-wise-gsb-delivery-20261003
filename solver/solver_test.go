package solver

import (
	"reflect"
	"strings"
	"testing"
)

func mustRequirement(pkg, con, src string) Requirement {
	return Requirement{Package: pkg, Constraint: con, Source: src}
}

// greedyFailsCatalog: taking the highest a and the highest b forces c@2, but
// the root pins c to the 1.x line. Falling back to a@1.0.0 (then b@1.0.0)
// satisfies everything. A purely greedy pass that never backtracks fails.
func greedyFailsCatalog() Catalog {
	return Catalog{
		"a": {
			{Version: "2.0.0", Deps: []Dependency{{Package: "b", Constraint: "^2.0.0"}}},
			{Version: "1.0.0", Deps: []Dependency{{Package: "b", Constraint: "^1.0.0"}}},
		},
		"b": {
			{Version: "2.0.0", Deps: []Dependency{{Package: "c", Constraint: "^2.0.0"}}},
			{Version: "1.0.0", Deps: []Dependency{{Package: "c", Constraint: "^1.0.0"}}},
		},
		"c": {
			{Version: "2.0.0"},
			{Version: "1.0.0"},
		},
	}
}

func TestGreedyFailsButBacktrackingSolves(t *testing.T) {
	roots := []Requirement{
		mustRequirement("a", "*", ""),
		mustRequirement("c", "^1.0.0", ""),
	}
	cat := greedyFailsCatalog()

	greedy, err := Solve(roots, cat, Options{GreedyOnly: true})
	if err != nil {
		t.Fatalf("greedy solve returned error: %v", err)
	}
	if greedy.Status != Unsatisfiable {
		t.Fatalf("greedy-only: expected Unsatisfiable, got %s", greedy.Status)
	}
	if greedy.Conflict == nil || !strings.Contains(greedy.Conflict.Error(), `"c"`) {
		t.Fatalf("greedy-only: expected conflict chain mentioning c, got:\n%v", greedy.Conflict)
	}
	if !strings.Contains(greedy.Conflict.Error(), "2.0.0") {
		t.Fatalf("greedy-only: conflict chain should show the rejected c@2.0.0, got:\n%v", greedy.Conflict)
	}

	res, err := Solve(roots, cat, Options{})
	if err != nil {
		t.Fatalf("solve returned error: %v", err)
	}
	if res.Status != Solved {
		t.Fatalf("expected Solved, got %s: %v", res.Status, res.Conflict)
	}
	want := map[string]string{"a": "1.0.0", "b": "1.0.0", "c": "1.0.0"}
	if len(res.Selection) != len(want) {
		t.Fatalf("expected %d selected, got %d (%v)", len(want), len(res.Selection), res.Selection)
	}
	for pkg, ver := range want {
		if got := res.Selection[pkg]; got != ver {
			t.Errorf("package %q: want %s, got %s", pkg, ver, got)
		}
	}
	if res.Trials < 3 {
		t.Errorf("expected backtracking to attempt several candidates, trials=%d", res.Trials)
	}
}

func TestPackageNameOrderingPrefersHigherVersions(t *testing.T) {
	cat := Catalog{
		"zeta": {
			{Version: "1.5.0"},
			{Version: "1.4.0"},
			{Version: "2.0.0"},
		},
		"alpha": {
			{Version: "0.9.0"},
			{Version: "1.0.0"},
		},
	}
	res, err := Solve([]Requirement{
		mustRequirement("zeta", ">=1.0.0", ""),
		mustRequirement("alpha", "*", ""),
	}, cat, Options{})
	if err != nil {
		t.Fatalf("solve error: %v", err)
	}
	if res.Status != Solved {
		t.Fatalf("expected Solved, got %s: %v", res.Status, res.Conflict)
	}
	if res.Selection["alpha"] != "1.0.0" {
		t.Errorf("alpha: want 1.0.0, got %s", res.Selection["alpha"])
	}
	if res.Selection["zeta"] != "2.0.0" {
		t.Errorf("zeta: want 2.0.0, got %s", res.Selection["zeta"])
	}
}

// cyclicUnsatisfiableCatalog: a@1.0.0 needs b>=1.0.0 and b@1.0.0 needs
// a<1.0.0. The cycle itself is fine; the version constraints are not.
func cyclicUnsatisfiableCatalog() Catalog {
	return Catalog{
		"a": {
			{Version: "1.0.0", Deps: []Dependency{{Package: "b", Constraint: ">=1.0.0"}}},
		},
		"b": {
			{Version: "1.0.0", Deps: []Dependency{{Package: "a", Constraint: "<1.0.0"}}},
		},
	}
}

func TestCyclicDependencyUnsatisfiable(t *testing.T) {
	res, err := Solve([]Requirement{mustRequirement("a", "*", "")},
		cyclicUnsatisfiableCatalog(), Options{})
	if err != nil {
		t.Fatalf("solve error: %v", err)
	}
	if res.Status != Unsatisfiable {
		t.Fatalf("expected Unsatisfiable, got %s", res.Status)
	}
	if res.BudgetExhausted {
		t.Fatalf("finite cycle must not report budget exhaustion")
	}
	msg := res.Conflict.Error()
	if !strings.Contains(msg, `"a"`) || !strings.Contains(msg, `"b"`) {
		t.Fatalf("conflict chain should walk the a<->b cycle, got:\n%s", msg)
	}
	if !strings.Contains(msg, "<1.0.0") {
		t.Fatalf("conflict chain should name the offending constraint, got:\n%s", msg)
	}
	if !strings.Contains(msg, "b@1.0.0") {
		t.Fatalf("conflict chain should identify the source b@1.0.0, got:\n%s", msg)
	}
	t.Logf("conflict chain:\n%s", msg)
}

func TestSatisfiableCycle(t *testing.T) {
	cat := Catalog{
		"a": {
			{Version: "1.0.0", Deps: []Dependency{{Package: "b", Constraint: "^1.0.0"}}},
		},
		"b": {
			{Version: "1.0.0", Deps: []Dependency{{Package: "a", Constraint: "^1.0.0"}}},
		},
	}
	res, err := Solve([]Requirement{mustRequirement("a", "*", "")}, cat, Options{})
	if err != nil {
		t.Fatalf("solve error: %v", err)
	}
	if res.Status != Solved {
		t.Fatalf("expected Solved for a benign cycle, got %s: %v", res.Status, res.Conflict)
	}
	if res.Selection["a"] != "1.0.0" || res.Selection["b"] != "1.0.0" {
		t.Fatalf("unexpected selection: %v", res.Selection)
	}
}

func TestPrereleaseHandling(t *testing.T) {
	cat := Catalog{
		"p": {
			{Version: "2.0.0-alpha.1"},
			{Version: "1.0.0"},
		},
	}

	// By default a release-only constraint skips the prerelease.
	res, err := Solve([]Requirement{mustRequirement("p", "^1.0.0", "")}, cat, Options{})
	if err != nil {
		t.Fatalf("solve error: %v", err)
	}
	if res.Status != Solved || res.Selection["p"] != "1.0.0" {
		t.Fatalf("default: want 1.0.0 release, got status=%s sel=%v", res.Status, res.Selection)
	}

	// An explicit prerelease constraint selects the prerelease.
	res, err = Solve([]Requirement{mustRequirement("p", ">=2.0.0-alpha.1", "")}, cat, Options{})
	if err != nil {
		t.Fatalf("solve error: %v", err)
	}
	if res.Status != Solved || res.Selection["p"] != "2.0.0-alpha.1" {
		t.Fatalf("explicit pre: want 2.0.0-alpha.1, got status=%s sel=%v", res.Status, res.Selection)
	}

	// Opting in globally also allows the prerelease candidate.
	res, err = Solve([]Requirement{mustRequirement("p", ">=1.0.0", "")}, cat, Options{IncludePrerelease: true})
	if err != nil {
		t.Fatalf("solve error: %v", err)
	}
	if res.Status != Solved || res.Selection["p"] != "2.0.0-alpha.1" {
		t.Fatalf("includePre: want 2.0.0-alpha.1, got status=%s sel=%v", res.Status, res.Selection)
	}
}

func TestBuildMetadataUsesLibraryPrecedence(t *testing.T) {
	cat := Catalog{
		"m": {
			{Version: "1.0.0+abc"},
			{Version: "1.0.0+xyz"},
		},
	}
	// Equal precedence per semver; input order is retained, and equality
	// constraint matches regardless of metadata.
	res, err := Solve([]Requirement{mustRequirement("m", "=1.0.0", "")}, cat, Options{})
	if err != nil {
		t.Fatalf("solve error: %v", err)
	}
	if res.Status != Solved || res.Selection["m"] != "1.0.0+abc" {
		t.Fatalf("build metadata: want first equal candidate 1.0.0+abc, got status=%s sel=%v",
			res.Status, res.Selection)
	}
}

func TestMultiPathConstraintsMerge(t *testing.T) {
	cat := Catalog{
		"x": {
			{Version: "1.2.0", Deps: []Dependency{{Package: "z", Constraint: ">=1.0.0"}}},
			{Version: "1.1.0", Deps: []Dependency{{Package: "z", Constraint: ">=1.0.0"}}},
		},
		"y": {
			{Version: "1.0.0", Deps: []Dependency{{Package: "z", Constraint: "<1.5.0"}}},
		},
		"z": {
			{Version: "2.0.0"},
			{Version: "1.4.0"},
			{Version: "0.9.0"},
		},
	}
	res, err := Solve([]Requirement{
		mustRequirement("x", "*", ""),
		mustRequirement("y", "*", ""),
	}, cat, Options{})
	if err != nil {
		t.Fatalf("solve error: %v", err)
	}
	if res.Status != Solved {
		t.Fatalf("expected Solved merging both paths, got %s: %v", res.Status, res.Conflict)
	}
	if res.Selection["z"] != "1.4.0" {
		t.Fatalf("z must satisfy >=1.0.0 and <1.5.0, got %s", res.Selection["z"])
	}
}

func TestBudgetDistinguishesExhaustedFromUnsatisfiable(t *testing.T) {
	res, err := Solve([]Requirement{
		mustRequirement("a", "*", ""),
		mustRequirement("c", "^1.0.0", ""),
	}, greedyFailsCatalog(), Options{MaxTrials: 1})
	if err != nil {
		t.Fatalf("solve error: %v", err)
	}
	if res.Status != Exhausted {
		t.Fatalf("with one trial expected Exhausted, got %s", res.Status)
	}
	if !res.BudgetExhausted {
		t.Fatalf("BudgetExhausted flag should be set")
	}
	if res.Trials != 1 {
		t.Fatalf("expected exactly 1 trial, got %d", res.Trials)
	}
	if len(res.Selection) == 0 {
		t.Fatalf("exhausted result should retain the partial assignment")
	}
	if res.Conflict == nil || !strings.Contains(res.Conflict.Error(), "budget") {
		t.Fatalf("exhausted conflict should mention the budget, got: %v", res.Conflict)
	}

	// The same instance with an unlimited budget proves true unsatisfiability
	// is reported distinctly.
	res, err = Solve([]Requirement{mustRequirement("a", "*", "")},
		cyclicUnsatisfiableCatalog(), Options{})
	if err != nil {
		t.Fatalf("solve error: %v", err)
	}
	if res.Status != Unsatisfiable || res.BudgetExhausted {
		t.Fatalf("expected Unsatisfiable without budget flag, got %s flag=%v",
			res.Status, res.BudgetExhausted)
	}
}

func TestMissingPackageReportsReviewableConflict(t *testing.T) {
	cat := Catalog{
		"a": {
			{Version: "1.0.0", Deps: []Dependency{{Package: "ghost", Constraint: "*"}}},
		},
	}
	res, err := Solve([]Requirement{mustRequirement("a", "*", "")}, cat, Options{})
	if err != nil {
		t.Fatalf("solve error: %v", err)
	}
	if res.Status != Unsatisfiable {
		t.Fatalf("expected Unsatisfiable for unknown package, got %s", res.Status)
	}
	if !strings.Contains(res.Conflict.Error(), "ghost") ||
		!strings.Contains(res.Conflict.Error(), "no versions are listed") {
		t.Fatalf("conflict should explain the missing catalog entry, got:\n%v", res.Conflict)
	}
}

func TestDeterministicSelection(t *testing.T) {
	first, err := Solve([]Requirement{
		mustRequirement("a", "*", ""),
		mustRequirement("c", "^1.0.0", ""),
	}, greedyFailsCatalog(), Options{})
	if err != nil {
		t.Fatalf("first solve error: %v", err)
	}
	for i := 0; i < 5; i++ {
		next, err := Solve([]Requirement{
			mustRequirement("a", "*", ""),
			mustRequirement("c", "^1.0.0", ""),
		}, greedyFailsCatalog(), Options{})
		if err != nil {
			t.Fatalf("repeat solve error: %v", err)
		}
		if !reflect.DeepEqual(next.Selection, first.Selection) {
			t.Fatalf("non-deterministic selection: %v vs %v", next.Selection, first.Selection)
		}
	}
}

func TestInvalidInputsReturnErrors(t *testing.T) {
	if _, err := Solve([]Requirement{mustRequirement("a", "~notaversion", "")},
		Catalog{}, Options{}); err == nil {
		t.Fatalf("expected error for invalid root constraint")
	}
	bad := Catalog{"a": {{Version: "not-a-version"}}}
	if _, err := Solve([]Requirement{mustRequirement("a", "*", "")}, bad, Options{}); err == nil {
		t.Fatalf("expected error for invalid catalog version")
	}
}
