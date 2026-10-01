package expr_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/file"
	"github.com/expr-lang/expr/internal/testify/assert"
	"github.com/expr-lang/expr/internal/testify/require"
)

type matchStatus interface{ matchStatus() }

type MatchPending struct{}

func (MatchPending) matchStatus() {}

type MatchPaid struct {
	Amount int
}

func (MatchPaid) matchStatus() {}

type MatchRefunded struct {
	Reason matchReason
	Note   string
}

func (MatchRefunded) matchStatus() {}

type matchReason interface{ matchReason() }

type MatchFraud struct {
	Code string
}

func (MatchFraud) matchReason() {}

type MatchChangedMind struct{}

func (MatchChangedMind) matchReason() {}

type matchOrder struct {
	Status matchStatus
}

type matchEnv struct {
	Order matchOrder
}

func matchOptions() []expr.Option {
	return []expr.Option{
		expr.Env(matchEnv{}),
		expr.VariantSet((*matchStatus)(nil), MatchPending{}, MatchPaid{}, MatchRefunded{}),
		expr.VariantSet((*matchReason)(nil), MatchFraud{}, MatchChangedMind{}),
	}
}

func matchRun(t *testing.T, code string, env matchEnv) (any, error) {
	t.Helper()
	program, err := expr.Compile(code, matchOptions()...)
	require.NoError(t, err)
	return expr.Run(program, env)
}

func TestMatch_basic(t *testing.T) {
	code := `
match Order.Status {
	MatchPending: "pending",
	MatchPaid { Amount }: "paid",
	MatchRefunded { Reason }: "refunded",
}`
	tests := []struct {
		status matchStatus
		want   string
	}{
		{MatchPending{}, "pending"},
		{MatchPaid{Amount: 42}, "paid"},
		{MatchRefunded{Reason: MatchFraud{Code: "f"}}, "refunded"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%T", tt.status), func(t *testing.T) {
			out, err := matchRun(t, code, matchEnv{Order: matchOrder{Status: tt.status}})
			require.NoError(t, err)
			assert.Equal(t, tt.want, out)
		})
	}
}

func TestMatch_binding(t *testing.T) {
	code := `
match Order.Status {
	MatchPaid { Amount }: Amount * 2,
	_: 0,
}`
	out, err := matchRun(t, code, matchEnv{Order: matchOrder{Status: MatchPaid{Amount: 21}}})
	require.NoError(t, err)
	assert.Equal(t, 42, out)
}

func TestMatch_binding_alias(t *testing.T) {
	code := `
match Order.Status {
	MatchPaid { Amount: total }: total + 1,
	_: 0,
}`
	out, err := matchRun(t, code, matchEnv{Order: matchOrder{Status: MatchPaid{Amount: 41}}})
	require.NoError(t, err)
	assert.Equal(t, 42, out)
}

func TestMatch_guard(t *testing.T) {
	code := `
match Order.Status {
	MatchPaid { Amount } if Amount > 100: "big",
	MatchPaid { Amount }: "small",
	_: "other",
}`
	out, err := matchRun(t, code, matchEnv{Order: matchOrder{Status: MatchPaid{Amount: 500}}})
	require.NoError(t, err)
	assert.Equal(t, "big", out)

	out, err = matchRun(t, code, matchEnv{Order: matchOrder{Status: MatchPaid{Amount: 5}}})
	require.NoError(t, err)
	assert.Equal(t, "small", out)

	out, err = matchRun(t, code, matchEnv{Order: matchOrder{Status: MatchPending{}}})
	require.NoError(t, err)
	assert.Equal(t, "other", out)
}

func TestMatch_nested_pattern(t *testing.T) {
	code := `
match Order.Status {
	MatchRefunded { Reason: MatchFraud { Code } }: "fraud: " + Code,
	MatchRefunded { Reason: MatchChangedMind }: "changed mind",
	_: "other",
}`
	out, err := matchRun(t, code, matchEnv{Order: matchOrder{Status: MatchRefunded{Reason: MatchFraud{Code: "stolen"}}}})
	require.NoError(t, err)
	assert.Equal(t, "fraud: stolen", out)

	out, err = matchRun(t, code, matchEnv{Order: matchOrder{Status: MatchRefunded{Reason: MatchChangedMind{}}}})
	require.NoError(t, err)
	assert.Equal(t, "changed mind", out)

	out, err = matchRun(t, code, matchEnv{Order: matchOrder{Status: MatchPending{}}})
	require.NoError(t, err)
	assert.Equal(t, "other", out)
}

func TestMatch_field_wildcard(t *testing.T) {
	code := `
match Order.Status {
	MatchPaid { Amount: _ }: "paid",
	_: "other",
}`
	out, err := matchRun(t, code, matchEnv{Order: matchOrder{Status: MatchPaid{Amount: 1}}})
	require.NoError(t, err)
	assert.Equal(t, "paid", out)
}

