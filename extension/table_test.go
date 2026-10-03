package extension

import (
	"strings"
	"testing"

	"github.com/yuin/goldmark/v2/ast"
	east "github.com/yuin/goldmark/v2/extension/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/testutil"
	"github.com/yuin/goldmark/v2/text"
	"github.com/yuin/goldmark/v2/util"
)

func TestTable(t *testing.T) {
	markdown := testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewTableParser()),
		),
		html.New(
			html.WithUnsafe(),
			html.WithXHTML(),
			html.WithExtensions(NewTableHTMLRenderer()),
		),
	)
	testutil.DoTestCaseFile(markdown, "testdata/table.txt", t, testutil.ParseCliCaseArg()...)
}

func newTableMergeParser() parser.Parser {
	return parser.New(
		parser.WithExtensions(NewTableParser(WithTableCellMerge())),
	)
}

func newTableMergeMarkdown() testutil.MarkdownToStringFunc {
	return testutil.NewMarkdownToStringFunc(
		newTableMergeParser(),
		html.New(
			html.WithUnsafe(),
			html.WithXHTML(),
			html.WithExtensions(NewTableHTMLRenderer()),
		),
	)
}

func testTableCellAt(node ast.Node, index int) *east.TableCell {
	cell := node.FirstChild()
	for i := 0; i < index; i++ {
		cell = cell.NextSibling()
	}
	return cell.(*east.TableCell)
}

func TestTableCellMerge(t *testing.T) {
	source := `| H1 | >  | H3 |
|:--|:--:|--:|
| A  | >  | C  |
| ^  | >  | D  |
`
	doc := newTableMergeParser().Parse([]byte(source))
	table, ok := doc.FirstChild().(*east.Table)
	if !ok {
		t.Fatalf("expected a table, got %T", doc.FirstChild())
	}

	header := table.FirstChild().(*east.TableHeader)
	body := table.LastChild().(*east.TableBody)
	firstBodyRow := body.FirstChild().(*east.TableRow)
	secondBodyRow := firstBodyRow.NextSibling().(*east.TableRow)

	headerOrigin := testTableCellAt(header, 0)
	headerCovered := testTableCellAt(header, 1)
	if headerOrigin.ColSpan != 2 || headerOrigin.RowSpan != 1 || headerOrigin.OriginCell != nil {
		t.Fatalf("unexpected header origin: rowspan=%d colspan=%d origin=%v",
			headerOrigin.RowSpan, headerOrigin.ColSpan, headerOrigin.OriginCell)
	}
	if headerCovered.OriginCell != headerOrigin {
		t.Fatal("header covered cell does not point to origin")
	}

	origin := testTableCellAt(firstBodyRow, 0)
	rightCovered := testTableCellAt(firstBodyRow, 1)
	belowCovered := testTableCellAt(secondBodyRow, 0)
	diagonalCovered := testTableCellAt(secondBodyRow, 1)

	if origin.RowSpan != 2 || origin.ColSpan != 2 || origin.OriginCell != nil {
		t.Fatalf("unexpected origin cell: rowspan=%d colspan=%d origin=%v",
			origin.RowSpan, origin.ColSpan, origin.OriginCell)
	}
	for name, cell := range map[string]*east.TableCell{
		"right":    rightCovered,
		"below":    belowCovered,
		"diagonal": diagonalCovered,
	} {
		if cell.OriginCell != origin {
			t.Fatalf("%s covered cell does not point to origin", name)
		}
		if len(cell.Source()) != 0 {
			t.Fatalf("%s covered cell retained marker source", name)
		}
	}

	ordinaryCells := []*east.TableCell{
		testTableCellAt(header, 2),
		testTableCellAt(firstBodyRow, 2),
		testTableCellAt(secondBodyRow, 2),
	}
	for i, cell := range ordinaryCells {
		if cell.RowSpan != 1 || cell.ColSpan != 1 || cell.OriginCell != nil {
			t.Fatalf("ordinary cell %d: rowspan=%d colspan=%d origin=%v",
				i, cell.RowSpan, cell.ColSpan, cell.OriginCell)
		}
	}

	markdown := newTableMergeMarkdown()
	htmlOutput, err := markdown(source)
	if err != nil {
		t.Fatal(err)
	}
	expected := `<table>
<thead>
<tr>
<th colspan="2" align="left">H1</th>
<th align="right">H3</th>
</tr>
</thead>
<tbody>
<tr>
<td colspan="2" rowspan="2" align="left">A</td>
<td align="right">C</td>
</tr>
<tr>
<td align="right">D</td>
</tr>
</tbody>
</table>`
	if strings.TrimSpace(htmlOutput) != expected {
		t.Fatalf("unexpected HTML:\n%s", htmlOutput)
	}
}

