package extension

import (
	"bytes"
	"strings"
	"testing"

	gast "github.com/yuin/goldmark/v2/ast"
	east "github.com/yuin/goldmark/v2/extension/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/text"
	"github.com/yuin/goldmark/v2/util"
)

func mathParser() parser.Parser {
	return parser.New(parser.WithExtensions(NewMathParser()))
}

func renderMath(t *testing.T, source string, opts ...MathHTMLRendererOption) string {
	t.Helper()

	markdown := util.StringToReadOnlyBytes(source)
	var output bytes.Buffer
	renderer := NewMathHTMLRenderer(opts...)
	doc := mathParser().Parse(markdown)
	err := html.New(html.WithExtensions(renderer)).Render(&output, markdown, doc)
	if err != nil {
		t.Fatalf("render math: %v", err)
	}
	return output.String()
}

func renderWithoutMath(t *testing.T, source string) string {
	t.Helper()

	markdown := util.StringToReadOnlyBytes(source)
	var output bytes.Buffer
	err := html.New().Render(&output, markdown, parser.New().Parse(markdown))
	if err != nil {
		t.Fatalf("render markdown: %v", err)
	}
	return output.String()
}

func parseMath(t *testing.T, source string) gast.Node {
	t.Helper()

	markdown := util.StringToReadOnlyBytes(source)
	return mathParser().Parse(markdown)
}

func findNode(node gast.Node, kind gast.NodeKind) gast.Node {
	var found gast.Node
	gast.Walk(node, func(child gast.Node, entering bool) (gast.WalkStatus, error) {
		if entering && child.Kind() == kind {
			found = child
			return gast.WalkStop, nil
		}
		return gast.WalkContinue, nil
	})
	return found
}

func collectNodes(node gast.Node, kind gast.NodeKind) []gast.Node {
	var found []gast.Node
	gast.Walk(node, func(child gast.Node, entering bool) (gast.WalkStatus, error) {
		if entering && child.Kind() == kind {
			found = append(found, child)
		}
		return gast.WalkContinue, nil
	})
	return found
}

func TestMathInlinePriceAndAdjacentFormulas(t *testing.T) {
	output := renderMath(t, "The price is $10 and 5 $, but $a+b$ and $c+d$.\n")

	if strings.Contains(output, "<code>10 and 5 </code>") {
		t.Fatalf("currency-like dollar text was parsed as math: %s", output)
	}
	if !strings.Contains(output, "<code>a+b</code> and <code>c+d</code>") {
		t.Fatalf("adjacent math formulas were not rendered separately: %s", output)
	}

	if got := renderMath(t, "$$x$$\n"); got != "<p>$<code>x</code>$</p>\n" {
		t.Fatalf("non-standalone dollar fence output = %q", got)
	}

	doc := parseMath(t, "$x$$y$\n")
	nodes := collectNodes(doc, east.KindInlineMath)
	if len(nodes) != 2 {
		t.Fatalf("expected 2 inline math nodes, got %d", len(nodes))
	}

	first := nodes[0].(*east.InlineMath)
	second := nodes[1].(*east.InlineMath)
	if got := first.Value.Str(util.StringToReadOnlyBytes("$x$$y$")); got != "x" {
		t.Fatalf("first formula = %q, want x", got)
	}
	if got := second.Value.Str(util.StringToReadOnlyBytes("$x$$y$")); got != "y" {
		t.Fatalf("second formula = %q, want y", got)
	}
}

func TestMathEscapedDollarsAndBackslashes(t *testing.T) {
	source := `Price \$5 and $\alpha\\beta$ and $\$x\$$.` + "\n"
	output := renderMath(t, source)

	if !strings.Contains(output, `Price $5 and <code>\alpha\\beta</code> and <code>\$x\$</code>.`) {
		t.Fatalf("unexpected escaped math output: %s", output)
	}

	doc := parseMath(t, source)
	node := findNode(doc, east.KindInlineMath).(*east.InlineMath)
	if got := node.Value.Str(util.StringToReadOnlyBytes(source)); got != `\alpha\\beta` {
		t.Fatalf("formula body = %q, want raw backslashes", got)
	}
}

func TestMathCodeSpansAndEmphasisAndLinks(t *testing.T) {
	output := renderMath(t, "code `$x$`, *$x$*, and [link $x$](https://example.com/?$x).\n")

	for _, expected := range []string{
		"<code>$x$</code>",
		"<em><code>x</code></em>",
		`<a href="https://example.com/?$x">link <code>x</code></a>`,
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("output %q does not contain %q", output, expected)
		}
	}
}

func TestMathBlockInBlockquote(t *testing.T) {
	source := "> $$\n> a \\ne b\n>\n> $$\n"
	output := renderMath(t, source)

	if !strings.Contains(output, "<blockquote>\n<pre><code>a \\ne b\n\n</code></pre>\n</blockquote>") {
		t.Fatalf("blockquote math block was not rendered with raw bytes: %s", output)
	}

	doc := parseMath(t, source)
	node := findNode(doc, east.KindMathBlock).(*east.MathBlock)
	if got := node.Value.Str(util.StringToReadOnlyBytes(source)); got != "a \\ne b\n\n" {
		t.Fatalf("math block body = %q", got)
	}
}

