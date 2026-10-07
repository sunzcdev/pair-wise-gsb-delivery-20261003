package participle_test

import (
	"strings"
	"testing"

	require "github.com/alecthomas/assert/v2"

	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

func diffLexer() *lexer.StatefulDefinition {
	return lexer.MustSimple([]lexer.SimpleRule{
		{Name: "Number", Pattern: `[0-9]+`},
		{Name: "Ident", Pattern: `[a-zA-Z_][a-zA-Z0-9_]*`},
		{Name: "Punct", Pattern: `[-+*/%()]`},
		{Name: "whitespace", Pattern: `\s+`},
	})
}

// buildV1 constructs the first small version of a calculator grammar.
func buildV1(t *testing.T) participle.Grammar {
	t.Helper()
	type (
		Number struct {
			Value string `@Number`
		}
		Term struct {
			Factor Number   `@@`
			Op     string   `[ @("*" | "/") ]`
			Rest   []Number `{ @@ }`
		}
		Expr struct {
			Head Term `@@`
			Tail []struct {
				Op    string `@("+" | "-")`
				Right Term   `@@`
			} `{ @@ }`
		}
		Grammar struct {
			Root Expr `@@`
		}
	)
	p, err := participle.Build[Grammar](participle.Lexer(diffLexer()))
	require.NoError(t, err)
	return p
}

// buildV2 evolves the grammar across all change categories.
func buildV2(t *testing.T) participle.Grammar {
	t.Helper()
	type (
		Number struct {
			Value string `@Number`
		}
		Call struct {
			Callee string `@Ident "(" ")"`
		}
		Term struct {
			Factor Number `@@`
			// "/" and "*" swap order and "*" is replaced by "%": an ordered
			// reorder plus an in-place token change within the branches.
			Op   string   `[ @("/" | "%") ]`
			Rest []Number `{ @@ }!`     // quantifier zero-or-more -> non-empty marker
			Fn   *Call    `[ "->" @@ ]` // sequence change: new referenced node
		}
		Expr struct {
			Head Term `@@`
			Tail []struct {
				Op    string `@("+" | "-" | "&")` // token added
				Right Term   `@@`
			} `{ @@ }`
		}
		Grammar struct {
			Root Expr `@@`
		}
	)
	p, err := participle.Build[Grammar](participle.Lexer(diffLexer()))
	require.NoError(t, err)
	return p
}

func kindMap(changes []participle.ProductionChange) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, c := range changes {
		set := map[string]bool{}
		for _, k := range c.Kinds {
			set[k.String()] = true
		}
		out[c.ID] = set
	}
	return out
}

func containsID(list []string, id string) bool {
	for _, s := range list {
		if s == id {
			return true
		}
	}
	return false
}

func TestCompareGrammars_TwoVersions(t *testing.T) {
	old := buildV1(t)
	neo := buildV2(t)

	report, err := participle.CompareGrammars(old, neo)
	require.NoError(t, err)
	require.True(t, report.Changed())
	require.True(t, containsID(report.Added, "Call"), "added: %v", report.Added)

	changes := kindMap(report.Modified)
	termKinds, ok := changes["Term"]
	require.True(t, ok, "Term modified: %#v", report.Modified)
	require.True(t, termKinds["ordered branch"], "term kinds: %#v", termKinds)
	require.True(t, termKinds["token"], "term kinds: %#v", termKinds)
	require.True(t, termKinds["quantifier"], "term kinds: %#v", termKinds)
	require.True(t, termKinds["sequence"], "term kinds: %#v", termKinds)

	text := report.String()
	require.True(t, strings.Contains(text, "Added productions:"))
	require.True(t, strings.Contains(text, "Modified productions:"))
	t.Log("\n" + text)

	// Deterministic across repeated comparisons.
	again, err := participle.CompareGrammars(old, neo)
	require.NoError(t, err)
	require.Equal(t, text, again.String())
}

func TestCompareGrammars_NoChange(t *testing.T) {
	report, err := participle.CompareGrammars(buildV1(t), buildV1(t))
	require.NoError(t, err)
	require.False(t, report.Changed())
	require.Equal(t, "no grammar changes\n", report.String())
}

