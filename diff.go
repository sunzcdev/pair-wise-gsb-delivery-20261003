package semver

import (
	"fmt"
	"sort"
)

// CatalogScopeNote is attached to every DiffReport to make the scope of the
// conclusion explicit: a diff is computed against the supplied catalog only.
// In particular, empty Added and Removed sets mean the two constraints agree
// on the catalog entries. They do NOT mean the two constraints are equivalent
// over the (infinite) space of all possible versions.
const CatalogScopeNote = "results describe only the versions present in the supplied catalog; " +
	"empty added/removed sets do not imply the two constraints are equivalent over all possible versions"

// TermDecision records how a single AND term inside one OR branch of a
// constraint evaluated for a specific version.
type TermDecision struct {
	// Term is the individual constraint term, e.g. ">=1.2.0".
	Term string
	// Passed is true when the version satisfied this term.
	Passed bool
	// Reason explains a failure. It is empty when Passed is true.
	Reason string
}

// BranchDecision records how one OR branch (an AND group of terms) of a
// constraint evaluated for a specific version.
type BranchDecision struct {
	// Index is the zero-based position of the OR branch in the constraint.
	Index int
	// Terms holds the per-term outcomes in source order.
	Terms []TermDecision
	// Passed is true when every term in the branch passed.
	Passed bool
}

// ConstraintVerdict records how one constraint evaluated a specific version,
// branch by branch.
type ConstraintVerdict struct {
	// Constraint is the normalized constraint string.
	Constraint string
	// Allowed is the overall Check result.
	Allowed bool
	// Branches holds the per-branch (OR group) outcomes in source order.
	Branches []BranchDecision
}

// Witness is the minimal version of a diff category (added or removed)
// together with the branch-level decisions of both constraints for it.
// Minimal means the lowest version by semantic versioning precedence.
type Witness struct {
	// Version is the representative catalog entry for the group, chosen as
	// the lexicographically smallest original string in the group.
	Version string
	// Key is the canonical precedence key of the group (metadata excluded).
	Key string
	// Old is the verdict of the old constraint for this version.
	Old ConstraintVerdict
	// New is the verdict of the new constraint for this version.
	New ConstraintVerdict
}

// VersionGroup is a set of catalog entries that are equivalent by semantic
// versioning precedence: identical major, minor, patch and prerelease.
// Build metadata is ignored for precedence per the semver spec, so entries
// differing only in metadata (and exact duplicate strings) share a group.
type VersionGroup struct {
	// Key is the canonical precedence key, e.g. "1.2.3" or "1.2.3-rc.1".
	Key string
	// Versions holds the distinct original catalog strings in the group,
	// sorted lexicographically.
	Versions []string
	// Count is the total number of catalog occurrences in the group,
	// including duplicate strings.
	Count int

	version *Version
}

// CatalogError reports a catalog entry that could not be parsed. Entries
// that fail to parse are never silently dropped; they are surfaced here.
type CatalogError struct {
	// Input is the unparseable catalog string.
	Input string
	// Err is the parse error message.
	Err string
	// Count is the number of times this exact string occurred in the catalog.
	Count int
}

// DiffReport is the result of comparing an old and a new constraint against
// a version catalog. All slices are deterministically ordered and do not
// depend on the input order of the catalog.
type DiffReport struct {
	// OldConstraint and NewConstraint are the normalized constraint strings.
	OldConstraint string
	NewConstraint string

	// Added holds the groups rejected by the old constraint but allowed by
	// the new one, sorted by ascending precedence.
	Added []VersionGroup
	// Removed holds the groups allowed by the old constraint but rejected by
	// the new one, sorted by ascending precedence.
	Removed []VersionGroup

	// KeptCount is the number of groups allowed by both constraints.
	KeptCount int
	// ExcludedCount is the number of groups rejected by both constraints.
	ExcludedCount int

	// AddedWitness is the minimal added group with branch decisions, or nil
	// when Added is empty.
	AddedWitness *Witness
	// RemovedWitness is the minimal removed group with branch decisions, or
	// nil when Removed is empty.
	RemovedWitness *Witness

	// Errors holds the catalog entries that failed to parse, sorted by the
	// input string.
	Errors []CatalogError

	// Scope is always CatalogScopeNote.
	Scope string
}

