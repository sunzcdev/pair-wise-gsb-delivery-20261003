package etree

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type memoryResourceTable map[string][]byte

func (t memoryResourceTable) resolver() XIncludeResolver {
	return func(href string) (io.ReadCloser, error) {
		data, ok := t[href]
		if !ok {
			return nil, ErrXIncludeResourceNotFound
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
}

func expandTestDocument(t *testing.T, input string, base string, resources memoryResourceTable, settings ...XIncludeSettings) *Document {
	t.Helper()
	doc := NewDocument()
	if err := doc.ReadFromString(input); err != nil {
		t.Fatalf("read document: %v", err)
	}

	setting := XIncludeSettings{
		BaseURL:  base,
		Resolver: resources.resolver(),
	}
	if len(settings) == 1 {
		setting = settings[0]
		if setting.Resolver == nil {
			setting.Resolver = resources.resolver()
		}
	}

	if err := doc.ExpandXIncludes(setting); err != nil {
		t.Fatalf("expand XIncludes: %v", err)
	}
	return doc
}

func TestExpandXIncludesNestedXML(t *testing.T) {
	resources := memoryResourceTable{
		"https://example.test/root/a.xml": []byte(`<a xmlns:xi="http://www.w3.org/2001/XInclude" xml:lang="fr"><b><xi:include href="../dir/c.xml"/></b><d href="d-link"/></a>`),
		"https://example.test/dir/c.xml":  []byte(`<c xmlns:xi="http://www.w3.org/2001/XInclude"><e href="e-link"/><xi:include href="f.xml"/></c>`),
		"https://example.test/dir/f.xml":  []byte(`<f xml:lang="de"><g href="g-link"/></f>`),
	}

	input := `<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="root/a.xml"/><h href="h-link"/></doc>`
	doc := expandTestDocument(t, input, "https://example.test/main.xml", resources)

	a := doc.Root().SelectElement("a")
	if a == nil {
		t.Fatal("expected included element a")
	}
	if a.SelectAttrValue("xml:base", "") != "https://example.test/root/a.xml" {
		t.Fatalf("unexpected a xml:base: %s", a.SelectAttrValue("xml:base", ""))
	}
	if a.SelectAttrValue("xml:lang", "") != "fr" {
		t.Fatalf("unexpected a xml:lang: %s", a.SelectAttrValue("xml:lang", ""))
	}

	b := a.SelectElement("b")
	f := b.SelectElement("c").SelectElement("f")
	if f == nil {
		t.Fatal("expected nested include f")
	}
	if f.SelectAttrValue("xml:base", "") != "https://example.test/dir/f.xml" {
		t.Fatalf("unexpected f xml:base: %s", f.SelectAttrValue("xml:base", ""))
	}
	if f.SelectAttrValue("xml:lang", "") != "de" {
		t.Fatalf("unexpected f xml:lang: %s", f.SelectAttrValue("xml:lang", ""))
	}
	if f.SelectElement("g") == nil {
		t.Fatal("expected g")
	}
}

func TestExpandXIncludesRelativeBasePreservesLinkSemantics(t *testing.T) {
	resources := memoryResourceTable{
		"sub/a.xml": []byte(`<a xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="c.xml"/></a>`),
		"sub/c.xml": []byte(`<c href="c-link"/>`),
	}
	input := `<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="sub/a.xml"/></doc>`
	doc := expandTestDocument(t, input, "main.xml", resources)

	a := doc.Root().SelectElement("a")
	if a == nil || a.SelectAttrValue("xml:base", "") != "sub/a.xml" {
		t.Fatalf("unexpected included base: %#v", a)
	}
	c := a.SelectElement("c")
	if c == nil || c.SelectAttrValue("xml:base", "") != "sub/c.xml" {
		t.Fatalf("unexpected nested relative resolution: %#v", c)
	}
	if got, err := resolveRelativeURI("sub/c.xml", "c-link"); err != nil || got != "sub/c-link" {
		t.Fatalf("relative link no longer resolves against included resource: %q %v", got, err)
	}
}

func TestExpandXIncludesSameDocumentPointer(t *testing.T) {
	input := `<doc xmlns:xi="http://www.w3.org/2001/XInclude"><s xml:lang="en"><a xml:id="target"><x/></a></s><xi:include href="" xpointer="element(target)"/></doc>`
	doc := expandTestDocument(t, input, "main.xml", nil, XIncludeSettings{
		BaseURL:  "main.xml",
		Resolver: func(string) (io.ReadCloser, error) { return nil, ErrXIncludeResourceNotFound },
	})

	included := doc.Root().SelectElement("a")
	if included == nil {
		t.Fatal("expected same-document included element")
	}
	if a := included.SelectAttr("xml:base"); a != nil {
		t.Fatalf("same-base inclusion should omit redundant xml:base: %q", a.Value)
	}
	if included.SelectAttrValue("xml:lang", "") != "en" {
		t.Fatalf("unexpected same-document lang: %s", included.SelectAttrValue("xml:lang", ""))
	}

}

func TestExpandXIncludesSameDocumentCycleFails(t *testing.T) {
	resources := memoryResourceTable{
		"a.xml": []byte(`<a xmlns:xi="http://www.w3.org/2001/XInclude" xml:id="a"><xi:include href="a.xml" xpointer="element(a)"/></a>`),
	}

	doc := NewDocument()
	input := `<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="a.xml" xpointer="element(a)"/></doc>`
	if err := doc.ReadFromString(input); err != nil {
		t.Fatalf("read document: %v", err)
	}
	before, _ := doc.WriteToString()
	err := doc.ExpandXIncludes(XIncludeSettings{
		BaseURL:  "main.xml",
		Resolver: resources.resolver(),
	})
	if !errors.Is(err, ErrXInclude) {
		t.Fatalf("expected same-document cycle error, got %v", err)
	}
	after, _ := doc.WriteToString()
	if after != before {
		t.Fatal("document changed after same-document cycle failure")
	}
}

func TestExpandXIncludesMissingResourceUsesFallback(t *testing.T) {
	resources := memoryResourceTable{
		"https://example.test/root/missing-fallback.xml": []byte(`<a xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="absent.xml"><xi:fallback><replacement href="fallback-link"/><xi:include href="nested.xml"/></xi:fallback></xi:include></a>`),
		"https://example.test/root/nested.xml":           []byte(`<nested href="nested-link"/>`),
		"https://example.test/fallback-fragment.xml":     []byte(`<a xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="absent.xml" xpointer="element(r)"><xi:fallback>fallback text</xi:fallback></xi:include></a>`),
	}

	input := `<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="root/missing-fallback.xml"/></doc>`
	doc := expandTestDocument(t, input, "https://example.test/main.xml", resources)
	replacement := doc.Root().SelectElement("a").SelectElement("replacement")
	if replacement == nil {
		t.Fatal("expected fallback replacement")
	}
	if replacement.Parent().SelectAttrValue("xml:base", "") != "https://example.test/root/missing-fallback.xml" {
		t.Fatalf("unexpected fallback context base: %s", replacement.Parent().SelectAttrValue("xml:base", ""))
	}
	if replacement.SelectAttr("xml:base") != nil {
		t.Fatal("fallback token without xml:base should not be rewritten")
	}
	nested := doc.Root().SelectElement("a").SelectElement("nested")
	if nested == nil {
		t.Fatal("expected nested include inside fallback")
	}
	if nested.SelectAttrValue("xml:base", "") != "https://example.test/root/nested.xml" {
		t.Fatalf("unexpected nested fallback base: %s", nested.SelectAttrValue("xml:base", ""))
	}

	doc = expandTestDocument(t, `<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="fallback-fragment.xml"/></doc>`, "https://example.test/main.xml", resources)
	if got := doc.Root().SelectElement("a").Text(); got != "fallback text" {
		t.Fatalf("unexpected fallback text: %q", got)
	}
}

func TestExpandXIncludesMutualReferencesFail(t *testing.T) {
	resources := memoryResourceTable{
		"a.xml": []byte(`<a xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="b.xml"/></a>`),
		"b.xml": []byte(`<b xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="a.xml"/></b>`),
	}
	input := `<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="a.xml"/></doc>`

	doc := NewDocument()
	if err := doc.ReadFromString(input); err != nil {
		t.Fatalf("read document: %v", err)
	}
	before, err := doc.WriteToString()
	if err != nil {
		t.Fatalf("serialize before: %v", err)
	}

	err = doc.ExpandXIncludes(XIncludeSettings{
		BaseURL:  "main.xml",
		Resolver: resources.resolver(),
	})
	if err == nil || !errors.Is(err, ErrXInclude) {
		t.Fatalf("expected XInclude error, got %v", err)
	}

	after, err := doc.WriteToString()
	if err != nil {
		t.Fatalf("serialize after: %v", err)
	}
	if after != before {
		t.Fatalf("document changed after failure\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestExpandXIncludesElementPointer(t *testing.T) {
	resources := memoryResourceTable{
		"p.xml": []byte(`<root xml:lang="en"><a id="one"><x xmlns="urn:n"><y/></x></a><b><c id="two"><d/></c></b></root>`),
	}
	input := `<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="p.xml" xpointer="element(two/1)"/><xi:include href="p.xml" xpointer="element(/1/2/1/1)"/></doc>`
	doc := expandTestDocument(t, input, "main.xml", resources)

	d := doc.Root().SelectElements("d")
	if len(d) != 2 {
		t.Fatalf("expected two selected d elements, got %d", len(d))
	}
	if d[0].SelectAttrValue("xml:base", "") != "p.xml" {
		t.Fatalf("unexpected selected fragment base: %s", d[0].SelectAttrValue("xml:base", ""))
	}
	if d[0].SelectAttrValue("xml:lang", "") != "en" {
		t.Fatalf("unexpected selected fragment lang: %s", d[0].SelectAttrValue("xml:lang", ""))
	}
	x := doc.Root().SelectElement("a")
	if x != nil {
		t.Fatal("id selection must not include unselected siblings")
	}
}

func TestExpandXIncludesXMLEncodings(t *testing.T) {
	resources := memoryResourceTable{
		"latin1.xml": append([]byte(`<?xml version="1.0" encoding="ISO-8859-1"?><a>caf`), 0xE9, '<', '/', 'a', '>'),
		"utf16.xml":  utf16Encoded([]rune(`<?xml version="1.0" encoding="UTF-16"?><a>héllo</a>`), true),
	}

	doc := expandTestDocument(t, `<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="latin1.xml"/><xi:include href="utf16.xml"/></doc>`, "main.xml", resources)
	elements := doc.Root().SelectElements("a")
	if len(elements) != 2 {
		t.Fatalf("expected two decoded elements, got %d", len(elements))
	}
	if got := elements[0].Text(); got != "caf\u00e9" {
		t.Fatalf("unexpected Latin-1 XML text: %q", got)
	}
	if got := elements[1].Text(); got != "héllo" {
		t.Fatalf("unexpected UTF-16 XML text: %q", got)
	}
}

func utf16Encoded(runes []rune, littleEndian bool) []byte {
	data := make([]byte, 0, len(runes)*2+2)
	if littleEndian {
		data = append(data, 0xFF, 0xFE)
	} else {
		data = append(data, 0xFE, 0xFF)
	}
	for _, r := range runes {
		value := uint16(r)
		if littleEndian {
			data = append(data, byte(value), byte(value>>8))
		} else {
			data = append(data, byte(value>>8), byte(value))
		}
	}
	return data
}

func TestExpandXIncludesUnsupportedPointerFails(t *testing.T) {
	resources := memoryResourceTable{"p.xml": []byte(`<root><a/></root>`)}

	for _, pointer := range []string{"xpointer(/)", "xpath(/root)", "one"} {
		doc := NewDocument()
		input := `<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="p.xml" xpointer="` + pointer + `"/></doc>`
		if err := doc.ReadFromString(input); err != nil {
			t.Fatalf("read document: %v", err)
		}
		err := doc.ExpandXIncludes(XIncludeSettings{
			BaseURL:  "main.xml",
			Resolver: resources.resolver(),
		})
		if err == nil || !errors.Is(err, ErrXInclude) {
			t.Fatalf("pointer %q: expected explicit error, got %v", pointer, err)
		}
	}
}

func TestExpandXIncludesInvalidAttributesAndNonMissingFailure(t *testing.T) {
	resources := memoryResourceTable{
		"bad.xml":      []byte(`<root><unclosed>`),
		"external.txt": []byte("text"),
	}

	tests := []struct {
		name  string
		input string
	}{
		{"missing href", `<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include/></doc>`},
		{"text xpointer", `<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="external.txt" parse="text" xpointer="element(a)"/></doc>`},
		{"bad parse", `<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="external.txt" parse="html"/></doc>`},
		{"invalid XML does not fallback", `<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="bad.xml"><xi:fallback>fallback</xi:fallback></xi:include></doc>`},
	}
	for _, test := range tests {
		doc := NewDocument()
		if err := doc.ReadFromString(test.input); err != nil {
			t.Fatalf("%s: read document: %v", test.name, err)
		}
		err := doc.ExpandXIncludes(XIncludeSettings{
			BaseURL:  "main.xml",
			Resolver: resources.resolver(),
		})
		if !errors.Is(err, ErrXInclude) {
			t.Fatalf("%s: expected XInclude error, got %v", test.name, err)
		}
	}
}

func TestExpandXIncludesText(t *testing.T) {
	resources := memoryResourceTable{
		"text-utf8.txt":    []byte("hello <world> & friends"),
		"text-utf16le.txt": append([]byte{0xFF, 0xFE}, []byte{0, 0}...),
		"text-latin1.txt":  []byte{0x63, 0x61, 0x66, 0xE9},
		"text-plain-utf16": append([]byte{0xFF, 0xFE}, []byte{0x68, 0x00, 0x69, 0x00}...),
	}

	doc := expandTestDocument(t, `<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="text-utf8.txt" parse="text"/></doc>`, "main.xml", resources)
	if got := doc.Root().Text(); got != "hello <world> & friends" {
		t.Fatalf("unexpected UTF-8 text: %q", got)
	}

	doc = expandTestDocument(t, `<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="text-plain-utf16" parse="text" encoding="UTF-16"/></doc>`, "main.xml", resources)
	if got := doc.Root().Text(); got != "hi" {
		t.Fatalf("unexpected UTF-16 text: %q", got)
	}

	doc = expandTestDocument(t, `<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="text-latin1.txt" parse="text" encoding="ISO-8859-1"/></doc>`, "main.xml", resources)
	if got := doc.Root().Text(); got != "caf\u00e9" {
		t.Fatalf("unexpected Latin-1 text: %q", got)
	}

	_ = resources
}

func TestExpandXIncludesBudgets(t *testing.T) {
	resources := memoryResourceTable{
		"big.xml": bytes.Repeat([]byte("a"), 11),
		"a.xml":   []byte(`<a xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="b.xml"/></a>`),
		"b.xml":   []byte(`<b xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="a.xml"/></b>`),
	}

	doc := NewDocument()
	if err := doc.ReadFromString(`<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="big.xml"/></doc>`); err != nil {
		t.Fatalf("read document: %v", err)
	}
	err := doc.ExpandXIncludes(XIncludeSettings{
		BaseURL:          "main.xml",
		Resolver:         resources.resolver(),
		MaxResourceBytes: 10,
		MaxTotalBytes:    1024,
		MaxDepth:         10,
	})
	if !errors.Is(err, ErrXIncludeSize) {
		t.Fatalf("expected size error, got %v", err)
	}

	doc = NewDocument()
	if err := doc.ReadFromString(`<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="a.xml"/></doc>`); err != nil {
		t.Fatalf("read document: %v", err)
	}
	err = doc.ExpandXIncludes(XIncludeSettings{
		BaseURL:  "main.xml",
		Resolver: resources.resolver(),
		MaxDepth: 1,
	})
	if !errors.Is(err, ErrXIncludeDepth) {
		t.Fatalf("expected depth error, got %v", err)
	}

	small := memoryResourceTable{"a.xml": []byte(`<a/>`), "b.xml": []byte(`<b/>`)}
	calls := 0
	doc = NewDocument()
	if err := doc.ReadFromString(`<doc xmlns:xi="http://www.w3.org/2001/XInclude"><xi:include href="a.xml"/><xi:include href="b.xml"/></doc>`); err != nil {
		t.Fatalf("read budget document: %v", err)
	}
	err = doc.ExpandXIncludes(XIncludeSettings{
		BaseURL: "main.xml",
		Resolver: func(href string) (io.ReadCloser, error) {
			calls++
			return small.resolver()(href)
		},
		MaxResourceBytes: 5,
		MaxTotalBytes:    7,
	})
	if !errors.Is(err, ErrXIncludeSize) || calls != 2 {
		t.Fatalf("expected both resources to count against total budget, calls=%d err=%v", calls, err)
	}
}
