package extension

import (
	"bytes"
	"strings"
	"testing"

	gast "github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/testutil"
	"github.com/yuin/goldmark/v2/text"
	"github.com/yuin/goldmark/v2/util"
)

func TestFootnoteDefinitionReferencesAnotherFootnote(t *testing.T) {
	source := []byte("Text.[^outer]\n\n[^outer]: Outer [^inner].\n[^inner]: Inner.\n")
	markdownParser := parser.New(parser.WithExtensions(NewFootnoteParser()))
	renderer := html.New(html.WithUnsafe(), html.WithExtensions(NewFootnoteHTMLRenderer()))

	var output bytes.Buffer
	if err := renderer.Render(&output, source, markdownParser.Parse(source)); err != nil {
		t.Fatal(err)
	}

	rendered := output.String()
	for _, expected := range []string{
		`<p>Outer <sup id="fnref:2"><a href="#fn:2" class="footnote-ref" role="doc-noteref">2</a></sup>.&#160;<a href="#fnref:1" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>`,
		`<li id="fn:2">`,
		`<p>Inner.</p>`,
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("output %q does not contain %q", rendered, expected)
		}
	}
	parts := strings.Split(rendered, `<li id="fn:`)
	if len(parts) > 2 && strings.Contains(parts[2], `role="doc-backlink"`) {
		t.Fatalf("definition-only footnote should not have a backlink: %q", rendered)
	}
}

func TestFootnote(t *testing.T) {
	markdown := testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewFootnoteParser()),
		),
		html.New(
			html.WithUnsafe(),
			html.WithExtensions(NewFootnoteHTMLRenderer()),
		),
	)
	testutil.DoTestCaseFile(markdown, "testdata/footnote.txt", t, testutil.ParseCliCaseArg()...)
}

type footnoteID struct {
}

func (a *footnoteID) Transform(node *gast.Document, _ text.Reader, _ parser.Context) {
	node.Metadata()["footnote-prefix"] = "article12-"
}

func TestFootnoteOptions(t *testing.T) {
	markdown := testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewFootnoteParser()),
		),
		html.New(
			html.WithUnsafe(),
			html.WithExtensions(NewFootnoteHTMLRenderer(
				WithIDPrefix("article12-"),
				WithLinkClass("link-class"),
				WithBacklinkClass("backlink-class"),
				WithLinkTitle("link-title-%%-^^"),
				WithBacklinkTitle("backlink-title"),
				WithBacklinkHTML("^"),
			)),
		),
	)

	testutil.DoTestCase(
		markdown,
		testutil.MarkdownTestCase{
			No:          1,
			Description: "Footnote with options",
			Markdown: `That's some text with a footnote.[^1]

Same footnote.[^1]

Another one.[^2]

[^1]: And that's the footnote.
[^2]: Another footnote.
`,
			Expected: `<p>That's some text with a footnote.<sup id="article12-fnref:1"><a href="#article12-fn:1" class="link-class" title="link-title-2-1" role="doc-noteref">1</a></sup></p>
<p>Same footnote.<sup id="article12-fnref1:1"><a href="#article12-fn:1" class="link-class" title="link-title-2-1" role="doc-noteref">1</a></sup></p>
<p>Another one.<sup id="article12-fnref:2"><a href="#article12-fn:2" class="link-class" title="link-title-1-2" role="doc-noteref">2</a></sup></p>
<div class="footnotes" role="doc-endnotes">
<hr>
<ol>
<li id="article12-fn:1">
<p>And that's the footnote.&#160;<a href="#article12-fnref:1" class="backlink-class" title="backlink-title" role="doc-backlink">^</a>&#160;<a href="#article12-fnref1:1" class="backlink-class" title="backlink-title" role="doc-backlink">^</a></p>
</li>
<li id="article12-fn:2">
<p>Another footnote.&#160;<a href="#article12-fnref:2" class="backlink-class" title="backlink-title" role="doc-backlink">^</a></p>
</li>
</ol>
</div>`,
		},
		t,
	)

	markdown = testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithASTTransformers(
				util.Prioritized[parser.ASTTransformer](&footnoteID{}, 100),
			),
			parser.WithExtensions(NewFootnoteParser()),
		),
		html.New(
			html.WithUnsafe(),
			html.WithExtensions(NewFootnoteHTMLRenderer(
				WithIDPrefixFunction(func(n gast.Node) []byte {
					v, ok := n.OwnerDocument().Metadata()["footnote-prefix"]
					if ok {
						return util.StringToReadOnlyBytes(v.(string))
					}
					return nil
				}),
				WithLinkClass("link-class"),
				WithBacklinkClass("backlink-class"),
				WithLinkTitle("link-title-%%-^^"),
				WithBacklinkTitle("backlink-title"),
				WithBacklinkHTML("^"),
			)),
		),
	)

	testutil.DoTestCase(
		markdown,
		testutil.MarkdownTestCase{
			No:          2,
			Description: "Footnote with an id prefix function",
			Markdown: `That's some text with a footnote.[^1]

Same footnote.[^1]

Another one.[^2]

[^1]: And that's the footnote.
[^2]: Another footnote.
`,
			Expected: `<p>That's some text with a footnote.<sup id="article12-fnref:1"><a href="#article12-fn:1" class="link-class" title="link-title-2-1" role="doc-noteref">1</a></sup></p>
<p>Same footnote.<sup id="article12-fnref1:1"><a href="#article12-fn:1" class="link-class" title="link-title-2-1" role="doc-noteref">1</a></sup></p>
<p>Another one.<sup id="article12-fnref:2"><a href="#article12-fn:2" class="link-class" title="link-title-1-2" role="doc-noteref">2</a></sup></p>
<div class="footnotes" role="doc-endnotes">
<hr>
<ol>
<li id="article12-fn:1">
<p>And that's the footnote.&#160;<a href="#article12-fnref:1" class="backlink-class" title="backlink-title" role="doc-backlink">^</a>&#160;<a href="#article12-fnref1:1" class="backlink-class" title="backlink-title" role="doc-backlink">^</a></p>
</li>
<li id="article12-fn:2">
<p>Another footnote.&#160;<a href="#article12-fnref:2" class="backlink-class" title="backlink-title" role="doc-backlink">^</a></p>
</li>
</ol>
</div>`,
		},
		t,
	)
}
