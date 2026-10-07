package participle_test

import (
	"errors"
	"math/rand/v2"
	"strings"
	"testing"

	require "github.com/alecthomas/assert/v2"
	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

func testGenerateLexer(t *testing.T) *lexer.StatefulDefinition {
	t.Helper()
	return lexer.MustSimple([]lexer.SimpleRule{
		{Name: "Whitespace", Pattern: `\s+`},
		{Name: "Ident", Pattern: `[a-z]+`},
		{Name: "Number", Pattern: `[0-9]+`},
		{Name: "Punct", Pattern: `[()+*;]`},
	})
}

// fixedRenderer returns deterministic, lexer-valid text for named tokens.
func fixedRenderer(name string, _ *rand.Rand) (string, error) {
	switch name {
	case "Ident":
		return "x", nil
	case "Number":
		return "1", nil
	default:
		return "", errors.New("unexpected token " + name)
	}
}

// genExpr is a recursive grammar: a number or "(" expr ")".
//
//	Expr = Number | "(" Expr ")" .
type genExpr struct {
	Number int      `@Number |`
	Sub    *genExpr `"(" @@ ")"`
}

func TestGenerate_Recursive_DeterministicAndBounded(t *testing.T) {
	parser, err := participle.Build[genExpr](participle.Lexer(testGenerateLexer(t)), participle.Elide("Whitespace"))
	require.NoError(t, err)

	var first *participle.Generated
	for seed := uint64(0); seed < 200; seed++ {
		gen, err := parser.Generate(seed, 25, fixedRenderer)
		require.NoError(t, err, "seed", seed)

		// Generate re-parses internally; assert the round trip explicitly too.
		_, err = parser.ParseString("", gen.Text)
		require.NoError(t, err, "seed", seed, "text", gen.Text)

		require.True(t, len(gen.Tokens) <= 25, "seed %d produced %d tokens: %q", seed, len(gen.Tokens), gen.Text)
		require.True(t, len(gen.Path) >= 1, "seed", seed)

		if seed == 0 {
			first = gen
		}
	}
	require.NotZero(t, first)

	// The same seed must reproduce identical text and identical choice path.
	again, err := parser.Generate(0, 25, fixedRenderer)
	require.NoError(t, err)
	require.Equal(t, first.Text, again.Text)
	require.Equal(t, first.Path, again.Path)
	require.Equal(t, first.Tokens, again.Tokens)

	// A single number is the only derivation that fits one token.
	one, err := parser.Generate(1, 1, fixedRenderer)
	require.NoError(t, err)
	require.Equal(t, "1", one.Text)
	require.Equal(t, 1, len(one.Tokens))
}

// genNullableLoop contains a nullable repetition { ... } whose body costs three
// tokens per iteration.
//
//	List = { "(" Number ")" } .
type genNullableLoop struct {
	Items []genLoopItem `@@*`
}

type genLoopItem struct {
	Open   string `"("`
	Number int    `@Number`
	Close  string `")"`
}

// TestGenerate_NullableLoop_LengthIsTokenBounded proves maxTokens bounds the number
// of emitted terminals rather than merely counting recursion. Each loop iteration is
// a single structural step but emits three tokens, so a step/recursion counter would
// happily allow many iterations; the token budget must reject a second iteration at
// budget 4 and cap total tokens at 3*k.
func TestGenerate_NullableLoop_LengthIsTokenBounded(t *testing.T) {
	parser, err := participle.Build[genNullableLoop](participle.Lexer(testGenerateLexer(t)), participle.Elide("Whitespace"))
	require.NoError(t, err)

	// Zero tokens is a valid derivation: the loop is nullable.
	empty, err := parser.Generate(0, 0, fixedRenderer)
	require.NoError(t, err)
	require.Equal(t, "", empty.Text)
	require.Equal(t, 0, len(empty.Tokens))
	require.Equal(t, 1, len(empty.Path))
	require.Equal(t, participle.ChoiceRepetition, empty.Path[0].Kind)
	require.Equal(t, 0, empty.Path[0].Chosen)

	// Budget below a single full iteration forces zero iterations.
	below, err := parser.Generate(2, 2, fixedRenderer)
	require.NoError(t, err)
	require.Equal(t, 0, below.Path[0].Chosen, "no full iteration should fit a budget of 2")
	require.Equal(t, 0, len(below.Tokens))

	// Budget 4 admits one full iteration (3 tokens) but never a second: the cap is
	// measured in tokens, not in loop steps, so no seed may emit 6 tokens here.
	small4, err := parser.Generate(2, 4, fixedRenderer)
	require.NoError(t, err)
	require.True(t, small4.Path[0].Chosen <= 1, "got %d iterations at budget 4", small4.Path[0].Chosen)
	require.True(t, len(small4.Tokens) <= 4)
	for seed := uint64(0); seed < 128; seed++ {
		gen, err := parser.Generate(seed, 4, fixedRenderer)
		require.NoError(t, err)
		require.True(t, len(gen.Tokens) <= 4, "seed %d emitted %d tokens at budget 4", seed, len(gen.Tokens))
	}

	// Across many seeds the terminal count is always a multiple of three and never
	// exceeds the budget, even with a generous structural step allowance.
	for seed := uint64(0); seed < 300; seed++ {
		for _, budget := range []int{3, 6, 7, 9, 20} {
			gen, err := parser.Generate(seed, budget, fixedRenderer, participle.GenerateMaxSteps(512))
			require.NoError(t, err, "seed", seed, "budget", budget)
			require.True(t, len(gen.Tokens) <= budget,
				"seed %d budget %d emitted %d tokens: %q", seed, budget, len(gen.Tokens), gen.Text)
			require.Equal(t, 0, len(gen.Tokens)%3, "token count must be whole iterations of 3")
			_, err = parser.ParseString("", gen.Text)
			require.NoError(t, err)
		}
	}

	// At budget 6 a two-iteration, six-token sentence is reachable; a nine-token
	// sentence never is.
	foundTwo := false
	for seed := uint64(0); seed < 64; seed++ {
		gen, err := parser.Generate(seed, 6, fixedRenderer)
		require.NoError(t, err)
		if len(gen.Tokens) == 6 {
			foundTwo = true
			require.Equal(t, "( 1 ) ( 1 )", gen.Text)
			require.Equal(t, 2, gen.Path[0].Chosen)
		}
	}
	require.True(t, foundTwo, "expected some seed to pick two iterations at budget 6")

	// Raising the structural step limit dramatically does not lift the token cap.
	withSteps, err := parser.Generate(7, 9, fixedRenderer, participle.GenerateMaxSteps(1_000_000))
	require.NoError(t, err)
	require.True(t, len(withSteps.Tokens) <= 9)
}

func TestGenerate_NoDerivationWithinBudget(t *testing.T) {
	// Every sentence requires at least the two literal tokens.
	type required struct {
		A string `"a"`
		B string `"b"`
	}
	parser, err := participle.Build[required](participle.Lexer(testGenerateLexer(t)), participle.Elide("Whitespace"))
	require.NoError(t, err)

	_, err = parser.Generate(0, 1, fixedRenderer)
	require.Error(t, err)
	require.True(t, errors.Is(err, participle.ErrNoDerivation), "got %v", err)
	require.True(t, strings.Contains(err.Error(), "needs at least 2 token(s)"), "got %v", err)

	gen, err := parser.Generate(0, 2, fixedRenderer)
	require.NoError(t, err)
	require.Equal(t, "a b", gen.Text)
}

// genOpaque embeds a production the grammar cannot analyse.
type genOpaque struct {
	Head string            `"head"`
	Body *genParseableBody `@@`
}

type genParseableBody struct {
	Numbers []string
}

func (g *genParseableBody) Parse(lex *lexer.PeekingLexer) error {
	for {
		tok := lex.Next()
		if tok.EOF() {
			return nil
		}
		g.Numbers = append(g.Numbers, tok.Value)
	}
}

func TestGenerate_ParseableRequiresAdapter(t *testing.T) {
	lex := lexer.MustSimple([]lexer.SimpleRule{
		{Name: "Whitespace", Pattern: `\s+`},
		{Name: "Word", Pattern: `[a-z]+`},
	})
	parser, err := participle.Build[genOpaque](participle.Lexer(lex), participle.Elide("Whitespace"))
	require.NoError(t, err)

	// Without an adapter the generator refuses to invent a match.
	_, err = parser.Generate(0, 8, fixedRenderer)
	require.Error(t, err)
	require.True(t, errors.Is(err, participle.ErrUnsupportedGeneration), "got %v", err)
	require.True(t, strings.Contains(err.Error(), "genParseableBody"), "got %v", err)

	adapter := participle.Adapter{
		MinTokens: 1,
		Generate: func(r *rand.Rand) ([]participle.GeneratedToken, error) {
			count := 1 + int(r.Uint64()%3)
			out := make([]participle.GeneratedToken, count)
			for i := range out {
				out[i] = participle.GeneratedToken{Name: "Word"}
			}
			return out, nil
		},
	}
	wordRenderer := func(name string, _ *rand.Rand) (string, error) {
		if name == "Word" {
			return "num", nil
		}
		return "", errors.New("unexpected token " + name)
	}
	gen, err := parser.Generate(42, 8, wordRenderer, participle.GenerationAdapter[*genParseableBody](adapter))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(gen.Text, "head "), "got %q", gen.Text)
	require.Equal(t, "genParseableBody", gen.Tokens[1].From)
	_, err = parser.ParseString("", gen.Text)
	require.NoError(t, err)

	// Same seed reproduces the adapter output too.
	again, err := parser.Generate(42, 8, wordRenderer, participle.GenerationAdapter[*genParseableBody](adapter))
	require.NoError(t, err)
	require.Equal(t, gen.Text, again.Text)

	// An opaque production whose minimum exceeds the budget reports no derivation.
	one := participle.Adapter{
		MinTokens: 4,
		Generate: func(*rand.Rand) ([]participle.GeneratedToken, error) {
			return []participle.GeneratedToken{
				{Name: "Word"}, {Name: "Word"}, {Name: "Word"}, {Name: "Word"},
			}, nil
		},
	}
	_, err = parser.Generate(0, 4, wordRenderer, participle.GenerationAdapter[*genParseableBody](one))
	require.Error(t, err)
	require.True(t, errors.Is(err, participle.ErrNoDerivation), "got %v", err)
}

