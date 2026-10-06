package text_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension"
	gast "github.com/yuin/goldmark/v2/extension/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer"
	gtext "github.com/yuin/goldmark/v2/renderer/text"
	"github.com/yuin/goldmark/v2/util"
)

func render(t *testing.T, source string, opts ...gtext.Option) string {
	t.Helper()
	p := parser.New()
	r := gtext.New(opts...)
	var buf bytes.Buffer
	b := util.StringToReadOnlyBytes(source)
	doc := p.Parse(b)
	if err := r.Render(&buf, b, doc); err != nil {
		t.Fatalf("unexpected render error: %v", err)
	}
	return buf.String()
}

func renderWithParser(t *testing.T, p parser.Parser, source string, opts ...gtext.Option) string {
	t.Helper()
	r := gtext.New(opts...)
	var buf bytes.Buffer
	b := util.StringToReadOnlyBytes(source)
	doc := p.Parse(b)
	if err := r.Render(&buf, b, doc); err != nil {
		t.Fatalf("unexpected render error: %v", err)
	}
	return buf.String()
}

const sampleMarkdown = "# Title: a &amp; b\n" +
	"\n" +
	"A paragraph with `inline &amp; code`, a copyright\n" +
	"symbol &copy;, an escaped \\*star\\* and a [link\n" +
	"label](https://example.com).\n" +
	"\n" +
	"- first item with\n" +
	"  soft wrap\n" +
	"  - nested one\n" +
	"  - nested two with `code`\n" +
	"  1. ordered a\n" +
	"  2. ordered b\n" +
	"- second item\n" +
	"\n" +
	"Loose list:\n" +
	"\n" +
	"1. loose first\n" +
	"\n" +
	"   second paragraph\n" +
	"2. loose second\n" +
	"\n" +
	"```\n" +
	"func main() {\n" +
	"\tprintln(\"hi &amp;\")\n" +
	"}\n" +
	"```\n" +
	"\n" +
	"![alt *text*](image.png) and ![logo](logo.png).\n"

const expectedDefault = "Title: a & b\n" +
	"\n" +
	"A paragraph with `inline &amp; code`, a copyright symbol ©, an escaped *star* and a link label.\n" +
	"\n" +
	"- first item with soft wrap\n" +
	"  - nested one\n" +
	"  - nested two with `code`\n" +
	"  1. ordered a\n" +
	"  2. ordered b\n" +
	"- second item\n" +
	"\n" +
	"Loose list:\n" +
	"\n" +
	"1. loose first\n" +
	"\n" +
	"   second paragraph\n" +
	"2. loose second\n" +
	"\n" +
	"func main() {\n" +
	"\tprintln(\"hi &amp;\")\n" +
	"}\n" +
	"\n" +
	"alt text and logo.\n"

// TestGoldenSample fixes the plain text output for a document that contains
// nested lists, inline code, entities, images and soft line breaks.
func TestGoldenSample(t *testing.T) {
	got := render(t, sampleMarkdown)
	if got != expectedDefault {
		t.Fatalf("rendered text mismatch:\n--- got ---\n%q\n--- want ---\n%q\n", got, expectedDefault)
	}
}

func TestGoldenSampleWithURLs(t *testing.T) {
	src := "![alt text](https://example.com/a.png)\n\n[x](https://example.com/x)\n"
	want := "alt text (https://example.com/a.png)\n\nx (https://example.com/x)\n"
	if got := render(t, src, gtext.WithImageURLs(), gtext.WithLinkURLs()); got != want {
		t.Fatalf("rendered text mismatch:\n--- got ---\n%q\n--- want ---\n%q\n", got, want)
	}
}

func TestSoftLineBreakOption(t *testing.T) {
	src := "one\ntwo\n"
	if got := render(t, src); got != "one two\n" {
		t.Fatalf("default soft break, got %q", got)
	}
	if got := render(t, src, gtext.WithSoftLineBreakNewline()); got != "one\ntwo\n" {
		t.Fatalf("newline soft break, got %q", got)
	}

	hard := "a  \nb\n"
	if got := render(t, hard); got != "a\nb\n" {
		t.Fatalf("hard break, got %q", got)
	}
}

