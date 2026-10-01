package validator

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// ruleRootPath is the path of the root node of a Rule tree within a RuleFailure.
const ruleRootPath = "$"

// RuleOp identifies the logical operator of a node within a Rule tree.
type RuleOp uint8

const (
	// OpTag is a leaf node evaluated using the existing tag syntax,
	// eg. Tag("required,email").
	OpTag RuleOp = iota
	// OpAnd is a conjunction, every child rule must hold.
	OpAnd
	// OpOr is a disjunction, at least one child rule must hold.
	OpOr
	// OpNot is a negation, the child rule must not hold.
	OpNot
)

// String returns the human readable name of the RuleOp.
func (op RuleOp) String() string {
	switch op {
	case OpTag:
		return "tag"
	case OpAnd:
		return "and"
	case OpOr:
		return "or"
	case OpNot:
		return "not"
	default:
		return "unknown"
	}
}

// Rule is a node of an explicitly nested logical validation rule tree.
//
// Rules complement the flat tag syntax for cases that are hard to express
// with it, eg. "(A and B) or (C and not D)". Build them with the Tag, And,
// Or and Not constructors and either validate a single value directly via
// Validate.ValidateRule or register the rule as a reusable struct tag via
// Validate.RegisterRule.
//
// Evaluation always short-circuits in a fixed left-to-right order:
//   - And evaluates children in order and stops at the first failure.
//   - Or evaluates children in order and stops at the first success.
//   - Not evaluates its single child and inverts the result.
//
// Children that are skipped due to short-circuiting are never executed and
// therefore have no side effects (eg. custom validations are not called).
type Rule interface {
	fmt.Stringer

	// eval evaluates the rule against value, returning nil on success or a
	// *RuleFailure located at path within the rule tree.
	eval(ctx context.Context, v *Validate, value reflect.Value, path string) *RuleFailure
}

// Tag returns a leaf Rule evaluated using the existing validation tag syntax,
// eg. Tag("required"), Tag("min=3") or Tag("required,email"). The expression
// may reference baked-in, aliased and custom registered validations.
//
// It panics if expr is empty.
func Tag(expr string) Rule {
	if len(expr) == 0 {
		panic("validator: Tag rule expression cannot be empty")
	}
	return tagRule(expr)
}

// And returns a Rule requiring every child rule to hold. Children are
// evaluated left-to-right and evaluation stops at the first failure.
//
// It panics if called without rules or with a nil rule.
func And(rules ...Rule) Rule {
	if len(rules) == 0 {
		panic("validator: And requires at least one rule")
	}
	for _, rule := range rules {
		if rule == nil {
			panic("validator: And requires non-nil rules")
		}
	}
	return andRule(rules)
}

// Or returns a Rule requiring at least one child rule to hold. Children are
// evaluated left-to-right and evaluation stops at the first success.
//
// It panics if called without rules or with a nil rule.
func Or(rules ...Rule) Rule {
	if len(rules) == 0 {
		panic("validator: Or requires at least one rule")
	}
	for _, rule := range rules {
		if rule == nil {
			panic("validator: Or requires non-nil rules")
		}
	}
	return orRule(rules)
}

// Not returns a Rule requiring the child rule to NOT hold.
//
// It panics if called with a nil rule.
func Not(rule Rule) Rule {
	if rule == nil {
		panic("validator: Not requires a non-nil rule")
	}
	return notRule{rule: rule}
}

type tagRule string

func (r tagRule) String() string { return string(r) }

func (r tagRule) eval(ctx context.Context, v *Validate, value reflect.Value, path string) *RuleFailure {
	if runTagExpression(ctx, v, value, string(r)) {
		return nil
	}
	return &RuleFailure{Path: path, Op: OpTag, Tag: string(r), Rule: string(r)}
}

type andRule []Rule

func (r andRule) String() string { return joinRules("and", []Rule(r)) }

func (r andRule) eval(ctx context.Context, v *Validate, value reflect.Value, path string) *RuleFailure {
	for i, child := range r {
		if failure := child.eval(ctx, v, value, childRulePath(path, OpAnd.String(), i)); failure != nil {
			return &RuleFailure{Path: path, Op: OpAnd, Rule: r.String(), Causes: []*RuleFailure{failure}}
		}
	}
	return nil
}

type orRule []Rule

func (r orRule) String() string { return joinRules("or", []Rule(r)) }

func (r orRule) eval(ctx context.Context, v *Validate, value reflect.Value, path string) *RuleFailure {
	var alternatives []*RuleFailure
	for i, child := range r {
		failure := child.eval(ctx, v, value, childRulePath(path, OpOr.String(), i))
		if failure == nil {
			return nil
		}
		alternatives = append(alternatives, failure)
	}
	return &RuleFailure{Path: path, Op: OpOr, Rule: r.String(), Alternatives: alternatives}
}

type notRule struct {
	rule Rule
}

func (r notRule) String() string { return "not(" + r.rule.String() + ")" }

func (r notRule) eval(ctx context.Context, v *Validate, value reflect.Value, path string) *RuleFailure {
	if failure := r.rule.eval(ctx, v, value, path+"."+OpNot.String()); failure != nil {
		// the negated rule does not hold, so the negation holds
		return nil
	}
	return &RuleFailure{Path: path, Op: OpNot, Rule: r.String()}
}

