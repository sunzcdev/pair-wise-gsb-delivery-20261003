package etree

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

// memResources is an in-memory resource table used to verify that XInclude
// processing only ever accesses resources through the resolver.
type memResources map[string]string

func (m memResources) resolver() XIncludeResolver {
	return func(uri string) (io.ReadCloser, error) {
		s, ok := m[uri]
		if !ok {
			return nil, fmt.Errorf("no such resource: %s", uri)
		}
		return io.NopCloser(strings.NewReader(s)), nil
	}
}

func processXInclude(t *testing.T, docXML string, opts XIncludeOptions) (*Document, error) {
	t.Helper()
	doc := NewDocument()
	if err := doc.ReadFromString(docXML); err != nil {
		t.Fatalf("etree: failed to parse test document: %v", err)
	}
	err := doc.ProcessXIncludes(opts)
	return doc, err
}

func TestXIncludeNested(t *testing.T) {
	res := memResources{
		"https://cfg.example/app/level1.xml":     `<level1><xi:include xmlns:xi="http://www.w3.org/2001/XInclude" href="sub/level2.xml"/><link href="page.html"/></level1>`,
		"https://cfg.example/app/sub/level2.xml": `<level2><link href="deep.html"/></level2>`,
	}
	doc, err := processXInclude(t,
		`<config xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="level1.xml"/></config>`,
		XIncludeOptions{Resolver: res.resolver(), BaseURI: "https://cfg.example/app/main.xml"})
	if err != nil {
		t.Fatalf("etree: unexpected xinclude error: %v", err)
	}
	checkDocEq(t, doc, `<config xmlns:xi="http://www.w3.org/2001/XInclude"><level1 xml:base="https://cfg.example/app/level1.xml"><level2 xml:base="https://cfg.example/app/sub/level2.xml"><link href="deep.html"/></level2><link href="page.html"/></level1></config>`)
}

func TestXIncludeFallbackOnMissingResource(t *testing.T) {
	res := memResources{}
	doc, err := processXInclude(t,
		`<config xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="mem:///missing.xml"><xi:fallback><default><k>v</k></default></xi:fallback></xi:include></config>`,
		XIncludeOptions{Resolver: res.resolver()})
	if err != nil {
		t.Fatalf("etree: unexpected xinclude error: %v", err)
	}
	checkDocEq(t, doc, `<config xmlns:xi="http://www.w3.org/2001/XInclude"><default><k>v</k></default></config>`)
}

func TestXIncludeMissingResourceWithoutFallback(t *testing.T) {
	const docXML = `<config xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="mem:///missing.xml"/></config>`
	doc, err := processXInclude(t, docXML, XIncludeOptions{Resolver: memResources{}.resolver()})
	if err == nil {
		t.Fatal("etree: expected error for missing resource without fallback")
	}
	checkDocEq(t, doc, docXML) // document must be preserved on failure
}

func TestXIncludeMutualRecursion(t *testing.T) {
	res := memResources{
		"mem:///a.xml": `<a xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="b.xml"/></a>`,
		"mem:///b.xml": `<b xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="a.xml"/></b>`,
	}
	const docXML = `<config xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="mem:///a.xml"/></config>`
	doc, err := processXInclude(t, docXML, XIncludeOptions{Resolver: res.resolver()})
	if err == nil || !strings.Contains(err.Error(), "recursive") {
		t.Fatalf("etree: expected recursion error, got: %v", err)
	}
	checkDocEq(t, doc, docXML) // document must be preserved on failure
}

