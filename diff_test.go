package semver

import (
	"reflect"
	"testing"
)

func TestDiffConstraintsBasic(t *testing.T) {
	catalog := []string{"1.0.0", "1.4.9", "1.5.0", "1.9.0", "2.0.0", "0.9.9"}
	report, err := DiffConstraints(">=1.0.0 <2.0.0", ">=1.5.0 <2.0.0", catalog)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var addedKeys, removedKeys []string
	for _, g := range report.Added {
		addedKeys = append(addedKeys, g.Key)
	}
	for _, g := range report.Removed {
		removedKeys = append(removedKeys, g.Key)
	}

	if len(addedKeys) != 0 {
		t.Errorf("added keys = %v, want none", addedKeys)
	}
	if want := []string{"1.0.0", "1.4.9"}; !reflect.DeepEqual(removedKeys, want) {
		t.Errorf("removed keys = %v, want %v", removedKeys, want)
	}
	if report.KeptCount != 2 { // 1.5.0 and 1.9.0
		t.Errorf("kept = %d, want 2", report.KeptCount)
	}
	if report.ExcludedCount != 2 { // 2.0.0 and 0.9.9
		t.Errorf("excluded = %d, want 2", report.ExcludedCount)
	}
	if len(report.Errors) != 0 {
		t.Errorf("errors = %v, want none", report.Errors)
	}
	if report.Scope != CatalogScopeNote {
		t.Errorf("scope note missing")
	}
}

func TestDiffConstraintsGrouping(t *testing.T) {
	// Duplicates and metadata-only variants must collapse into one group.
	catalog := []string{
		"1.2.3",
		"1.2.3+build.1",
		"1.2.3+build.2",
		"1.2.3",
		"v1.2.3",
	}
	report, err := DiffConstraints("<1.0.0", ">=1.0.0", catalog)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(report.Added) != 1 {
		t.Fatalf("added groups = %d, want 1", len(report.Added))
	}
	g := report.Added[0]
	if g.Key != "1.2.3" {
		t.Errorf("key = %q, want %q", g.Key, "1.2.3")
	}
	if g.Count != 5 {
		t.Errorf("count = %d, want 5", g.Count)
	}
	wantVersions := []string{"1.2.3", "1.2.3+build.1", "1.2.3+build.2", "v1.2.3"}
	if !reflect.DeepEqual(g.Versions, wantVersions) {
		t.Errorf("versions = %v, want %v", g.Versions, wantVersions)
	}
}

func TestDiffConstraintsErrorsReported(t *testing.T) {
	catalog := []string{"1.0.0", "not-a-version", "1.2.3.4", "not-a-version", ""}
	report, err := DiffConstraints(">=1.0.0", ">=2.0.0", catalog)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(report.Errors) != 3 {
		t.Fatalf("errors = %d, want 3: %v", len(report.Errors), report.Errors)
	}
	// Errors are sorted by input string; "" sorts first.
	if report.Errors[0].Input != "" || report.Errors[1].Input != "1.2.3.4" || report.Errors[2].Input != "not-a-version" {
		t.Errorf("errors not sorted as expected: %v", report.Errors)
	}
	if report.Errors[2].Count != 2 {
		t.Errorf("duplicate error count = %d, want 2", report.Errors[2].Count)
	}
	for _, ce := range report.Errors {
		if ce.Err == "" {
			t.Errorf("error for %q has empty message", ce.Input)
		}
	}
}

func TestDiffConstraintsOrderIndependent(t *testing.T) {
	catalog := []string{
		"2.0.0", "not-a-version", "1.2.3+meta", "1.2.3", "0.1.0",
		"1.5.0-rc.1", "1.2.3", "3.0.0", "1.2.3+other",
	}
	base, err := DiffConstraints(">=1.0.0 <2.0.0", ">=1.2.0 <3.0.0", catalog)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Reverse and rotate the catalog; the report must be identical.
	reversed := make([]string, len(catalog))
	for i, s := range catalog {
		reversed[len(catalog)-1-i] = s
	}
	rotated := append(append([]string{}, catalog[3:]...), catalog[:3]...)

	for name, c := range map[string][]string{"reversed": reversed, "rotated": rotated} {
		got, err := DiffConstraints(">=1.0.0 <2.0.0", ">=1.2.0 <3.0.0", c)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !reflect.DeepEqual(base, got) {
			t.Errorf("%s catalog produced a different report\nbase: %+v\ngot:  %+v", name, base, got)
		}
	}
}

