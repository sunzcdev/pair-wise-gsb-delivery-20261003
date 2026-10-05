package section_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension"
	extensionast "github.com/yuin/goldmark/v2/extension/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/section"
	"github.com/yuin/goldmark/v2/text"
)

func newMarkdownParser() parser.Parser {
	return parser.New(parser.WithExtensions(extension.FootnoteParser))
}

func newHTMLRenderer() html.Renderer {
	return html.New(html.WithUnsafe(), html.WithExtensions(extension.FootnoteHTMLRenderer))
}

func renderDocument(t *testing.T, source []byte, document *ast.Document) string {
	t.Helper()
	var output bytes.Buffer
	if err := newHTMLRenderer().Render(&output, source, document); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func findHeading(t *testing.T, source []byte, document *ast.Document, title string) *ast.Heading {
	t.Helper()
	for node := range document.Children() {
		heading, ok := node.(*ast.Heading)
		if ok && nodeText(heading, source) == title {
			return heading
		}
	}
	t.Fatalf("heading %q not found", title)
	return nil
}

func nodeText(node ast.Node, source []byte) string {
	var builder strings.Builder
	_ = ast.Walk(node, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			if value, ok := node.(*ast.Text); ok {
				builder.WriteString(value.Value.Str(source))
			}
		}
		return ast.WalkContinue, nil
	})
	return builder.String()
}

func countNodes(document *ast.Document, kind ast.NodeKind) int {
	count := 0
	_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && node.Kind() == kind {
			count++
		}
		return ast.WalkContinue, nil
	})
	return count
}

func TestExtractCollectsTransitiveDependencies(t *testing.T) {
	source := []byte(strings.Join([]string{
		"[doc-link]: https://example.com/doc",
		"",
		"[^inner]: Inner note with [doc link][doc-link].",
		"",
		"# Intro",
		"",
		"Intro [^unrelated].",
		"",
		"[^unrelated]: Outside.",
		"",
		"# Chapter",
		"",
		"Chapter preamble [^outer].",
		"",
		"## Child",
		"",
		"Child [unrelated][doc-link] and unused.",
		"",
		"[^outer]: Outer note [^inner].",
		"",
		"# After",
		"",
		"[unused-link]: https://example.com/unused",
		"",
	}, "\n"))

	markdownParser := newMarkdownParser()
	document := markdownParser.Parse(source).(*ast.Document)
	heading := findHeading(t, source, document, "Chapter")
	extracted, err := section.Extract(source, heading)
	if err != nil {
		t.Fatal(err)
	}

	if len(extracted.MissingDefinitions) != 0 {
		t.Fatalf("unexpected missing definitions: %#v", extracted.MissingDefinitions)
	}
	if heading.Parent() != document {
		t.Fatal("source heading was removed from its original document")
	}
	if heading == extracted.Document.FirstChild() {
		t.Fatal("extracted heading is the source node")
	}
	if countNodes(extracted.Document, ast.KindLinkReferenceDefinition) != 1 {
		t.Fatal("expected exactly one required link definition")
	}
	if countNodes(extracted.Document, extensionast.KindFootnoteDefinition) != 2 {
		t.Fatal("expected outer and inner footnote definitions")
	}

	output := renderDocument(t, source, extracted.Document)
	for _, want := range []string{
		"<h1>Chapter</h1>",
		"Chapter preamble",
		"<h2>Child</h2>",
		`href="https://example.com/doc"`,
		`id="fn:2"`,
		`id="fn:3"`,
		`<ol start="2">`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	for _, unwanted := range []string{"# After", "Intro", "unused-link"} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("output unexpectedly contains %q:\n%s", unwanted, output)
		}
	}

	originalOutput := renderDocument(t, source, document)
	for _, want := range []string{`id="fn:1"`, `id="fn:2"`, `id="fn:3"`} {
		if !strings.Contains(originalOutput, want) {
			t.Fatalf("original output missing %q:\n%s", want, originalOutput)
		}
	}
}

func TestExtractBoundariesAndPreamble(t *testing.T) {
	source := []byte("# Intro\n\nIntro text.\n\n# Chapter\n\nChapter preamble.\n\n## First\n\nFirst body.\n\n### Grandchild\n\nGrandchild body.\n\n## Second\n\nSecond body.\n\n# After\n")
	document := newMarkdownParser().Parse(source).(*ast.Document)

	chapter := findHeading(t, source, document, "Chapter")
	extracted, err := section.Extract(source, chapter)
	if err != nil {
		t.Fatal(err)
	}
	output := renderDocument(t, source, extracted.Document)
	for _, want := range []string{"<h1>Chapter</h1>", "Chapter preamble.", "<h2>First</h2>", "<h3>Grandchild</h3>", "<h2>Second</h2>"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "Intro text.") || strings.Contains(output, "# After") {
		t.Fatalf("section boundary leaked:\n%s", output)
	}

	first := findHeading(t, source, document, "First")
	firstSection, err := section.Extract(source, first)
	if err != nil {
		t.Fatal(err)
	}
	firstOutput := renderDocument(t, source, firstSection.Document)
	for _, want := range []string{"<h2>First</h2>", "First body.", "<h3>Grandchild</h3>"} {
		if !strings.Contains(firstOutput, want) {
			t.Fatalf("output missing %q:\n%s", want, firstOutput)
		}
	}
	if strings.Contains(firstOutput, "Chapter preamble.") || strings.Contains(firstOutput, "Second body.") {
		t.Fatalf("subsection boundary leaked:\n%s", firstOutput)
	}

	grandchild := findHeading(t, source, document, "Grandchild")
	grandchildSection, err := section.Extract(source, grandchild)
	if err != nil {
		t.Fatal(err)
	}
	grandchildOutput := renderDocument(t, source, grandchildSection.Document)
	if !strings.Contains(grandchildOutput, "Grandchild body.") || strings.Contains(grandchildOutput, "Second body.") {
		t.Fatalf("grandchild extraction failed:\n%s", grandchildOutput)
	}
}

