package extension

import (
	"regexp"
	"strings"
	"testing"

	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/testutil"
)

const footnoteSectionSample = `Preface ref.[^shared]

## Chapter One

First ref.[^shared] Second ref.[^only-one] First again.[^shared]

` + "```" + `
## not a chapter [^only-one]
` + "```" + `

## Chapter Two

Only shared.[^shared] Never defined.[^missing]

[^shared]: shared definition, placed at the end
[^only-one]: used only in chapter one
[^unused]: this definition is never referenced
`

func footnoteSectionMarkdown(t *testing.T, level int) testutil.MarkdownToStringFunc {
	t.Helper()
	opts := []FootnoteHTMLRendererOption{}
	if level > 0 {
		opts = append(opts, WithSectionNumbering(level))
	}
	return testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewFootnoteParser()),
		),
		html.New(
			html.WithUnsafe(),
			html.WithExtensions(NewFootnoteHTMLRenderer(opts...)),
		),
	)
}

func TestFootnoteSectionNumberingRendering(t *testing.T) {
	markdown := footnoteSectionMarkdown(t, 2)
	testutil.DoTestCase(
		markdown,
		testutil.MarkdownTestCase{
			No:          1,
			Description: "Footnotes numbered per level-2 section",
			Markdown:    footnoteSectionSample,
			Expected: `<p>Preface ref.<sup id="fnref:1-1"><a href="#fn:1-1" class="footnote-ref" role="doc-noteref">1</a></sup></p>
<div class="footnotes" role="doc-endnotes">
<hr>
<ol>
<li id="fn:1-1">
<p>shared definition, placed at the end&#160;<a href="#fnref:1-1" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
</ol>
</div>
<h2>Chapter One</h2>
<p>First ref.<sup id="fnref:2-1"><a href="#fn:2-1" class="footnote-ref" role="doc-noteref">1</a></sup> Second ref.<sup id="fnref:2-2"><a href="#fn:2-2" class="footnote-ref" role="doc-noteref">2</a></sup> First again.<sup id="fnref1:2-1"><a href="#fn:2-1" class="footnote-ref" role="doc-noteref">1</a></sup></p>
<pre><code>## not a chapter [^only-one]
</code></pre>
<div class="footnotes" role="doc-endnotes">
<hr>
<ol>
<li id="fn:2-1">
<p>shared definition, placed at the end&#160;<a href="#fnref:2-1" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a>&#160;<a href="#fnref1:2-1" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
<li id="fn:2-2">
<p>used only in chapter one&#160;<a href="#fnref:2-2" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
</ol>
</div>
<h2>Chapter Two</h2>
<p>Only shared.<sup id="fnref:3-1"><a href="#fn:3-1" class="footnote-ref" role="doc-noteref">1</a></sup> Never defined.[^missing]</p>
<div class="footnotes" role="doc-endnotes">
<hr>
<ol>
<li id="fn:3-1">
<p>shared definition, placed at the end&#160;<a href="#fnref:3-1" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
</ol>
</div>`,
		},
		t,
	)
}

func TestFootnoteGlobalNumberingDefault(t *testing.T) {
	markdown := footnoteSectionMarkdown(t, 0)
	testutil.DoTestCase(
		markdown,
		testutil.MarkdownTestCase{
			No:          2,
			Description: "Footnotes numbered globally by default",
			Markdown:    footnoteSectionSample,
			Expected: `<p>Preface ref.<sup id="fnref:1"><a href="#fn:1" class="footnote-ref" role="doc-noteref">1</a></sup></p>
<h2>Chapter One</h2>
<p>First ref.<sup id="fnref1:1"><a href="#fn:1" class="footnote-ref" role="doc-noteref">1</a></sup> Second ref.<sup id="fnref:2"><a href="#fn:2" class="footnote-ref" role="doc-noteref">2</a></sup> First again.<sup id="fnref2:1"><a href="#fn:1" class="footnote-ref" role="doc-noteref">1</a></sup></p>
<pre><code>## not a chapter [^only-one]
</code></pre>
<h2>Chapter Two</h2>
<p>Only shared.<sup id="fnref3:1"><a href="#fn:1" class="footnote-ref" role="doc-noteref">1</a></sup> Never defined.[^missing]</p>
<div class="footnotes" role="doc-endnotes">
<hr>
<ol>
<li id="fn:1">
<p>shared definition, placed at the end&#160;<a href="#fnref:1" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a>&#160;<a href="#fnref1:1" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a>&#160;<a href="#fnref2:1" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a>&#160;<a href="#fnref3:1" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
<li id="fn:2">
<p>used only in chapter one&#160;<a href="#fnref:2" class="footnote-backref" role="doc-backlink">&#x21a9;&#xfe0e;</a></p>
</li>
</ol>
</div>`,
		},
		t,
	)
}

