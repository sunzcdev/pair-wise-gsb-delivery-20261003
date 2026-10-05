package extension

import (
	"sync"
	"testing"

	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/testutil"
)

func smartQuotesMarkdown(style SmartQuoteStyle) testutil.MarkdownToStringFunc {
	return testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewSmartQuotesParser(WithSmartQuoteStyle(style))),
		),
		html.New(
			html.WithUnsafe(),
		),
	)
}

func TestSmartQuotesEnglish(t *testing.T) {
	markdown := smartQuotesMarkdown(EnglishSmartQuotes)
	testutil.DoTestCases(markdown, []testutil.MarkdownTestCase{
		{
			No:          1,
			Description: "opener before emphasis and closer after emphasis",
			Markdown:    `"*hello*"`,
			Expected:    "<p>“<em>hello</em>”</p>\n",
		},
		{
			No:          2,
			Description: "opener before a link and closer after a link",
			Markdown:    `"[hi](https://example.com/?q=a%22b)"`,
			Expected:    `<p>“<a href="https://example.com/?q=a%22b">hi</a>”</p>` + "\n",
		},
		{
			No:          3,
			Description: "contractions and possessives use apostrophes",
			Markdown:    `Alice's dog, I'm here, don't stop, the Smiths' cat, doin' it`,
			Expected:    "<p>Alice’s dog, I’m here, don’t stop, the Smiths’ cat, doin’ it</p>\n",
		},
		{
			No:          4,
			Description: "leading elisions and decade abbreviations",
			Markdown:    `the '90s and rock 'n roll, 'twas so`,
			Expected:    "<p>the ’90s and rock ’n roll, ’twas so</p>\n",
		},
		{
			No:          5,
			Description: "feet and inches stay straight",
			Markdown:    `My height is 5'6" and monitor 21"".`,
			Expected:    `<p>My height is 5'6&quot; and monitor 21&quot;&quot;.</p>` + "\n",
		},
		{
			No:          6,
			Description: "nested double and single quotes",
			Markdown:    `Janet said "he called it 'breaking news' today".`,
			Expected:    "<p>Janet said “he called it ‘breaking news’ today”.</p>\n",
		},
		{
			No:          7,
			Description: "unclosed opener and unmatched closer",
			Markdown:    `"open end and stray" then "still open`,
			Expected:    "<p>“open end and stray” then “still open</p>\n",
		},
		{
			No:          8,
			Description: "consecutive quotes after punctuation",
			Markdown:    `reported "issue 1", "issue 2", 'issue 3'`,
			Expected:    "<p>reported “issue 1&quot;, “issue 2&quot;, ‘issue 3’</p>\n",
		},
		{
			No:          9,
			Description: "quotes cannot cross a code span",
			Markdown:    `"open ` + "`code\"x`" + ` not closed"`,
			Expected:    "<p>“open <code>code&quot;x</code> not closed”</p>\n",
		},
		{
			No:          10,
			Description: "quotes cannot cross inline raw html",
			Markdown:    `"open <span title="q">text</span> close"`,
			Expected:    `<p>“open <span title="q">text</span> close”</p>` + "\n",
		},
		{
			No:          11,
			Description: "autolink text is not converted",
			Markdown:    `"see <https://example.com/?q=a%22b> end"`,
			Expected:    `<p>“see <a href="https://example.com/?q=a%22b">https://example.com/?q=a%22b</a> end”</p>` + "\n",
		},
		{
			No:          12,
			Description: "pairing does not cross paragraphs",
			Markdown:    "\"first paragraph\n\nsecond\"",
			Expected:    "<p>“first paragraph</p>\n<p>second”</p>\n",
		},
		{
			No:          13,
			Description: "escaped quote stays literal",
			Markdown:    `say \"hello\" now`,
			Expected:    "<p>say &quot;hello&quot; now</p>\n",
		},
		{
			No:          14,
			Description: "pairing flows across a soft line break",
			Markdown:    "\"hello\nworld\"",
			Expected:    "<p>“hello\nworld”</p>\n",
		},
		{
			No:          15,
			Description: "mixed chinese and english keeps apostrophe",
			Markdown:    `他说 "don't" 然后走了`,
			Expected:    "<p>他说 “don’t” 然后走了</p>\n",
		},
		{
			No:          16,
			Description: "headings are converted independently",
			Markdown:    "# \"Title\"\n\n\"body\"",
			Expected:    "<h1>“Title”</h1>\n<p>“body”</p>\n",
		},
		{
			No:          17,
			Description: "image alt is converted but url and title are not",
			Markdown:    `![a "q" alt](img.png "t\"itle")`,
			Expected:    `<p><img src="img.png" alt="a “q” alt" title="t&quot;itle"></p>` + "\n",
		},
		{
			No:          18,
			Description: "paragraph scoped pairing inside a blockquote",
			Markdown:    "> \"quoted\"\n>\n> \"second\"",
			Expected:    "<blockquote>\n<p>“quoted”</p>\n<p>“second”</p>\n</blockquote>\n",
		},
	}, t)
}

