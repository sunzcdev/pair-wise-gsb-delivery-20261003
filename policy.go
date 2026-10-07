package semver

import (
	"errors"
	"fmt"
	"strings"
)

// PolicyRuleAction describes how a policy rule affects a version.
type PolicyRuleAction string

const (
	// PolicyRuleAllow allows versions matched by the rule's constraint.
	PolicyRuleAllow PolicyRuleAction = "allow"

	// PolicyRuleExclude excludes versions matched by the rule's constraint.
	PolicyRuleExclude PolicyRuleAction = "exclude"

	// PolicyRuleBlock blocks Version.Original exactly. The optional v prefix
	// and build metadata are part of that identifier.
	PolicyRuleBlock PolicyRuleAction = "block"
)

// PolicyRule is a numbered rule in a composite policy.
type PolicyRule struct {
	// ID is the stable number used in a decision reason. IDs must be positive
	// and unique within a policy.
	ID int

	// Action selects whether this rule allows, excludes, or blocks a version.
	Action PolicyRuleAction

	// Expression is a constraint for allow and exclude rules, or a complete
	// semantic version for block rules.
	Expression string
}

// PolicyDecision is the result of evaluating a version against a composite
// policy.
type PolicyDecision struct {
	// Allowed reports whether the version may be released.
	Allowed bool

	// RuleID identifies the allow, exclude, or block rule that determined the
	// decision. It is zero when no rule allowed the version.
	RuleID int

	// Action identifies the action taken by the rule that determined the
	// decision. It is empty when no rule allowed the version.
	Action PolicyRuleAction

	// Expression is the constraint or exact version of the determining rule.
	Expression string

	// Reason describes why the decision was made.
	Reason string
}

// CompositePolicy combines allow ranges, exclude ranges, and exact version
// blocks. Block rules are evaluated first, followed by excludes and allows.
// A version not matched by an allow rule is denied.
type CompositePolicy struct {
	allow   []compiledPolicyRule
	exclude []compiledPolicyRule
	blocked map[string]compiledPolicyRule
}

type compiledPolicyRule struct {
	id         int
	expression string
	constraint *Constraints
}

// CompileCompositePolicy validates all policy rules and returns an immutable
// policy. Configuration problems from every rule are reported together.
func CompileCompositePolicy(rules []PolicyRule) (*CompositePolicy, error) {
	var errs []error
	seenIDs := make(map[int]struct{}, len(rules))
	duplicateIDs := make(map[int]struct{})
	blocked := make(map[string]compiledPolicyRule, len(rules))
	duplicateBlocks := make(map[string]struct{})
	policy := &CompositePolicy{blocked: blocked}

	for i, rule := range rules {
		if rule.ID <= 0 {
			errs = append(errs, fmt.Errorf("rule at index %d: ID must be positive", i))
		} else if _, ok := seenIDs[rule.ID]; ok {
			duplicateIDs[rule.ID] = struct{}{}
		} else {
			seenIDs[rule.ID] = struct{}{}
		}

		expressionEmpty := strings.TrimSpace(rule.Expression) == ""
		if expressionEmpty {
			errs = append(errs, fmt.Errorf("rule %d: expression is empty", rule.ID))
		}

		switch rule.Action {
		case PolicyRuleAllow, PolicyRuleExclude:
			if expressionEmpty {
				continue
			}

			constraint, err := NewConstraint(rule.Expression)
			if err != nil {
				errs = append(errs, fmt.Errorf("rule %d: invalid constraint %q: %w", rule.ID, rule.Expression, err))
				continue
			}

			compiled := *constraint
			compiledRule := compiledPolicyRule{
				id:         rule.ID,
				expression: rule.Expression,
				constraint: &compiled,
			}
			if rule.Action == PolicyRuleAllow {
				policy.allow = append(policy.allow, compiledRule)
			} else {
				policy.exclude = append(policy.exclude, compiledRule)
			}
		case PolicyRuleBlock:
			if expressionEmpty {
				continue
			}

			if err := validateBlockExpression(rule.Expression); err != nil {
				errs = append(errs, fmt.Errorf("rule %d: invalid block version %q: %w", rule.ID, rule.Expression, err))
				continue
			}

			if _, ok := blocked[rule.Expression]; ok {
				duplicateBlocks[rule.Expression] = struct{}{}
				continue
			}
			blocked[rule.Expression] = compiledPolicyRule{
				id:         rule.ID,
				expression: rule.Expression,
			}
		default:
			errs = append(errs, fmt.Errorf("rule %d: unknown action %q", rule.ID, rule.Action))
		}
	}

	for id := range duplicateIDs {
		errs = append(errs, fmt.Errorf("rule ID %d is used more than once", id))
	}
	for expression := range duplicateBlocks {
		errs = append(errs, fmt.Errorf("block version %q is used more than once", expression))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	return policy, nil
}

// NewCompositePolicy compiles rules into a composite policy.
func NewCompositePolicy(rules []PolicyRule) (*CompositePolicy, error) {
	return CompileCompositePolicy(rules)
}

// Decide evaluates a version and returns the policy decision. The compiled
// policy is safe for concurrent use.
func (p *CompositePolicy) Decide(v *Version) PolicyDecision {
	if p == nil {
		return PolicyDecision{Reason: "cannot evaluate a nil composite policy"}
	}
	if v == nil {
		return PolicyDecision{Reason: "cannot evaluate a nil version"}
	}

	original := v.Original()
	if rule, ok := p.blocked[original]; ok {
		return denied(rule, v, PolicyRuleBlock, "blocked by exact version rule")
	}

	for _, rule := range p.exclude {
		if rule.constraint.Check(v) {
			return denied(rule, v, PolicyRuleExclude, "excluded by range rule")
		}
	}

	for _, rule := range p.allow {
		if rule.constraint.Check(v) {
			return PolicyDecision{
				Allowed:    true,
				RuleID:     rule.id,
				Action:     PolicyRuleAllow,
				Expression: rule.expression,
				Reason:     fmt.Sprintf("%q is allowed by rule %d", original, rule.id),
			}
		}
	}

	return PolicyDecision{
		Reason: fmt.Sprintf("%q is not covered by an allow rule", original),
	}
}

// Evaluate evaluates a version using the same semantics as Decide.
func (p *CompositePolicy) Evaluate(v *Version) PolicyDecision {
	return p.Decide(v)
}

func denied(rule compiledPolicyRule, v *Version, action PolicyRuleAction, reason string) PolicyDecision {
	return PolicyDecision{
		RuleID:     rule.id,
		Action:     action,
		Expression: rule.expression,
		Reason:     fmt.Sprintf("%q is %s %d", v.Original(), reason, rule.id),
	}
}

func validateBlockExpression(expression string) error {
	strictExpression := strings.TrimPrefix(expression, "v")
	_, err := StrictNewVersion(strictExpression)
	return err
}
