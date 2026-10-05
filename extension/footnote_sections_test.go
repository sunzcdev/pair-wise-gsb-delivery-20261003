package extension

import (
	"regexp"
	"strings"
	"testing"

	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/testutil"
)

const footnoteSectionsDoc = `Intro[^a] and shared[^s].

# Top Heading

Top body[^b].

## Chapter One

First[^s] chapter.

` + "```" + `
## Fake heading
[^fake]
` + "```" + `

Again[^s] and missing[^missing].

New[^c].

## Chapter Two

Second[^s] chapter and [^d].

## Chapter Three

No footnotes here.

## Chapter Four

End[^e].

[^a]: a definition
[^b]: b definition
[^s]: shared definition
[^c]: c definition
[^d]: d definition
[^e]: e definition
[^unused]: never referenced
`

func renderFootnoteSectionsDoc(level int) (string, error) {
	md := testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewFootnoteParser()),
		),
		html.New(
			html.WithUnsafe(),
			html.WithExtensions(NewFootnoteHTMLRenderer(WithSectionHeadingLevel(level))),
		),
	)
	return md(footnoteSectionsDoc)
}

func TestFootnoteSectionsGlobalGolden(t *testing.T) {
	got, err := renderFootnoteSectionsDoc(0)
	if err != nil {
		t.Fatal(err)
	}
	if got != footnoteSectionsGlobalGolden {
		t.Errorf("global output mismatch:\n--- got ---\n%s\n--- want ---\n%s", got,
			footnoteSectionsGlobalGolden)
	}
}

func TestFootnoteSectionsChapterGolden(t *testing.T) {
	got, err := renderFootnoteSectionsDoc(2)
	if err != nil {
		t.Fatal(err)
	}
	if got != footnoteSectionsChapterGolden {
		t.Errorf("section output mismatch:\n--- got ---\n%s\n--- want ---\n%s", got,
			footnoteSectionsChapterGolden)
	}
}

var (
	idAttrRE   = regexp.MustCompile(`id="([^"]+)"`)
	hrefAttrRE = regexp.MustCompile(`href="#([^"]+)"`)
)

// TestFootnoteSectionsLinkIntegrity verifies that every footnote link and
// backlink points at an id that exists exactly once, and that no two elements
// share the same id. It runs in both global and sectioned modes.
func TestFootnoteSectionsLinkIntegrity(t *testing.T) {
	for _, level := range []int{0, 2} {
		out, err := renderFootnoteSectionsDoc(level)
		if err != nil {
			t.Fatal(err)
		}

		ids := map[string]int{}
		for _, m := range idAttrRE.FindAllStringSubmatch(out, -1) {
			ids[m[1]]++
		}
		for id, count := range ids {
			if count != 1 {
				t.Errorf("level %d: id %q appears %d times, want exactly 1", level, id, count)
			}
		}

		hrefs := map[string]int{}
		links := hrefAttrRE.FindAllStringSubmatch(out, -1)
		for _, m := range links {
			hrefs[m[1]]++
		}
		if len(links) != 18 {
			t.Errorf("level %d: got %d footnote links/backlinks, want 18 (9 references + 9 backlinks)",
				level, len(links))
		}
		for target, count := range hrefs {
			if ids[target] != 1 {
				t.Errorf("level %d: link target %q resolves to %d elements, want exactly 1",
					level, target, ids[target])
			}
			if count == 0 {
				t.Errorf("level %d: link target %q is never linked", level, target)
			}
		}

		if strings.Contains(out, "fn:missing") || strings.Contains(out, "fn:unused") ||
			strings.Contains(out, "fn:fake") {
			t.Errorf("level %d: missing/unreferenced/fake definition rendered", level)
		}
		if !strings.Contains(out, "missing[^missing]") {
			t.Errorf("level %d: reference without definition was not left as literal text",
				level)
		}
	}
}

