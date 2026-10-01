package match_test

import (
	"strings"
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/internal/testify/assert"
	"github.com/expr-lang/expr/internal/testify/require"
	"github.com/expr-lang/expr/variants"
	"github.com/expr-lang/expr/vm"
)

// Shape is a closed set of geometric shapes.
type Shape interface{ shape() }

type Circle struct{ Radius int }
type Square struct {
	Side int
	Tag  string
}
type Point struct{}

func (*Circle) shape() {}
func (*Square) shape() {}
func (*Point) shape()  {}

func registry() *variants.Registry {
	return variants.NewRegistry(
		variants.New((*Shape)(nil)).
			Case("Circle", Circle{}).
			Case("Square", Square{}).
			Case("Point", Point{}).MustBuild(),
	)
}

func compile(t *testing.T, code string) *vm.Program {
	t.Helper()
	program, err := expr.Compile(code,
		expr.Env(map[string]any{"shape": (*Shape)(nil), "n": 0}),
		expr.Variants(registry()),
	)
	require.NoError(t, err)
	return program
}

func runWith(program *vm.Program, shape any) (any, error) {
	return expr.Run(program, map[string]any{"shape": shape, "n": 0})
}

func TestMatch_basic(t *testing.T) {
	program := compile(t, `
match shape {
  Circle{Radius: r} -> { 3 * r * r }
  Square{Side: s} -> { s * s }
  Point -> { 0 }
}`)

	tests := []struct {
		shape any
		want  any
	}{
		{&Circle{Radius: 2}, 12},
		{&Square{Side: 3, Tag: "x"}, 9},
		{&Point{}, 0},
	}
	for _, tt := range tests {
		out, err := runWith(program, tt.shape)
		require.NoError(t, err)
		assert.Equal(t, tt.want, out)
	}
}

func TestMatch_guard(t *testing.T) {
	program := compile(t, `
match shape {
  Square{Side: s, Tag: tag} if s > 10 -> { "big " + tag }
  Square{Side: s} -> { "small" }
  Circle -> { "circle" }
  Point -> { "point" }
}`)

	out, err := expr.Run(program, map[string]any{"shape": &Square{Side: 20, Tag: "A"}, "n": 0})
	require.NoError(t, err)
	assert.Equal(t, "big A", out)

	out, err = expr.Run(program, map[string]any{"shape": &Square{Side: 2, Tag: "A"}, "n": 0})
	require.NoError(t, err)
	assert.Equal(t, "small", out)
}

func TestMatch_wildcard_binding(t *testing.T) {
	program := compile(t, `
match shape {
  Circle{Radius: r} -> { r }
  other -> { -1 }
}`)
	out, err := runWith(program, &Square{Side: 1})
	require.NoError(t, err)
	assert.Equal(t, -1, out)

	out, err = runWith(program, &Circle{Radius: 7})
	require.NoError(t, err)
	assert.Equal(t, 7, out)
}

func TestMatch_value_receiver(t *testing.T) {
	program := compile(t, `
match shape {
  Point -> { "point" }
  Circle -> { "circle" }
  Square -> { "square" }
}`)
	out, err := runWith(program, Point{})
	require.NoError(t, err)
	assert.Equal(t, "point", out)
}

