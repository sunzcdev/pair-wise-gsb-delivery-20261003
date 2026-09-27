package expr_test

import (
	"fmt"
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/internal/testify/assert"
	"github.com/expr-lang/expr/internal/testify/require"
)

// pureRiskEnv is the input of the archived risk-control rule.
type pureRiskEnv struct {
	Amount    float64
	AvgAmount float64
	Country   string
	Age       int
	Tags      []string
}

// IsAdult is promoted to an expression function by expr.Env; in pure rule
// mode it still requires an explicit host declaration.
func (e pureRiskEnv) IsAdult() bool { return e.Age >= 18 }

func pureRiskOptions() []expr.Option {
	return []expr.Option{
		expr.Env(pureRiskEnv{}),
		expr.Function("riskScore", func(params ...any) (any, error) {
			amount := params[0].(float64)
			country := params[1].(string)
			score := int(amount / 10)
			if country == "CN" {
				score += 10
			}
			return score, nil
		}),
		// auditLog stands in for a host function with side effects; it is
		// rejected while undeclared and accepted once the host declares it.
		expr.Function("auditLog", func(params ...any) (any, error) {
			return true, nil
		}),
		expr.Pure("riskScore"),
	}
}

func TestPure_reproducible_rule(t *testing.T) {
	rule := `riskScore(Amount, Country) >= 50 && len(Tags) > 0 && abs(Amount - AvgAmount) < 1000 && Age >= 18`

	program, err := expr.Compile(rule, pureRiskOptions()...)
	require.NoError(t, err)

	env := pureRiskEnv{
		Amount:    800,
		AvgAmount: 100,
		Country:   "CN",
		Age:       30,
		Tags:      []string{"vip"},
	}

	// The same program must replay deterministically for archived rules.
	for i := 0; i < 3; i++ {
		out, err := expr.Run(program, env)
		require.NoError(t, err)
		require.Equal(t, true, out)
	}
}

func TestPure_rejected_calls(t *testing.T) {
	tests := []struct {
		name     string
		rule     string
		contains []string
	}{
		{
			name:     "undeclared function",
			rule:     `auditLog(Amount)`,
			contains: []string{`pure mode: call to undeclared function "auditLog" is not allowed`, `call chain: auditLog(...)`},
		},
		{
			name:     "undeclared function nested in builtin",
			rule:     `len(filter(Tags, auditLog(#))) > 0`,
			contains: []string{`undeclared function "auditLog"`, `call chain: len(...) -> filter(...) -> auditLog(...)`},
		},
		{
			name:     "builtin reading current time",
			rule:     `now() > date("2026-01-01")`,
			contains: []string{`pure mode: builtin "now" reads external state and is not allowed`, `call chain: now(...)`},
		},
		{
			name:     "env method as function",
			rule:     `IsAdult()`,
			contains: []string{`pure mode: call to undeclared function "IsAdult" is not allowed`},
		},
		{
			name:     "indirect call through let variable",
			rule:     `let f = riskScore; f(Amount, Country) > 0`,
			contains: []string{`pure mode: indirect call of variable "f" is not allowed`, `call chain: f(...)`},
		},
		{
			name:     "call introduced by operator overloading",
			rule:     `Country == "CN"`,
			contains: []string{`pure mode: call to undeclared function "deepEq" is not allowed`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := pureRiskOptions()
			if tt.name == "call introduced by operator overloading" {
				options = append(options,
					expr.Function("deepEq", func(params ...any) (any, error) {
						return params[0] == params[1], nil
					}, new(func(string, string) bool)),
					expr.Operator("==", "deepEq"),
				)
			}
			_, err := expr.Compile(tt.rule, options...)
			require.Error(t, err)
			for _, want := range tt.contains {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

// IsAdult is an object method: never allowed in pure rule mode.
type pureUser struct {
	Age int
}

func (u pureUser) IsAdult() bool { return u.Age >= 18 }

func TestPure_object_method_rejected(t *testing.T) {
	type env struct {
		User pureUser
	}
	_, err := expr.Compile(`User.Age >= 18`, expr.Env(env{}), expr.Pure())
	require.NoError(t, err)

	// Methods cannot be declared pure; calls through objects are rejected.
	_, err = expr.Compile(`User.IsAdult()`, expr.Env(env{}), expr.Pure())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `pure mode: method call "IsAdult" is not allowed`)
	assert.Contains(t, err.Error(), `call chain: IsAdult(...)`)
}

func TestPure_impure_builtin_rejected_even_if_declared(t *testing.T) {
	// The host declaration is a trust boundary for host functions, but the
	// library knows now() reads the current time and always rejects it.
	_, err := expr.Compile(`now() > date("2026-01-01")`, expr.Env(pureRiskEnv{}), expr.Pure("now"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `builtin "now" reads external state`)
}

func TestPure_host_declaration_is_trust_boundary(t *testing.T) {
	// auditLog is declared pure by the host, so it compiles even though the
	// library cannot verify the absence of side effects.
	options := append(pureRiskOptions(), expr.Pure("auditLog"))
	program, err := expr.Compile(`auditLog(Amount) && riskScore(Amount, Country) > 0`, options...)
	require.NoError(t, err)

	out, err := expr.Run(program, pureRiskEnv{Amount: 800, Country: "CN"})
	require.NoError(t, err)
	require.Equal(t, true, out)
}

func TestPure_const_expr_must_be_declared(t *testing.T) {
	env := map[string]any{
		"compute": func(n int) int { return n * 2 },
	}

	// Undeclared const expression functions are rejected during the
	// optimization phase, as they may execute host code at compile time.
	_, err := expr.Compile(`1 + 1`,
		expr.Env(env),
		expr.ConstExpr("compute"),
		expr.Pure(),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `pure mode: const expression "compute" must be declared as a pure function`)

	// Declared const expression functions are folded as usual.
	program, err := expr.Compile(`compute(21) > 40`,
		expr.Env(env),
		expr.ConstExpr("compute"),
		expr.Pure("compute"),
	)
	require.NoError(t, err)
	out, err := expr.Run(program, env)
	require.NoError(t, err)
	require.Equal(t, true, out)
}

func TestPure_enforced_after_optimization(t *testing.T) {
	// With constant folding enabled, the pure rule still compiles and
	// replays; the folded tree is re-validated after optimization.
	program, err := expr.Compile(`riskScore(Amount, Country) > 1 + 1 && upper(Country) == "CN"`, pureRiskOptions()...)
	require.NoError(t, err)

	out, err := expr.Run(program, pureRiskEnv{Amount: 800, Country: "CN"})
	require.NoError(t, err)
	require.Equal(t, true, out)
}

func ExamplePure() {
	program, err := expr.Compile(
		`riskScore(Amount) >= 50 && len(Tags) > 0`,
		expr.Env(map[string]any{"Amount": 0.0, "Tags": []string{}}),
		expr.Function("riskScore", func(params ...any) (any, error) {
			return int(params[0].(float64) / 10), nil
		}),
		expr.Pure("riskScore"),
	)
	if err != nil {
		fmt.Println(err)
		return
	}
	out, err := expr.Run(program, map[string]any{"Amount": 800.0, "Tags": []string{"vip"}})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(out)
	// Output: true
}
