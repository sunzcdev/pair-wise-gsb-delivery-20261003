package section

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension"
	extast "github.com/yuin/goldmark/v2/extension/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/text"
)

func parseMarkdown(t *testing.T, source string) (*ast.Document, []byte) {
	t.Helper()
	sourceBytes := []byte(source)
	document := parser.New(
		parser.WithExtensions(extension.FootnoteParser),
	).Parse(sourceBytes)
	return document.(*ast.Document), sourceBytes
}

func renderHTML(t *testing.T, document *ast.Document, source []byte) string {
	t.Helper()
	var output bytes.Buffer
	renderer := html.New(
		html.WithUnsafe(),
		html.WithExtensions(extension.FootnoteHTMLRenderer),
	)
	if err := renderer.Render(&output, source, document); err != nil {
		t.Fatalf("render document: %v", err)
	}
	return output.String()
}

func findTopLevelHeading(t *testing.T, document *ast.Document, source []byte, text string) *ast.Heading {
	t.Helper()
	for child := range document.Children() {
		heading, ok := child.(*ast.Heading)
		if ok && headingText(heading, source) == text {
			return heading
		}
	}
	t.Fatalf("heading %q not found", text)
	return nil
}

func findNestedHeading(t *testing.T, document *ast.Document, source []byte, text string) *ast.Heading {
	t.Helper()
	var found *ast.Heading
	_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && found == nil {
			if heading, ok := node.(*ast.Heading); ok && headingText(heading, source) == text {
				found = heading
			}
		}
		return ast.WalkContinue, nil
	})
	if found == nil {
		t.Fatalf("heading %q not found", text)
	}
	return found
}

func headingText(heading *ast.Heading, source []byte) string {
	var builder strings.Builder
	_ = ast.Walk(heading, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			if text, ok := node.(*ast.Text); ok {
				builder.WriteString(text.Value.Str(source))
			}
		}
		return ast.WalkContinue, nil
	})
	return builder.String()
}

func topLevelKinds(document *ast.Document) []string {
	var kinds []string
	for child := range document.Children() {
		kinds = append(kinds, child.Kind().String())
	}
	return kinds
}

func TestExtractChapterAndDefinitionClosure(t *testing.T) {
	source := `[used]: https://example.com/used "Used"
[unused]: https://example.com/unused

[^one]: First footnote, which points on.[^two]

[^two]: Second footnote.

# Target

See [used] in the chapter.[^one]

## Child

Child content stays in this chapter.

# Next

This must not be extracted.
`
	document, sourceBytes := parseMarkdown(t, source)
	heading := findTopLevelHeading(t, document, sourceBytes, "Target")

	result, err := Extract(document, heading, sourceBytes)
	if err != nil {
		t.Fatalf("extract section: %v", err)
	}
	if result.Document == document {
		t.Fatal("extracted document is the same root as the source document")
	}
	if result.Document.FirstChild() == heading {
		t.Fatal("extracted first child is the original heading node")
	}
	if heading.Parent() != document {
		t.Fatal("original heading parent changed")
	}

	kinds := topLevelKinds(result.Document)
	if strings.Join(kinds, ",") != "Heading,Paragraph,Heading,Paragraph,LinkReferenceDefinition,FootnoteDefinition,FootnoteDefinition" {
		t.Fatalf("unexpected extracted top-level nodes: %v", kinds)
	}

	output := renderHTML(t, result.Document, sourceBytes)
	checks := []string{
		"<h1>Target</h1>",
		"<h2>Child</h2>",
		"Child content stays",
		`href="https://example.com/used"`,
		`<sup id="fnref:2"><a href="#fn:2"`,
		`<li id="fn:2">`,
		`<li id="fn:1">`,
		"First footnote, which points on.",
		"Second footnote.",
	}
	for _, check := range checks {
		if !strings.Contains(output, check) {
			t.Fatalf("extracted render %q does not contain %q", output, check)
		}
	}
	for _, excluded := range []string{"This must not be extracted", "https://example.com/unused"} {
		if strings.Contains(output, excluded) {
			t.Fatalf("extracted render unexpectedly contains %q", excluded)
		}
	}
	if len(result.MissingDefinitions) != 0 {
		t.Fatalf("unexpected missing definitions: %#v", result.MissingDefinitions)
	}
}