func TestExtractRejectsInvalidHeadings(t *testing.T) {
	source := []byte("# Title\n\n> # Nested\n")
	document := newMarkdownParser().Parse(source).(*ast.Document)

	if _, err := section.Extract(source, nil); !errors.Is(err, section.ErrNilHeading) {
		t.Fatalf("got %v, want ErrNilHeading", err)
	}
	if _, err := section.Extract(nil, document.FirstChild().(*ast.Heading)); !errors.Is(err, section.ErrNilSource) {
		t.Fatalf("got %v, want ErrNilSource", err)
	}

	heading := ast.NewHeading(2, ast.HeadingKindATX)
	if _, err := section.Extract(source, heading); !errors.Is(err, section.ErrHeadingNotInDocument) {
		t.Fatalf("got %v, want ErrHeadingNotInDocument", err)
	}

	var nested *ast.Heading
	_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if candidate, ok := node.(*ast.Heading); ok && entering && candidate.Level == 1 && candidate.Parent() != document {
			nested = candidate
		}
		return ast.WalkContinue, nil
	})
	if _, err := section.Extract(source, nested); !errors.Is(err, section.ErrHeadingNotTopLevel) {
		t.Fatalf("got %v, want ErrHeadingNotTopLevel", err)
	}
}

func TestExtractReportsMissingDefinitions(t *testing.T) {
	source := []byte("# Chapter\n\nText.\n")
	document := newMarkdownParser().Parse(source).(*ast.Document)
	heading := document.FirstChild().(*ast.Heading)
	paragraph := ast.NewParagraph()
	paragraph.AppendChild(ast.NewText(text.NewSingleLineValueFromString("ref ", nil)))
	paragraph.AppendChild(extensionast.NewFootnoteReference(text.NewSingleLineValueFromString("missing-fn", nil)))
	document.AppendChild(paragraph)

	extracted, err := section.Extract(source, heading)
	if err != nil {
		t.Fatal(err)
	}
	want := section.MissingDefinition{Kind: section.FootnoteDefinition, Label: "missing-fn"}
	if len(extracted.MissingDefinitions) != 1 || extracted.MissingDefinitions[0] != want {
		t.Fatalf("missing definitions = %#v, want %#v", extracted.MissingDefinitions, want)
	}
}

func TestExtractReportsMissingLinkDefinitions(t *testing.T) {
	source := []byte("# Chapter\n\nText.\n")
	document := newMarkdownParser().Parse(source).(*ast.Document)
	heading := document.FirstChild().(*ast.Heading)
	paragraph := ast.NewParagraph()
	paragraph.AppendChild(ast.NewLink(
		text.SingleLineValue{},
		ast.WithLinkReference(
			ast.ReferenceLinkKindShortcut,
			text.NewMultiLineValueFromString("missing-link", nil),
		),
	))
	document.AppendChild(paragraph)

	extracted, err := section.Extract(source, heading)
	if err != nil {
		t.Fatal(err)
	}
	want := section.MissingDefinition{Kind: section.LinkDefinition, Label: "missing-link"}
	if len(extracted.MissingDefinitions) != 1 || extracted.MissingDefinitions[0] != want {
		t.Fatalf("missing definitions = %#v, want %#v", extracted.MissingDefinitions, want)
	}
}

func TestExtractedTreeDoesNotShareNodeState(t *testing.T) {
	source := []byte("# Chapter\n\nOriginal paragraph.\n")
	document := newMarkdownParser().Parse(source).(*ast.Document)
	heading := findHeading(t, source, document, "Chapter")
	extracted, err := section.Extract(source, heading)
	if err != nil {
		t.Fatal(err)
	}

	extractedHeading := extracted.Document.FirstChild().(*ast.Heading)
	extractedHeading.AppendChild(ast.NewText(text.NewSingleLineValueFromString(" modified", nil)))

	originalOutput := renderDocument(t, source, document)
	extractedOutput := renderDocument(t, source, extracted.Document)
	if !strings.Contains(originalOutput, "<h1>Chapter</h1>") || strings.Contains(originalOutput, "modified") {
		t.Fatalf("original tree changed:\n%s", originalOutput)
	}
	if !strings.Contains(extractedOutput, "<h1>Chapter modified</h1>") {
		t.Fatalf("extracted tree was not modified as expected:\n%s", extractedOutput)
	}
	if heading.FirstChild() == extractedHeading.FirstChild() {
		t.Fatal("heading children are shared")
	}
}
