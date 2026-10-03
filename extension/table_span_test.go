package extension

import (
	"strings"
	"testing"

	gast "github.com/yuin/goldmark/v2/ast"
	east "github.com/yuin/goldmark/v2/extension/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/testutil"
)

func newSpanMarkdown(errHandler TableCellMergeErrorHandler) testutil.MarkdownToStringFunc {
	opts := []TableParserOption{WithTableCellSpan()}
	if errHandler != nil {
		opts = append(opts, WithTableCellMergeErrorHandler(errHandler))
	}
	return testutil.NewMarkdownToStringFunc(
		parser.New(parser.WithExtensions(NewTableParser(opts...))),
		html.New(
			html.WithUnsafe(),
			html.WithXHTML(),
			html.WithExtensions(NewTableHTMLRenderer()),
		),
	)
}

func newPlainTableMarkdown() testutil.MarkdownToStringFunc {
	return testutil.NewMarkdownToStringFunc(
		parser.New(parser.WithExtensions(NewTableParser())),
		html.New(
			html.WithUnsafe(),
			html.WithXHTML(),
			html.WithExtensions(NewTableHTMLRenderer()),
		),
	)
}

func TestTableCellSpanRender(t *testing.T) {
	cases := []struct {
		description string
		markdown    string
		expected    string
		disabled    bool
	}{
		{
			description: "Horizontal and vertical merge forming a rectangle",
			markdown: `
| A | B | C |
| - | - | - |
| a | > | b |
| ^ | ^ | c |
| d | e | f |
`,
			expected: `<table>
<thead>
<tr>
<th>A</th>
<th>B</th>
<th>C</th>
</tr>
</thead>
<tbody>
<tr>
<td colspan="2" rowspan="2">a</td>
<td>b</td>
</tr>
<tr>
<td>c</td>
</tr>
<tr>
<td>d</td>
<td>e</td>
<td>f</td>
</tr>
</tbody>
</table>`,
		},
		{
			description: "Horizontal merges only",
			markdown: `
| A | B | C |
| - | - | - |
| a | > | > |
| b | c | d |
`,
			expected: `<table>
<thead>
<tr>
<th>A</th>
<th>B</th>
<th>C</th>
</tr>
</thead>
<tbody>
<tr>
<td colspan="3">a</td>
</tr>
<tr>
<td>b</td>
<td>c</td>
<td>d</td>
</tr>
</tbody>
</table>`,
		},
		{
			description: "Vertical merges within the body",
			markdown: `
| A | B |
| - | - |
| a | b |
| ^ | c |
| ^ | ^ |
`,
			expected: `<table>
<thead>
<tr>
<th>A</th>
<th>B</th>
</tr>
</thead>
<tbody>
<tr>
<td rowspan="3">a</td>
<td>b</td>
</tr>
<tr>
<td rowspan="2">c</td>
</tr>
<tr>
</tr>
</tbody>
</table>`,
		},
		{
			description: "Horizontal merge in the header keeps the alignment attribute",
			markdown: `
| A | > | B |
| :-- | :-: | --: |
| 1 | 2 | 3 |
`,
			expected: `<table>
<thead>
<tr>
<th align="left" colspan="2">A</th>
<th align="right">B</th>
</tr>
</thead>
<tbody>
<tr>
<td align="left">1</td>
<td align="center">2</td>
<td align="right">3</td>
</tr>
</tbody>
</table>`,
		},
		{
			description: "Empty cells are not markers",
			markdown: `
| A | B |
| - | - |
|   | > |
`,
			expected: `<table>
<thead>
<tr>
<th>A</th>
<th>B</th>
</tr>
</thead>
<tbody>
<tr>
<td colspan="2"></td>
</tr>
</tbody>
</table>`,
		},
		{
			description: "Rows of empty cells stay ordinary cells",
			markdown: `
| A | B |
| - | - |
|   |   |
| c | d |
`,
			expected: `<table>
<thead>
<tr>
<th>A</th>
<th>B</th>
</tr>
</thead>
<tbody>
<tr>
<td></td>
<td></td>
</tr>
<tr>
<td>c</td>
<td>d</td>
</tr>
</tbody>
</table>`,
		},
		{
			description: "Ragged rows are padded before span resolution",
			markdown: `
| A | B |
| - | - |
| a |
| ^ | b |
`,
			expected: `<table>
<thead>
<tr>
<th>A</th>
<th>B</th>
</tr>
</thead>
<tbody>
<tr>
<td rowspan="2">a</td>
<td></td>
</tr>
<tr>
<td>b</td>
</tr>
</tbody>
</table>`,
		},
		{
			description: "Header-only table with a horizontal merge",
			markdown: `
| A | > | B |
| - | - | - |
`,
			expected: `<table>
<thead>
<tr>
<th colspan="2">A</th>
<th>B</th>
</tr>
</thead>
</table>`,
		},
		{
			description: "Escaped marker and code span marker are ordinary text",
			markdown: `
| A | B |
| - | - |
| \> | ` + "`^`" + ` |
`,
			expected: `<table>
<thead>
<tr>
<th>A</th>
<th>B</th>
</tr>
</thead>
<tbody>
<tr>
<td>&gt;</td>
<td><code>^</code></td>
</tr>
</tbody>
</table>`,
		},
		{
			description: "Marker surrounded by spaces is ordinary text",
			markdown: `
| A | B |
| - | - |
| a | > b |
`,
			expected: `<table>
<thead>
<tr>
<th>A</th>
<th>B</th>
</tr>
</thead>
<tbody>
<tr>
<td>a</td>
<td>&gt; b</td>
</tr>
</tbody>
</table>`,
		},
		{
			description: "Markers are ordinary text when the span option is disabled",
			markdown: `
| A | B | C |
| - | - | - |
| a | > | ^ |
`,
			expected: `<table>
<thead>
<tr>
<th>A</th>
<th>B</th>
<th>C</th>
</tr>
</thead>
<tbody>
<tr>
<td>a</td>
<td>&gt;</td>
<td>^</td>
</tr>
</tbody>
</table>`,
			disabled: true,
		},
	}

	for i, tc := range cases {
		markdown := newSpanMarkdown(nil)
		if tc.disabled {
			markdown = newPlainTableMarkdown()
		}
		testutil.DoTestCase(markdown, testutil.MarkdownTestCase{
			No:          i + 1,
			Description: tc.description,
			Markdown:    tc.markdown,
			Expected:    tc.expected,
		}, t)
	}
}

