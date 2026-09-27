package expr_test

import (
	"testing"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/internal/testify/assert"
	"github.com/expr-lang/expr/internal/testify/require"
)

// pureRuleEnv is a risk-control environment: a rule over it must be
// archivable and re-evaluated later with the exact same result.
type pureRuleEnv struct {
	Amount    float64
	Limit     float64
	Score     int
	Threshold int
	Events    []int
	CreatedAt time.Time
	Callback  func() int
	Handlers  []func() int
}

func pureRuleBaseOptions() []expr.Option {
	boost := expr.Function(
		"boost",
		func(params ...any) (any, error) {
			return params[0].(int) * 2, nil
		},
		new(func(int) int),
	)
	return []expr.Option{
		expr.Env(pureRuleEnv{}),
		boost,
		expr.AsBool(),
	}
}

// A rule built only from host-declared pure functions compiles in pure rule
// mode and can be re-evaluated (复算) with a deterministic result.
func TestPureRule_replayable(t *testing.T) {
	rule := `Amount <= Limit && Score + boost(Score) >= Threshold && all(Events, # > 0)`

	opts := append(pureRuleBaseOptions(), expr.PureRule("boost", "all"))
	program, err := expr.Compile(rule, opts...)
	require.NoError(t, err)

	env := pureRuleEnv{
		Amount:    100,
		Limit:     200,
		Score:     30,
		Threshold: 90,
		Events:    []int{1, 2, 3},
	}

	first, err := expr.Run(program, env)
	require.NoError(t, err)
	second, err := expr.Run(program, env)
	require.NoError(t, err)

	assert.Equal(t, true, first)
	assert.Equal(t, first, second)
}

// Calls that could read the current time, randomness, or external state —
// directly or through indirection — are rejected, and the error points at
// the call chain that broke the constraint.
func TestPureRule_rejected(t *testing.T) {
	tests := []struct {
		name     string
		rule     string
		allowed  []string
		contains []string
	}{
		{
			name:    "undeclared builtin reads current time",
			rule:    `now().Unix() > 0`,
			allowed: nil,
			contains: []string{
				"pure rule violation",
				`call to builtin "now" is not allowed`,
				"call chain: now",
			},
		},
		{
			name:    "undeclared custom function",
			rule:    `boost(Score) > Threshold`,
			allowed: nil,
			contains: []string{
				`call to function "boost" is not allowed`,
				"call chain: boost",
			},
		},
		{
			name:    "env function not declared as pure",
			rule:    `Callback() > 0`,
			allowed: nil,
			contains: []string{
				`call to function "Callback" is not allowed`,
				"call chain: Callback",
			},
		},
		{
			name:    "object method call",
			rule:    `CreatedAt.After(CreatedAt)`,
			allowed: nil,
			contains: []string{
				`method call "After" is not allowed`,
				"call chain: After",
			},
		},
		{
			name:    "indirect call through variable",
			rule:    `let f = Callback; f() > 0`,
			allowed: nil,
			contains: []string{
				`indirect call through "f" is not allowed`,
				"call chain: f",
			},
		},
		{
			name:    "indirect call through array element",
			rule:    `let f = Handlers[0]; f() > 0`,
			allowed: nil,
			contains: []string{
				`indirect call through "f" is not allowed`,
				"call chain: f",
			},
		},
		{
			name:    "impure call nested inside an allowed builtin",
			rule:    `all(Events, # > now().Unix())`,
			allowed: []string{"all"},
			contains: []string{
				`call to builtin "now" is not allowed`,
				"call chain: all -> now",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := append(pureRuleBaseOptions(), expr.PureRule(tt.allowed...))

			// The restriction holds with and without the optimizer.
			for _, optimize := range []bool{true, false} {
				_, err := expr.Compile(tt.rule, append(opts, expr.Optimize(optimize))...)
				require.Error(t, err, "optimize=%v", optimize)
				for _, want := range tt.contains {
					assert.ErrorContains(t, err, want, "optimize=%v", optimize)
				}
			}
		})
	}
}

// Operator overloading rewrites `Score + 1` into `add(Score, 1)` before type
// checking. Pure rule mode checks the typed, patched AST — not the source
// text — so the introduced call must be declared as pure as well.
func TestPureRule_operator_overloading(t *testing.T) {
	add := expr.Function(
		"add",
		func(params ...any) (any, error) {
			return params[0].(int) + params[1].(int), nil
		},
		new(func(int, int) int),
	)

	_, err := expr.Compile(
		`Score + 1 > Threshold`,
		expr.Env(pureRuleEnv{}),
		add,
		expr.Operator("+", "add"),
		expr.PureRule(),
	)
	require.Error(t, err)
	assert.ErrorContains(t, err, `call to function "add" is not allowed`)
	assert.ErrorContains(t, err, "call chain: add")

	program, err := expr.Compile(
		`Score + 1 > Threshold`,
		expr.Env(pureRuleEnv{}),
		add,
		expr.Operator("+", "add"),
		expr.PureRule("add"),
	)
	require.NoError(t, err)

	out, err := expr.Run(program, pureRuleEnv{Score: 42, Threshold: 42})
	require.NoError(t, err)
	assert.Equal(t, true, out)
}

// Without pure rule mode the same expressions compile as before.
func TestPureRule_disabled_by_default(t *testing.T) {
	program, err := expr.Compile(`now().Unix() > 0`, expr.Env(pureRuleEnv{}))
	require.NoError(t, err)

	out, err := expr.Run(program, pureRuleEnv{})
	require.NoError(t, err)
	assert.Equal(t, true, out)
}
