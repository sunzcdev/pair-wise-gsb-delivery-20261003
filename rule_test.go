package validator

import (
	"context"
	"strings"
	"testing"

	. "github.com/go-playground/assert/v2"
)

// TestRuleAndOrNot exercises the "(A and B) or (C and not D)" shape that the
// flat tag syntax cannot express.
func TestRuleAndOrNot(t *testing.T) {
	validate := New()

	// (gte=10 and lte=20) or (eq=42 and not lt=0)
	rule := Or(
		And(Tag("gte=10"), Tag("lte=20")),
		And(Tag("eq=42"), Not(Tag("lt=0"))),
	)

	// first branch holds
	Equal(t, validate.ValidateRule(15, rule), nil)
	// second branch holds
	Equal(t, validate.ValidateRule(42, rule), nil)
	// neither branch holds
	NotEqual(t, validate.ValidateRule(7, rule), nil)
	NotEqual(t, validate.ValidateRule(-5, rule), nil)

	// negation failing on its own
	err := validate.ValidateRule(5, Not(Tag("eq=5")))
	NotEqual(t, err, nil)
	failure, ok := err.(*RuleFailure)
	Equal(t, ok, true)
	Equal(t, failure.Op, OpNot)
	Equal(t, failure.Path, "$")
	Equal(t, failure.Rule, "not(eq=5)")

	// negation holding
	Equal(t, validate.ValidateRule(6, Not(Tag("eq=5"))), nil)
}

// TestRuleShortCircuitOrder verifies the fixed left-to-right short-circuit
// order and, via counting custom validations, that skipped branches are never
// executed and therefore have no side effects.
func TestRuleShortCircuitOrder(t *testing.T) {
	validate := New()

	var calls []string
	record := func(name string, ok bool) Func {
		return func(fl FieldLevel) bool {
			calls = append(calls, name)
			return ok
		}
	}

	Equal(t, validate.RegisterValidation("rulepass", record("pass", true)), nil)
	Equal(t, validate.RegisterValidation("rulefail", record("fail", false)), nil)

	// Or stops at the first success: the second branch is never executed
	calls = nil
	Equal(t, validate.ValidateRule("x", Or(Tag("rulepass"), Tag("rulefail"))), nil)
	Equal(t, calls, []string{"pass"})

	// And stops at the first failure: later conditions are never executed
	calls = nil
	NotEqual(t, validate.ValidateRule("x", And(Tag("rulefail"), Tag("rulepass"))), nil)
	Equal(t, calls, []string{"fail"})

	// And evaluates left-to-right and stops at the first failure
	calls = nil
	NotEqual(t, validate.ValidateRule("x", And(Tag("rulepass"), Tag("rulefail"), Tag("rulepass"))), nil)
	Equal(t, calls, []string{"pass", "fail"})

	// Or evaluates left-to-right until the first success
	calls = nil
	Equal(t, validate.ValidateRule("x", Or(Tag("rulefail"), Tag("rulepass"), Tag("rulefail"))), nil)
	Equal(t, calls, []string{"fail", "pass"})

	// Not always evaluates its single child
	calls = nil
	NotEqual(t, validate.ValidateRule("x", Not(Tag("rulepass"))), nil)
	Equal(t, calls, []string{"pass"})
}

// TestRuleOrFailureAlternatives verifies that an Or failure reports failed
// branches as alternatives (any one of which would resolve the failure) and
// not as a list of conditions that all must be fixed.
func TestRuleOrFailureAlternatives(t *testing.T) {
	validate := New()

	rule := Or(Tag("email"), Tag("uuid"))
	err := validate.ValidateRule("not-an-email-nor-uuid", rule)
	NotEqual(t, err, nil)

	failure, ok := err.(*RuleFailure)
	Equal(t, ok, true)
	Equal(t, failure.Op, OpOr)
	Equal(t, failure.Path, "$")
	Equal(t, len(failure.Alternatives), 2)
	Equal(t, len(failure.Causes), 0)

	Equal(t, failure.Alternatives[0].Op, OpTag)
	Equal(t, failure.Alternatives[0].Tag, "email")
	Equal(t, failure.Alternatives[0].Path, "$.or[0]")
	Equal(t, failure.Alternatives[1].Tag, "uuid")
	Equal(t, failure.Alternatives[1].Path, "$.or[1]")

	msg := failure.Error()
	Equal(t, strings.Contains(msg, "at least one of these alternatives must hold"), true)
	Equal(t, strings.Contains(msg, "$.or[0]: validation 'email' failed"), true)
	Equal(t, strings.Contains(msg, "$.or[1]: validation 'uuid' failed"), true)
}