// genNegation uses a construct the generator cannot synthesise on its own.
type genNegation struct {
	Value string `@!"x"`
}

func TestGenerate_NegationUnsupported(t *testing.T) {
	parser, err := participle.Build[genNegation](participle.Lexer(testGenerateLexer(t)), participle.Elide("Whitespace"))
	require.NoError(t, err)
	_, err = parser.Generate(0, 4, fixedRenderer)
	require.Error(t, err)
	require.True(t, errors.Is(err, participle.ErrUnsupportedGeneration), "got %v", err)
}

// genAllNullable only contains optional constructs, so every derivation can emit
// zero tokens. This exercises the nullable choice path without relying on a
// nullable repetition body, which the parser itself cannot consume.
type genAllNullable struct {
	A string `["a"]?`
	B string `["b"]?`
	C string `["c"]?`
}

func TestGenerate_AllNullable_TerminatesAtZeroBudget(t *testing.T) {
	parser, err := participle.Build[genAllNullable](
		participle.Lexer(testGenerateLexer(t)), participle.Elide("Whitespace"))
	require.NoError(t, err)

	for seed := uint64(0); seed < 50; seed++ {
		gen, err := parser.Generate(seed, 0, fixedRenderer)
		require.NoError(t, err, "seed", seed)
		require.Equal(t, 0, len(gen.Tokens), "seed %d", seed)
		require.Equal(t, "", gen.Text)
		_, err = parser.ParseString("", gen.Text)
		require.NoError(t, err)
	}
}