func TestTableCellSpanStructuralErrors(t *testing.T) {
	cases := []struct {
		description string
		reason      TableCellMergeErrorReason
		row         int
		column      int
		checkPos    bool
		markdown    string
	}{
		{
			description: "merge left in the first column",
			reason:      TableCellMergeErrorMergeLeftInFirstColumn,
			row:         1,
			column:      0,
			checkPos:    true,
			markdown:    "| A | B |\n| - | - |\n| > | b |\n",
		},
		{
			description: "merge up in the first row",
			reason:      TableCellMergeErrorMergeUpInFirstRow,
			row:         0,
			column:      1,
			checkPos:    true,
			markdown:    "| A | ^ |\n| - | - |\n| a | b |\n",
		},
		{
			description: "merge across the header/body boundary",
			reason:      TableCellMergeErrorMergeAcrossHeaderBody,
			row:         1,
			column:      0,
			checkPos:    true,
			markdown:    "| A | B |\n| - | - |\n| ^ | b |\n",
		},
		{
			description: "L-shape: horizontal extension without the cell below",
			reason:      TableCellMergeErrorNonRectangular,
			markdown: "| A | B | C |\n| - | - | - |\n" +
				"| a | > | b |\n| ^ | c | d |\n",
		},
		{
			description: "L-shape: vertical extension without the cell on the right",
			reason:      TableCellMergeErrorNonRectangular,
			markdown: "| A | B | C |\n| - | - | - |\n" +
				"| a | b | > |\n| c | ^ | d |\n",
		},
		{
			description: "Ragged row leaves a hole in the merged rectangle",
			reason:      TableCellMergeErrorNonRectangular,
			markdown: "| A | B | C |\n| - | - | - |\n" +
				"| a | > | > |\n| ^\n",
		},
	}

	for i, tc := range cases {
		var got *TableCellMergeError
		markdown := newSpanMarkdown(func(err *TableCellMergeError) {
			got = err
		})
		out, err := markdown(tc.markdown)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Fatalf("case %d (%s): expected a structural error, got output:\n%s",
				i+1, tc.description, out)
		}
		if got.Reason != tc.reason {
			t.Fatalf("case %d (%s): reason = %q, want %q",
				i+1, tc.description, got.Reason, tc.reason)
		}
		if tc.checkPos && (got.Row != tc.row || got.Column != tc.column) {
			t.Fatalf("case %d (%s): position = (%d,%d), want (%d,%d)",
				i+1, tc.description, got.Row, got.Column, tc.row, tc.column)
		}
		if !strings.Contains(out, "<p>") {
			t.Fatalf("case %d (%s): malformed table must fall back to a paragraph, got:\n%s",
				i+1, tc.description, out)
		}
	}
}

func TestTableCellSpanAST(t *testing.T) {
	source := []byte("| A | B | C |\n| - | - | - |\n" +
		"| a | > | b |\n| ^ | ^ | c |\n| d | e | f |\n")
	p := parser.New(parser.WithExtensions(NewTableParser(WithTableCellSpan())))
	doc := p.Parse(source)

	table, ok := doc.FirstChild().(*east.Table)
	if !ok {
		t.Fatalf("expected a Table, got %T", doc.FirstChild())
	}
	header := table.FirstChild().(*east.TableHeader)
	body := header.NextSibling().(*east.TableBody)

	if childCount(header) != 3 {
		t.Fatalf("header cell count = %d, want 3", childCount(header))
	}

	rows := bodyRows(body)
	if len(rows) != 3 {
		t.Fatalf("body row count = %d, want 3", len(rows))
	}
	if childCount(rows[0]) != 2 || childCount(rows[1]) != 1 || childCount(rows[2]) != 3 {
		t.Fatalf("unexpected cell counts: %d %d %d",
			childCount(rows[0]), childCount(rows[1]), childCount(rows[2]))
	}

	origin := rows[0].FirstChild().(*east.TableCell)
	if origin.Colspan != 2 || origin.Rowspan != 2 {
		t.Fatalf("origin span = colspan %d rowspan %d, want 2/2",
			origin.Colspan, origin.Rowspan)
	}
	if len(origin.CoveredCells) != 3 {
		t.Fatalf("covered cell count = %d, want 3", len(origin.CoveredCells))
	}

	if err := gast.Walk(table, func(n gast.Node, entering bool) (gast.WalkStatus, error) {
		if !entering {
			return gast.WalkContinue, nil
		}
		cell, ok := n.(*east.TableCell)
		if !ok {
			return gast.WalkContinue, nil
		}
		for _, covered := range cell.CoveredCells {
			if covered.Parent() != nil {
				t.Errorf("covered cell must be detached from its row")
			}
		}
		return gast.WalkContinue, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func childCount(n gast.Node) int {
	count := 0
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		count++
	}
	return count
}

func bodyRows(body *east.TableBody) []*east.TableRow {
	var rows []*east.TableRow
	for n := body.FirstChild(); n != nil; n = n.NextSibling() {
		rows = append(rows, n.(*east.TableRow))
	}
	return rows
}
