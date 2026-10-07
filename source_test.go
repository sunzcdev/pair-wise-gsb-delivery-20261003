package participle_test

import (
	"reflect"
	"testing"

	"github.com/alecthomas/assert/v2"

	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

var sourceViewLexer = lexer.MustSimple([]lexer.SimpleRule{
	{Name: "Ident", Pattern: `[a-zA-Z_][a-zA-Z0-9_]*`},
	{Name: "String", Pattern: `"(\\.|[^"])*"`},
	{Name: "Int", Pattern: `\d+`},
	{Name: "Comment", Pattern: `//[^\n]*`},
	{Name: "Punct", Pattern: `[{}:!]`},
	{Name: "Whitespace", Pattern: `\s+`},
})

func sourceViewParser[G any](options ...participle.Option) *participle.Parser[G] {
	return participle.MustBuild[G](append([]participle.Option{
		participle.Lexer(sourceViewLexer),
		participle.Elide("Comment", "Whitespace"),
	}, options...)...)
}

func recordTypes(view *participle.SourceView) []reflect.Type {
	out := []reflect.Type{}
	for _, record := range view.Records() {
		out = append(out, record.Type)
	}
	return out
}

func elidedValues(view *participle.SourceView, record participle.SourceRecord, symbol string) []string {
	typ := sourceViewLexer.Symbols()[symbol]
	out := []string{}
	for _, token := range view.Elided(record) {
		if token.Type == typ {
			out = append(out, token.Value)
		}
	}
	return out
}

type sourceItem struct {
	Name  string `@Ident`
	Value string `@(String | Int)`
}

type sourceBlock struct {
	Name  string        `@Ident`
	Items []*sourceItem `"{" @@* "}"`
}

type sourceFile struct {
	Blocks []*sourceBlock `@@*`
}

func TestSourceViewNestedComments(t *testing.T) {
	parser := sourceViewParser[sourceFile]()
	input := "main { // c1\n  alpha \"héllo\" // c2\n  beta 42\n}\n"

	view := &participle.SourceView{}
	ast, err := parser.ParseString("", input, participle.RecordSource(view))
	assert.NoError(t, err)
	assert.Equal(t, "main", ast.Blocks[0].Name)

	// Records are published innermost-first.
	assert.Equal(t, []reflect.Type{
		reflect.TypeFor[sourceItem](),
		reflect.TypeFor[sourceItem](),
		reflect.TypeFor[sourceBlock](),
		reflect.TypeFor[sourceFile](),
	}, recordTypes(view))

	records := view.Records()
	itemAlpha, itemBeta, block, file := records[0], records[1], records[2], records[3]

	// Ranges slice the original input byte-for-byte, including elided
	// comments and whitespace. Leading trivia attaches to the following
	// production.
	assert.Equal(t, " // c1\n  alpha \"héllo\"", itemAlpha.Range.Slice(input))
	assert.Equal(t, " // c2\n  beta 42", itemBeta.Range.Slice(input))
	assert.Equal(t, "main { // c1\n  alpha \"héllo\" // c2\n  beta 42\n}", block.Range.Slice(input))
	assert.Equal(t, "main { // c1\n  alpha \"héllo\" // c2\n  beta 42\n}", file.Range.Slice(input))

	// Sibling ranges tile the input with no gaps or overlaps.
	assert.Equal(t, itemAlpha.Range.End, itemBeta.Range.Start)

	// Multibyte characters use byte offsets.
	assert.Equal(t, 6, itemAlpha.Range.Start.Offset)
	assert.Equal(t, 29, itemAlpha.Range.End.Offset)

	// Elided tokens are queryable per record.
	assert.Equal(t, []string{"// c1"}, elidedValues(view, itemAlpha, "Comment"))
	assert.Equal(t, []string{"// c2"}, elidedValues(view, itemBeta, "Comment"))
	assert.Equal(t, []string{"// c1", "// c2"}, elidedValues(view, block, "Comment"))
}

func TestSourceViewTrailingWhitespace(t *testing.T) {
	type grammar struct {
		Values []string `@Ident*`
	}
	// The default text/scanner lexer does not tokenise whitespace, so the
	// root production's range extends to the EOF position, covering trailing
	// whitespace.
	parser := participle.MustBuild[grammar]()
	input := "one two   \n\n"

	view := &participle.SourceView{}
	_, err := parser.ParseString("", input, participle.RecordSource(view))
	assert.NoError(t, err)

	records := view.Records()
	assert.Equal(t, 1, len(records))
	assert.Equal(t, input, records[0].Range.Slice(input))

	// Reusing a view across parses resets it.
	_, err = parser.ParseString("", "one", participle.RecordSource(view))
	assert.NoError(t, err)
	assert.Equal(t, 1, len(view.Records()))
	assert.Equal(t, "one", view.Records()[0].Range.Slice("one"))
}

type sourcePair struct {
	Key string `@Ident ":"`
}

type sourceWord struct {
	Word string `@Ident`
}

type sourceStmt struct {
	Pair *sourcePair `@@ |`
	Word *sourceWord `@@`
}

func TestSourceViewBacktrackedProductionNotRecorded(t *testing.T) {
	parser := sourceViewParser[sourceStmt]()
	input := "hello"

	view := &participle.SourceView{}
	ast, err := parser.ParseString("", input, participle.RecordSource(view))
	assert.NoError(t, err)
	assert.Equal(t, "hello", ast.Word.Word)

	// The sourcePair alternative matched "hello" then failed on ":", so it
	// must not appear in the records.
	assert.Equal(t, []reflect.Type{
		reflect.TypeFor[sourceWord](),
		reflect.TypeFor[sourceStmt](),
	}, recordTypes(view))
	assert.Equal(t, "hello", view.Records()[0].Range.Slice(input))
}

func TestSourceViewFailedParsePublishesNothing(t *testing.T) {
	parser := sourceViewParser[sourceFile]()

	view := &participle.SourceView{}
	_, err := parser.ParseString("", "main { } 42", participle.RecordSource(view))
	assert.Error(t, err)
	assert.Equal(t, 0, len(view.Records()))
}

type sourceMaybe struct {
	Value string `@Ident?`
}

type sourceOuter struct {
	Maybe *sourceMaybe `@@ "!"`
}

func TestSourceViewEmptyProductionZeroWidth(t *testing.T) {
	parser := sourceViewParser[sourceOuter]()
	input := "!"

	view := &participle.SourceView{}
	_, err := parser.ParseString("", input, participle.RecordSource(view))
	assert.NoError(t, err)

	assert.Equal(t, []reflect.Type{
		reflect.TypeFor[sourceMaybe](),
		reflect.TypeFor[sourceOuter](),
	}, recordTypes(view))

	records := view.Records()
	// The empty production is recorded at a zero-width position.
	assert.Equal(t, records[0].Range.Start, records[0].Range.End)
	assert.Equal(t, 0, records[0].Range.Start.Offset)
	assert.Equal(t, "", records[0].Range.Slice(input))
	assert.Equal(t, 0, len(view.Elided(records[0])))
	// The outer production spans the whole input.
	assert.Equal(t, "!", records[1].Range.Slice(input))
}

type sourceMappedItem struct {
	Name string `@Ident`
}

type sourceMapped struct {
	Items []*sourceMappedItem `@@*`
}

func TestSourceViewMappedTokensAndDuplicateText(t *testing.T) {
	parser := sourceViewParser[sourceMapped](participle.Upper("Ident"))
	input := "foo foo"

	view := &participle.SourceView{}
	ast, err := parser.ParseString("", input, participle.RecordSource(view))
	assert.NoError(t, err)

	// The mapper rewrote the captured values...
	assert.Equal(t, "FOO", ast.Items[0].Name)
	assert.Equal(t, "FOO", ast.Items[1].Name)

	// ...but source ranges still point at the original text, and duplicate
	// text is located by offset, not by searching for the value.
	records := view.Records()
	assert.Equal(t, 3, len(records))
	assert.Equal(t, 0, records[0].Range.Start.Offset)
	assert.Equal(t, "foo", records[0].Range.Slice(input))
	assert.Equal(t, 3, records[1].Range.Start.Offset)
	assert.Equal(t, " foo", records[1].Range.Slice(input))
	assert.Equal(t, "foo foo", records[2].Range.Slice(input))
}