func TestXIncludeParseText(t *testing.T) {
	res := memResources{
		"mem:///note.txt":  "\xef\xbb\xbfhello 世界",
		"mem:///latin.txt": "caf\xe9",
	}
	doc, err := processXInclude(t,
		`<config xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include parse="text" href="mem:///note.txt"/><xi:include parse="text" encoding="latin1" href="mem:///latin.txt"/></config>`,
		XIncludeOptions{
			Resolver: res.resolver(),
			CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
				if charset != "latin1" {
					return nil, fmt.Errorf("unsupported charset %q", charset)
				}
				b, err := io.ReadAll(input)
				if err != nil {
					return nil, err
				}
				runes := make([]rune, 0, len(b))
				for _, c := range b {
					runes = append(runes, rune(c))
				}
				return strings.NewReader(string(runes)), nil
			},
		})
	if err != nil {
		t.Fatalf("etree: unexpected xinclude error: %v", err)
	}
	checkDocEq(t, doc, `<config xmlns:xi="http://www.w3.org/2001/XInclude">hello 世界café</config>`)
}

func TestXIncludeParseTextUnsupportedEncoding(t *testing.T) {
	res := memResources{"mem:///x.txt": "abc"}
	const docXML = `<config xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include parse="text" encoding="utf-16" href="mem:///x.txt"/></config>`
	doc, err := processXInclude(t, docXML, XIncludeOptions{Resolver: res.resolver()})
	if err == nil || !strings.Contains(err.Error(), "encoding") {
		t.Fatalf("etree: expected encoding error, got: %v", err)
	}
	checkDocEq(t, doc, docXML)
}

func TestXIncludeElementXPointer(t *testing.T) {
	res := memResources{
		"mem:///lib.xml": `<lib><item id="a">A</item><item id="b"><part>P1</part><part>P2</part></item></lib>`,
	}
	cases := []struct {
		xpointer string
		want     string
	}{
		{"element(/1/1)", `<item id="a" xml:base="mem:///lib.xml">A</item>`},
		{"element(/1/2)", `<item id="b" xml:base="mem:///lib.xml"><part>P1</part><part>P2</part></item>`},
		{"element(a)", `<item id="a" xml:base="mem:///lib.xml">A</item>`},
		{"element(b/2)", `<part xml:base="mem:///lib.xml">P2</part>`},
	}
	for _, c := range cases {
		doc, err := processXInclude(t,
			`<config xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="mem:///lib.xml" xpointer="`+c.xpointer+`"/></config>`,
			XIncludeOptions{Resolver: res.resolver()})
		if err != nil {
			t.Fatalf("etree: unexpected xinclude error for %s: %v", c.xpointer, err)
		}
		checkDocEq(t, doc, `<config xmlns:xi="http://www.w3.org/2001/XInclude">`+c.want+`</config>`)
	}
}

func TestXIncludeXPointerFallback(t *testing.T) {
	res := memResources{"mem:///lib.xml": `<lib><item/></lib>`}
	doc, err := processXInclude(t,
		`<config xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="mem:///lib.xml" xpointer="element(/1/9)"><xi:fallback><missing/></xi:fallback></xi:include></config>`,
		XIncludeOptions{Resolver: res.resolver()})
	if err != nil {
		t.Fatalf("etree: unexpected xinclude error: %v", err)
	}
	checkDocEq(t, doc, `<config xmlns:xi="http://www.w3.org/2001/XInclude"><missing/></config>`)
}

func TestXIncludeUnsupportedXPointer(t *testing.T) {
	res := memResources{"mem:///lib.xml": `<lib><item/></lib>`}
	for _, xptr := range []string{`xpointer(/lib/item)`, `element()`, `element(/x)`, `foo`} {
		docXML := `<config xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="mem:///lib.xml" xpointer="` + xptr + `"/></config>`
		doc, err := processXInclude(t, docXML, XIncludeOptions{Resolver: res.resolver()})
		if err == nil || !strings.Contains(err.Error(), "xpointer") {
			t.Fatalf("etree: expected unsupported xpointer error for %q, got: %v", xptr, err)
		}
		checkDocEq(t, doc, docXML) // document must be preserved on failure
	}
}