const footnoteSectionsGlobalGolden = `<p>Intro<sup id="fnref:1"><a href="#fn:1" class="footnote-ref" role="doc-noteref">1</a></sup> and shared<sup id="fnref:2"><a href="#fn:2" class="footnote-ref" role="doc-noteref">2</a></sup>.</p>
<h1>Top Heading</h1>
<p>Top body<sup id="fnref:3"><a href="#fn:3" class="footnote-ref" role="doc-noteref">3</a></sup>.</p>
<h2>Chapter One</h2>
<p>First<sup id="fnref1:2"><a href="#fn:2" class="footnote-ref" role="doc-noteref">2</a></sup> chapter.</p>
<pre><code>## Fake heading
[^fake]
</code></pre>
<p>Again<sup id="fnref2:2"><a href="#fn:2" class="footnote-ref" role="doc-noteref">2</a></sup> and missing[^missing].</p>
<p>New<sup id="fnref:4"><a href="#fn:4" class="footnote-ref" role="doc-noteref">4</a></sup>.</p>
<h2>Chapter Two</h2>
<p>Second<sup id="fnref3:2"><a href="#fn:2" class="footnote-ref" role="doc-noteref">2</a></sup> chapter and <sup id="fnref:5"><a href="#fn:5" class="footnote-ref" role="doc-noteref">5</a></sup>.</p>
<h2>Chapter Three</h2>
<p>No footnotes here.</p>
<h2>Chapter Four</h2>
<p>End<sup id="fnref:6"><a href="#fn:6" class="footnote-ref" role="doc-noteref">6</a></sup>.</p>
<div class="footnotes" role="doc-endnotes">
<hr>
<ol>
<li id="fn:1">
<p>a definition&#160;<a href="#fnref:1" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
<li id="fn:2">
<p>shared definition&#160;<a href="#fnref:2" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a>&#160;<a href="#fnref1:2" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a>&#160;<a href="#fnref2:2" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a>&#160;<a href="#fnref3:2" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
<li id="fn:3">
<p>b definition&#160;<a href="#fnref:3" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
<li id="fn:4">
<p>c definition&#160;<a href="#fnref:4" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
<li id="fn:5">
<p>d definition&#160;<a href="#fnref:5" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
<li id="fn:6">
<p>e definition&#160;<a href="#fnref:6" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
</ol>
</div>
`

const footnoteSectionsChapterGolden = `<p>Intro<sup id="fnref:0:1"><a href="#fn:0:1" class="footnote-ref" role="doc-noteref">1</a></sup> and shared<sup id="fnref:0:2"><a href="#fn:0:2" class="footnote-ref" role="doc-noteref">2</a></sup>.</p>
<h1>Top Heading</h1>
<p>Top body<sup id="fnref:0:3"><a href="#fn:0:3" class="footnote-ref" role="doc-noteref">3</a></sup>.</p>
<div class="footnotes" role="doc-endnotes">
<hr>
<ol>
<li id="fn:0:1">
<p>a definition&#160;<a href="#fnref:0:1" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
<li id="fn:0:2">
<p>shared definition&#160;<a href="#fnref:0:2" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
<li id="fn:0:3">
<p>b definition&#160;<a href="#fnref:0:3" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
</ol>
</div>
<h2>Chapter One</h2>
<p>First<sup id="fnref:1:1"><a href="#fn:1:1" class="footnote-ref" role="doc-noteref">1</a></sup> chapter.</p>
<pre><code>## Fake heading
[^fake]
</code></pre>
<p>Again<sup id="fnref1:1:1"><a href="#fn:1:1" class="footnote-ref" role="doc-noteref">1</a></sup> and missing[^missing].</p>
<p>New<sup id="fnref:1:2"><a href="#fn:1:2" class="footnote-ref" role="doc-noteref">2</a></sup>.</p>
<div class="footnotes" role="doc-endnotes">
<hr>
<ol>
<li id="fn:1:1">
<p>shared definition&#160;<a href="#fnref:1:1" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a>&#160;<a href="#fnref1:1:1" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
<li id="fn:1:2">
<p>c definition&#160;<a href="#fnref:1:2" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
</ol>
</div>
<h2>Chapter Two</h2>
<p>Second<sup id="fnref:2:1"><a href="#fn:2:1" class="footnote-ref" role="doc-noteref">1</a></sup> chapter and <sup id="fnref:2:2"><a href="#fn:2:2" class="footnote-ref" role="doc-noteref">2</a></sup>.</p>
<div class="footnotes" role="doc-endnotes">
<hr>
<ol>
<li id="fn:2:1">
<p>shared definition&#160;<a href="#fnref:2:1" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
<li id="fn:2:2">
<p>d definition&#160;<a href="#fnref:2:2" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
</ol>
</div>
<h2>Chapter Three</h2>
<p>No footnotes here.</p>
<h2>Chapter Four</h2>
<p>End<sup id="fnref:4:1"><a href="#fn:4:1" class="footnote-ref" role="doc-noteref">1</a></sup>.</p>
<div class="footnotes" role="doc-endnotes">
<hr>
<ol>
<li id="fn:4:1">
<p>e definition&#160;<a href="#fnref:4:1" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
</ol>
</div>
`