func TestParagraphSeparation(t *testing.T) {
	src := "first\n\n\n\nsecond\n"
	want := "first\n\nsecond\n"
	if got := render(t, src); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRawHTMLOptions(t *testing.T) {
	src := "<div>raw block</div>\n\ntext <em>inline</em>\n"
	if got := render(t, src); got != "text inline\n" {
		t.Fatalf("default should drop raw HTML, got %q", got)
	}
	want := "<div>raw block</div>\n\ntext <em>inline</em>\n"
	if got := render(t, src, gtext.WithRawHTML()); got != want {
		t.Fatalf("WithRawHTML mismatch:\n--- got ---\n%q\n--- want ---\n%q\n", got, want)
	}
}

func TestEntitiesDecodedOnce(t *testing.T) {
	// Entities are decoded by the parser exactly once; the text renderer must
	// not decode them again nor escape them.
	cases := map[string]string{
		"&amp;amp;\n":    "&amp;\n",
		"&#35; &#x23;\n": "# #\n",
		"\\* \\[ \\`\n":  "* [ `\n",
	}
	for src, want := range cases {
		if got := render(t, src); got != want {
			t.Fatalf("src=%q got %q want %q", src, got, want)
		}
	}
}

// failingWriter fails with errFail on the first Write call.
type failingWriter struct {
	err error
}

func (w *failingWriter) Write(_ []byte) (int, error) {
	return 0, w.err
}

var errFail = errors.New("simulated write failure")

// TestRendererPropagatesWriterError verifies the renderer surfaces writer
// errors instead of swallowing them. It uses only in-memory types and does not
// depend on a browser or external tools.
func TestRendererPropagatesWriterError(t *testing.T) {
	p := parser.New()
	r := gtext.New()
	source := "some text\n"
	doc := p.Parse(util.StringToReadOnlyBytes(source))
	err := r.Render(&failingWriter{err: errFail}, util.StringToReadOnlyBytes(source), doc)
	if !errors.Is(err, errFail) {
		t.Fatalf("expected errFail to propagate, got %v", err)
	}
}

// errorExtension registers a renderer for Text that aborts the render with an
// error. Extensions applied after the built-in CommonMark extension take
// precedence.
type errorExtension struct{}

func (errorExtension) RendererOptions(*gtext.Config) []gtext.Option {
	return []gtext.Option{
		gtext.WithNodeRenderer(ast.KindText, gtext.NodeRendererFunc(
			func(_ io.Writer, _ []byte, _ ast.Node, entering bool, _ renderer.Context) (ast.WalkStatus, error) {
				if entering {
					return ast.WalkStop, errCustom
				}
				return ast.WalkContinue, nil
			})),
	}
}

var errCustom = errors.New("boom")

// TestCustomNodeRendererCanError verifies a registered node renderer can stop
// the walk and return an error upwards.
func TestCustomNodeRendererCanError(t *testing.T) {
	p := parser.New()
	r := gtext.New(gtext.WithExtensions(errorExtension{}))
	source := "abc\n"
	doc := p.Parse(util.StringToReadOnlyBytes(source))
	var buf strings.Builder
	err := r.Render(&buf, util.StringToReadOnlyBytes(source), doc)
	if !errors.Is(err, errCustom) {
		t.Fatalf("expected custom renderer error, got %v", err)
	}
}

// TestUnknownExtensionNodesWalkChildren verifies that extension node kinds
// without a registered renderer are not dropped: their text contents are
// rendered by walking the children.
func TestUnknownExtensionNodesWalkChildren(t *testing.T) {
	p := parser.New(parser.WithExtensions(
		extension.NewTableParser(),
		extension.NewStrikethroughParser(),
		extension.NewFootnoteParser(),
	))
	src := "| Name | Age |\n| ---- | --- |\n| Bob  | 42  |\n\nThis is ~~gone~~ kept.\n\nA note[^1].\n\n[^1]: footnote text\n"
	got := renderWithParser(t, p, src)
	for _, want := range []string{"Name", "Age", "Bob", "42", "kept.", "A note", "footnote text"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<") || strings.Contains(got, ">") {
		t.Fatalf("extension fallback must emit text, not markup:\n%s", got)
	}
}

// tablePipeRenderer renders a TableCell by wrapping its rendered text with
// pipes, demonstrating that extension nodes can register custom processors.
type tablePipeExtension struct{}

func (tablePipeExtension) RendererOptions(*gtext.Config) []gtext.Option {
	return []gtext.Option{
		gtext.WithNodeRenderer(gast.KindTableCell, gtext.NodeRendererFunc(
			func(w io.Writer, source []byte, n ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
				pw := gtext.ContextPlainWriter(rc)
				if entering {
					if err := pw.WriteString("|"); err != nil {
						return ast.WalkStop, err
					}
				} else {
					if err := pw.WriteString("|"); err != nil {
						return ast.WalkStop, err
					}
				}
				return ast.WalkContinue, nil
			})),
	}
}

func TestRegisteredExtensionProcessor(t *testing.T) {
	p := parser.New(parser.WithExtensions(extension.NewTableParser()))
	src := "| A | B |\n| - | - |\n| 1 | 2 |\n"
	got := renderWithParser(t, p, src, gtext.WithExtensions(tablePipeExtension{}))
	if !strings.Contains(got, "|A|") || !strings.Contains(got, "|1|") {
		t.Fatalf("custom table processor output not found, got:\n%s", got)
	}
}

func TestMiscBlocks(t *testing.T) {
	src := "before\n\n***\n\nafter <http://example.com>\n"
	want := "before\n\n***\n\nafter http://example.com\n"
	if got := render(t, src); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRenderStringSource(t *testing.T) {
	p := parser.New()
	r := gtext.New()
	doc := p.Parse(util.StringToReadOnlyBytes("hello\n"))
	var sb strings.Builder
	if err := r.RenderStringSource(&sb, "hello\n", doc); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sb.String() != "hello\n" {
		t.Fatalf("got %q", sb.String())
	}
}

// helperExtension exercises the public plainWriter helpers from a custom
// node renderer.
type helperExtension struct{}

func (helperExtension) RendererOptions(*gtext.Config) []gtext.Option {
	return []gtext.Option{
		gtext.WithNodeRenderer(ast.KindThematicBreak, gtext.NodeRendererFunc(
			func(_ io.Writer, _ []byte, _ ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error) {
				if !entering {
					return ast.WalkContinue, nil
				}
				pw := gtext.ContextPlainWriter(rc)
				if err := pw.WriteBlankLine(); err != nil {
					return ast.WalkStop, err
				}
				if err := pw.WriteString("==="); err != nil {
					return ast.WalkStop, err
				}
				if err := pw.WriteNewLine(); err != nil {
					return ast.WalkStop, err
				}
				return ast.WalkSkipChildren, nil
			})),
	}
}

func TestPlainWriterHelpers(t *testing.T) {
	p := parser.New()
	src := "a\n\n---\n\nb\n"
	r := gtext.New(gtext.WithExtensions(helperExtension{}))
	var buf bytes.Buffer
	b := util.StringToReadOnlyBytes(src)
	doc := p.Parse(b)
	if err := r.Render(&buf, b, doc); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "a\n\n===\n\nb\n"
	if buf.String() != want {
		t.Fatalf("got %q want %q", buf.String(), want)
	}
}