func TestXIncludeXMLBaseAndLangFixup(t *testing.T) {
	res := memResources{
		"https://cfg.example/app/frag/part.xml": `<part><link href="rel/page.html"/></part>`,
	}
	doc, err := processXInclude(t,
		`<config xmlns:xi="http://www.w3.org/2001/XInclude" xml:lang="zh-CN"><section xml:base="sub/"><xi:include href="../frag/part.xml"/></section></config>`,
		XIncludeOptions{Resolver: res.resolver(), BaseURI: "https://cfg.example/app/main.xml"})
	if err != nil {
		t.Fatalf("etree: unexpected xinclude error: %v", err)
	}
	checkDocEq(t, doc, `<config xmlns:xi="http://www.w3.org/2001/XInclude" xml:lang="zh-CN"><section xml:base="sub/"><part xml:base="https://cfg.example/app/frag/part.xml" xml:lang="zh-CN"><link href="rel/page.html"/></part></section></config>`)
}

func TestXIncludeBudgets(t *testing.T) {
	res := memResources{
		"mem:///d1.xml":  `<d1 xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="d2.xml"/></d1>`,
		"mem:///d2.xml":  `<d2 xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="d3.xml"/></d2>`,
		"mem:///d3.xml":  `<d3/>`,
		"mem:///big.xml": `<big>` + strings.Repeat("x", 1024) + `</big>`,
	}
	const chainXML = `<config xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="mem:///d1.xml"/></config>`

	doc, err := processXInclude(t, chainXML, XIncludeOptions{Resolver: res.resolver(), MaxDepth: 2})
	if err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("etree: expected depth budget error, got: %v", err)
	}
	checkDocEq(t, doc, chainXML)

	doc, err = processXInclude(t, chainXML, XIncludeOptions{Resolver: res.resolver(), MaxIncludes: 2})
	if err == nil || !strings.Contains(err.Error(), "count") {
		t.Fatalf("etree: expected include count budget error, got: %v", err)
	}
	checkDocEq(t, doc, chainXML)

	const bigXML = `<config xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="mem:///big.xml"/></config>`
	doc, err = processXInclude(t, bigXML, XIncludeOptions{Resolver: res.resolver(), MaxBytes: 512})
	if err == nil || !strings.Contains(err.Error(), "byte budget") {
		t.Fatalf("etree: expected byte budget error, got: %v", err)
	}
	checkDocEq(t, doc, bigXML)
}

func TestXIncludeNoResolver(t *testing.T) {
	const docXML = `<config xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="mem:///a.xml"/></config>`
	doc, err := processXInclude(t, docXML, XIncludeOptions{})
	if err == nil {
		t.Fatal("etree: expected error when resolver is missing")
	}
	checkDocEq(t, doc, docXML)
}

func TestXIncludeInvalidParseValue(t *testing.T) {
	res := memResources{"mem:///a.xml": `<a/>`}
	const docXML = `<config xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include parse="dom" href="mem:///a.xml"/></config>`
	doc, err := processXInclude(t, docXML, XIncludeOptions{Resolver: res.resolver()})
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("etree: expected parse value error, got: %v", err)
	}
	checkDocEq(t, doc, docXML)
}

func TestXIncludeIncludedContentIsCopied(t *testing.T) {
	res := memResources{"mem:///a.xml": `<a><b>text</b></a>`}
	doc, err := processXInclude(t,
		`<config xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="mem:///a.xml"/><xi:include href="mem:///a.xml"/></config>`,
		XIncludeOptions{Resolver: res.resolver()})
	if err != nil {
		t.Fatalf("etree: unexpected xinclude error: %v", err)
	}
	checkDocEq(t, doc, `<config xmlns:xi="http://www.w3.org/2001/XInclude"><a xml:base="mem:///a.xml"><b>text</b></a><a xml:base="mem:///a.xml"><b>text</b></a></config>`)
	root := doc.Root()
	if root.ChildElements()[0] == root.ChildElements()[1] {
		t.Error("etree: included elements must be independent copies")
	}
}
