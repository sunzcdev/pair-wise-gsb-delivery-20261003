package participle_test

import (
	"testing"

	require "github.com/alecthomas/assert/v2"
	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

type analysisAB struct {
	B bool `@"b"`
}
type analysisAC struct {
	C bool `@"c"`
}

// analysisPrefixGrammar shares a single token prefix but disambiguates with
// the token that follows. Ordered choice can still select either branch.
type analysisPrefixGrammar struct {
	AB *analysisAB `  "a" @@`
	AC *analysisAC `| "a" @@`
}

// analysisShadowGrammar places a shorter fixed production in front of a longer
// one whose entire form extends it, so the long branch is unreachable.
type analysisShadowGrammar struct {
	First  string `  @"a"`
	Second string `| @"a" @"b"`
}

// analysisNullableList is nullable through an optional self recursion. The
// nullability and first set must converge by fixed point iteration.
type analysisNullableList struct {
	Item string                `@"a"?`
	More *analysisNullableList `@@?`
}

type analysisCustomValue interface{ analysisCustom() }

type analysisCustomGrammar struct {
	Literal string              `  @"x"`
	Custom  analysisCustomValue `| @@`
}

func analysisLexer(t *testing.T) *lexer.StatefulDefinition {
	t.Helper()
	return lexer.MustSimple([]lexer.SimpleRule{
		{Name: "whitespace", Pattern: `\s+`},
		{Name: "Chars", Pattern: `[a-z]`},
	})
}

func productionByName(t *testing.T, report *participle.AnalysisReport, name string) *participle.ProductionAnalysis {
	t.Helper()
	for i := range report.Productions {
		if report.Productions[i].Name == name {
			return &report.Productions[i]
		}
	}
	t.Fatalf("production %q not found", name)
	return nil
}

func firstNames(tokens []participle.FirstToken) []string {
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		out = append(out, token.Name)
	}
	return out
}

func TestAnalysis_PrefixIsPossibleConflict(t *testing.T) {
	parser, err := participle.Build[analysisPrefixGrammar](participle.Lexer(analysisLexer(t)))
	require.NoError(t, err)

	// Parsing is unaffected by asking for the report, and both branches parse.
	ab, err := parser.ParseString("", "ab")
	require.NoError(t, err)
	require.NotZero(t, ab.AB)
	ac, err := parser.ParseString("", "ac")
	require.NoError(t, err)
	require.NotZero(t, ac.AC)

	report := parser.Analysis()
	prod := productionByName(t, report, "AnalysisPrefixGrammar")
	require.False(t, prod.Nullable)
	require.True(t, prod.NullableKnown)
	require.Equal(t, []string{`"a"`}, firstNames(prod.First))
	require.True(t, prod.FirstKnown)

	require.Equal(t, 1, len(report.Diagnostics))
	finding := report.Diagnostics[0]
	require.Equal(t, participle.DiagnosticPossibleConflict, finding.Kind)
	require.Equal(t, []string{`"a"`}, firstNames(finding.Overlapping))
	require.Equal(t, 1, finding.Earlier)
	require.Equal(t, 2, finding.Later)
	require.False(t, finding.Unknown)
}

func TestAnalysis_ShadowIsDefinite(t *testing.T) {
	parser, err := participle.Build[analysisShadowGrammar](participle.Lexer(analysisLexer(t)))
	require.NoError(t, err)

	report := parser.Analysis()
	require.Equal(t, 1, len(report.Diagnostics))
	finding := report.Diagnostics[0]
	require.Equal(t, participle.DiagnosticShadow, finding.Kind)
	require.Equal(t, 1, finding.Earlier)
	require.Equal(t, 2, finding.Later)
	require.Equal(t, []string{`"a"`}, firstNames(finding.Overlapping))

	// The shadow is a static report, not a build or parse error.
	v, err := parser.ParseString("", "ab", participle.AllowTrailing(true))
	require.NoError(t, err)
	require.Equal(t, "a", v.First)
	require.Zero(t, v.Second)
}

func TestAnalysis_NullableRecursionConverges(t *testing.T) {
	parser, err := participle.Build[analysisNullableList](participle.Lexer(analysisLexer(t)))
	require.NoError(t, err)

	report := parser.Analysis()
	prod := productionByName(t, report, "AnalysisNullableList")
	require.True(t, prod.Nullable)
	require.True(t, prod.NullableKnown)
	require.Equal(t, []string{`"a"`}, firstNames(prod.First))
	require.True(t, prod.FirstKnown)

	for _, finding := range report.Diagnostics {
		require.NotEqual(t, participle.DiagnosticShadow, finding.Kind)
	}
}

func TestAnalysis_CustomProductionIsUnknown(t *testing.T) {
	parser, err := participle.Build[analysisCustomGrammar](
		participle.Lexer(analysisLexer(t)),
		participle.ParseTypeWith[analysisCustomValue](func(*lexer.PeekingLexer) (analysisCustomValue, error) {
			return nil, nil
		}),
	)
	require.NoError(t, err)

	report := parser.Analysis()
	root := productionByName(t, report, "AnalysisCustomGrammar")
	require.False(t, root.NullableKnown)
	require.False(t, root.FirstKnown)

	custom := productionByName(t, report, "AnalysisCustomValue")
	require.Equal(t, "custom", custom.Kind)
	require.False(t, custom.NullableKnown)
	require.False(t, custom.FirstKnown)

	require.Equal(t, 1, len(report.Diagnostics))
	finding := report.Diagnostics[0]
	require.Equal(t, participle.DiagnosticPossibleConflict, finding.Kind)
	require.True(t, finding.Unknown)
}

func TestAnalysis_DisjointBranchesHaveNoFindings(t *testing.T) {
	type disjointGrammar struct {
		A string `  "a"`
		B string `| "b"`
	}
	parser, err := participle.Build[disjointGrammar](participle.Lexer(analysisLexer(t)))
	require.NoError(t, err)

	report := parser.Analysis()
	require.Equal(t, 0, len(report.Diagnostics))
}

// A nullable first alternative accepts the empty prefix, masking everything
// ordered after it.
func TestAnalysis_NullableFirstBranchShadows(t *testing.T) {
	type nullableFirst struct {
		Optional string `  @"a"?`
		Required string `| @"b"`
	}
	parser, err := participle.Build[nullableFirst](participle.Lexer(analysisLexer(t)))
	require.NoError(t, err)

	report := parser.Analysis()
	require.Equal(t, 1, len(report.Diagnostics))
	finding := report.Diagnostics[0]
	require.Equal(t, participle.DiagnosticShadow, finding.Kind)
	require.Equal(t, 1, finding.Earlier)
	require.Equal(t, 2, finding.Later)
	require.Zero(t, finding.Overlapping)
}

type analysisParseable struct{}

func (*analysisParseable) Parse(*lexer.PeekingLexer) error { return nil }

func TestAnalysis_ParseableIsUnknown(t *testing.T) {
	type parseableGrammar struct {
		Literal string             `  @"x"`
		Custom  *analysisParseable `| @@`
	}
	parser, err := participle.Build[parseableGrammar](participle.Lexer(analysisLexer(t)))
	require.NoError(t, err)

	report := parser.Analysis()
	root := productionByName(t, report, "ParseableGrammar")
	require.False(t, root.FirstKnown)

	require.Equal(t, 1, len(report.Diagnostics))
	require.True(t, report.Diagnostics[0].Unknown)
}
