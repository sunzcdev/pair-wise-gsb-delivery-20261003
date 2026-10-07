package participle_test

import (
	"testing"

	require "github.com/alecthomas/assert/v2"
	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

// Grammar 1: two branches share a prefix but diverge afterwards. FIRST overlaps, so this must be
// reported as a possible conflict only — never as a definite error.
type analysisSharedPrefix struct {
	Short *analysisSharedShort `parser:"  @@"`
	Long  *analysisSharedLong  `parser:"| @@"`
}

type analysisSharedShort struct {
	Keyword string `parser:"@\"let\" @Ident"`
}

type analysisSharedLong struct {
	Keyword string `parser:"@\"let\" @Ident \"=\" @Ident"`
}

func TestAnalysisSharedPrefixIsPossibleNotDefinite(t *testing.T) {
	parser, err := participle.Build[analysisSharedPrefix]()
	require.NoError(t, err)

	// Existing ordered-choice behaviour is unchanged: the first branch wins for a shared prefix.
	out, err := parser.ParseString("", "let a")
	require.NoError(t, err)
	require.NotZero(t, out.Short)

	report := parser.Analysis()

	prod := productionByName(t, report, "AnalysisSharedPrefix")
	require.Equal(t, participle.NullableFalse, prod.Nullable)
	require.True(t, containsFirst(prod.First, `"let"`))

	require.Equal(t, 1, len(report.Findings))
	finding := report.Findings[0]
	require.Equal(t, participle.SeverityPossible, finding.Severity)
	require.Equal(t, participle.KindFirstConflict, finding.Kind)
	require.Equal(t, 0, finding.Earlier)
	require.Equal(t, 1, finding.Later)
	require.True(t, containsFirst(finding.Overlap, `"let"`))
	require.Equal(t, []string{"AnalysisSharedPrefix"}, finding.Path)
}

// Grammar 2: the first branch is a strict prefix of the second, so the second branch ("a" "b")
// is completely shadowed and unreachable.
type analysisShadowed struct {
	A string `parser:"  @\"a\""`
	B string `parser:"| @\"a\" @\"b\""`
}

func TestAnalysisCompleteShadow(t *testing.T) {
	parser := participle.MustBuild[analysisShadowed]()
	report := parser.Analysis()

	definite := findFinding(t, report, participle.KindShadowed)
	require.NotZero(t, definite)
	require.Equal(t, 0, definite.Earlier)
	require.Equal(t, 1, definite.Later)
	require.Equal(t, []string{"AnalysisShadowed"}, definite.Path)
	require.Contains(t, report.String(), "[definite] shadowed branch")
}

// An earlier branch that can match the empty string deterministically hides everything after it.
type analysisNullableBranch struct {
	Optional string `parser:"  @\"x\"?"`
	Always   string `parser:"| @\"y\""`
}

func TestAnalysisNullableBranchHidesLater(t *testing.T) {
	parser := participle.MustBuild[analysisNullableBranch]()
	report := parser.Analysis()

	finding := findFinding(t, report, participle.KindNullableBranch)
	require.Equal(t, participle.SeverityDefinite, finding.Severity)
	require.Equal(t, 0, finding.Earlier)
	require.Equal(t, 1, finding.Later)
	require.Equal(t, []string{"AnalysisNullableBranch"}, finding.Path)
	require.Contains(t, report.String(), "[definite] nullable branch")
}

// Grammar 3: a genuinely nullable recursive production.
//
//	X = ("(" X ")")?
//
// Nullability of X is the least fixed point of the equation nullable(X) = true (the wrapping
// group is optional), which naive recursion could never establish.
type analysisNullableRecurse struct {
	Inner *analysisNullableWrap `parser:"@@?"`
}

type analysisNullableWrap struct {
	Open  string                   `parser:"@\"(\""`
	Inner *analysisNullableRecurse `parser:"@@?"`
	Close string                   `parser:"@\")\""`
}

func TestAnalysisNullableRecursionFixpoint(t *testing.T) {
	parser := participle.MustBuild[analysisNullableRecurse]()

	// Sanity: the empty input and nested forms still parse.
	_, err := parser.ParseString("", "")
	require.NoError(t, err)
	_, err = parser.ParseString("", "( )")
	require.NoError(t, err)
	_, err = parser.ParseString("", "( ( ) )")
	require.NoError(t, err)

	report := parser.Analysis()

	recurse := productionByName(t, report, "AnalysisNullableRecurse")
	require.Equal(t, participle.NullableTrue, recurse.Nullable)
	require.True(t, containsFirst(recurse.First, `"("`))

	wrap := productionByName(t, report, "AnalysisNullableWrap")
	require.Equal(t, participle.NullableFalse, wrap.Nullable)
	require.True(t, containsFirst(wrap.First, `"("`))

	// No ordered disjunction means no findings.
	require.Equal(t, 0, len(report.Findings))
}

// A ParseTypeWith custom production has no inspectable grammar and must be marked unknown.
type analysisCustom interface{ customMarker() }

type analysisCustomGrammar struct {
	Custom analysisCustom `parser:"  @@"`
	Direct string         `parser:"| @\"kw\""`
}

func TestAnalysisParseTypeWithIsUnknown(t *testing.T) {
	parser, err := participle.Build[analysisCustomGrammar](
		participle.ParseTypeWith(func(lex *lexer.PeekingLexer) (analysisCustom, error) {
			return nil, participle.NextMatch
		}),
	)
	require.NoError(t, err)

	report := parser.Analysis()

	var unknown *participle.BranchFinding
	for i := range report.Findings {
		if report.Findings[i].Kind == participle.KindUnknownConflict {
			unknown = &report.Findings[i]
		}
	}
	require.NotZero(t, unknown)
	require.Equal(t, participle.SeverityUnknown, unknown.Severity)
	require.Equal(t, 0, unknown.Earlier)
	require.Equal(t, 1, unknown.Later)
	require.Contains(t, report.String(), "[unknown] unknown interaction")

	prod := productionByName(t, report, "analysisCustom")
	require.True(t, prod.Opaque)
	require.Equal(t, participle.NullableUnknown, prod.Nullable)
}

// A type implementing Parseable is equally opaque to the analyser.
type analysisParseableType struct{}

func (*analysisParseableType) Parse(*lexer.PeekingLexer) error {
	return participle.NextMatch
}

type analysisParseableGrammar struct {
	Custom *analysisParseableType `parser:"  @@"`
	Direct string                 `parser:"| @\"kw\""`
}

func TestAnalysisParseableIsUnknown(t *testing.T) {
	parser := participle.MustBuild[analysisParseableGrammar]()
	report := parser.Analysis()

	var unknown *participle.BranchFinding
	for i := range report.Findings {
		if report.Findings[i].Kind == participle.KindUnknownConflict {
			unknown = &report.Findings[i]
		}
	}
	require.NotZero(t, unknown)
	require.Equal(t, participle.SeverityUnknown, unknown.Severity)

	prod := productionByName(t, report, "analysisParseableType")
	require.True(t, prod.Opaque)
	require.Equal(t, participle.NullableUnknown, prod.Nullable)
}

func productionByName(t *testing.T, report *participle.AnalysisReport, name string) participle.ProductionAnalysis {
	t.Helper()
	for _, prod := range report.Productions {
		if prod.Name == name {
			return prod
		}
	}
	t.Fatalf("production %q not found in report", name)
	return participle.ProductionAnalysis{}
}

func findFinding(t *testing.T, report *participle.AnalysisReport, kind participle.AnalysisKind) *participle.BranchFinding {
	t.Helper()
	for i := range report.Findings {
		if report.Findings[i].Kind == kind {
			return &report.Findings[i]
		}
	}
	t.Fatalf("no finding of kind %s in report:\n%s", kind, report)
	return nil
}

func containsFirst(tokens []participle.FirstToken, value string) bool {
	for _, token := range tokens {
		if token.String() == value {
			return true
		}
	}
	return false
}