func TestMatch_missing_variant_error(t *testing.T) {
	_, err := expr.Compile(`
match shape {
  Circle{Radius: r} -> { r }
  Square{Side: s} -> { s }
}`,
		expr.Env(map[string]any{"shape": (*Shape)(nil)}),
		expr.Variants(registry()),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing variant(s)")
	assert.Contains(t, err.Error(), `"Point"`)
	assert.Contains(t, err.Error(), "match shape")
	// The error must bind to a source location and render a snippet.
	assert.True(t, strings.Contains(err.Error(), "(2:1)"), "error should carry a location: %v", err)
	assert.True(t, strings.Contains(err.Error(), "\n |  ^") || strings.Contains(err.Error(), "\n | ^"),
		"error should render a source snippet: %v", err)
}

func TestMatch_unreachable_arm_error(t *testing.T) {
	_, err := expr.Compile(`
match shape {
  _ -> { 1 }
  Point -> { 2 }
}`,
		expr.Env(map[string]any{"shape": (*Shape)(nil)}),
		expr.Variants(registry()),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unreachable match arm")
	assert.Contains(t, err.Error(), "Point")
}

func TestMatch_guarded_arm_does_not_cover(t *testing.T) {
	_, err := expr.Compile(`
match shape {
  x if n > 0 -> { 1 }
  Circle -> { 2 }
  Square -> { 3 }
}`,
		expr.Env(map[string]any{"shape": (*Shape)(nil), "n": 0}),
		expr.Variants(registry()),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"Point"`)

	// A guarded Point followed by an unconditional Point is reachable.
	program := compile(t, `
match shape {
  Circle -> { 2 }
  Square -> { 3 }
  Point if n > 0 -> { 4 }
  Point -> { 5 }
}`)
	out, err := runWith(program, &Point{})
	require.NoError(t, err)
	assert.Equal(t, 5, out)
}

func TestMatch_unknown_variant_error(t *testing.T) {
	_, err := expr.Compile(`
match shape {
  Circle{Radius: r} -> { r }
  Hexagon{} -> { 0 }
}`,
		expr.Env(map[string]any{"shape": (*Shape)(nil)}),
		expr.Variants(registry()),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown variant "Hexagon"`)
}

func TestMatch_unknown_field_error(t *testing.T) {
	_, err := expr.Compile(`
match shape {
  Circle{Raidus: r} -> { r }
  _ -> { 0 }
}`,
		expr.Env(map[string]any{"shape": (*Shape)(nil)}),
		expr.Variants(registry()),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `has no field "Raidus"`)
}

func TestMatch_lazy_branch_evaluation(t *testing.T) {
	var circleBody, pointBody int
	probe := func(which int) bool {
		if which == 1 {
			circleBody++
		} else {
			pointBody++
		}
		return true
	}
	env := map[string]any{
		"shape": (*Shape)(nil),
		"probe": probe,
	}
	program, err := expr.Compile(`
match shape {
  Circle -> { probe(1) }
  Point -> { probe(2) }
  Square -> { false }
}`, expr.Env(env), expr.Variants(registry()))
	require.NoError(t, err)

	_, err = expr.Run(program, map[string]any{"shape": &Circle{}, "probe": probe})
	require.NoError(t, err)
	assert.Equal(t, 1, circleBody)
	assert.Equal(t, 0, pointBody)
}

func TestMatch_bindings_do_not_leak(t *testing.T) {
	program := compile(t, `
(match shape {
  Circle{Radius: r} -> { r }
  Point -> { 0 }
  Square{Side: s} -> { s }
}) + (match shape {
  Circle{Radius: r2} -> { r2 }
  _ -> { 7 }
})`)
	out, err := runWith(program, &Circle{Radius: 4})
	require.NoError(t, err)
	assert.Equal(t, 8, out)
}

func TestMatch_guard_non_bool_error(t *testing.T) {
	_, err := expr.Compile(`
match shape {
  Circle if 1 -> { 0 }
  _ -> { 1 }
}`,
		expr.Env(map[string]any{"shape": (*Shape)(nil)}),
		expr.Variants(registry()),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "guard must be boolean")
}

func TestMatch_partial_struct_coverage(t *testing.T) {
	// A partial Square pattern still covers the Square variant.
	program := compile(t, `
match shape {
  Square{Side: s} -> { s }
  Circle -> { -1 }
  Point -> { 0 }
}`)
	out, err := runWith(program, &Square{Side: 5, Tag: "y"})
	require.NoError(t, err)
	assert.Equal(t, 5, out)
}

func TestMatch_nested_guarded_variant(t *testing.T) {
	// A guarded nested variant must not cover Square unconditionally, while an
	// irrefutable nested pattern covers it.
	program := compile(t, `
match shape {
  Square{Tag: "special", Side: s} if s > 100 -> { -s }
  Square{Side: s} -> { s }
  Circle -> { 0 }
  Point -> { 0 }
}`)
	out, err := runWith(program, &Square{Side: 5, Tag: "special"})
	require.NoError(t, err)
	assert.Equal(t, 5, out)

	out, err = runWith(program, &Square{Side: 200, Tag: "special"})
	require.NoError(t, err)
	assert.Equal(t, -200, out)

	// Guarded-only nested pattern leaves Square covered by the next Square arm.
	out, err = runWith(program, &Square{Side: 1, Tag: "other"})
	require.NoError(t, err)
	assert.Equal(t, 1, out)
}

func TestMatch_nil_pattern(t *testing.T) {
	program := compile(t, `
match shape {
  nil -> { "nil" }
  Point -> { "point" }
  Circle -> { "circle" }
  Square -> { "square" }
}`)
	out, err := runWith(program, nil)
	require.NoError(t, err)
	assert.Equal(t, "nil", out)

	out, err = runWith(program, &Point{})
	require.NoError(t, err)
	assert.Equal(t, "point", out)
}

func TestMatch_no_match_runtime_error(t *testing.T) {
	program, err := expr.Compile(`match n {
  1 -> { "one" }
  2 -> { "two" }
}`, expr.Env(map[string]any{"n": 0}))
	require.NoError(t, err)
	_, err = expr.Run(program, map[string]any{"n": 3})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no match arm matched")
}

func TestVariants_registration_errors(t *testing.T) {
	_, err := variants.New((*Shape)(nil)).
		Case("Circle", Circle{}).
		Case("Circle", Point{}).
		Build()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate variant name "Circle"`)

	type notShape struct{}
	_, err = variants.New((*Shape)(nil)).Case("X", notShape{}).Build()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not implement")

	assert.Panics(t, func() {
		variants.NewRegistry(
			variants.New((*Shape)(nil)).Case("Circle", Circle{}).MustBuild(),
			variants.New((*Shape)(nil)).Case("Circle", Point{}).MustBuild(),
		)
	})
}

func TestMatch_scalar_literal(t *testing.T) {
	program, err := expr.Compile(`
match n {
  1 -> { "one" }
  2 -> { "two" }
  x -> { "other" }
}`, expr.Env(map[string]any{"n": 0}))
	require.NoError(t, err)

	for _, tt := range []struct {
		n    int
		want string
	}{{1, "one"}, {2, "two"}, {9, "other"}} {
		out, err := expr.Run(program, map[string]any{"n": tt.n})
		require.NoError(t, err)
		assert.Equal(t, tt.want, out)
	}
}