func TestTableCellMergeEmptyAndRaggedRows(t *testing.T) {
	source := `| H1 | H2 |
|---|---|
|    | >  |
| ^  | ^  |
| z  | extra | > |
`
	doc := newTableMergeParser().Parse([]byte(source))
	table, ok := doc.FirstChild().(*east.Table)
	if !ok {
		t.Fatalf("expected a table, got %T", doc.FirstChild())
	}

	body := table.LastChild().(*east.TableBody)
	if body.ChildCount() != 3 {
		t.Fatalf("delimiter row was parsed as a body row: %d body rows", body.ChildCount())
	}
	firstRow := body.FirstChild().(*east.TableRow)
	secondRow := firstRow.NextSibling().(*east.TableRow)
	thirdRow := secondRow.NextSibling().(*east.TableRow)
	origin := testTableCellAt(firstRow, 0)
	if origin.RowSpan != 2 || origin.ColSpan != 2 {
		t.Fatalf("empty origin spans = %dx%d", origin.RowSpan, origin.ColSpan)
	}
	if thirdRow.ChildCount() != 2 {
		t.Fatalf("expected the ragged row to be truncated to the delimiter column count")
	}
	for i := 0; i < thirdRow.ChildCount(); i++ {
		cell := testTableCellAt(thirdRow, i)
		if cell.OriginCell != nil {
			t.Fatalf("extra-column marker was incorrectly merged")
		}
	}

	htmlOutput, err := newTableMergeMarkdown()(source)
	if err != nil {
		t.Fatal(err)
	}
	expected := `<table>
<thead>
<tr>
<th>H1</th>
<th>H2</th>
</tr>
</thead>
<tbody>
<tr>
<td colspan="2" rowspan="2"></td>
</tr>
<tr>
</tr>
<tr>
<td>z</td>
<td>extra</td>
</tr>
</tbody>
</table>`
	if strings.TrimSpace(htmlOutput) != expected {
		t.Fatalf("unexpected HTML:\n%s", htmlOutput)
	}
}