func TestExtractBoundaryAndPreface(t *testing.T) {
	source := `Preface with [used].

# First

First body.

## Nested

Nested body.

# Second

Second body.

[used]: https://example.com/used
`
	document, sourceBytes := parseMarkdown(t, source)
	first := findTopLevelHeading(t, document, sourceBytes, "First")

	firstResult, err := Extract(document, first, sourceBytes)
	if err != nil {
		t.Fatalf("extract first chapter: %v", err)
	}
	firstOutput := renderHTML(t, firstResult.Document, sourceBytes)
	if !strings.Contains(firstOutput, "<h2>Nested</h2>") || !strings.Contains(firstOutput, "Nested body.") {
		t.Fatalf("nested heading was not included: %s", firstOutput)
	}
	if strings.Contains(firstOutput, "Second body") {
		t.Fatalf("same-level chapter boundary was ignored: %s", firstOutput)
	}

	preface, err := ExtractPreface(document, sourceBytes)
	if err != nil {
		t.Fatalf("extract preface: %v", err)
	}
	prefaceOutput := renderHTML(t, preface.Document, sourceBytes)
	if !strings.Contains(prefaceOutput, "Preface with") {
		t.Fatalf("preface content missing: %s", prefaceOutput)
	}
	if strings.Contains(prefaceOutput, "<h1>First</h1>") || strings.Contains(prefaceOutput, "First body") {
		t.Fatalf("preface contains chapter content: %s", prefaceOutput)
	}
	if len(preface.MissingDefinitions) != 0 {
		t.Fatalf("preface link definition closure missing: %#v", preface.MissingDefinitions)
	}
}

func TestExtractMissingDefinitions(t *testing.T) {
	source := `# Missing

Known [used], missing [bad][missing-link], and missing footnote.[^absent]

[used]: https://example.com/used
`
	document, sourceBytes := parseMarkdown(t, source)
	heading := findTopLevelHeading(t, document, sourceBytes, "Missing")

	result, err := Extract(document, heading, sourceBytes)
	if err != nil {
		t.Fatalf("extract section: %v", err)
	}

	expected := []MissingDefinition{
		{Kind: LinkDefinition, Label: "missing-link"},
		{Kind: FootnoteDefinition, Label: "absent"},
	}
	for i, missing := range expected {
		if result.MissingDefinitions[i] != missing {
			t.Fatalf("missing definition %d = %#v, want %#v", i, result.MissingDefinitions[i], missing)
		}
	}
}

func TestExtractRejectsInvalidHeading(t *testing.T) {
	document, sourceBytes := parseMarkdown(t, "# Chapter\n\nBody.\n")
	nestedSource := []byte("> # Nested\n>\n> Body.\n")
	nestedDocument := parser.New(
		parser.WithExtensions(extension.FootnoteParser),
	).Parse(nestedSource)
	nested := findNestedHeading(t, nestedDocument.(*ast.Document), nestedSource, "Nested")

	if _, err := Extract(document, nil, sourceBytes); !errors.Is(err, ErrInvalidHeading) {
		t.Fatalf("nil heading error = %v, want %v", err, ErrInvalidHeading)
	}
	if _, err := Extract(document, nested, sourceBytes); !errors.Is(err, ErrHeadingNotInDocument) {
		t.Fatalf("foreign heading error = %v, want %v", err, ErrHeadingNotInDocument)
	}

	otherDocument, _ := parseMarkdown(t, "# Other\n")
	if _, err := Extract(document, findTopLevelHeading(t, otherDocument, []byte("# Other\n"), "Other"), sourceBytes); !errors.Is(err, ErrHeadingNotInDocument) {
		t.Fatalf("heading from another document error = %v, want %v", err, ErrHeadingNotInDocument)
	}

	nestedDocument2 := parser.New(
		parser.WithExtensions(extension.FootnoteParser),
	).Parse(nestedSource)
	nestedDocument2Root := nestedDocument2.(*ast.Document)
	nested2 := findNestedHeading(t, nestedDocument2Root, nestedSource, "Nested")
	if _, err := Extract(nestedDocument2Root, nested2, nestedSource); !errors.Is(err, ErrInvalidHeading) {
		t.Fatalf("nested heading error = %v, want %v", err, ErrInvalidHeading)
	}
}