func TestFootnoteSectionsSetextAndPrefix(t *testing.T) {
	// A setext level-1 heading splits sections; an ATX level-2 heading and an
	// ATX level-1 heading nested in a block quote do not.
	src := "Preface[^s]\n\nChapter One\n===========\n\nOne[^s] and two[^a].\n\n## Subsection\n\nStill one[^s].\n\n> # Quoted\n>\n> quoted[^b]\n\nChapter Two\n===========\n\nTwo[^s].\n\n[^s]: shared\n[^a]: alpha\n[^b]: bravo\n"

	md := testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewFootnoteParser()),
		),
		html.New(
			html.WithUnsafe(),
			html.WithExtensions(NewFootnoteHTMLRenderer(
				WithSectionHeadingLevel(1),
				WithIDPrefix("doc-"),
			)),
		),
	)
	out, err := md(src)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		`id="doc-fnref:0:1"`,
		`<a href="#doc-fn:0:1"`,
		`<li id="doc-fn:0:1">`,
		`id="doc-fnref:1:1"`,
		`id="doc-fnref1:1:1"`,
		`id="doc-fnref:1:2"`,
		`<li id="doc-fn:1:1">`,
		`<li id="doc-fn:1:2">`,
		`id="doc-fnref:2:1"`,
		`<li id="doc-fn:2:1">`,
	}
	for _, frag := range want {
		if !strings.Contains(out, frag) {
			t.Errorf("output missing %q\nfull output:\n%s", frag, out)
		}
	}

	ids := map[string]int{}
	for _, m := range idAttrRE.FindAllStringSubmatch(out, -1) {
		ids[m[1]]++
	}
	for id, count := range ids {
		if count != 1 {
			t.Errorf("id %q appears %d times, want exactly 1", id, count)
		}
	}
	for _, m := range hrefAttrRE.FindAllStringSubmatch(out, -1) {
		if ids[m[1]] != 1 {
			t.Errorf("link target %q resolves to %d elements, want exactly 1",
				m[1], ids[m[1]])
		}
	}

	if strings.Count(out, `<li id="doc-fn:1:1">`) != 1 ||
		strings.Count(out, `href="#doc-fn:1:1"`) != 2 {
		t.Errorf("chapter-one shared definition should be linked twice, got output:\n%s", out)
	}
	if strings.Count(out, `<div class="footnotes"`) != 3 {
		t.Errorf("want 3 endnote sections (preface, chapter one, chapter two), got:\n%s", out)
	}
}

func TestFootnoteSectionsXHTML(t *testing.T) {
	md := testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewFootnoteParser()),
		),
		html.New(
			html.WithUnsafe(),
			html.WithXHTML(),
			html.WithExtensions(NewFootnoteHTMLRenderer(WithSectionHeadingLevel(2))),
		),
	)
	out, err := md("Intro[^a]\n\n## One\n\nOne[^a].\n\n[^a]: note\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<hr />\n") {
		t.Errorf("sectioned XHTML output must use <hr />:\n%s", out)
	}
}