var (
	hrefRe  = regexp.MustCompile(`href="#([^"]+)"`)
	idRe    = regexp.MustCompile(`\sid="([^"]+)"`)
	fnrefRe = regexp.MustCompile(`<sup id="(fnref[^"]+)"`)
	fnliRe  = regexp.MustCompile(`<li id="(fn:[^"]+)"`)
)

// assertFootnoteLinkIntegrity verifies that every footnote reference link
// and every backlink points at exactly one existing id, and that every
// footnote id is reached by exactly one incoming link.
func assertFootnoteLinkIntegrity(t *testing.T, out string, wantBlocks int) {
	t.Helper()

	if got := strings.Count(out, `<div class="footnotes"`); got != wantBlocks {
		t.Fatalf("endnotes blocks: got %d, want %d", got, wantBlocks)
	}

	ids := map[string]int{}
	for _, m := range idRe.FindAllStringSubmatch(out, -1) {
		ids[m[1]]++
	}
	for id, count := range ids {
		if count != 1 {
			t.Errorf("id %q appears %d times, want exactly 1", id, count)
		}
	}

	incoming := map[string]int{}
	var links []string
	for _, m := range hrefRe.FindAllStringSubmatch(out, -1) {
		target := m[1]
		links = append(links, target)
		incoming[target]++
	}

	var refIDs, defIDs []string
	for _, m := range fnrefRe.FindAllStringSubmatch(out, -1) {
		refIDs = append(refIDs, m[1])
	}
	for _, m := range fnliRe.FindAllStringSubmatch(out, -1) {
		defIDs = append(defIDs, strings.TrimPrefix(m[1], "fn:"))
	}

	if len(refIDs) != 5 {
		t.Errorf("reference sup elements: got %d, want 5", len(refIDs))
	}
	for _, id := range refIDs {
		if ids[id] != 1 {
			t.Errorf("reference id %q missing or duplicated", id)
		}
		if incoming[id] != 1 {
			t.Errorf("reference id %q has %d backlinks, want 1", id, incoming[id])
		}
	}
	for _, token := range defIDs {
		id := "fn:" + token
		if ids[id] != 1 {
			t.Errorf("definition id %q missing or duplicated", id)
		}
		linksTo := 0
		for _, target := range links {
			if target == id {
				linksTo++
			}
		}
		if linksTo == 0 {
			t.Errorf("definition id %q has no incoming reference link", id)
		}
	}

	if strings.Contains(out, "unused") {
		t.Errorf("unreferenced definition was rendered")
	}
	if !strings.Contains(out, "[^missing]") {
		t.Errorf("missing reference should remain literal text")
	}
}

func TestFootnoteLinkIntegrity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		level      int
		wantBlocks int
	}{
		{"global", 0, 1},
		{"per-section", 2, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			markdown := footnoteSectionMarkdown(t, tc.level)
			out, err := markdown(footnoteSectionSample)
			if err != nil {
				t.Fatal(err)
			}
			assertFootnoteLinkIntegrity(t, out, tc.wantBlocks)
		})
	}
}

func TestFootnoteSectionNumberingRestarts(t *testing.T) {
	markdown := footnoteSectionMarkdown(t, 3)
	out, err := markdown(`# Top

` + "```" + `
### code fence
` + "```" + `

### H3 One

a[^x]

### H3 Two

b[^x] c[^y]

[^x]: x def
[^y]: y def
`)
	if err != nil {
		t.Fatal(err)
	}
	// No preface references and no h3 delimiter inside the code fence: the
	// preface section is empty, so exactly two endnotes blocks are emitted.
	if got := strings.Count(out, `<div class="footnotes"`); got != 2 {
		t.Fatalf("endnotes blocks: got %d, want 2", got)
	}
	for _, want := range []string{
		`id="fnref:2-1"`,
		`href="#fn:2-1"`,
		`id="fn:2-1"`,
		`href="#fnref:2-1"`,
		`id="fnref:3-1"`,
		`href="#fn:3-1"`,
		`id="fn:3-1"`,
		`href="#fnref:3-1"`,
		`id="fnref:3-2"`,
		`href="#fn:3-2"`,
		`id="fn:3-2"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
	// Each chapter restarts display numbering at 1.
	if !strings.Contains(out, `a<sup id="fnref:2-1"><a href="#fn:2-1"`) {
		t.Errorf("first chapter footnote should display 1")
	}
	if !strings.Contains(out, "b<sup id=\"fnref:3-1\"") ||
		!strings.Contains(out, "c<sup id=\"fnref:3-2\"") {
		t.Errorf("second chapter footnotes should display 1 and 2")
	}
	if strings.Count(out, `href="#fn:2-1"`) != 1 ||
		strings.Count(out, `href="#fn:3-1"`) != 1 ||
		strings.Count(out, `href="#fn:3-2"`) != 1 {
		t.Errorf("unexpected forward-link counts")
	}
}
