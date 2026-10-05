package extension

import (
	"strings"
	"sync"
	"testing"

	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/testutil"
)

func smartQuotesMarkdown(mode SmartQuotesMode) testutil.MarkdownToStringFunc {
	return testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewSmartQuotesParser(mode)),
		),
		html.New(
			html.WithUnsafe(),
		),
	)
}

func TestSmartQuotesEnglish(t *testing.T) {
	markdown := smartQuotesMarkdown(SmartQuotesEnglish)
	cases := []testutil.MarkdownTestCase{
		{
			No:          1,
			Description: "opening quote before emphasis and closing quote after emphasis",
			Markdown:    "She said \"*hello*\" and left.",
			Expected:    "<p>She said \u201c<em>hello</em>\u201d and left.</p>",
		},
		{
			No:          2,
			Description: "pairing flows through a link label",
			Markdown:    "\"see [the docs](https://example.com/p) now\"",
			Expected:    "<p>\u201csee <a href=\"https://example.com/p\">the docs</a> now\u201d</p>",
		},
		{
			No:          3,
			Description: "abbreviations: contractions, possessives, decades and elided stems",
			Markdown:    "Alice's, don't, 90's, the '90s, 'net and doin'",
			Expected:    "<p>Alice\u2019s, don\u2019t, 90\u2019s, the \u201990s, \u2019net and doin\u2019</p>",
		},
		{
			No:          4,
			Description: "feet and inches marks are not opening quotes",
			Markdown:    "My height is 5'6\"\". \"Monitor 21\"\" and \"x\"",
			Expected:    "<p>My height is 5'6&quot;&quot;. \u201cMonitor 21\u201d&quot; and \u201cx\u201d</p>",
		},
		{
			No:          5,
			Description: "nested double and single quotes",
			Markdown:    "Janet said, \"when 'breaking' news\" stops.",
			Expected:    "<p>Janet said, \u201cwhen \u2018breaking\u2019 news\u201d stops.</p>",
		},
		{
			No:          6,
			Description: "unclosed quotes keep their opening glyph",
			Markdown:    "\"an unclosed double and 'a single",
			Expected:    "<p>\u201can unclosed double and \u2018a single</p>",
		},
		{
			No:          7,
			Description: "consecutive quotes pair LIFO",
			Markdown:    "empty \"\" and '' end",
			Expected:    "<p>empty \u201c\u201d and \u2018\u2019 end</p>",
		},
		{
			No:          8,
			Description: "mixed Chinese and English prose",
			Markdown:    "他 wrote \"hello\" 说 '你好' end",
			Expected:    "<p>他 wrote \u201chello\u201d 说 \u2018你好\u2019 end</p>",
		},
		{
			No:          9,
			Description: "code span severs pairing and stays literal",
			Markdown:    "`\"x\"` then \"open and 'nested",
			Expected:    "<p><code>&quot;x&quot;</code> then \u201copen and \u2018nested</p>",
		},
		{
			No:          10,
			Description: "raw HTML is an opaque barrier",
			Markdown:    "\"before <b> tag \"after\" text",
			Expected:    "<p>\u201cbefore <b> tag \u201cafter\u201d text</p>",
		},
		{
			No:          11,
			Description: "pairing does not cross paragraphs",
			Markdown:    "\"first paragraph\n\n\"second\" paragraph",
			Expected:    "<p>\u201cfirst paragraph</p>\n<p>\u201csecond\u201d paragraph</p>",
		},
		{
			No:          12,
			Description: "URL destinations and image alt text are untouched",
			Markdown:    "![it's a \"cat\"](https://example.com/a'b) and \"[link](https://example.com/c\"d)\"",
			Expected:    "<p><img src=\"https://example.com/a'b\" alt=\"it's a &quot;cat&quot;\"> and \u201c<a href=\"https://example.com/c%22d\">link</a>\u201d</p>",
		},
	}
	for _, tc := range cases {
		testutil.DoTestCase(markdown, tc, t)
	}
}

func TestSmartQuotesChinese(t *testing.T) {
	markdown := smartQuotesMarkdown(SmartQuotesChinese)
	cases := []testutil.MarkdownTestCase{
		{
			No:          1,
			Description: "corner double and single quotes with nesting",
			Markdown:    "他说：\"外层 '内层' 结束\"。",
			Expected:    "<p>他说：\u300c外层 \u300e内层\u300f 结束\u300d。</p>",
		},
		{
			No:          2,
			Description: "English apostrophes still use the curly apostrophe",
			Markdown:    "Alice's likes \"引用\" text",
			Expected:    "<p>Alice\u2019s likes \u300c引用\u300d text</p>",
		},
		{
			No:          3,
			Description: "pairing crosses emphasis but a code span severs it",
			Markdown:    "\"a *b* c `'x'` d\"",
			Expected:    "<p>\u300ca <em>b</em> c <code>'x'</code> d&quot;</p>",
		},
		{
			No:          4,
			Description: "unclosed and consecutive corner quotes",
			Markdown:    "\"\" and 'half",
			Expected:    "<p>\u300c\u300d and \u300ehalf</p>",
		},
	}
	for _, tc := range cases {
		testutil.DoTestCase(markdown, tc, t)
	}
}

func TestSmartQuotesDefaultModeUnchanged(t *testing.T) {
	markdown := testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewTypographerParser()),
		),
		html.New(html.WithUnsafe()),
	)
	testutil.DoTestCase(markdown, testutil.MarkdownTestCase{
		No:          1,
		Description: "default typographer output uses HTML entities as before",
		Markdown:    "This should \"be\" replaced and Alice's",
		Expected:    "<p>This should \u201cbe\u201d replaced and Alice\u2019s</p>",
	}, t)
}

func TestSmartQuotesConcurrentDocuments(t *testing.T) {
	english := smartQuotesMarkdown(SmartQuotesEnglish)
	chinese := smartQuotesMarkdown(SmartQuotesChinese)

	const workers = 8
	const iterations = 50
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				if id%2 == 0 {
					out, err := english("doc one: \"alpha 'beta' gamma\"")
					if err != nil {
						t.Errorf("english: %v", err)
						return
					}
					want := "<p>doc one: \u201calpha \u2018beta\u2019 gamma\u201d</p>"
					if strings.TrimSpace(out) != want {
						t.Errorf("english mismatch:\n got: %q\nwant: %q", out, want)
						return
					}
				} else {
					out, err := chinese("文档二：\"甲 '乙' 丙\"")
					if err != nil {
						t.Errorf("chinese: %v", err)
						return
					}
					want := "<p>文档二：\u300c甲 \u300e乙\u300f 丙\u300d</p>"
					if strings.TrimSpace(out) != want {
						t.Errorf("chinese mismatch:\n got: %q\nwant: %q", out, want)
						return
					}
				}
			}
		}(worker)
	}
	wg.Wait()
}