func TestDiffConstraintsWitnessBranches(t *testing.T) {
	catalog := []string{"1.5.0", "0.9.0"}
	report, err := DiffConstraints(">=0.1.0", ">=1.5.0", catalog)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if report.AddedWitness != nil {
		t.Errorf("added witness should be nil, got %+v", report.AddedWitness)
	}
	w := report.RemovedWitness
	if w == nil {
		t.Fatalf("removed witness is nil")
	}
	if w.Version != "0.9.0" || w.Key != "0.9.0" {
		t.Errorf("witness = %q (%q), want 0.9.0", w.Version, w.Key)
	}

	// 0.9.0 is allowed by the old constraint and rejected by the new one.
	if !w.Old.Allowed {
		t.Errorf("old verdict should allow 0.9.0")
	}
	if w.New.Allowed {
		t.Errorf("new verdict should reject 0.9.0")
	}
	if len(w.New.Branches) != 1 || w.New.Branches[0].Passed {
		t.Errorf("new branches = %+v, want single failed branch", w.New.Branches)
	}
}

func TestDiffConstraintsWitnessBranchDetail(t *testing.T) {
	catalog := []string{"1.5.0"}
	report, err := DiffConstraints(">=1.0.0 <1.5.0 || >=2.0.0", ">=1.0.0", catalog)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	w := report.AddedWitness
	if w == nil {
		t.Fatalf("added witness is nil")
	}
	if w.Old.Allowed {
		t.Errorf("old verdict should reject 1.5.0")
	}
	if !w.New.Allowed {
		t.Errorf("new verdict should allow 1.5.0")
	}

	if len(w.Old.Branches) != 2 {
		t.Fatalf("old branches = %d, want 2", len(w.Old.Branches))
	}
	b0 := w.Old.Branches[0]
	if b0.Passed {
		t.Errorf("branch 0 should fail for 1.5.0")
	}
	if len(b0.Terms) != 2 {
		t.Fatalf("branch 0 terms = %d, want 2", len(b0.Terms))
	}
	if !b0.Terms[0].Passed || b0.Terms[0].Term != ">=1.0.0" {
		t.Errorf("term 0 = %+v, want passed >=1.0.0", b0.Terms[0])
	}
	if b0.Terms[1].Passed || b0.Terms[1].Term != "<1.5.0" || b0.Terms[1].Reason == "" {
		t.Errorf("term 1 = %+v, want failed <1.5.0 with reason", b0.Terms[1])
	}
	b1 := w.Old.Branches[1]
	if b1.Index != 1 || b1.Passed {
		t.Errorf("branch 1 = %+v, want failed", b1)
	}

	if len(w.New.Branches) != 1 || !w.New.Branches[0].Passed {
		t.Errorf("new branches = %+v, want single passing branch", w.New.Branches)
	}
}

func TestDiffConstraintsWitnessPrereleaseBranch(t *testing.T) {
	catalog := []string{"1.5.0-rc.1"}
	report, err := DiffConstraints(">=1.0.0", ">=1.0.0-0", catalog)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	w := report.AddedWitness
	if w == nil {
		t.Fatalf("added witness is nil")
	}
	if w.Old.Allowed {
		t.Errorf("old constraint should reject the prerelease")
	}
	if len(w.Old.Branches) != 1 || len(w.Old.Branches[0].Terms) != 1 {
		t.Fatalf("unexpected old branches: %+v", w.Old.Branches)
	}
	term := w.Old.Branches[0].Terms[0]
	if term.Passed || term.Reason == "" {
		t.Errorf("prerelease term = %+v, want failed with reason", term)
	}
	if !w.New.Allowed {
		t.Errorf("new constraint should allow the prerelease")
	}
}

func TestDiffConstraintsEmptyDiffIsCatalogScoped(t *testing.T) {
	// The two constraints are not equivalent in general (e.g. 9.9.9
	// distinguishes them) but they agree on this catalog. The report must
	// not claim equivalence; it only carries the catalog scope note.
	catalog := []string{"1.0.0", "2.0.0"}
	report, err := DiffConstraints(">=1.0.0 <5.0.0", ">=1.0.0 <=5.0.0", catalog)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(report.Added) != 0 || len(report.Removed) != 0 {
		t.Fatalf("expected empty diff, got added=%v removed=%v", report.Added, report.Removed)
	}
	if report.AddedWitness != nil || report.RemovedWitness != nil {
		t.Errorf("witnesses should be nil for an empty diff")
	}
	if report.Scope != CatalogScopeNote {
		t.Errorf("report must carry the catalog scope note")
	}
	if report.KeptCount != 2 {
		t.Errorf("kept = %d, want 2", report.KeptCount)
	}
}

func TestDiffConstraintsInvalidConstraint(t *testing.T) {
	if _, err := DiffConstraints("not a constraint!!", ">=1.0.0", nil); err == nil {
		t.Errorf("expected error for invalid old constraint")
	}
	if _, err := DiffConstraints(">=1.0.0", "not a constraint!!", nil); err == nil {
		t.Errorf("expected error for invalid new constraint")
	}
}
