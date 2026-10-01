package validator

import (
	"context"
	"fmt"
	"reflect"
)

// RuleKind identifies the boolean operation a Rule represents.
type RuleKind uint8

const (
	// RuleAnd succeeds when every child rule succeeds (short-circuits on the
	// first failure, in declaration order).
	RuleAnd RuleKind = iota + 1
	// RuleOr succeeds when at least one child rule succeeds (short-circuits on
	// the first success, in declaration order).
	RuleOr
	// RuleNot negates a single child rule.
	RuleNot
	// RuleLeaf wraps an existing validator (a tag expression or a custom
	// validation function) and carries no children.
	RuleLeaf
)

// Rule is an explicitly nested validation rule. Rules are combined with And,
// Or and Not and may wrap any existing tag based validator or a custom FuncCtx.
//
// Rules are evaluated left-to-right, in declaration order, using the standard
// boolean short-circuit semantics:
//   - And stops at the first failing child; later children are never executed.
//   - Or stops at the first succeeding child; later children are never executed.
//   - Not evaluates exactly one child and negates its result.
//
// The Rule interface is intentionally sealed: rules are created with the And,
// Or, Not, Tag, FuncRule and FuncCtxRule constructors.
//
// Build rules once (ideally at package initialization time) and reuse them; tag
// execution plans are compiled once and cached on the Validate instance.
type Rule interface {
	kind() RuleKind
	children() []Rule
}

// FieldRule binds a nested Rule to a field by name. It is used together with
// Validate.StructRules.
type FieldRule struct {
	fieldName string
	rule      Rule
}

// Field binds rule to a struct field by name. Field names are Go struct field
// names, identical to those used by cross-field tags such as eqfield.
func Field(name string, rule Rule) FieldRule {
	return FieldRule{fieldName: name, rule: rule}
}

// Name returns the bound struct field name.
func (fr FieldRule) Name() string {
	return fr.fieldName
}

// Rule returns the nested rule bound to the field.
func (fr FieldRule) Rule() Rule {
	return fr.rule
}

type andRule struct {
	rules []Rule
}

type orRule struct {
	rules []Rule
}

type notRule struct {
	rule Rule
}

type leafRule struct {
	tag  string
	name string
	ct   *cTag
}

func (andRule) kind() RuleKind { return RuleAnd }

func (r andRule) children() []Rule { return r.rules }

func (orRule) kind() RuleKind { return RuleOr }

func (r orRule) children() []Rule { return r.rules }

func (notRule) kind() RuleKind { return RuleNot }

func (r notRule) children() []Rule {
	if r.rule == nil {
		return nil
	}
	return []Rule{r.rule}
}

func (leafRule) kind() RuleKind   { return RuleLeaf }
func (leafRule) children() []Rule { return nil }

// And combines rules with logical AND. Every rule must succeed. Rules are
// evaluated in declaration order and evaluation stops at the first failure.
//
// At least one rule must be supplied.
func And(rules ...Rule) Rule {
	if len(rules) == 0 {
		panic("validator: And requires at least one rule")
	}

	return andRule{rules: rules}
}

// Or combines rules with logical OR. Any single succeeding rule makes the whole
// group succeed. Rules are evaluated in declaration order and evaluation stops
// at the first success.
//
// When every alternative fails, the returned error explains that one of the
// alternatives must be satisfied, presenting each failed branch independently
// rather than as a list of conditions that all need fixing.
//
// At least one rule must be supplied.
func Or(rules ...Rule) Rule {
	if len(rules) == 0 {
		panic("validator: Or requires at least one rule")
	}

	return orRule{rules: rules}
}

// Not negates rule: it succeeds exactly when rule fails. The negated rule is
// evaluated once; its failure is consumed and not reported as a standalone
// error.
func Not(rule Rule) Rule {
	if rule == nil {
		panic("validator: Not requires a rule")
	}

	return notRule{rule: rule}
}

// Tag wraps an existing tag expression as a leaf rule. The tag uses the exact
// same syntax as struct tags (for example "min=10", "email" or
// "omitempty,oneof=a b c"), so every built-in, alias and custom registered tag
// can be reused unchanged.
func Tag(tag string) Rule {
	if len(tag) == 0 || tag == skipValidationTag {
		panic("validator: Tag requires a non-empty tag")
	}

	return leafRule{tag: tag, name: tag}
}

// FuncRule wraps a custom validation function as a leaf rule and assigns it a
// name.
// The name is used in error reports and node paths to locate the originating
// rule. The function receives the same FieldLevel as RegisterValidation.
func FuncRule(name string, fn Func) Rule {
	return FuncCtxRule(name, wrapFunc(fn))
}

// FuncCtxRule wraps a context aware custom validation function as a leaf rule.
// See FuncRule for details.
func FuncCtxRule(name string, fn FuncCtx) Rule {
	if len(name) == 0 {
		panic("validator: Func rule requires a name")
	}

	if fn == nil {
		panic("validator: Func rule requires a non-nil function")
	}

	// A function leaf's plan depends only on the function itself, so it is built
	// once at construction and safely shared across concurrent validations.
	return leafRule{
		name: name,
		ct: &cTag{
			tag:      name,
			aliasTag: name,
			fn:       fn,
			typeof:   typeDefault,
			hasTag:   true,
		},
	}
}