func TestExtractedTreeDoesNotShareASTState(t *testing.T) {
	source := `[used]: https://example.com/used

[^one]: Footnote with [used].[^two]

[^two]: Nested footnote.

# Target

Chapter with [used].[^one]

# Next

Next chapter.
`
	document, sourceBytes := parseMarkdown(t, source)
	heading := findTopLevelHeading(t, document, sourceBytes, "Target")
	originalBefore := renderHTML(t, document, sourceBytes)

	result, err := Extract(document, heading, sourceBytes)
	if err != nil {
		t.Fatalf("extract section: %v", err)
	}
	originalParagraph := firstParagraph(document)
	cloneParagraph := firstParagraph(result.Document)
	if cloneParagraph == nil || cloneParagraph.Parent() != result.Document {
		t.Fatal("could not locate the cloned paragraph")
	}
	if cloneParagraph == originalParagraph {
		t.Fatal("extracted paragraph is the original node")
	}
	cloneParagraph.AppendChild(ast.NewText(text.NewSingleLineValueFromString(" mutated", nil)))

	if heading.NextSibling() != originalParagraph {
		t.Fatal("modifying extracted siblings changed the original sibling chain")
	}
	if originalParagraph.Parent() != document {
		t.Fatal("modifying extracted tree changed the original parent")
	}

	originalAfter := renderHTML(t, document, sourceBytes)
	if originalAfter != originalBefore {
		t.Fatalf("original render changed after extracted AST mutation\nbefore:\n%s\nafter:\n%s", originalBefore, originalAfter)
	}

	cloneHeading := result.Document.FirstChild().(*ast.Heading)
	heading.SetAttribute("data-original", text.NewMultiLineValueFromString("yes", nil))
	cloneHeading.SetAttribute("data-clone", text.NewMultiLineValueFromString("yes", nil))
	if _, ok := heading.Attribute("data-clone"); ok {
		t.Fatal("attribute added to extracted tree leaked into original")
	}
	if _, ok := cloneHeading.Attribute("data-original"); ok {
		t.Fatal("attribute added to original tree leaked into extracted tree")
	}

	extractedOutput := renderHTML(t, result.Document, sourceBytes)
	if !strings.Contains(extractedOutput, `href="#fn:2"`) || !strings.Contains(extractedOutput, `id="fn:2"`) ||
		!strings.Contains(extractedOutput, `href="#fn:1"`) || !strings.Contains(extractedOutput, `id="fn:1"`) {
		t.Fatalf("extracted footnote numbering changed: %s", extractedOutput)
	}
	if strings.Contains(extractedOutput, "Next chapter") {
		t.Fatalf("extracted render contains following chapter: %s", extractedOutput)
	}
	if strings.Contains(extractedOutput, "data-original") || !strings.Contains(extractedOutput, "data-clone") {
		t.Fatalf("extracted attribute state leaked: %s", extractedOutput)
	}

	var originalFootnotes []*extast.FootnoteDefinition
	for child := range document.Children() {
		if footnote, ok := child.(*extast.FootnoteDefinition); ok {
			originalFootnotes = append(originalFootnotes, footnote)
		}
	}
	if len(originalFootnotes) != 2 || originalFootnotes[0].Parent() != document {
		t.Fatal("original footnote parent chain changed")
	}

	var originalBlock ast.BlockNode = heading
	cloneBlock := result.Document.FirstChild().(ast.BlockNode)
	originalSegments := originalBlock.Source()
	cloneSegments := cloneBlock.Source()
	cloneSegments[0].Start = -1
	if originalSegments[0].Start == -1 {
		t.Fatal("source segment slice backing arrays are shared")
	}
}

func firstParagraph(document *ast.Document) *ast.Paragraph {
	for child := range document.Children() {
		if paragraph, ok := child.(*ast.Paragraph); ok {
			return paragraph
		}
	}
	return nil
}