// TestRuleAndFailureCauses verifies that an And failure reports the required
// conditions that must be fixed.
func TestRuleAndFailureCauses(t *testing.T) {
	validate := New()

	rule := And(Tag("required"), Tag("min=3"))
	err := validate.ValidateRule("ab", rule)
	NotEqual(t, err, nil)

	failure, ok := err.(*RuleFailure)
	Equal(t, ok, true)
	Equal(t, failure.Op, OpAnd)
	Equal(t, failure.Path, "$")
	Equal(t, len(failure.Causes), 1)
	Equal(t, failure.Causes[0].Op, OpTag)
	Equal(t, failure.Causes[0].Tag, "min=3")
	Equal(t, failure.Causes[0].Path, "$.and[1]")

	msg := failure.Error()
	Equal(t, strings.Contains(msg, "all of these conditions must hold"), true)
	Equal(t, strings.Contains(msg, "$.and[1]: validation 'min=3' failed"), true)
}

// TestRuleFailurePaths verifies that failures within complex nested rules can
// be located back to the original rule node via Path.
func TestRuleFailurePaths(t *testing.T) {
	validate := New()

	rule := And(
		Tag("required"),
		Or(
			Tag("email"),
			And(Tag("min=3"), Tag("max=5")),
		),
	)

	err := validate.ValidateRule("x", rule)
	NotEqual(t, err, nil)

	failure, ok := err.(*RuleFailure)
	Equal(t, ok, true)

	// root And failed at "$" because its second child failed
	Equal(t, failure.Op, OpAnd)
	Equal(t, failure.Path, "$")
	Equal(t, len(failure.Causes), 1)

	// the Or node at "$.and[1]" failed with two alternatives
	orFailure := failure.Causes[0]
	Equal(t, orFailure.Op, OpOr)
	Equal(t, orFailure.Path, "$.and[1]")
	Equal(t, len(orFailure.Alternatives), 2)

	// first alternative: the email leaf
	Equal(t, orFailure.Alternatives[0].Op, OpTag)
	Equal(t, orFailure.Alternatives[0].Tag, "email")
	Equal(t, orFailure.Alternatives[0].Path, "$.and[1].or[0]")

	// second alternative: the nested And, whose first child failed
	nestedAnd := orFailure.Alternatives[1]
	Equal(t, nestedAnd.Op, OpAnd)
	Equal(t, nestedAnd.Path, "$.and[1].or[1]")
	Equal(t, len(nestedAnd.Causes), 1)
	Equal(t, nestedAnd.Causes[0].Tag, "min=3")
	Equal(t, nestedAnd.Causes[0].Path, "$.and[1].or[1].and[0]")

	// the full message locates every failing node
	msg := failure.Error()
	for _, path := range []string{"$.and[1].or[0]", "$.and[1].or[1]", "$.and[1].or[1].and[0]"} {
		Equal(t, strings.Contains(msg, path), true)
	}
}

// TestRegisterRule verifies that a rule can be registered as a struct tag,
// combined with the existing tag syntax, and that the resulting FieldError
// carries the logic aware *RuleFailure.
func TestRegisterRule(t *testing.T) {
	validate := New()

	rule := Or(
		And(Tag("gte=10"), Tag("lte=20")),
		And(Tag("eq=42"), Not(Tag("lt=0"))),
	)
	Equal(t, validate.RegisterRule("range_or_answer", rule), nil)

	type Test struct {
		Value int `validate:"range_or_answer"`
	}

	// passing values
	Equal(t, validate.Struct(Test{Value: 15}), nil)
	Equal(t, validate.Struct(Test{Value: 42}), nil)

	// failing value
	err := validate.Struct(Test{Value: 7})
	NotEqual(t, err, nil)

	errs, ok := err.(ValidationErrors)
	Equal(t, ok, true)
	Equal(t, len(errs), 1)
	Equal(t, errs[0].Tag(), "range_or_answer")
	Equal(t, errs[0].Field(), "Value")

	failure := RuleFailureOf(errs[0])
	NotEqual(t, failure, nil)
	Equal(t, failure.Op, OpOr)
	Equal(t, failure.Path, "$")
	Equal(t, len(failure.Alternatives), 2)
	Equal(t, failure.Alternatives[0].Path, "$.or[0]")
	Equal(t, failure.Alternatives[1].Path, "$.or[1]")

	// non rule FieldErrors carry no RuleFailure
	Equal(t, RuleFailureOf(nil), nil)
}