func buildCompactDense(t *testing.T) participle.Grammar {
	t.Helper()
	lex := lexer.MustSimple([]lexer.SimpleRule{
		{Name: "Ident", Pattern: `[a-z]+`},
		{Name: "whitespace", Pattern: `\s+`},
	})
	type dense struct {
		A string `@Ident "," @Ident`
	}
	p, err := participle.Build[dense](participle.Lexer(lex))
	require.NoError(t, err)
	return p
}

func buildAiryDense(t *testing.T) participle.Grammar {
	t.Helper()
	lex := lexer.MustSimple([]lexer.SimpleRule{
		{Name: "Ident", Pattern: `[a-z]+`},
		{Name: "whitespace", Pattern: `\s+`},
	})
	type dense struct {
		A string `  @Ident   ","   @Ident  `
	}
	p, err := participle.Build[dense](participle.Lexer(lex))
	require.NoError(t, err)
	return p
}

func TestCompareGrammars_WhitespaceIsCosmetic(t *testing.T) {
	report, err := participle.CompareGrammars(buildCompactDense(t), buildAiryDense(t))
	require.NoError(t, err)
	require.False(t, report.Changed())
}

func TestCompareGrammars_BranchReorderReported(t *testing.T) {
	lex := lexer.MustSimple([]lexer.SimpleRule{
		{Name: "A", Pattern: `a`},
		{Name: "B", Pattern: `b`},
		{Name: "whitespace", Pattern: `\s+`},
	})
	p1 := buildBranchAB(t, lex)
	p2 := buildBranchBA(t, lex)
	report, err := participle.CompareGrammars(p1, p2)
	require.NoError(t, err)
	require.Equal(t, 1, len(report.Modified))
	var hasOrdered bool
	for _, k := range report.Modified[0].Kinds {
		if k == participle.OrderedBranchChange {
			hasOrdered = true
		}
	}
	require.True(t, hasOrdered, "kinds: %#v", report.Modified[0].Kinds)
	require.True(t, strings.Contains(report.String(), "branches reordered"))
}

func buildBranchAB(t *testing.T, lex *lexer.StatefulDefinition) participle.Grammar {
	t.Helper()
	type choice struct {
		V string `@"a" | @"b"`
	}
	p, err := participle.Build[choice](participle.Lexer(lex))
	require.NoError(t, err)
	return p
}

func buildBranchBA(t *testing.T, lex *lexer.StatefulDefinition) participle.Grammar {
	t.Helper()
	type choice struct {
		V string `@"b" | @"a"`
	}
	p, err := participle.Build[choice](participle.Lexer(lex))
	require.NoError(t, err)
	return p
}

type customThing interface{ customThingMarker() }

type customImpl struct{}

func (customImpl) customThingMarker() {}

func customParse(*lexer.PeekingLexer) (customThing, error) { return customImpl{}, nil }

func TestCompareGrammars_CustomIsOpaque(t *testing.T) {
	lex := lexer.MustSimple([]lexer.SimpleRule{
		{Name: "Ident", Pattern: `[a-z]+`},
		{Name: "whitespace", Pattern: `\s+`},
	})
	type grammar struct {
		Thing customThing `@@`
	}
	opt := participle.ParseTypeWith[customThing](customParse)
	p1, err := participle.Build[grammar](participle.Lexer(lex), opt)
	require.NoError(t, err)
	p2, err := participle.Build[grammar](participle.Lexer(lex),
		participle.ParseTypeWith[customThing](customParse))
	require.NoError(t, err)
	report, err := participle.CompareGrammars(p1, p2)
	require.NoError(t, err)
	// Same declared opaque identity: differing parse functions are invisible.
	require.False(t, report.Changed())
}

type otherCustom interface{ otherCustomMarker() }

type otherCustomImpl struct{}

func (otherCustomImpl) otherCustomMarker() {}

func otherCustomParse(*lexer.PeekingLexer) (otherCustom, error) {
	return otherCustomImpl{}, nil
}