func TestMatch_pointer_variant(t *testing.T) {
	program, err := expr.Compile(`
match Order.Status {
	MatchPaid { Amount }: Amount,
	_: 0,
}`,
		expr.Env(matchEnv{}),
		expr.VariantSet((*matchStatus)(nil), MatchPending{}, (*MatchPaid)(nil), MatchRefunded{}),
	)
	require.NoError(t, err)

	out, err := expr.Run(program, matchEnv{Order: matchOrder{Status: &MatchPaid{Amount: 42}}})
	require.NoError(t, err)
	assert.Equal(t, 42, out)
}

func TestMatch_named_variant(t *testing.T) {
	program, err := expr.Compile(`
match Order.Status {
	paid { Amount }: Amount,
	_: 0,
}`,
		expr.Env(matchEnv{}),
		expr.VariantSet((*matchStatus)(nil),
			MatchPending{},
			expr.Variant("paid", MatchPaid{}),
			MatchRefunded{},
		),
	)
	require.NoError(t, err)

	out, err := expr.Run(program, matchEnv{Order: matchOrder{Status: MatchPaid{Amount: 42}}})
	require.NoError(t, err)
	assert.Equal(t, 42, out)
}

func TestMatch_only_selected_branch_evaluated(t *testing.T) {
	var calls []string
	track := func(args ...any) (any, error) {
		calls = append(calls, args[0].(string))
		return args[0], nil
	}

	options := append(matchOptions(), expr.Function("track", track))
	program, err := expr.Compile(`
match Order.Status {
	MatchPending: track("pending"),
	MatchPaid { Amount } if track("guard") == "never": "guarded",
	MatchPaid { Amount }: track("paid"),
	_: track("other"),
}`, options...)
	require.NoError(t, err)

	out, err := expr.Run(program, matchEnv{Order: matchOrder{Status: MatchPaid{Amount: 1}}})
	require.NoError(t, err)
	assert.Equal(t, "paid", out)
	assert.Equal(t, []string{"guard", "paid"}, calls)
}

