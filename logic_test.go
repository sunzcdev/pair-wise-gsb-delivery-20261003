package validator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	. "github.com/go-playground/assert/v2"
)

// countingRule records how many times it executed. It is used to prove that
// short-circuited branches never run and therefore produce no side effects.
type countingRule struct {
	name   string
	called int
	fn     FuncCtx
}

func (cr *countingRule) asRule() Rule {
	return FuncCtxRule(cr.name, func(ctx context.Context, fl FieldLevel) bool {
		cr.called++
		return cr.fn(ctx, fl)
	})
}

func TestLogicAndOrNotVarSuccess(t *testing.T) {
	v := New()

	// (min=3 AND (oneof red green OR eq=blue)) AND NOT(eq=forbidden)
	rule := And(
		Tag("min=3"),
		Or(
			Tag("oneof=red green"),
			Tag("eq=blue"),
		),
		Not(Tag("eq=forbidden")),
	)

	Equal(t, v.VarRule("red", rule), nil)
	Equal(t, v.VarRule("green", rule), nil)
	Equal(t, v.VarRule("blue", rule), nil)
}

func TestLogicAndFailurePointsAtFirstFailingBranch(t *testing.T) {
	v := New()

	rule := And(
		Tag("min=3"),
		Tag("eq=abc"),
	)

	err := v.VarRule("ab", rule)
	NotEqual(t, err, nil)

	var le *LogicError
	Equal(t, errors.As(err, &le), true)
	Equal(t, le.Op, RuleAnd)
	Equal(t, le.Path, "")

	// First failing branch is min=3 at [0]; eq=abc at [1] never runs.
	Equal(t, len(le.Children), 1)
	Equal(t, le.Children[0].Op, RuleLeaf)
	Equal(t, le.Children[0].Path, "[0]")
	Equal(t, le.Children[0].Leaves[0].Tag(), "min")
}

func TestLogicOrFailureShowsAlternatives(t *testing.T) {
	v := New()

	rule := Or(
		Tag("email"),
		Tag("uuid"),
	)

	err := v.VarRule("nope", rule)
	NotEqual(t, err, nil)

	var le *LogicError
	Equal(t, errors.As(err, &le), true)
	Equal(t, le.Op, RuleOr)

	// Both alternatives are presented as choices, each with its own reason.
	Equal(t, len(le.Children), 2)
	Equal(t, le.Children[0].Op, RuleLeaf)
	Equal(t, le.Children[0].Path, "[0]")
	Equal(t, le.Children[0].Leaves[0].Tag(), "email")
	Equal(t, le.Children[1].Path, "[1]")
	Equal(t, le.Children[1].Leaves[0].Tag(), "uuid")

	msg := err.Error()
	Equal(t, strings.Contains(msg, "OR rule failed"), true)
	Equal(t, strings.Contains(msg, "satisfy one of the following alternatives"), true)
}

func TestLogicNotFailureAndSuccess(t *testing.T) {
	v := New()

	rule := Not(Tag("eq=bad"))

	// NOT(eq=bad) succeeds for a different value; inner failure is consumed.
	Equal(t, v.VarRule("good", rule), nil)

	err := v.VarRule("bad", rule)
	NotEqual(t, err, nil)

	var le *LogicError
	Equal(t, errors.As(err, &le), true)
	Equal(t, le.Op, RuleNot)
	Equal(t, len(le.Children), 0)
	Equal(t, len(le.Flatten()), 0)
}

func TestLogicOrShortCircuitHasNoSideEffects(t *testing.T) {
	v := New()

	first := &countingRule{name: "first", fn: func(_ context.Context, _ FieldLevel) bool { return true }}
	second := &countingRule{name: "second", fn: func(_ context.Context, _ FieldLevel) bool { return true }}

	rule := Or(first.asRule(), second.asRule())

	Equal(t, v.VarRule("x", rule), nil)

	// First alternative succeeds, so the second must never execute.
	Equal(t, first.called, 1)
	Equal(t, second.called, 0)
}

func TestLogicAndShortCircuitHasNoSideEffects(t *testing.T) {
	v := New()

	first := &countingRule{name: "first", fn: func(_ context.Context, _ FieldLevel) bool { return false }}
	second := &countingRule{name: "second", fn: func(_ context.Context, _ FieldLevel) bool { return true }}

	rule := And(first.asRule(), second.asRule())

	NotEqual(t, v.VarRule("x", rule), nil)

	// First alternative fails, so the second must never execute.
	Equal(t, first.called, 1)
	Equal(t, second.called, 0)
}

func TestLogicOrEvaluatesInFixedOrder(t *testing.T) {
	v := New()

	var order []string

	mk := func(name string, ok bool) Rule {
		return FuncCtxRule(name, func(_ context.Context, _ FieldLevel) bool {
			order = append(order, name)
			return ok
		})
	}

	rule := Or(mk("a", false), mk("b", false), mk("c", true), mk("d", true))

	Equal(t, v.VarRule("x", rule), nil)
	Equal(t, len(order), 3)
	Equal(t, order[0], "a")
	Equal(t, order[1], "b")
	Equal(t, order[2], "c")
}

func TestLogicNotDoesNotRunAfterInnerFailure(t *testing.T) {
	v := New()

	calls := 0

	// NOT(Func): the custom func fails (returns false), so NOT succeeds; the
	// function must still have run exactly once.
	rule := Not(FuncCtxRule("never", func(_ context.Context, _ FieldLevel) bool {
		calls++
		return false
	}))

	Equal(t, v.VarRule("x", rule), nil)
	Equal(t, calls, 1)
}