// logicRunner carries the per-evaluation state needed to execute a rule tree.
type logicRunner struct {
	v        *Validate
	ctx      context.Context
	vd       *validate
	parent   reflect.Value
	current  reflect.Value
	ns       []byte
	structNs []byte
	cf       *cField
}

// evaluate runs the rule tree rooted at node against the runner's current value
// and returns a LogicError describing the failing node when the rule fails.
// Path components are assigned top-down as the tree is walked, so every
// reported leaf failure can be traced back to its originating rule node.
func (lr *logicRunner) evaluate(node Rule, path string) *LogicError {
	switch n := node.(type) {
	case andRule:
		for i, child := range n.rules {
			childPath := fmt.Sprintf("%s[%d]", path, i)
			// Short-circuit on the first failing child; later children are
			// never executed. The child keeps its own identity and path and is
			// nested under this AND node.
			if childErr := lr.evaluate(child, childPath); childErr != nil {
				return lr.wrapError(RuleAnd, path, i, childErr)
			}
		}
		return nil

	case orRule:
		branches := make([]*LogicError, 0, len(n.rules))

		for i, child := range n.rules {
			childPath := fmt.Sprintf("%s[%d]", path, i)

			// Fixed short-circuit order: children run strictly left-to-right;
			// once one succeeds, remaining children are never invoked and so
			// cannot produce side effects. Every failing alternative is kept so
			// the report can present them as choices.
			if childErr := lr.evaluate(child, childPath); childErr == nil {
				return nil
			} else {
				branches = append(branches, childErr)
			}
		}

		return lr.nodeError(RuleOr, path, branches)

	case notRule:
		if fe := lr.evaluate(n.rule, path+"[0]"); fe != nil {
			// Negation consumes the inner failure: NOT(rule) succeeds.
			return nil
		}

		return lr.nodeError(RuleNot, path, nil)

	case leafRule:
		return lr.evaluateLeaf(n, path)

	default:
		panic("validator: unknown rule type")
	}
}

// wrapError nests a single child failure under a failing boolean node.
func (lr *logicRunner) wrapError(kind RuleKind, path string, index int, child *LogicError) *LogicError {
	le := lr.nodeError(kind, path, []*LogicError{child})
	le.Index = index

	return le
}

// nodeError builds a boolean node failure describing the current field.
func (lr *logicRunner) nodeError(kind RuleKind, path string, children []*LogicError) *LogicError {
	return &LogicError{
		Op:       kind,
		Path:     path,
		Children: children,
		ns:       lr.fieldNamespace(),
		structNs: lr.fieldStructNamespace(),
		field:    lr.cf.altName,
		value:    getValue(lr.current),
		kind:     kindOf(lr.current),
		typ:      currentType(lr.current),
	}
}

func kindOf(val reflect.Value) reflect.Kind {
	if !val.IsValid() {
		return reflect.Invalid
	}
	return val.Kind()
}

func currentType(val reflect.Value) reflect.Type {
	if !val.IsValid() {
		return nil
	}
	return val.Type()
}

// evaluateLeaf executes a single leaf's compiled tag plan and converts every
// newly produced FieldError into a leaf LogicError carrying the node path.
func (lr *logicRunner) evaluateLeaf(r leafRule, path string) *LogicError {
	var ct *cTag
	if r.tag != "" {
		ct = lr.v.fetchCacheTag(r.tag)
	} else {
		ct = r.ct
	}

	start := len(lr.vd.errs)

	lr.vd.rulePath = path
	lr.vd.ruleLabel = r.name
	lr.vd.traverseField(lr.ctx, lr.parent, lr.current, lr.ns, lr.structNs, lr.cf, ct)

	errs := lr.vd.errs[start:]
	lr.vd.errs = lr.vd.errs[:start]
	lr.vd.rulePath = ""
	lr.vd.ruleLabel = ""

	if len(errs) == 0 {
		return nil
	}

	leaves := make([]FieldError, 0, len(errs))

	for _, fe := range errs {
		fe := fe.(*fieldError)
		fe.rulePath = path
		fe.ruleLabel = r.name
		leaves = append(leaves, fe)
	}

	return &LogicError{
		Op:       RuleLeaf,
		Path:     path,
		Leaves:   leaves,
		ns:       leaves[0].Namespace(),
		structNs: leaves[0].StructNamespace(),
		field:    leaves[0].Field(),
		value:    leaves[0].Value(),
		kind:     leaves[0].Kind(),
		typ:      leaves[0].Type(),
	}
}

func (lr *logicRunner) fieldNamespace() string {
	return appendAltName(lr.ns, lr.cf.altName)
}

func (lr *logicRunner) fieldStructNamespace() string {
	if lr.v.hasTagNameFunc {
		if n := len(lr.structNs); n > 0 && lr.structNs[n-1] == '.' {
			return string(lr.structNs[:n-1])
		}
		return string(lr.structNs)
	}
	return lr.fieldNamespace()
}
