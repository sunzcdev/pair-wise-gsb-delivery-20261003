package semver

import (
	"errors"
	"fmt"
)

// PolicySpec describes a composite publish policy. Allow and Deny hold
// constraint ranges compared with semantic version precedence, while Ban
// holds exact versions matched on the full original version identity,
// including build metadata. IncludePrerelease controls whether prerelease
// versions are considered when evaluating the Allow and Deny ranges, the
// same way it does on Constraints.
type PolicySpec struct {
	Allow             []string
	Deny              []string
	Ban               []string
	IncludePrerelease bool
}

// Policy is a compiled, immutable composite policy. It is safe for
// concurrent use and its evaluation does not depend on any package-level
// switches.
type Policy struct {
	allow []*Constraints
	deny  []*Constraints
	// ban maps the canonical form (including build metadata) of a banned
	// version to the index of the ban rule that introduced it.
	ban map[string]int
}

// RuleKind identifies which rule list produced a Decision.
type RuleKind int

const (
	// RuleNone means no rule matched; the version was rejected because it
	// is not covered by any allow rule.
	RuleNone RuleKind = iota
	// RuleAllow means an allow rule matched.
	RuleAllow
	// RuleDeny means a deny (exclusion) rule matched.
	RuleDeny
	// RuleBan means an exact ban rule matched.
	RuleBan
)

// Decision is the outcome of evaluating a Policy against a version. Index
// is the position of the rule within its list (Allow, Deny, or Ban) that
// determined the decision. It is -1 when Rule is RuleNone.
type Decision struct {
	Allowed bool
	Rule    RuleKind
	Index   int
}

// String returns a human readable reason for the decision, naming the
// exact rule that triggered it.
func (d Decision) String() string {
	switch d.Rule {
	case RuleAllow:
		return fmt.Sprintf("allowed by allow rule %d", d.Index)
	case RuleDeny:
		return fmt.Sprintf("denied by exclude rule %d", d.Index)
	case RuleBan:
		return fmt.Sprintf("denied by ban rule %d", d.Index)
	default:
		return "denied: no allow rule matched"
	}
}

// CompilePolicy compiles a PolicySpec into a Policy. All configuration
// problems are reported at once as a joined error; each entry names the
// rule list and index it came from.
func CompilePolicy(spec PolicySpec) (*Policy, error) {
	var errs []error

	allow := make([]*Constraints, len(spec.Allow))
	for i, s := range spec.Allow {
		c, err := NewConstraint(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("allow rule %d: %w", i, err))
			continue
		}
		c.IncludePrerelease = spec.IncludePrerelease
		allow[i] = c
	}

	deny := make([]*Constraints, len(spec.Deny))
	for i, s := range spec.Deny {
		c, err := NewConstraint(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("deny rule %d: %w", i, err))
			continue
		}
		c.IncludePrerelease = spec.IncludePrerelease
		deny[i] = c
	}

	ban := make(map[string]int, len(spec.Ban))
	for i, s := range spec.Ban {
		v, err := StrictNewVersion(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("ban rule %d: %w", i, err))
			continue
		}
		key := v.String()
		if _, dup := ban[key]; !dup {
			ban[key] = i
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	return &Policy{allow: allow, deny: deny, ban: ban}, nil
}

// Check evaluates the policy against a version. Ban rules are checked
// first, then deny rules, then allow rules. A version that matches no
// allow rule is rejected even if it is not excluded.
func (p *Policy) Check(v *Version) Decision {
	if i, ok := p.ban[v.String()]; ok {
		return Decision{Allowed: false, Rule: RuleBan, Index: i}
	}

	for i, c := range p.deny {
		if c.Check(v) {
			return Decision{Allowed: false, Rule: RuleDeny, Index: i}
		}
	}

	for i, c := range p.allow {
		if c.Check(v) {
			return Decision{Allowed: true, Rule: RuleAllow, Index: i}
		}
	}

	return Decision{Allowed: false, Rule: RuleNone, Index: -1}
}

// CheckString parses the version string strictly and evaluates the policy
// against it. Parsing does not consult the package-level CoerceNewVersion
// or DetailedNewVersionErrors switches, so results are stable no matter
// how those are set by other code.
func (p *Policy) CheckString(s string) (Decision, error) {
	v, err := StrictNewVersion(s)
	if err != nil {
		return Decision{}, err
	}
	return p.Check(v), nil
}