func TestTableCellMergeInvalidStructure(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{
			name: "L shape",
			source: `| A | > | C |
|---|---|---|
| ^ | D | E |
`,
		},
		{
			name: "non-rectangular T shape",
			source: `| A | > | > |
|---|---|---|
| ^ | D | > |
| ^ | ^ | G |
`,
		},
		{
			name: "merge from first column",
			source: `| > | A |
|---|---|
| B | C |
`,
		},
		{
			name: "merge from first row",
			source: `| ^ | A |
|---|---|
| B | C |
`,
		},
		{
			name: "merge across header and body",
			source: `| A | B |
|---|---|
| ^ | > |
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := newTableMergeParser().Parse([]byte(tt.source))
			if _, ok := doc.FirstChild().(*east.Table); ok {
				t.Fatal("invalid merge structure was accepted as a table")
			}
			htmlOutput, err := newTableMergeMarkdown()(tt.source)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(htmlOutput, "<table>") {
				t.Fatalf("invalid merge structure rendered as a table:\n%s", htmlOutput)
			}
		})
	}
}

func TestTableCellMergeInvalidCandidateDoesNotBlockLaterTable(t *testing.T) {
	source := `| > | invalid |
|---|---|
| A | > |

| valid | V |
|---|---|
| B | > |
`
	doc := newTableMergeParser().Parse([]byte(source))
	tableNode := doc.LastChild()
	if _, ok := tableNode.(*east.Table); !ok {
		t.Fatalf("expected the later valid table to be parsed, got %T", tableNode)
	}
	htmlOutput, err := newTableMergeMarkdown()(source)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(htmlOutput, `<td colspan="2">B</td>`) {
		t.Fatalf("later valid table was not rendered:\n%s", htmlOutput)
	}
}

func TestTableCellMergeDisabled(t *testing.T) {
	source := `| H1 | H2 |
|---|---|
| A  | >  |
| ^  | >  |
`
	markdown := testutil.NewMarkdownToStringFunc(
		parser.New(parser.WithExtensions(NewTableParser())),
		html.New(
			html.WithUnsafe(),
			html.WithXHTML(),
			html.WithExtensions(NewTableHTMLRenderer()),
		),
	)
	htmlOutput, err := markdown(source)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(htmlOutput, "rowspan") || strings.Contains(htmlOutput, "colspan") {
		t.Fatalf("merge attributes were rendered with the switch disabled:\n%s", htmlOutput)
	}
	if !strings.Contains(htmlOutput, "<td>&gt;</td>") || !strings.Contains(htmlOutput, "<td>^</td>") {
		t.Fatalf("merge markers were not rendered as text:\n%s", htmlOutput)
	}
}

func TestTableCellMergeMarkersInEscapeAndCodeSpan(t *testing.T) {
	source := `| \> | ` + "`>`" + ` |` + "\n" +
		`|---|---|` + "\n" +
		`| \> | ` + "`>`" + ` |` + "\n" +
		`| \^ | ` + "`^`" + ` |` + "\n" +
		`| z | z |`
	doc := newTableMergeParser().Parse([]byte(source))
	table, ok := doc.FirstChild().(*east.Table)
	if !ok {
		t.Fatalf("expected a table, got %T", doc.FirstChild())
	}
	body := table.LastChild().(*east.TableBody)
	for row := body.FirstChild(); row != nil; row = row.NextSibling() {
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			tableCell := cell.(*east.TableCell)
			if tableCell.OriginCell != nil {
				t.Fatal("an escaped or code-span marker was used as a merge marker")
			}
		}
	}

	htmlOutput, err := newTableMergeMarkdown()(source)
	if err != nil {
		t.Fatal(err)
	}
	expected := `<table>
<thead>
<tr>
<th>&gt;</th>
<th><code>&gt;</code></th>
</tr>
</thead>
<tbody>
<tr>
<td>&gt;</td>
<td><code>&gt;</code></td>
</tr>
<tr>
<td>^</td>
<td><code>^</code></td>
</tr>
<tr>
<td>z</td>
<td>z</td>
</tr>
</tbody>
</table>`
	if strings.TrimSpace(htmlOutput) != expected {
		t.Fatalf("unexpected HTML:\n%s", htmlOutput)
	}
}

func TestTableWithAlignDefault(t *testing.T) {
	markdown := testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewTableParser()),
		),
		html.New(
			html.WithXHTML(),
			html.WithUnsafe(),
			html.WithExtensions(NewTableHTMLRenderer(
				WithTableCellAlignMethod(TableCellAlignDefault),
			)),
		),
	)
	testutil.DoTestCase(
		markdown,
		testutil.MarkdownTestCase{
			No:          1,
			Description: "Cell with TableCellAlignDefault and XHTML should be rendered as an align attribute",
			Markdown: `
| abc | defghi |
:-: | -----------:
bar | baz
`,
			Expected: `<table>
<thead>
<tr>
<th align="center">abc</th>
<th align="right">defghi</th>
</tr>
</thead>
<tbody>
<tr>
<td align="center">bar</td>
<td align="right">baz</td>
</tr>
</tbody>
</table>`,
		},
		t,
	)

	markdown = testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewTableParser()),
		),
		html.New(
			html.WithUnsafe(),
			html.WithExtensions(NewTableHTMLRenderer(
				WithTableCellAlignMethod(TableCellAlignDefault),
			)),
		),
	)
	testutil.DoTestCase(
		markdown,
		testutil.MarkdownTestCase{
			No:          2,
			Description: "Cell with TableCellAlignDefault and HTML5 should be rendered as a style attribute",
			Markdown: `
| abc | defghi |
:-: | -----------:
bar | baz
`,
			Expected: `<table>
<thead>
<tr>
<th style="text-align:center">abc</th>
<th style="text-align:right">defghi</th>
</tr>
</thead>
<tbody>
<tr>
<td style="text-align:center">bar</td>
<td style="text-align:right">baz</td>
</tr>
</tbody>
</table>`,
		},
		t,
	)
}

func TestTableWithAlignAttribute(t *testing.T) {
	markdown := testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewTableParser()),
		),
		html.New(
			html.WithXHTML(),
			html.WithUnsafe(),
			html.WithExtensions(NewTableHTMLRenderer(
				WithTableCellAlignMethod(TableCellAlignAttribute),
			)),
		),
	)
	testutil.DoTestCase(
		markdown,
		testutil.MarkdownTestCase{
			No:          1,
			Description: "Cell with TableCellAlignAttribute and XHTML should be rendered as an align attribute",
			Markdown: `
| abc | defghi |
:-: | -----------:
bar | baz
`,
			Expected: `<table>
<thead>
<tr>
<th align="center">abc</th>
<th align="right">defghi</th>
</tr>
</thead>
<tbody>
<tr>
<td align="center">bar</td>
<td align="right">baz</td>
</tr>
</tbody>
</table>`,
		},
		t,
	)

	markdown = testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewTableParser()),
		),
		html.New(
			html.WithUnsafe(),
			html.WithExtensions(NewTableHTMLRenderer(
				WithTableCellAlignMethod(TableCellAlignAttribute),
			)),
		),
	)
	testutil.DoTestCase(
		markdown,
		testutil.MarkdownTestCase{
			No:          2,
			Description: "Cell with TableCellAlignAttribute and HTML5 should be rendered as an align attribute",
			Markdown: `
| abc | defghi |
:-: | -----------:
bar | baz
`,
			Expected: `<table>
<thead>
<tr>
<th align="center">abc</th>
<th align="right">defghi</th>
</tr>
</thead>
<tbody>
<tr>
<td align="center">bar</td>
<td align="right">baz</td>
</tr>
</tbody>
</table>`,
		},
		t,
	)
}

type tableStyleTransformer struct {
}

func (a *tableStyleTransformer) Transform(node *ast.Document, _ text.Reader, _ parser.Context) {
	cell := node.FirstChild().FirstChild().FirstChild().(*east.TableCell)
	cell.SetAttribute("style", text.NewMultiLineValueFromString("font-size:1em", text.IdentityDecoder))
}

func TestTableWithAlignStyle(t *testing.T) {
	markdown := testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewTableParser()),
		),
		html.New(
			html.WithXHTML(),
			html.WithUnsafe(),
			html.WithExtensions(NewTableHTMLRenderer(
				WithTableCellAlignMethod(TableCellAlignStyle),
			)),
		),
	)
	testutil.DoTestCase(
		markdown,
		testutil.MarkdownTestCase{
			No:          1,
			Description: "Cell with TableCellAlignStyle and XHTML should be rendered as a style attribute",
			Markdown: `
| abc | defghi |
:-: | -----------:
bar | baz
`,
			Expected: `<table>
<thead>
<tr>
<th style="text-align:center">abc</th>
<th style="text-align:right">defghi</th>
</tr>
</thead>
<tbody>
<tr>
<td style="text-align:center">bar</td>
<td style="text-align:right">baz</td>
</tr>
</tbody>
</table>`,
		},
		t,
	)

	markdown = testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewTableParser()),
		),
		html.New(
			html.WithUnsafe(),
			html.WithExtensions(NewTableHTMLRenderer(
				WithTableCellAlignMethod(TableCellAlignStyle),
			)),
		),
	)
	testutil.DoTestCase(
		markdown,
		testutil.MarkdownTestCase{
			No:          2,
			Description: "Cell with TableCellAlignStyle and HTML5 should be rendered as a style attribute",
			Markdown: `
| abc | defghi |
:-: | -----------:
bar | baz
`,
			Expected: `<table>
<thead>
<tr>
<th style="text-align:center">abc</th>
<th style="text-align:right">defghi</th>
</tr>
</thead>
<tbody>
<tr>
<td style="text-align:center">bar</td>
<td style="text-align:right">baz</td>
</tr>
</tbody>
</table>`,
		},
		t,
	)

	markdown = testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithASTTransformers(
				util.Prioritized[parser.ASTTransformer](&tableStyleTransformer{}, 0),
			),
			parser.WithExtensions(NewTableParser()),
		),
		html.New(
			html.WithUnsafe(),
			html.WithExtensions(NewTableHTMLRenderer(
				WithTableCellAlignMethod(TableCellAlignStyle),
			)),
		),
	)

	testutil.DoTestCase(
		markdown,
		testutil.MarkdownTestCase{
			No:          3,
			Description: "Styled cell should not be broken the style by the alignments",
			Markdown: `
| abc | defghi |
:-: | -----------:
bar | baz
`,
			Expected: `<table>
<thead>
<tr>
<th style="font-size:1em;text-align:center">abc</th>
<th style="text-align:right">defghi</th>
</tr>
</thead>
<tbody>
<tr>
<td style="text-align:center">bar</td>
<td style="text-align:right">baz</td>
</tr>
</tbody>
</table>`,
		},
		t,
	)
}

func TestTableWithAlignNone(t *testing.T) {
	markdown := testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewTableParser()),
		),
		html.New(
			html.WithXHTML(),
			html.WithUnsafe(),
			html.WithExtensions(NewTableHTMLRenderer(
				WithTableCellAlignMethod(TableCellAlignNone),
			)),
		),
	)
	testutil.DoTestCase(
		markdown,
		testutil.MarkdownTestCase{
			No:          1,
			Description: "Cell with TableCellAlignStyle and XHTML should not be rendered",
			Markdown: `
| abc | defghi |
:-: | -----------:
bar | baz
`,
			Expected: `<table>
<thead>
<tr>
<th>abc</th>
<th>defghi</th>
</tr>
</thead>
<tbody>
<tr>
<td>bar</td>
<td>baz</td>
</tr>
</tbody>
</table>`,
		},
		t,
	)
}

func TestTableFuzzedPanics(t *testing.T) {
	markdown := testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewTableParser()),
		),
		html.New(
			html.WithXHTML(),
			html.WithUnsafe(),
			html.WithExtensions(NewTableHTMLRenderer()),
		),
	)
	testutil.DoTestCase(
		markdown,
		testutil.MarkdownTestCase{
			No:          1,
			Description: "This should not panic",
			Markdown:    "* 0\n-|\n\t0",
			Expected: `<ul>
<li>
<table>
<thead>
<tr>
<th>0</th>
</tr>
</thead>
<tbody>
<tr>
<td>0</td>
</tr>
</tbody>
</table>
</li>
</ul>`,
		},
		t,
	)
}
