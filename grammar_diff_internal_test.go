package participle

import (
	"strings"
	"testing"

	"github.com/alecthomas/assert/v2"
)

// compareAnyParsers compares the production graphs of two parsers whose root types are
// function-local and therefore cannot appear in a helper signature.
func compareAnyParsers(t *testing.T, oldParser, newParser any) *GrammarDiffReport {
	t.Helper()
	type producer interface {
		grammarProductions() (map[string]*diffProduction, error)
	}
	oldProds, err := oldParser.(producer).grammarProductions()
	assert.NoError(t, err)
	newProds, err := newParser.(producer).grammarProductions()
	assert.NoError(t, err)
	return buildDiffReport(oldProds, newProds)
}

// buildSmallGrammarReport builds two releases of a small rule language and compares them.
//
// The types are function-local named types on purpose: they still carry a stable
// package/type identity, which is what production matching relies on rather than
// reflect.Type pointer equality.
//
// v1 -> v2 contains:
//   - a token change in Header (Ident -> String)
//   - a quantifier change in Item (+ -> *)
//   - a pure ordered-branch change in Body (the two alternatives swap places)
//   - a sequence change in Footer (an extra trailing "ok" term)
//   - a new Trailing production, added as an extra field of Doc
//
// Meta is structurally unchanged and must never be reported.
func buildSmallGrammarReport(t *testing.T) *GrammarDiffReport {
	t.Helper()

	var oldParser, newParser any

	{
		type diffMeta struct {
			Tag string `@Ident ":"`
		}
		type diffHeader struct {
			Name string `@Ident ":"`
		}
		type diffItem struct {
			Value string `@String+`
		}
		type diffBody struct {
			Head string `  @Ident "="`
			Tail string `| @Int`
		}
		type diffFooter struct {
			End string `@Ident "end"`
		}
		type diffDoc struct {
			Meta   diffMeta   `@@`
			Header diffHeader `@@`
			Items  []diffItem `@@*`
			Body   diffBody   `@@`
			Footer diffFooter `@@`
		}
		p, err := Build[diffDoc]()
		assert.NoError(t, err)
		oldParser = p
	}

	{
		type diffMeta struct {
			Tag string `  @Ident   ":"  `
		}
		type diffHeader struct {
			Name string `@String ":"`
		}
		type diffItem struct {
			Value string `@String*`
		}
		type diffBody struct {
			// Alternatives swapped; branch contents themselves are unchanged.
			Tail string `  @Int`
			Head string `| @Ident "="`
		}
		type diffFooter struct {
			End string `@Ident "end" "ok"`
		}
		type diffTrailing struct {
			Note string `@String`
		}
		type diffDoc struct {
			Meta     diffMeta     `@@`
			Header   diffHeader   `@@`
			Items    []diffItem   `@@*`
			Body     diffBody     `@@`
			Footer   diffFooter   `@@`
			Trailing diffTrailing `@@`
		}
		p, err := Build[diffDoc]()
		assert.NoError(t, err)
		newParser = p
	}

	return compareAnyParsers(t, oldParser, newParser)
}

func TestCompareGrammar_Report(t *testing.T) {
	report := buildSmallGrammarReport(t)
	assert.True(t, report.Changed())

	assert.Equal(t, 1, len(report.Added))
	assert.True(t, strings.HasSuffix(report.Added[0].Production, ".diffTrailing"))
	assert.Equal(t, AddedProduction, report.Added[0].Kind)
	assert.Equal(t, 0, len(report.Removed))

	byProduction := map[string][]GrammarChange{}
	for _, c := range report.Modified {
		byProduction[c.Production] = append(byProduction[c.Production], c)
	}

	find := func(suffix string) []GrammarChange {
		for id, changes := range byProduction {
			if strings.HasSuffix(id, suffix) {
				return changes
			}
		}
		return nil
	}

	header := find(".diffHeader")
	assert.Equal(t, 1, len(header))
	assert.Equal(t, TokenChanged, header[0].Kind)
	assert.Equal(t, "<Ident>", header[0].From)
	assert.Equal(t, "<String>", header[0].To)

	item := find(".diffItem")
	assert.Equal(t, 1, len(item))
	assert.Equal(t, QuantifierChanged, item[0].Kind)
	assert.Equal(t, "+", item[0].From)
	assert.Equal(t, "*", item[0].To)

	body := find(".diffBody")
	assert.Equal(t, 1, len(body))
	assert.Equal(t, OrderedBranchChanged, body[0].Kind)

	footer := find(".diffFooter")
	assert.Equal(t, 1, len(footer))
	assert.Equal(t, SequenceChanged, footer[0].Kind)

	// Meta differs only by whitespace and must be absent.
	assert.Equal(t, 0, len(find(".diffMeta")))
}

func TestCompareGrammar_Stable(t *testing.T) {
	first := buildSmallGrammarReport(t)
	second := buildSmallGrammarReport(t)

	assert.Equal(t, first.String(), second.String())
	assert.Equal(t, first.Added, second.Added)
	assert.Equal(t, first.Removed, second.Removed)
	assert.Equal(t, first.Modified, second.Modified)

	// Spot-check readability of the rendered report.
	out := first.String()
	assert.Contains(t, out, "Added productions:")
	assert.Contains(t, out, "Modified productions:")
	assert.Contains(t, out, ".diffTrailing")
	assert.Contains(t, out, "token changed from <Ident> to <String>")
	assert.Contains(t, out, "quantifier changed from + to *")
	assert.Contains(t, out, "ordered branch changed from")
	assert.Contains(t, out, "sequence changed from")
}

func TestCompareGrammar_NoChanges(t *testing.T) {
	build := func() any {
		type sameHeader struct {
			Name string `@Ident ":"`
		}
		type sameDoc struct {
			Header sameHeader `@@`
		}
		p, err := Build[sameDoc]()
		assert.NoError(t, err)
		return p
	}
	report := compareAnyParsers(t, build(), build())
	assert.False(t, report.Changed())
	assert.Equal(t, 0, len(report.Added))
	assert.Equal(t, 0, len(report.Removed))
	assert.Equal(t, 0, len(report.Modified))
	assert.Equal(t, "no grammar changes", report.String())
}