// TestRegisterRuleWithTagSyntax verifies the original flat tag syntax keeps
// working alongside registered rules.
func TestRegisterRuleWithTagSyntax(t *testing.T) {
	validate := New()

	Equal(t, validate.RegisterRule("answer", Not(Tag("eq=42"))), nil)

	type Test struct {
		Name  string `validate:"required,min=3"`
		Value int    `validate:"gte=0,answer"`
	}

	// all good
	Equal(t, validate.Struct(Test{Name: "abc", Value: 7}), nil)

	// flat tag fails
	err := validate.Struct(Test{Name: "ab", Value: 7})
	NotEqual(t, err, nil)
	errs := err.(ValidationErrors)
	Equal(t, len(errs), 1)
	Equal(t, errs[0].Tag(), "min")
	Equal(t, RuleFailureOf(errs[0]), nil)

	// rule fails
	err = validate.Struct(Test{Name: "abc", Value: 42})
	NotEqual(t, err, nil)
	errs = err.(ValidationErrors)
	Equal(t, len(errs), 1)
	Equal(t, errs[0].Tag(), "answer")
	failure := RuleFailureOf(errs[0])
	NotEqual(t, failure, nil)
	Equal(t, failure.Op, OpNot)

	// the flat or syntax is untouched
	Equal(t, validate.Var("abc", "eq=ab|eq=abc"), nil)
	NotEqual(t, validate.Var("zz", "eq=ab|eq=abc"), nil)
}

// TestRuleCustomValidationCtx verifies contextual custom validations compose
// within rules and receive the context.
func TestRuleCustomValidationCtx(t *testing.T) {
	validate := New()

	type ctxKey string
	const key ctxKey = "marker"

	err := validate.RegisterValidationCtx("ctxmarker", func(ctx context.Context, fl FieldLevel) bool {
		return ctx.Value(key) == "yes"
	})
	Equal(t, err, nil)

	rule := And(Tag("required"), Tag("ctxmarker"))

	NotEqual(t, validate.ValidateRuleCtx(context.Background(), "x", rule), nil)
	Equal(t, validate.ValidateRuleCtx(context.WithValue(context.Background(), key, "yes"), "x", rule), nil)
}

// TestRuleInvalidConstruction verifies invalid rule construction is rejected.
func TestRuleInvalidConstruction(t *testing.T) {
	validate := New()

	PanicMatches(t, func() { Tag("") }, "validator: Tag rule expression cannot be empty")
	PanicMatches(t, func() { And() }, "validator: And requires at least one rule")
	PanicMatches(t, func() { Or() }, "validator: Or requires at least one rule")
	PanicMatches(t, func() { Not(nil) }, "validator: Not requires a non-nil rule")
	PanicMatches(t, func() { And(nil) }, "validator: And requires non-nil rules")
	PanicMatches(t, func() { Or(nil) }, "validator: Or requires non-nil rules")

	NotEqual(t, validate.ValidateRule("x", nil), nil)
	NotEqual(t, validate.RegisterRule("nilrule", nil), nil)

	// restricted tag names are rejected just like RegisterValidation
	PanicMatches(t, func() {
		_ = validate.RegisterRule("required", Tag("required"))
	}, "Tag 'required' either contains restricted characters or is the same as a restricted tag needed for normal operation")
}

// TestRuleString verifies the string form of rule trees.
func TestRuleString(t *testing.T) {
	rule := And(
		Tag("required"),
		Or(Tag("email"), Not(Tag("contains=@"))),
	)
	Equal(t, rule.String(), "and(required, or(email, not(contains=@)))")
	Equal(t, Tag("min=3").String(), "min=3")
	Equal(t, OpTag.String(), "tag")
	Equal(t, OpAnd.String(), "and")
	Equal(t, OpOr.String(), "or")
	Equal(t, OpNot.String(), "not")
	Equal(t, RuleOp(255).String(), "unknown")
}

// TestRuleLeafTagExpression verifies leaf rules accept full tag expressions,
// including aliases and the flat or syntax.
func TestRuleLeafTagExpression(t *testing.T) {
	validate := New()
	validate.RegisterAlias("myalias", "required,min=3")

	Equal(t, validate.ValidateRule("abcd", Tag("myalias")), nil)
	NotEqual(t, validate.ValidateRule("ab", Tag("myalias")), nil)

	// flat or inside a leaf keeps working
	Equal(t, validate.ValidateRule("abc", Tag("eq=ab|eq=abc")), nil)

	// omitempty semantics are preserved within leaves
	Equal(t, validate.ValidateRule("", Tag("omitempty,email")), nil)
	NotEqual(t, validate.ValidateRule("nope", Tag("omitempty,email")), nil)
}