func TestMathBlockMultipleLinesAndClosingLine(t *testing.T) {
	source := "  $$\na \\ne b\n  c\n  $$\n"
	output := renderMath(t, source)

	if !strings.Contains(output, "<pre><code>a \\ne b\n  c\n</code></pre>") {
		t.Fatalf("multiline math block output = %q", output)
	}
}

func TestMathUnclosedInputsRemainText(t *testing.T) {
	inline := parseMath(t, "text $x\n\nmore\n")
	if findNode(inline, east.KindInlineMath) != nil {
		t.Fatal("unclosed inline math created an AST node")
	}
	if got := renderMath(t, "text $x\n\nmore\n"); got != "<p>text $x</p>\n<p>more</p>\n" {
		t.Fatalf("unclosed inline math HTML = %q", got)
	}

	inlineEOF := parseMath(t, "text $x")
	if findNode(inlineEOF, east.KindInlineMath) != nil {
		t.Fatal("unclosed inline math at EOF created an AST node")
	}
	if got := renderMath(t, "text $x"); got != "<p>text $x</p>\n" {
		t.Fatalf("unclosed inline EOF HTML = %q", got)
	}

	block := parseMath(t, "$$\nx\n")
	node := findNode(block, east.KindMathBlock).(*east.MathBlock)
	if got := node.Value.Str(util.StringToReadOnlyBytes("$$\nx\n")); got != "x\n" {
		t.Fatalf("unclosed math block body = %q", got)
	}
}

func TestMathDisabledKeepsCommonMarkOutput(t *testing.T) {
	source := "$x$\n\n$$\nx\n$$\n"
	got := renderWithoutMath(t, source)
	want := "<p>$x$</p>\n<p>$$\nx\n$$</p>\n"
	if got != want {
		t.Fatalf("disabled math output = %q, want %q", got, want)
	}
}

func TestMathHTMLRendererOptions(t *testing.T) {
	output := renderMath(
		t,
		"$x$\n\n$$\ny\n$$\n",
		WithMathInlineTag("span"),
		WithMathBlockTag("math"),
	)

	if !strings.Contains(output, "<span>x</span>") {
		t.Fatalf("custom inline tag output = %q", output)
	}
	if !strings.Contains(output, "<pre><math>y\n</math></pre>") {
		t.Fatalf("custom block tag output = %q", output)
	}
}

func TestMathEscapesHTML(t *testing.T) {
	output := renderMath(t, "$<b>&\"x</b>$\n\n$$\n<img onerror=1>\n$$\n")

	if strings.Contains(output, "<b>") || strings.Contains(output, "<img onerror=1>") {
		t.Fatalf("math source was rendered without escaping: %s", output)
	}
	if !strings.Contains(output, "&lt;b&gt;&amp;&quot;x&lt;/b&gt;") {
		t.Fatalf("inline math was not HTML escaped: %s", output)
	}
	if !strings.Contains(output, "&lt;img onerror=1&gt;") {
		t.Fatalf("block math was not HTML escaped: %s", output)
	}
}

func TestMathInlineDoesNotCrossBlankLine(t *testing.T) {
	doc := parseMath(t, "$a\n\nb$\n")
	if findNode(doc, east.KindInlineMath) != nil {
		t.Fatal("inline math crossed a blank line")
	}
}

func TestMathInlineEmptyValue(t *testing.T) {
	doc := parseMath(t, "$ $ and $$\n")
	if findNode(doc, east.KindInlineMath) != nil {
		t.Fatal("whitespace immediately after an opener created inline math")
	}
}

func TestMathInlineCloserRestrictions(t *testing.T) {
	doc := parseMath(t, "$a $ and $a$5\n")
	if findNode(doc, east.KindInlineMath) != nil {
		t.Fatal("invalid closer created inline math")
	}

	doc = parseMath(t, "$$ and $x\n$ $\n")
	if findNode(doc, east.KindInlineMath) != nil {
		t.Fatal("empty formula or opener followed by a line break created inline math")
	}
}

func TestMathInlineCanSpanSoftLineBreak(t *testing.T) {
	doc := parseMath(t, "$a\nb$\n")
	node := findNode(doc, east.KindInlineMath).(*east.InlineMath)
	got := node.Value.Str(util.StringToReadOnlyBytes("$a\nb$"))
	if got != "a\nb" {
		t.Fatalf("multiline inline math = %q, want a\nb", got)
	}
}

func TestNewInlineMathNode(t *testing.T) {
	value := text.NewMultiLineValueFromString("x", text.IdentityDecoder)
	node := east.NewInlineMath(value)
	if node.Kind() != east.KindInlineMath {
		t.Fatalf("kind = %s", node.Kind())
	}
	if node.Value.Str(nil) != "x" {
		t.Fatal("inline math value was not retained")
	}
}

func TestNewMathBlockNode(t *testing.T) {
	value := text.NewLinesFromString("x\n")
	node := east.NewMathBlock(value)
	if node.Kind() != east.KindMathBlock {
		t.Fatalf("kind = %s", node.Kind())
	}
	if node.Value.Str(nil) != "x\n" {
		t.Fatal("math block value was not retained")
	}
}