// DiffConstraints compares an old and a new constraint against a catalog of
// version strings and reports which catalog versions would be newly admitted
// and which would lose eligibility when replacing the old constraint with
// the new one.
//
// Catalog entries are parsed with NewVersion. Entries that fail to parse are
// reported in the report's Errors field and never silently dropped. Entries
// equivalent by precedence (duplicates and metadata-only variants) are
// grouped with a stable rule, and every slice in the report is sorted so the
// output does not depend on the catalog order.
//
// The conclusion is scoped to the supplied catalog: empty Added and Removed
// sets must not be read as the two constraints being equivalent over all
// possible versions.
//
// An error is returned only when one of the two constraints fails to parse.
func DiffConstraints(oldRaw, newRaw string, catalog []string) (*DiffReport, error) {
	oldC, err := NewConstraint(oldRaw)
	if err != nil {
		return nil, fmt.Errorf("old constraint: %w", err)
	}
	newC, err := NewConstraint(newRaw)
	if err != nil {
		return nil, fmt.Errorf("new constraint: %w", err)
	}

	groups := make(map[string]*VersionGroup)
	errs := make(map[string]*CatalogError)

	for _, raw := range catalog {
		v, perr := NewVersion(raw)
		if perr != nil {
			ce, ok := errs[raw]
			if !ok {
				ce = &CatalogError{Input: raw, Err: perr.Error()}
				errs[raw] = ce
			}
			ce.Count++
			continue
		}

		key := precedenceKey(v)
		g, ok := groups[key]
		if !ok {
			g = &VersionGroup{Key: key, version: v}
			groups[key] = g
		}
		g.Count++
		found := false
		for _, existing := range g.Versions {
			if existing == raw {
				found = true
				break
			}
		}
		if !found {
			g.Versions = append(g.Versions, raw)
		}
	}

	report := &DiffReport{
		OldConstraint: oldC.String(),
		NewConstraint: newC.String(),
		Scope:         CatalogScopeNote,
	}

	for _, g := range groups {
		sort.Strings(g.Versions)
		oldOK := oldC.Check(g.version)
		newOK := newC.Check(g.version)
		switch {
		case !oldOK && newOK:
			report.Added = append(report.Added, *g)
		case oldOK && !newOK:
			report.Removed = append(report.Removed, *g)
		case oldOK:
			report.KeptCount++
		default:
			report.ExcludedCount++
		}
	}

	sortGroups(report.Added)
	sortGroups(report.Removed)

	if len(report.Added) > 0 {
		report.AddedWitness = buildWitness(&report.Added[0], oldC, newC)
	}
	if len(report.Removed) > 0 {
		report.RemovedWitness = buildWitness(&report.Removed[0], oldC, newC)
	}

	for _, ce := range errs {
		report.Errors = append(report.Errors, *ce)
	}
	sort.Slice(report.Errors, func(i, j int) bool {
		return report.Errors[i].Input < report.Errors[j].Input
	})

	return report, nil
}

// precedenceKey returns the canonical grouping key for a version. Build
// metadata is excluded because it does not take part in precedence per the
// semver spec.
func precedenceKey(v *Version) string {
	if v.pre != "" {
		return fmt.Sprintf("%d.%d.%d-%s", v.major, v.minor, v.patch, v.pre)
	}
	return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
}

// sortGroups orders groups by ascending precedence. Keys are unique across
// groups, so the ordering is total and deterministic.
func sortGroups(groups []VersionGroup) {
	sort.Slice(groups, func(i, j int) bool {
		if c := groups[i].version.Compare(groups[j].version); c != 0 {
			return c < 0
		}
		return groups[i].Key < groups[j].Key
	})
}

// buildWitness creates the witness for a group, evaluating its representative
// version against both constraints with branch-level detail.
func buildWitness(g *VersionGroup, oldC, newC *Constraints) *Witness {
	return &Witness{
		Version: g.Versions[0],
		Key:     g.Key,
		Old:     oldC.explain(g.version),
		New:     newC.explain(g.version),
	}
}

// explain evaluates a version against the constraint and records the outcome
// of every OR branch and every AND term within it.
func (cs *Constraints) explain(v *Version) ConstraintVerdict {
	verdict := ConstraintVerdict{
		Constraint: cs.String(),
		Branches:   make([]BranchDecision, len(cs.constraints)),
	}

	for i, o := range cs.constraints {
		branch := BranchDecision{Index: i, Passed: true}
		for _, c := range o {
			term := TermDecision{Term: c.string(), Passed: true}
			switch {
			case !cs.IncludePrerelease && !cs.containsPre[i] && v.pre != "":
				term.Passed = false
				term.Reason = fmt.Sprintf("%q is a prerelease version and the constraint is only looking for release versions", v)
			default:
				if _, err := c.check(v, cs.IncludePrerelease || cs.containsPre[i]); err != nil {
					term.Passed = false
					term.Reason = err.Error()
				}
			}
			if !term.Passed {
				branch.Passed = false
			}
			branch.Terms = append(branch.Terms, term)
		}
		if branch.Passed {
			verdict.Allowed = true
		}
		verdict.Branches[i] = branch
	}

	return verdict
}