func TestCompareGrammars_CustomIdentityChange(t *testing.T) {
	lex := lexer.MustSimple([]lexer.SimpleRule{
		{Name: "Ident", Pattern: `[a-z]+`},
		{Name: "whitespace", Pattern: `\s+`},
	})
	type grammarA struct {
		Thing customThing `@@`
	}
	type grammarB struct {
		Thing otherCustom `@@`
	}
	p1, err := participle.Build[grammarA](participle.Lexer(lex),
		participle.ParseTypeWith[customThing](customParse))
	require.NoError(t, err)
	p2, err := participle.Build[grammarB](participle.Lexer(lex),
		participle.ParseTypeWith[otherCustom](otherCustomParse))
	require.NoError(t, err)
	report, err := participle.CompareGrammars(p1, p2)
	require.NoError(t, err)
	// The opaque node changed its declared identity: customThing removed,
	// otherCustom added, and the referencing production modified.
	require.True(t, containsID(report.Removed, "customThing"), "removed: %v", report.Removed)
	require.True(t, containsID(report.Added, "otherCustom"), "added: %v", report.Added)
}

func TestCompareGrammars_QuantifierAndToken(t *testing.T) {
	lex := lexer.MustSimple([]lexer.SimpleRule{
		{Name: "A", Pattern: `a`},
		{Name: "B", Pattern: `b`},
		{Name: "whitespace", Pattern: `\s+`},
	})
	p1 := buildQuantGrammar(t, lex, `@"a"*`)
	p2 := buildQuantGrammar(t, lex, `@"b"+`)
	report, err := participle.CompareGrammars(p1, p2)
	require.NoError(t, err)
	require.Equal(t, 1, len(report.Modified))
	set := map[string]bool{}
	for _, k := range report.Modified[0].Kinds {
		set[k.String()] = true
	}
	require.True(t, set["quantifier"], "kinds: %#v", set)
	require.True(t, set["token"], "kinds: %#v", set)
}

func buildQuantGrammar(t *testing.T, lex *lexer.StatefulDefinition, tag string) participle.Grammar {
	t.Helper()
	switch tag {
	case `@"a"*`:
		type quant struct {
			V []string `@"a"*`
		}
		p, err := participle.Build[quant](participle.Lexer(lex))
		require.NoError(t, err)
		return p
	default:
		type quant struct {
			V []string `@"b"+`
		}
		p, err := participle.Build[quant](participle.Lexer(lex))
		require.NoError(t, err)
		return p
	}
}

func buildRecV1(t *testing.T) participle.Grammar {
	t.Helper()
	lex := lexer.MustSimple([]lexer.SimpleRule{
		{Name: "Ident", Pattern: `[a-z]+`},
		{Name: "Punct", Pattern: `[()x]`},
		{Name: "whitespace", Pattern: `\s+`},
	})
	type recList struct {
		Head string     `@Ident`
		Tail []*recList `{ "(" @@ ")" }`
	}
	p, err := participle.Build[recList](participle.Lexer(lex))
	require.NoError(t, err)
	return p
}

func buildRecV2(t *testing.T) participle.Grammar {
	t.Helper()
	lex := lexer.MustSimple([]lexer.SimpleRule{
		{Name: "Ident", Pattern: `[a-z]+`},
		{Name: "Punct", Pattern: `[()x]`},
		{Name: "whitespace", Pattern: `\s+`},
	})
	type recList struct {
		Head string     `@Ident`
		Tail []*recList `{ "(" @@ ")" }`
		Mark string     `[ @"x" ]`
	}
	p, err := participle.Build[recList](participle.Lexer(lex))
	require.NoError(t, err)
	return p
}

func TestCompareGrammars_RecursiveReferenceBounded(t *testing.T) {
	p1 := buildRecV1(t)
	p2 := buildRecV2(t)
	report, err := participle.CompareGrammars(p1, p2)
	require.NoError(t, err)
	require.True(t, report.Changed())
	// Self reference compares node + identity only: a single added element,
	// never an infinite expansion of the recursive production.
	require.True(t, len(report.Modified) == 1, "modified: %#v", report.Modified)
}

func TestCompareGrammars_NilErrors(t *testing.T) {
	_, err := participle.CompareGrammars(nil, nil)
	require.Error(t, err)
}