func TestSmartQuotesChinese(t *testing.T) {
	markdown := smartQuotesMarkdown(ChineseSmartQuotes)
	testutil.DoTestCases(markdown, []testutil.MarkdownTestCase{
		{
			No:          1,
			Description: "outer and nested corner brackets",
			Markdown:    `他说："你好，'世界'！"`,
			Expected:    "<p>他说：「你好，『世界』！」</p>\n",
		},
		{
			No:          2,
			Description: "english apostrophe inside chinese quotes",
			Markdown:    `"it's 中文"`,
			Expected:    "<p>「it’s 中文」</p>\n",
		},
		{
			No:          3,
			Description: "digits and inches stay straight",
			Markdown:    `高度 5'6"，型号 21""。`,
			Expected:    "<p>高度 5'6&quot;，型号 21&quot;&quot;。</p>\n",
		},
		{
			No:          4,
			Description: "pairing crosses emphasis and links",
			Markdown:    `"*强调* 与 [链接](https://example.com/?q=a)"`,
			Expected:    `<p>「<em>强调</em> 与 <a href="https://example.com/?q=a">链接</a>」</p>` + "\n",
		},
		{
			No:          5,
			Description: "code span is a barrier",
			Markdown:    `"开头 ` + "`\"代码\"`" + ` 结尾"`,
			Expected:    "<p>「开头 <code>&quot;代码&quot;</code> 结尾」</p>\n",
		},
		{
			No:          6,
			Description: "unclosed quote is rendered as an opener",
			Markdown:    `"没有闭合`,
			Expected:    "<p>「没有闭合</p>\n",
		},
		{
			No:          7,
			Description: "nested double and single corner brackets",
			Markdown:    `"一'二"三"二'一"`,
			Expected:    "<p>「一『二」三『二』一』</p>\n",
		},
	}, t)
}

func TestSmartQuotesConcurrent(t *testing.T) {
	markdown := smartQuotesMarkdown(EnglishSmartQuotes)
	const goroutines = 16
	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				got, err := markdown(`"a" and 'b'`)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
					return
				}
				want := "<p>“a” and ‘b’</p>\n"
				if got != want {
					t.Errorf("concurrent parse mismatch:\n got: %q\nwant: %q", got, want)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestSmartQuotesLeavesDefaultUntouched(t *testing.T) {
	// The classic inline typographer must keep producing its historical
	// output even when the new extension is present in the same package.
	markdown := testutil.NewMarkdownToStringFunc(
		parser.New(
			parser.WithExtensions(NewTypographerParser()),
		),
		html.New(html.WithUnsafe()),
	)
	got, err := markdown(`This should "be" replaced`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "<p>This should “be” replaced</p>\n"
	if got != want {
		t.Errorf("default typographer output changed:\n got: %q\nwant: %q", got, want)
	}
}