func TestMatch_variables_do_not_leak(t *testing.T) {
	_, err := expr.Compile(`
match Order.Status {
	MatchPaid { Amount }: Amount,
	_: 0,
}; Amount`, matchOptions()...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown name Amount")

	// A binding of one arm is not visible in another arm.
	_, err = expr.Compile(`
match Order.Status {
	MatchPaid { Amount }: Amount,
	MatchPending: Amount,
	_: 0,
}`, matchOptions()...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown name Amount")
}

func TestMatch_non_exhaustive(t *testing.T) {
	_, err := expr.Compile(`
match Order.Status {
	MatchPending: 1,
	MatchPaid: 2,
}`, matchOptions()...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-exhaustive match: missing variant(s) MatchRefunded")

	// The error points at the match keyword.
	var fileErr *file.Error
	require.True(t, errors.As(err, &fileErr))
	assert.Equal(t, 2, fileErr.Line)
}

func TestMatch_guard_does_not_cover(t *testing.T) {
	_, err := expr.Compile(`
match Order.Status {
	MatchPending: 1,
	MatchPaid { Amount } if Amount > 0: 2,
	MatchRefunded { Reason } if Reason != nil: 3,
}`, matchOptions()...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-exhaustive match: missing variant(s) MatchPaid, MatchRefunded")
}

func TestMatch_unreachable_arm(t *testing.T) {
	t.Run("duplicate variant", func(t *testing.T) {
		_, err := expr.Compile(`
match Order.Status {
	MatchPending: 1,
	MatchPaid: 2,
	MatchPaid: 3,
	MatchRefunded: 4,
}`, matchOptions()...)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unreachable match arm: variant MatchPaid is already covered by a previous arm")

		var fileErr *file.Error
		require.True(t, errors.As(err, &fileErr))
		assert.Equal(t, 5, fileErr.Line)
	})

	t.Run("guarded arm after covering arm", func(t *testing.T) {
		_, err := expr.Compile(`
match Order.Status {
	MatchPaid { Amount }: Amount,
	MatchPaid { Amount } if Amount > 0: 1,
	_: 0,
}`, matchOptions()...)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unreachable match arm")
	})

	t.Run("arm after wildcard", func(t *testing.T) {
		_, err := expr.Compile(`
match Order.Status {
	_: 0,
	MatchPaid: 1,
}`, matchOptions()...)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unreachable match arm")
	})

	t.Run("wildcard after full coverage", func(t *testing.T) {
		_, err := expr.Compile(`
match Order.Status {
	MatchPending: 1,
	MatchPaid: 2,
	MatchRefunded: 3,
	_: 4,
}`, matchOptions()...)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unreachable match arm: all variants are already covered by previous arms")
	})
}

func TestMatch_unknown_variant(t *testing.T) {
	_, err := expr.Compile(`
match Order.Status {
	MatchUnknown: 1,
	_: 0,
}`, matchOptions()...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown variant MatchUnknown (expected one of: MatchPending, MatchPaid, MatchRefunded)")

	var fileErr *file.Error
	require.True(t, errors.As(err, &fileErr))
	assert.Equal(t, 3, fileErr.Line)
}

func TestMatch_unknown_field(t *testing.T) {
	_, err := expr.Compile(`
match Order.Status {
	MatchPaid { total }: total,
	_: 0,
}`, matchOptions()...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "variant MatchPaid has no field total")
}

func TestMatch_duplicate_field(t *testing.T) {
	_, err := expr.Compile(`
match Order.Status {
	MatchPaid { Amount, Amount }: 1,
	_: 0,
}`, matchOptions()...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate field Amount in match pattern")
}

func TestMatch_duplicate_binding(t *testing.T) {
	_, err := expr.Compile(`
match Order.Status {
	MatchRefunded { Reason: x, Note: x }: 1,
	_: 0,
}`, matchOptions()...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate binding x in match pattern")
}

func TestMatch_no_variant_set_registered(t *testing.T) {
	_, err := expr.Compile(`
match Order.Status {
	MatchPaid { Amount: MatchFraud { Code } }: "x",
	_: "",
}`, matchOptions()...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no variant set registered for type int of field Amount")

	_, err = expr.Compile(`
match Order.Note {
	_: 1,
}`, expr.Env(struct {
		Order struct{ Note string }
	}{}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no variant set registered for type string")
}

func TestMatch_unknown_input_type(t *testing.T) {
	_, err := expr.Compile(`
match Anything {
	_: 1,
}`, expr.Env(matchEnv{}), expr.AllowUndefinedVariables())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot use match on expression of unknown type")
}

func TestMatch_guard_must_be_bool(t *testing.T) {
	_, err := expr.Compile(`
match Order.Status {
	MatchPaid { Amount } if Amount: 1,
	_: 0,
}`, matchOptions()...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-bool expression (type int) used as match guard")
}

func TestMatch_runtime_no_matching_variant(t *testing.T) {
	program, err := expr.Compile(`
match Order.Status {
	MatchPending: "pending",
	MatchPaid { Amount }: "paid",
	MatchRefunded { Reason }: "refunded",
}`, matchOptions()...)
	require.NoError(t, err)

	_, err = expr.Run(program, matchEnv{Order: matchOrder{Status: nil}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no matching variant in match expression")
}

func TestMatch_result_type(t *testing.T) {
	program, err := expr.Compile(`
match Order.Status {
	MatchPaid { Amount }: Amount,
	_: 0,
}`, append(matchOptions(), expr.AsInt())...)
	require.NoError(t, err)

	out, err := expr.Run(program, matchEnv{Order: matchOrder{Status: MatchPaid{Amount: 42}}})
	require.NoError(t, err)
	assert.Equal(t, 42, out)
}

func TestMatch_nested_match(t *testing.T) {
	code := `
match Order.Status {
	MatchPaid { Amount }: match Order.Status {
		MatchPaid { Amount: total } if total > 100: "big",
		_: "small",
	},
	_: "other",
}`
	out, err := matchRun(t, code, matchEnv{Order: matchOrder{Status: MatchPaid{Amount: 500}}})
	require.NoError(t, err)
	assert.Equal(t, "big", out)
}

func TestMatch_disable_match_operator(t *testing.T) {
	env := map[string]any{"match": 41}
	program, err := expr.Compile(`match + 1`, expr.Env(env), expr.DisableMatchOperator())
	require.NoError(t, err)

	out, err := expr.Run(program, env)
	require.NoError(t, err)
	assert.Equal(t, 42, out)
}

func TestMatch_variant_set_validation(t *testing.T) {
	t.Run("first argument must be a nil pointer to interface", func(t *testing.T) {
		assert.Panics(t, func() {
			expr.VariantSet(MatchPending{}, MatchPaid{})
		})
	})

	t.Run("variant must implement the base interface", func(t *testing.T) {
		assert.Panics(t, func() {
			expr.VariantSet((*matchStatus)(nil), 42)
		})
	})

	t.Run("duplicate variant name", func(t *testing.T) {
		assert.Panics(t, func() {
			expr.VariantSet((*matchStatus)(nil), MatchPaid{}, expr.Variant("MatchPaid", MatchPending{}))
		})
	})

	t.Run("duplicate variant type", func(t *testing.T) {
		assert.Panics(t, func() {
			expr.VariantSet((*matchStatus)(nil), MatchPaid{}, expr.Variant("paid", MatchPaid{}))
		})
	})
}

func TestMatch_inside_predicate(t *testing.T) {
	type env struct {
		Orders []matchOrder
	}
	program, err := expr.Compile(`
map(Orders, match .Status {
	MatchPaid { Amount }: Amount,
	_: 0,
})`,
		expr.Env(env{}),
		expr.VariantSet((*matchStatus)(nil), MatchPending{}, MatchPaid{}, MatchRefunded{}),
	)
	require.NoError(t, err)

	out, err := expr.Run(program, env{Orders: []matchOrder{
		{Status: MatchPaid{Amount: 1}},
		{Status: MatchPending{}},
		{Status: MatchPaid{Amount: 2}},
	}})
	require.NoError(t, err)
	assert.Equal(t, []any{1, 0, 2}, out)
}

func TestMatch_without_checker(t *testing.T) {
	// expr.Eval does not run the checker; match expressions require
	// expr.Compile with registered variant sets.
	_, err := expr.Eval(`match a { B: 1, _: 2 }`, map[string]any{"a": 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "match expression is not checked")
}