func TestLogicLeafFailureCarriesRuleNodePath(t *testing.T) {
	v := New()

	// OR's second branch is itself an AND whose second leaf fails.
	rule := Or(
		Tag("email"),
		And(
			Tag("min=10"),
			Tag("eq=abcdefghij"),
		),
	)

	err := v.VarRule("short", rule)
	NotEqual(t, err, nil)

	var le *LogicError
	Equal(t, errors.As(err, &le), true)

	flattened := le.Flatten()

	// email at [0], min at [1][0]
	Equal(t, len(flattened), 2)
	Equal(t, flattened[0].Tag(), "email")
	Equal(t, flattened[1].Tag(), "min")

	rp, ok := flattened[1].(RulePathError)
	Equal(t, ok, true)
	Equal(t, rp.RulePath(), "[1][0]")
	Equal(t, rp.RuleName(), "min=10")

	// The tree node for that leaf is locatable at the same path.
	andNode := le.Children[1]
	Equal(t, andNode.Op, RuleAnd)
	Equal(t, andNode.Children[0].Path, "[1][0]")
}

func TestLogicFuncRuleFailureIsNamed(t *testing.T) {
	v := New()

	rule := FuncRule("is_admin", func(fl FieldLevel) bool {
		return fl.Field().String() == "admin"
	})

	err := v.VarRule("user", rule)
	NotEqual(t, err, nil)

	var le *LogicError
	Equal(t, errors.As(err, &le), true)
	Equal(t, le.Op, RuleLeaf)
	Equal(t, le.Leaves[0].Tag(), "is_admin")

	rp := le.Leaves[0].(RulePathError)
	Equal(t, rp.RuleName(), "is_admin")
}

type logicStruct struct {
	Type  string
	Value string
}

func TestLogicStructRules(t *testing.T) {
	v := New()

	// Success: value equals type.
	Equal(t, v.StructRules(logicStruct{Type: "x", Value: "x"}, Field("Value",
		Or(
			And(Tag("min=3"), Tag("nefield=Type")),
			Tag("eqfield=Type"),
		),
	)), nil)

	// Failure: value neither long enough/different, nor equal to type.
	err := v.StructRules(logicStruct{Type: "z", Value: "z"}, Field("Value",
		Or(
			And(Tag("min=3"), Tag("nefield=Type")),
			Tag("eqfield=z"),
		),
	))
	NotEqual(t, err, nil)

	msg := err.Error()
	Equal(t, strings.Contains(msg, "logicStruct.Value"), true)
	Equal(t, strings.Contains(msg, "OR rule failed"), true)
}

func TestLogicTagSyntaxStillWorksUnchanged(t *testing.T) {
	v := New()

	// Existing flat tag syntax continues to validate exactly as before.
	type s struct {
		Email string `validate:"required,email"`
	}

	NotEqual(t, v.Struct(s{Email: "bad"}), nil)
	Equal(t, v.Struct(s{Email: "user@example.com"}), nil)

	// A Tag leaf reuses the very same expressions, including aliases.
	Equal(t, v.VarRule("user@example.com", Tag("email")), nil)
	NotEqual(t, v.VarRule("bad", Tag("required,email")), nil)
}

func TestLogicContextPropagates(t *testing.T) {
	v := New()

	type ctxKey string

	key := ctxKey("seen")

	rule := FuncCtxRule("ctx", func(ctx context.Context, _ FieldLevel) bool {
		_, ok := ctx.Value(key).(string)
		return ok
	})

	Equal(t, v.VarRuleCtx(context.WithValue(context.Background(), key, "yes"), "x", rule), nil)
	NotEqual(t, v.VarRule("x", rule), nil)
}

func TestLogicNilRuleIsInvalid(t *testing.T) {
	v := New()

	var le *LogicError
	err := v.VarRule("x", nil)
	Equal(t, errors.As(err, &le), false)
	_, ok := err.(*InvalidValidationError)
	Equal(t, ok, true)
}

func TestLogicConstructorPanics(t *testing.T) {
	PanicMatches(t, func() { And() }, "validator: And requires at least one rule")
	PanicMatches(t, func() { Or() }, "validator: Or requires at least one rule")
	PanicMatches(t, func() { Not(nil) }, "validator: Not requires a rule")
	PanicMatches(t, func() { Tag("") }, "validator: Tag requires a non-empty tag")
	PanicMatches(t, func() { FuncRule("x", nil) }, "validator: Func rule requires a non-nil function")
}

func TestLogicSharedRuleIsSafeForConcurrentUse(t *testing.T) {
	v := New()

	// A rule tree built once, mixing function and tag leaves, reused across
	// goroutines. Cached plans must not leak node paths between evaluations.
	rule := Or(
		FuncRule("admin", func(fl FieldLevel) bool { return fl.Field().String() == "admin" }),
		And(Tag("min=4"), Tag("email")),
	)

	var wg sync.WaitGroup

	inputs := []string{"admin", "user@example.com", "bad", "no"}

	for g := 0; g < 32; g++ {
		wg.Add(1)

		go func(seed int) {
			defer wg.Done()

			for k := 0; k < 100; k++ {
				input := inputs[(seed+k)%len(inputs)]
				err := v.VarRule(input, rule)

				switch input {
				case "admin", "user@example.com":
					Equal(t, err, nil)
				default:
					NotEqual(t, err, nil)

					var le *LogicError
					Equal(t, errors.As(err, &le), true)

					for _, fe := range le.Flatten() {
						rp := fe.(RulePathError)
						// Paths must be stable and never cross-contaminated.
						if fe.Tag() == "admin" {
							Equal(t, rp.RulePath(), "[0]")
						} else {
							Equal(t, strings.HasPrefix(rp.RulePath(), "[1]"), true)
						}
					}
				}
			}
		}(g)
	}

	wg.Wait()
}
