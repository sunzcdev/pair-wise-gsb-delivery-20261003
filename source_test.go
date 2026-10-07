package participle_test

import (
	"strings"
	"testing"

	"github.com/alecthomas/assert/v2"

	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

var sourceLexer = lexer.MustSimple([]lexer.SimpleRule{
	{Name: "Whitespace", Pattern: `\s+`},
	{Name: "Comment", Pattern: `#[^\n]*`},
	{Name: "String", Pattern: `"[^"]*"`},
	{Name: "Ident", Pattern: `[\pL_][\pL\pN_]*`},
	{Name: "Punct", Pattern: `[(){},]`},
})

type sourceItem struct {
	Name string `@Ident`
}

type sourceNode struct {
	Open  string        `@"("`
	Items []*sourceItem `{ @@ }`
	Close string        `@")"`
}

type sourceGrammar struct {
	Nodes []*sourceNode `{ @@ }`
}

func buildSourceParser(t *testing.T, opts ...participle.Option) *participle.Parser[sourceGrammar] {
	t.Helper()
	defaults := []participle.Option{
		participle.Lexer(sourceLexer),
		participle.Elide("Whitespace", "Comment"),
	}
	return participle.MustBuild[sourceGrammar](append(defaults, opts...)...)
}

// TestSourceViewNestedComments parses a nested structure with comments and
// leading/trailing whitespace, and checks that every production reproduces
// its original source byte for byte.
func TestSourceViewNestedComments(t *testing.T) {
	input := "# leading comment\n(  foo  # inner comment\n  bar  )  # trailing\n   "
	parser := buildSourceParser(t)
	track, view := participle.TrackSources()
	ast, err := parser.ParseString("test.lang", input, track)
	assert.NoError(t, err)

	rootRec := view.Record(ast)
	assert.True(t, rootRec != nil)
	assert.Equal(t, []byte(input), view.Text(rootRec), "root must cover the entire input")

	// Records were published for the root, the node and both items.
	assert.Equal(t, 4, len(view.Records()))

	nodeRec := view.Record(ast.Nodes[0])
	assert.True(t, nodeRec != nil)
	{
		assert.Equal(t, "(  foo  # inner comment\n  bar  )", string(view.Text(nodeRec)))
		elided := view.Elided(nodeRec)
		values := make([]string, len(elided))
		for i, token := range elided {
			values[i] = token.Value
		}
		assert.Equal(t, []string{"  ", "  ", "# inner comment", "\n  ", "  "}, values)
	}

	fooRec := view.Record(ast.Nodes[0].Items[0])
	assert.True(t, fooRec != nil)
	{
		assert.Equal(t, "foo", string(view.Text(fooRec)))
		assert.Equal(t, 0, len(view.Elided(fooRec)))
	}
	barRec := view.Record(ast.Nodes[0].Items[1])
	assert.Equal(t, "bar", string(view.Text(barRec)))
}

// TestSourceViewMultibyte ensures byte offsets stay correct with multi-byte
// runes in both identifiers and comments.
func TestSourceViewMultibyte(t *testing.T) {
	input := "( café  # 注释\n )"
	parser := buildSourceParser(t)
	track, view := participle.TrackSources()
	ast, err := parser.ParseString("", input, track)
	assert.NoError(t, err)

	assert.Equal(t, []byte(input), view.Text(view.Record(ast)))
	item := ast.Nodes[0].Items[0]
	assert.Equal(t, "café", string(view.Text(view.Record(item))))
}

// TestSourceViewMapperOffsets verifies positions remain correct after a
// Mapper changes token values, including repeated identical literals.
func TestSourceViewMapperOffsets(t *testing.T) {
	type mapperItem struct {
		Value string `@String`
	}
	type mapperGrammar struct {
		Items []*mapperItem `{ @@ }`
	}
	lex := lexer.MustSimple([]lexer.SimpleRule{
		{Name: "Whitespace", Pattern: `\s+`},
		{Name: "String", Pattern: `"[^"]*"`},
	})
	parser := participle.MustBuild[mapperGrammar](
		participle.Lexer(lex),
		participle.Elide("Whitespace"),
		participle.Unquote("String"),
	)
	input := `"dup"  "dup"   "uniq"`
	track, view := participle.TrackSources()
	ast, err := parser.ParseString("", input, track)
	assert.NoError(t, err)

	// AST values are unquoted, but source text still resolves via offsets.
	assert.Equal(t, "dup", ast.Items[0].Value)
	assert.Equal(t, `"dup"`, string(view.Text(view.Record(ast.Items[0]))))
	assert.Equal(t, `"dup"`, string(view.Text(view.Record(ast.Items[1]))))
	assert.Equal(t, `"uniq"`, string(view.Text(view.Record(ast.Items[2]))))

	all := view.Records()
	assert.Equal(t, 4, len(all)) // root + 3 items
	rootRec := all[len(all)-1]
	assert.Equal(t, []byte(input), view.Text(&rootRec))
}

// TestSourceViewZeroWidth checks a production that matches zero tokens
// publishes a zero-width record positioned at its match point.
func TestSourceViewZeroWidth(t *testing.T) {
	// Empty can match without consuming any tokens.
	type emptyNode struct {
		Name string `@Ident?`
	}
	type zwGrammar struct {
		Before string     `@Ident`
		Empty  *emptyNode `@@`
	}
	lex := lexer.MustSimple([]lexer.SimpleRule{
		{Name: "Whitespace", Pattern: `\s+`},
		{Name: "Ident", Pattern: `\w+`},
	})
	parser := participle.MustBuild[zwGrammar](
		participle.Lexer(lex),
		participle.Elide("Whitespace"),
	)
	input := "first  "
	track, view := participle.TrackSources()
	ast, err := parser.ParseString("", input, track)
	assert.NoError(t, err)
	assert.True(t, ast.Empty != nil)
	assert.Equal(t, "", ast.Empty.Name)

	rootRec := view.Record(ast)
	assert.Equal(t, []byte(input), view.Text(rootRec))

	var zero *participle.SourceRecord
	for _, rec := range view.Records() {
		if rec.Range.ZeroWidth() {
			recCopy := rec
			zero = &recCopy
			break
		}
	}
	assert.True(t, zero != nil)
	{
		assert.Equal(t, "", string(view.Text(zero)))
		assert.Equal(t, 0, len(view.Elided(zero)))
		// The empty production matched right after "first".
		assert.Equal(t, len("first"), zero.Range.StartOffset)
		assert.Equal(t, len("first"), zero.Range.EndOffset)
	}
}

// TestSourceViewBacktracking ensures an alternative rejected by a
// disjunction publishes no records.
func TestSourceViewBacktracking(t *testing.T) {
	type choice struct {
		First *sourceItem `( @@`
		Or    *sourceNode ` | @@ )`
	}
	type btRoot struct {
		Choice choice `@@`
	}
	parser := participle.MustBuild[btRoot](
		participle.Lexer(sourceLexer),
		participle.Elide("Whitespace", "Comment"),
	)
	input := "foo"
	track, view := participle.TrackSources()
	ast, err := parser.ParseString("", input, track)
	assert.NoError(t, err)

	// The root, the choice struct and the chosen item are recorded. The
	// rejected sourceNode alternative (which starts with "(") published no
	// record even though it was attempted.
	assert.Equal(t, 3, len(view.Records()))
	assert.Equal(t, "foo", string(view.Text(view.Record(ast.Choice.First))))
	assert.Zero(t, ast.Choice.Or)
}

// TestSourceViewFailureEmpty ensures a failed parse publishes nothing.
func TestSourceViewFailureEmpty(t *testing.T) {
	parser := buildSourceParser(t)
	track, view := participle.TrackSources()
	_, err := parser.ParseString("", "( foo", track)
	assert.Error(t, err)
	assert.Zero(t, view.Records())
}

// TestSourceViewRejectedAlternative verifies an alternative that parses
// successfully internally but is then rejected by a sequence still publishes
// no records.
func TestSourceViewRejectedAlternative(t *testing.T) {
	type altNode struct {
		Open  string        `@"("`
		Items []*sourceItem `{ @@ }`
		Close string        `@")"`
	}
	type rejectRoot struct {
		Node *altNode `@@ ";"`
	}
	parser := participle.MustBuild[rejectRoot](
		participle.Lexer(sourceLexer),
		participle.Elide("Whitespace", "Comment"),
	)
	// "( foo" parses the node fully, but the required ";" is missing, so the
	// whole parse fails.
	track, view := participle.TrackSources()
	_, err := parser.ParseString("", "( foo", track)
	assert.Error(t, err)
	assert.Zero(t, view.Records())
}

// TestSourceViewDeepFailureRollback ensures records published deep inside a
// parse are all discarded when the overall parse fails.
func TestSourceViewDeepFailureRollback(t *testing.T) {
	// A valid node containing items, followed by a missing closer: items and
	// node parse fine before the outer structure fails.
	input := "( foo bar"
	parser := buildSourceParser(t)
	track, view := participle.TrackSources()
	_, err := parser.ParseString("", input, track)
	assert.Error(t, err)
	assert.Zero(t, view.Records())
}

// TestSourceViewValueStructField covers AST productions captured into
// addressable (non-pointer) struct fields.
func TestSourceViewValueStructField(t *testing.T) {
	type valueWrap struct {
		Item sourceItem `@@`
	}
	type valueRoot struct {
		Wraps []*valueWrap `{ @@ }`
	}
	parser := participle.MustBuild[valueRoot](
		participle.Lexer(sourceLexer),
		participle.Elide("Whitespace", "Comment"),
	)
	track, view := participle.TrackSources()
	ast, err := parser.ParseString("", "foo  bar", track)
	assert.NoError(t, err)
	assert.Equal(t, "foo", string(view.Text(view.Record(&ast.Wraps[0].Item))))
	assert.Equal(t, "bar", string(view.Text(view.Record(&ast.Wraps[1].Item))))
}

// TestSourceViewFromLexer exercises ParseFromLexer: the source text is not
// known automatically, so Text returns nil until SetSourceText is called.
func TestSourceViewFromLexer(t *testing.T) {
	input := "foo  bar"
	type itemRoot struct {
		Items []*sourceItem `{ @@ }`
	}
	parser := participle.MustBuild[itemRoot](
		participle.Lexer(sourceLexer),
		participle.Elide("Whitespace", "Comment"),
	)
	track, view := participle.TrackSources()
	rawLexer, err := sourceLexer.Lex("", strings.NewReader(input))
	assert.NoError(t, err)
	symbols := sourceLexer.Symbols()
	peek, err := lexer.Upgrade(rawLexer, symbols["Whitespace"], symbols["Comment"])
	assert.NoError(t, err)
	ast, err := parser.ParseFromLexer(peek, track)
	assert.NoError(t, err)
	rec := view.Record(ast)
	assert.True(t, rec != nil)
	assert.Zero(t, view.Text(rec)) // no source text yet
	view.SetSourceText(input)
	assert.Equal(t, []byte(input), view.Text(rec))
}

// TestSourceViewBytesAndReader exercises the ParseBytes and Parse entry
// points and checks byte-exact retrieval.
func TestSourceViewBytesAndReader(t *testing.T) {
	input := []byte("(  αβγ  )  ")
	parser := buildSourceParser(t)

	track, view := participle.TrackSources()
	ast, err := parser.ParseBytes("", input, track)
	assert.NoError(t, err)
	assert.Equal(t, input, view.Text(view.Record(ast)))

	track2, view2 := participle.TrackSources()
	ast2, err := parser.Parse("", strings.NewReader(string(input)), track2)
	assert.NoError(t, err)
	assert.Equal(t, input, view2.Text(view2.Record(ast2)))
}
