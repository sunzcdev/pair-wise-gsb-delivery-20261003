package semver

import (
	"errors"
	"reflect"
	"testing"
)

func TestDiffConstraints(t *testing.T) {
	oldConstraint := ">=1.0.0 <2.0.0"
	newConstraint := ">=1.5.0 <3.0.0"
	versions := []string{
		"2.5.0",
		"2.0.0+build.2",
		"not-a-version",
		"1.0.0",
		"1.5.0",
		"3.0.0",
		"2.0.0+build.1",
		"1.0.0+old-build",
		"not-a-version",
	}

	diff, err := DiffConstraints(oldConstraint, newConstraint, versions)
	if err != nil {
		t.Fatalf("DiffConstraints returned error: %v", err)
	}

	expectedAdded := []VersionEquivalence{
		{
			Version:     "2.0.0",
			Inputs:      []string{"2.0.0+build.1", "2.0.0+build.2"},
			Occurrences: 2,
		},
		{
			Version:     "2.5.0",
			Inputs:      []string{"2.5.0"},
			Occurrences: 1,
		},
	}
	if !reflect.DeepEqual(diff.Added.Versions, expectedAdded) {
		t.Fatalf("unexpected added versions: %#v", diff.Added.Versions)
	}

	expectedRemoved := []VersionEquivalence{
		{
			Version:     "1.0.0",
			Inputs:      []string{"1.0.0", "1.0.0+old-build"},
			Occurrences: 2,
		},
	}
	if !reflect.DeepEqual(diff.Removed.Versions, expectedRemoved) {
		t.Fatalf("unexpected removed versions: %#v", diff.Removed.Versions)
	}

	if diff.Added.Witness == nil {
		t.Fatal("expected added witness")
	}
	if diff.Added.Witness.Version.Version != "2.0.0" {
		t.Fatalf("unexpected added witness: %#v", diff.Added.Witness)
	}
	if diff.Added.Witness.OldAllowed || !diff.Added.Witness.NewAllowed {
		t.Fatalf("unexpected added witness decisions: %#v", diff.Added.Witness)
	}

	if diff.Removed.Witness == nil {
		t.Fatal("expected removed witness")
	}
	if diff.Removed.Witness.Version.Version != "1.0.0" {
		t.Fatalf("unexpected removed witness: %#v", diff.Removed.Witness)
	}
	if !diff.Removed.Witness.OldAllowed || diff.Removed.Witness.NewAllowed {
		t.Fatalf("unexpected removed witness decisions: %#v", diff.Removed.Witness)
	}

	expectedInvalid := []InvalidVersion{
		{Input: "not-a-version", Occurrences: 2, Err: ErrInvalidSemVer},
	}
	if len(diff.InvalidVersions) != len(expectedInvalid) {
		t.Fatalf("unexpected invalid versions: %#v", diff.InvalidVersions)
	}
	for i, expected := range expectedInvalid {
		actual := diff.InvalidVersions[i]
		if actual.Input != expected.Input || actual.Occurrences != expected.Occurrences {
			t.Fatalf("unexpected invalid version at %d: %#v", i, actual)
		}
		if !errors.Is(actual.Err, expected.Err) {
			t.Fatalf("unexpected invalid version error at %d: %v", i, actual.Err)
		}
	}
}

func TestDiffConstraintsIsOrderIndependent(t *testing.T) {
	versions := []string{
		"2.5.0",
		"1.0.0+z",
		"bad",
		"2.0.0",
		"1.0.0+a",
	}

	first, err := DiffConstraints(">=1.0.0 <2.0.0", ">=1.5.0 <3.0.0", versions)
	if err != nil {
		t.Fatalf("DiffConstraints returned error: %v", err)
	}

	reversed := []string{
		"1.0.0+a",
		"2.0.0",
		"bad",
		"1.0.0+z",
		"2.5.0",
	}
	second, err := DiffConstraints(">=1.0.0 <2.0.0", ">=1.5.0 <3.0.0", reversed)
	if err != nil {
		t.Fatalf("DiffConstraints returned error: %v", err)
	}

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("diff changed with catalog order:\n%#v\n%#v", first, second)
	}
}

func TestDiffConstraintsEmptyDiffIsCatalogScoped(t *testing.T) {
	diff, err := DiffConstraints("<1.0.0", ">2.0.0", []string{"1.5.0", "bad"})
	if err != nil {
		t.Fatalf("DiffConstraints returned error: %v", err)
	}

	if len(diff.Added.Versions) != 0 || diff.Added.Witness != nil {
		t.Fatalf("expected no added versions, got %#v", diff.Added)
	}
	if len(diff.Removed.Versions) != 0 || diff.Removed.Witness != nil {
		t.Fatalf("expected no removed versions, got %#v", diff.Removed)
	}
	if len(diff.InvalidVersions) != 1 {
		t.Fatalf("expected invalid version to remain reported, got %#v", diff.InvalidVersions)
	}
}

func TestDiffConstraintsInvalidConstraint(t *testing.T) {
	if _, err := DiffConstraints("bad", "*", nil); err == nil {
		t.Fatal("expected old constraint error")
	}
	if _, err := DiffConstraints("*", "bad", nil); err == nil {
		t.Fatal("expected new constraint error")
	}
}

func TestDiffConstraintsWitnessUsesSemverPrecedence(t *testing.T) {
	diff, err := DiffConstraints("<1.0.0-alpha", "*-0", []string{"1.0.0", "1.0.0-alpha"})
	if err != nil {
		t.Fatalf("DiffConstraints returned error: %v", err)
	}

	if diff.Added.Witness == nil {
		t.Fatal("expected added witness")
	}
	if diff.Added.Witness.Version.Version != "1.0.0-alpha" {
		t.Fatalf("expected prerelease to be minimum witness, got %#v", diff.Added.Witness)
	}
}