func joinRules(op string, rules []Rule) string {
	strs := make([]string, len(rules))
	for i, rule := range rules {
		strs[i] = rule.String()
	}
	return op + "(" + strings.Join(strs, ", ") + ")"
}

func childRulePath(parentPath, op string, idx int) string {
	return parentPath + "." + op + "[" + strconv.Itoa(idx) + "]"
}

// runTagExpression evaluates a tag expression against a single value using the
// standard tag machinery and reports whether it passed.
func runTagExpression(ctx context.Context, v *Validate, value reflect.Value, expr string) bool {
	ctag := v.fetchCacheTag(expr)

	vd := v.pool.Get().(*validate)
	vd.top = value
	vd.isPartial = false
	vd.traverseField(ctx, value, value, vd.ns[0:0], vd.actualNs[0:0], defaultCField, ctag)
	ok := len(vd.errs) == 0
	vd.errs = nil
	v.pool.Put(vd)
	return ok
}

// RuleFailure describes why a Rule tree failed while preserving its logical
// structure, so failures can be reported according to the logic instead of as
// a flat list of conditions that all must be fixed.
type RuleFailure struct {
	// Path locates the failing node within the rule tree, eg. "$.and[1].or[0]".
	// The root node is "$".
	Path string

	// Op is the logical operator of the failing node.
	Op RuleOp

	// Rule is the string form of the failing rule node, eg. "or(email, min=3)".
	Rule string

	// Tag is the failed tag expression, set only when Op == OpTag.
	Tag string

	// Alternatives contains one failure per Or branch when Op == OpOr.
	// These are alternatives: satisfying ANY single one of them resolves
	// the failure, they do NOT all need to be fixed.
	Alternatives []*RuleFailure

	// Causes contains the failed required conditions when Op == OpAnd.
	// Every one of these must be fixed for the rule to pass.
	Causes []*RuleFailure
}

// Error returns a human readable, logic aware description of the failure.
func (f *RuleFailure) Error() string {
	sb := new(strings.Builder)
	f.writeTo(sb, 0)
	return strings.TrimRight(sb.String(), "\n")
}

func (f *RuleFailure) writeTo(sb *strings.Builder, depth int) {
	indent := strings.Repeat("  ", depth)

	switch f.Op {
	case OpAnd:
		fmt.Fprintf(sb, "%s%s: all of these conditions must hold (%s):\n", indent, f.Path, f.Rule)
		for _, cause := range f.Causes {
			cause.writeTo(sb, depth+1)
		}
	case OpOr:
		fmt.Fprintf(sb, "%s%s: at least one of these alternatives must hold (%s), failed alternatives:\n", indent, f.Path, f.Rule)
		for _, alternative := range f.Alternatives {
			alternative.writeTo(sb, depth+1)
		}
	case OpNot:
		fmt.Fprintf(sb, "%s%s: this rule must NOT hold: %s\n", indent, f.Path, f.Rule)
	default:
		fmt.Fprintf(sb, "%s%s: validation '%s' failed\n", indent, f.Path, f.Tag)
	}
}

// ValidateRule validates a single value against a nested logical Rule tree.
//
// It returns nil when the rule holds, otherwise a *RuleFailure describing the
// failure according to the rule's logic; Or failures report the failed
// alternatives (any one of which would resolve the failure) while And
// failures report the required conditions that must be fixed.
func (v *Validate) ValidateRule(value interface{}, rule Rule) error {
	return v.ValidateRuleCtx(context.Background(), value, rule)
}

// ValidateRuleCtx does the same as ValidateRule but allows passing of
// contextual validation information via context.Context.
func (v *Validate) ValidateRuleCtx(ctx context.Context, value interface{}, rule Rule) error {
	if rule == nil {
		return errors.New("validator: rule cannot be nil")
	}
	if failure := rule.eval(ctx, v, reflect.ValueOf(value), ruleRootPath); failure != nil {
		return failure
	}
	return nil
}

// RegisterRule registers a nested logical Rule tree as a validation tag so it
// can be used within struct tags, eg. `validate:"myrule"`, and combined with
// the existing tag syntax, eg. `validate:"required,myrule"`.
//
// When the rule fails the resulting FieldError carries the full *RuleFailure,
// retrievable via RuleFailureOf.
//
// NOTE: this method is not thread-safe it is intended that these all be registered prior to any validation
func (v *Validate) RegisterRule(name string, rule Rule) error {
	if rule == nil {
		return errors.New("validator: rule cannot be nil")
	}
	return v.registerValidation(name, func(ctx context.Context, fl FieldLevel) bool {
		vd := fl.(*validate)
		failure := rule.eval(ctx, v, vd.flField, ruleRootPath)
		if failure == nil {
			return true
		}
		vd.ruleFailure = failure
		return false
	}, false, false)
}

// RuleFailureOf extracts the *RuleFailure from a FieldError produced by a
// rule registered via RegisterRule. It returns nil when the FieldError was
// not produced by a logical rule.
func RuleFailureOf(fe FieldError) *RuleFailure {
	if fe == nil {
		return nil
	}
	if ferr, ok := fe.(*fieldError); ok {
		return ferr.ruleFailure
	}
	return nil
}
