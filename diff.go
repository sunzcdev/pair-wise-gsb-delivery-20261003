package semver

import (
	"fmt"
	"sort"
)

// ConstraintDiff describes how two constraints differ on a fixed catalog of
// versions. Added and Removed contain only versions found in that catalog; an
// empty diff does not mean the constraints accept identical sets outside it.
type ConstraintDiff struct {
	Added           ConstraintChange
	Removed         ConstraintChange
	InvalidVersions []InvalidVersion
}

// ConstraintChange contains equivalence groups in a change set and, when the
// set is non-empty, its lowest-precedence witness.
type ConstraintChange struct {
	Versions []VersionEquivalence
	Witness  *ConstraintWitness
}

// VersionEquivalence groups catalog entries with equal semantic-version
// precedence. Build metadata is not part of the group key.
type VersionEquivalence struct {
	Version     string
	Inputs      []string
	Occurrences int
}

// ConstraintWitness identifies one equivalence group and records whether it
// matches each constraint.
type ConstraintWitness struct {
	Version    VersionEquivalence
	OldAllowed bool
	NewAllowed bool
}

// InvalidVersion reports catalog entries that could not be parsed. Repeated
// invalid entries are grouped by their exact input string.
type InvalidVersion struct {
	Input       string
	Occurrences int
	Err         error
}

// DiffConstraints compares oldConstraint and newConstraint using only the
// supplied version catalog. Versions with equal precedence, including versions
// that differ only in build metadata, form one stable equivalence group.
func DiffConstraints(oldConstraint, newConstraint string, versions []string) (*ConstraintDiff, error) {
	oldConstraints, err := NewConstraint(oldConstraint)
	if err != nil {
		return nil, fmt.Errorf("old constraint: %w", err)
	}

	newConstraints, err := NewConstraint(newConstraint)
	if err != nil {
		return nil, fmt.Errorf("new constraint: %w", err)
	}

	return DiffParsedConstraints(oldConstraints, newConstraints, versions), nil
}

// DiffParsedConstraints compares previously parsed constraints using only the
// supplied version catalog.
func DiffParsedConstraints(oldConstraints, newConstraints *Constraints, versions []string) *ConstraintDiff {
	groups := make(map[string]*versionEquivalenceGroup)
	invalid := make(map[string]*InvalidVersion)

	for _, input := range versions {
		version, err := NewVersion(input)
		if err != nil {
			item := invalid[input]
			if item == nil {
				item = &InvalidVersion{Input: input, Err: err}
				invalid[input] = item
			}
			item.Occurrences++
			continue
		}

		key := canonicalVersionKey(version)
		group := groups[key]
		if group == nil {
			group = &versionEquivalenceGroup{
				version: &Version{
					major:    version.major,
					minor:    version.minor,
					patch:    version.patch,
					pre:      version.pre,
					original: key,
				},
				inputs: make(map[string]struct{}),
			}
			groups[key] = group
		}

		group.Occurrences++
		group.inputs[input] = struct{}{}
	}

	groupList := make([]*versionEquivalenceGroup, 0, len(groups))
	for _, group := range groups {
		groupList = append(groupList, group)
	}
	sort.Slice(groupList, func(i, j int) bool {
		return groupList[i].version.Compare(groupList[j].version) < 0
	})

	diff := &ConstraintDiff{
		Added:           ConstraintChange{Versions: []VersionEquivalence{}},
		Removed:         ConstraintChange{Versions: []VersionEquivalence{}},
		InvalidVersions: make([]InvalidVersion, 0, len(invalid)),
	}

	for _, group := range groupList {
		oldAllowed := oldConstraints.Check(group.version)
		newAllowed := newConstraints.Check(group.version)
		if oldAllowed == newAllowed {
			continue
		}

		change := VersionEquivalence{
			Version:     group.version.String(),
			Inputs:      sortedInputs(group.inputs),
			Occurrences: group.Occurrences,
		}

		switch {
		case !oldAllowed && newAllowed:
			diff.Added.Versions = append(diff.Added.Versions, change)
		case oldAllowed && !newAllowed:
			diff.Removed.Versions = append(diff.Removed.Versions, change)
		}
	}

	diff.Added.Witness = firstWitness(diff.Added.Versions, false, true)
	diff.Removed.Witness = firstWitness(diff.Removed.Versions, true, false)

	for _, item := range invalid {
		diff.InvalidVersions = append(diff.InvalidVersions, *item)
	}
	sort.Slice(diff.InvalidVersions, func(i, j int) bool {
		return diff.InvalidVersions[i].Input < diff.InvalidVersions[j].Input
	})

	return diff
}

type versionEquivalenceGroup struct {
	version     *Version
	inputs      map[string]struct{}
	Occurrences int
}

func canonicalVersionKey(v *Version) string {
	if v.pre == "" {
		return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
	}
	return fmt.Sprintf("%d.%d.%d-%s", v.major, v.minor, v.patch, v.pre)
}

func sortedInputs(inputs map[string]struct{}) []string {
	result := make([]string, 0, len(inputs))
	for input := range inputs {
		result = append(result, input)
	}
	sort.Strings(result)
	return result
}

func firstWitness(versions []VersionEquivalence, oldAllowed, newAllowed bool) *ConstraintWitness {
	if len(versions) == 0 {
		return nil
	}
	return &ConstraintWitness{
		Version:    versions[0],
		OldAllowed: oldAllowed,
		NewAllowed: newAllowed,
	}
}
