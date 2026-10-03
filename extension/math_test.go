package extension

import (
	"bytes"
	"strings"
	"testing"

	gast "github.com/yuin/goldmark/v2/ast"
	east "github.com/yuin/goldmark/v2/extension/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/testutil"
	"github.com/yuin/goldmark/v2/util"
)

func newMathMarkdown(rendererOpts ...html.Option) testutil.MarkdownToStringFunc {
	return testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewMathParser()),
		),
		html.New(
			append([]html.Option{html.WithExtensions(NewMathHTMLRenderer())}, rendererOpts...)...,
		),
	)
}

func TestMath(t *testing.T) {
	testutil.DoTestCaseFile(newMathMarkdown(), "testdata/math.txt", t, testutil.ParseCliCaseArg()...)
}

func TestMathCustomTags(t *testing.T) {
	markdown := newMathMarkdown(html.WithExtensions(
		NewMathHTMLRenderer(WithInlineMathTag("span"), WithBlockMathTag("div")),
	))
	out, err := markdown("$x + y$\n\n$$\na = b\n$$\n")
	if err != nil {
		t.Fatal(err)
	}
	want := "<p><span>x + y</span></p>\n<div>a = b\n</div>\n"
	if out != want {
		t.Errorf("custom tags mismatch:\nwant: %q\ngot:  %q", want, out)
	}
}

func TestMathDisabledByDefault(t *testing.T) {
	source := "$x$\n\n$$\ny\n$$\n"
	doc := parser.New().ParseStringSource(source)
	var buf bytes.Buffer
	if err := html.New().Render(&buf, util.StringToReadOnlyBytes(source), doc); err != nil {
		t.Fatal(err)
	}
	want := "<p>$x$</p>\n<p>$$\ny\n$$</p>\n"
	if buf.String() != want {
		t.Errorf("disabled math output mismatch:\nwant: %q\ngot:  %q", want, buf.String())
	}
}

func TestMathDedicatedASTNodes(t *testing.T) {
	source := "$x$\n\n$$\ny\n$$\n"
	doc := parser.New(
		parser.WithExtensions(NewMathParser()),
	).ParseStringSource(source)

	var inline *east.InlineMath
	var block *east.BlockMath
	if err := gast.Walk(doc, func(n gast.Node, entering bool) (gast.WalkStatus, error) {
		if !entering {
			return gast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *east.InlineMath:
			inline = v
		case *east.BlockMath:
			block = v
		}
		return gast.WalkContinue, nil
	}); err != nil {
		t.Fatal(err)
	}
	if inline == nil {
		t.Fatal("expected an InlineMath node")
	}
	if block == nil {
		t.Fatal("expected a BlockMath node")
	}
	src := util.StringToReadOnlyBytes(source)
	if got := strings.TrimSpace(inline.Value.Str(src)); got != "x" {
		t.Errorf("inline math value = %q, want %q", got, "x")
	}
	if got := block.Value.Str(src); got != "y\n" {
		t.Errorf("block math value = %q, want %q", got, "y\n")
	}
}