type genCustomThing interface{ customMarker() }

type genCustomA struct {
	N int
}

func (*genCustomA) customMarker() {}

type genCustomGrammar struct {
	Thing genCustomThing `@@`
}

func TestGenerate_CustomParseTypeWith_Adapter(t *testing.T) {
	lex := lexer.MustSimple([]lexer.SimpleRule{
		{Name: "Whitespace", Pattern: `\s+`},
		{Name: "Number", Pattern: `[0-9]+`},
	})
	parser, err := participle.Build[genCustomGrammar](
		participle.Lexer(lex),
		participle.Elide("Whitespace"),
		participle.ParseTypeWith(func(pl *lexer.PeekingLexer) (genCustomThing, error) {
			tok := pl.Next()
			if tok.EOF() {
				return nil, participle.NextMatch
			}
			return &genCustomA{}, nil
		}),
	)
	require.NoError(t, err)

	// The interface is opaque: an adapter is mandatory.
	_, err = parser.Generate(0, 4, fixedRenderer)
	require.Error(t, err)
	require.True(t, errors.Is(err, participle.ErrUnsupportedGeneration), "got %v", err)

	adapter := participle.Adapter{
		MinTokens: 1,
		Generate: func(*rand.Rand) ([]participle.GeneratedToken, error) {
			return []participle.GeneratedToken{{Name: "Number"}}, nil
		},
	}
	gen, err := parser.Generate(1, 1, fixedRenderer, participle.GenerationAdapter[genCustomThing](adapter))
	require.NoError(t, err)
	require.Equal(t, "1", gen.Text)
	_, err = parser.ParseString("", gen.Text)
	require.NoError(t, err)
}

type genUnionExpr interface{ unionExprMarker() }

type genUnionNum struct {
	N int `@Number`
}

func (*genUnionNum) unionExprMarker() {}

type genUnionPair struct {
	L genUnionExpr `"(" @@`
	R genUnionExpr `@@ ")"`
}

func (*genUnionPair) unionExprMarker() {}

type genUnionRoot struct {
	Expr genUnionExpr `@@`
}

func TestGenerate_Union_BoundedAndRoundTrips(t *testing.T) {
	parser, err := participle.Build[genUnionRoot](
		participle.Lexer(testGenerateLexer(t)),
		participle.Elide("Whitespace"),
		participle.Union[genUnionExpr](&genUnionNum{}, &genUnionPair{}),
	)
	require.NoError(t, err)

	for seed := uint64(0); seed < 150; seed++ {
		gen, err := parser.Generate(seed, 20, fixedRenderer)
		require.NoError(t, err, "seed", seed)
		require.True(t, len(gen.Tokens) <= 20, "seed %d emitted %d: %q", seed, len(gen.Tokens), gen.Text)
		require.True(t, len(gen.Tokens) >= 1, "seed %d produced empty sentence", seed)
		_, err = parser.ParseString("", gen.Text)
		require.NoError(t, err, "seed", seed, gen.Text)
	}

	// Budget 1 forces the numeric member.
	one, err := parser.Generate(0, 1, fixedRenderer)
	require.NoError(t, err)
	require.Equal(t, "1", one.Text)
}
