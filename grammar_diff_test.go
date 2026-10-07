package participle_test

import (
	"strings"
	"testing"

	require "github.com/alecthomas/assert/v2"
	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

// ---- public API: two small grammar versions with top-level named types ----

// extHeader is the shared nested production; its token changes between the two builds.
type extHeader struct {
	Name string `@Ident ":"`
}

type extDocA struct {
	Header extHeader `@@`
	Head   string    `  @Ident "="`
	Tail   string    `| @Int`
}
type extDocB struct {
	Header extHeader `@@`
	Head   string    `  @Ident "="   `
	Tail   string    `| @Int`
}

func buildExtA(t *testing.T) *participle.Parser[extDocA] {
	t.Helper()
	// Identical structure to extDocB; whitespace in tags is presentation only.
	return mustTestParser[extDocA](t)
}
func buildExtB(t *testing.T) *participle.Parser[extDocB] {
	t.Helper()
	return mustTestParser[extDocB](t)
}

func TestCompareGrammar_PublicAPI(t *testing.T) {
	// Same-named nested productions with an identical body compare cleanly even though
	// the root types differ: no nested production is reported.
	oldParser := buildExtA(t)
	newParser := buildExtB(t)
	report, err := participle.CompareGrammar(oldParser, newParser)
	require.NoError(t, err)
	// Root productions differ by name, but the shared extHeader must be untouched.
	for _, c := range report.Modified {
		require.False(t, strings.HasSuffix(c.Production, ".extHeader"), c.String())
	}

	// Same parser compared with itself must be stable and empty.
	self, err := participle.CompareGrammar(oldParser, oldParser)
	require.NoError(t, err)
	require.False(t, self.Changed())
	require.Equal(t, "no grammar changes", self.String())
}

func TestCompareGrammar_NilParser(t *testing.T) {
	p := mustTestParser[extDocA](t)
	_, err := participle.CompareGrammar[extDocA, extDocA](nil, nil)
	require.Error(t, err)
	var typed *participle.Parser[extDocA]
	_, err = participle.CompareGrammar(p, typed)
	require.Error(t, err)
}

// ---- recursive grammars: references are not expanded ----

// extRecursive appears in both grammar versions under the same identity, so identical
// shape must compare equal despite self-reference.
type extRecursive struct {
	Head string          `@Ident`
	Tail []*extRecursive `@@*`
}

type extRecRootA struct {
	Node extRecursive `@@`
}
type extRecRootB struct {
	Node extRecursive `@@`
}

func TestCompareGrammar_RecursionTerminates(t *testing.T) {
	oldParser := mustTestParser[extRecRootA](t)
	newParser := mustTestParser[extRecRootB](t)

	report, err := participle.CompareGrammar(oldParser, newParser)
	require.NoError(t, err)

	var recursiveChanges []participle.GrammarChange
	for _, c := range report.Modified {
		if strings.HasSuffix(c.Production, ".extRecursive") {
			recursiveChanges = append(recursiveChanges, c)
		}
	}
	require.Equal(t, 0, len(recursiveChanges), "recursive production must compare equal: %v", report.String())
}

// ---- opaque custom parsers: only declared identity matters ----

type extCustom interface{ extCustomMarker() }

type extCustomImpl string

func (extCustomImpl) extCustomMarker() {}

func extCustomParser() participle.Option {
	return participle.ParseTypeWith(func(lex *lexer.PeekingLexer) (extCustom, error) {
		token := lex.Peek()
		_ = token
		return extCustomImpl("x"), nil
	})
}

type extOpaqueA struct {
	Value extCustom `@@`
}
type extOpaqueB struct {
	Value extCustom `@@`
}

func TestCompareGrammar_OpaqueCustomIdentity(t *testing.T) {
	oldParser := mustTestParser[extOpaqueA](t, extCustomParser())
	newParser := mustTestParser[extOpaqueB](t, extCustomParser())

	report, err := participle.CompareGrammar(oldParser, newParser)
	require.NoError(t, err)

	for _, c := range report.Modified {
		require.False(t, strings.HasSuffix(c.Production, ".extCustom"), "opaque parser must not be expanded: %v", c.String())
	}
}

// extParseable is an external custom parser referenced inline as a struct field.
type extParseable struct{}

func (*extParseable) Parse(*lexer.PeekingLexer) error { return nil }

type extParseableDocA struct {
	Custom *extParseable `@@`
}
type extParseableDocB struct {
	Custom *extParseable `@@`
}

func TestCompareGrammar_ParseableOpaqueIdentity(t *testing.T) {
	oldParser := mustTestParser[extParseableDocA](t)
	newParser := mustTestParser[extParseableDocB](t)

	report, err := participle.CompareGrammar(oldParser, newParser)
	require.NoError(t, err)

	for _, c := range report.Modified {
		require.False(t, strings.HasSuffix(c.Production, ".extParseable"),
			"Parseable type must stay opaque: %v", c.String())
	}
}

// Union member ordering is semantically significant (first match wins), so swapping two
// branches must be reported even though the set of members is unchanged.
type extUnionMember interface{ extUnionMemberMarker() }

type extUnionLeft struct {
	L string `@Ident`
}
type extUnionRight struct {
	R string `@Int`
}

func (extUnionLeft) extUnionMemberMarker()  {}
func (extUnionRight) extUnionMemberMarker() {}

type extUnionDocA struct {
	Choice extUnionMember `@@`
}
type extUnionDocB struct {
	Choice extUnionMember `@@`
}

func TestCompareGrammar_UnionBranchReorderReported(t *testing.T) {
	oldParser := mustTestParser[extUnionDocA](t,
		participle.Union[extUnionMember](extUnionLeft{}, extUnionRight{}))
	newParser := mustTestParser[extUnionDocB](t,
		participle.Union[extUnionMember](extUnionRight{}, extUnionLeft{}))

	report, err := participle.CompareGrammar(oldParser, newParser)
	require.NoError(t, err)

	var found bool
	for _, c := range report.Modified {
		if strings.HasSuffix(c.Production, ".extUnionMember") &&
			c.Kind == participle.OrderedBranchChanged {
			found = true
		}
	}
	require.True(t, found, "expected ordered-branch change for swapped union members: %s", report.String())
}

type extRemovedInner struct {
	Value string `@Ident`
}
type extRemovedDocA struct {
	Inner extRemovedInner `@@`
}
type extRemovedDocB struct {
	Value string `@Ident`
}

func TestCompareGrammar_AddedAndRemoved(t *testing.T) {
	oldParser := mustTestParser[extRemovedDocA](t)
	newParser := mustTestParser[extRemovedDocB](t)

	report, err := participle.CompareGrammar(oldParser, newParser)
	require.NoError(t, err)

	var removed, added int
	for _, c := range report.Removed {
		if strings.HasSuffix(c.Production, ".extRemovedInner") {
			removed++
			require.Equal(t, participle.RemovedProduction, c.Kind)
		}
	}
	for _, c := range report.Added {
		if strings.HasSuffix(c.Production, ".extRemovedDocB") {
			added++
			require.Equal(t, participle.AddedProduction, c.Kind)
		}
	}
	require.Equal(t, 1, removed)
	require.Equal(t, 1, added)
	require.Contains(t, report.String(), "Removed productions:")
}
