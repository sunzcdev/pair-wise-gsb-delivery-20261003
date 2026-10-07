package etree

import (
	"errors"
	"testing"
)

// TestExcC14NSpecExample2_2 checks the example from section 2.2 of the
// W3C Exclusive XML Canonicalization 1.0 recommendation. The namespace
// declaration for n3 is rendered on the n3:stuff element, where the prefix
// is visibly utilized, and not hoisted to the parent element.
func TestExcC14NSpecExample2_2(t *testing.T) {
	input := `<n1:elem1 xmlns:n1="http://example.net" xml:lang="en">` + "\n" +
		`  <n3:stuff xmlns:n3="ftp://example.org"/>` + "\n" +
		`</n1:elem1>`
	want := `<n1:elem1 xmlns:n1="http://example.net" xml:lang="en">` + "\n" +
		`  <n3:stuff xmlns:n3="ftp://example.org"></n3:stuff>` + "\n" +
		`</n1:elem1>`
	doc := newDocumentFromString(t, input)
	got, err := doc.ExcC14N(nil)
	if err != nil {
		t.Fatalf("etree: unexpected ExcC14N error: %v", err)
	}
	checkStrEq(t, string(got), want)
}

// TestExcC14NSpecExample2_1 checks the example from section 2.1 of the
// W3C Exclusive XML Canonicalization 1.0 recommendation. The n1:elem2
// subtree is canonicalized in the context of its ancestors: the unused n0
// and n3 declarations of the ancestor are dropped, while n3 is rendered at
// its point of use.
func TestExcC14NSpecExample2_1(t *testing.T) {
	input := `<n0:local xmlns:n0="foo:bar"` + "\n" +
		`    xmlns:n3="ftp://example.org">` + "\n" +
		`        <n1:elem2 xmlns:n1="http://example.net"` + "\n" +
		`            xml:lang="en">` + "\n" +
		`            <n3:stuff xmlns:n3="ftp://example.org"/>` + "\n" +
		`    </n1:elem2>` + "\n" +
		`</n0:local>`
	want := `<n1:elem2 xmlns:n1="http://example.net" xml:lang="en">` + "\n" +
		`            <n3:stuff xmlns:n3="ftp://example.org"></n3:stuff>` + "\n" +
		`    </n1:elem2>`
	doc := newDocumentFromString(t, input)
	elem2 := doc.FindElement("//n1:elem2")
	if elem2 == nil {
		t.Fatal("etree: failed to locate n1:elem2")
	}
	got, err := elem2.ExcC14N(nil)
	if err != nil {
		t.Fatalf("etree: unexpected ExcC14N error: %v", err)
	}
	checkStrEq(t, string(got), want)
}

// TestExcC14NDetachedSubtree verifies that a subtree canonicalization
// retains ancestor-declared prefix bindings that are visibly utilized
// within the subtree, so the canonical form remains valid after the
// subtree is detached from its ancestors. It also verifies that the
// original tree is not modified.
func TestExcC14NDetachedSubtree(t *testing.T) {
	input := `<root xmlns:n1="http://example.net"><wrap><n1:item attr="1"/></wrap></root>`
	want := `<wrap><n1:item xmlns:n1="http://example.net" attr="1"></n1:item></wrap>`
	doc := newDocumentFromString(t, input)
	wrap := doc.FindElement("//wrap")
	if wrap == nil {
		t.Fatal("etree: failed to locate wrap element")
	}
	got, err := wrap.ExcC14N(nil)
	if err != nil {
		t.Fatalf("etree: unexpected ExcC14N error: %v", err)
	}
	checkStrEq(t, string(got), want)
	checkIntEq(t, len(wrap.ChildElements()[0].Attr), 1)
	s, err := doc.WriteToString()
	if err != nil {
		t.Fatalf("etree: unexpected WriteToString error: %v", err)
	}
	checkStrEq(t, s, input)
}

// TestExcC14NW3CExample3_1 checks example 3.1 (PIs, comments, and content
// outside the document element) of the W3C Canonical XML 1.0
// recommendation, which involves no namespaces and is therefore identical
// under exclusive canonicalization. Per the XPath data model, processing
// instruction data excludes whitespace following the target.
func TestExcC14NW3CExample3_1(t *testing.T) {
	input := `<?xml version="1.0"?>` + "\n" +
		"\n" +
		`<?xml-stylesheet   href="doc.xsl"` + "\n" +
		`   type="text/xsl"   ?>` + "\n" +
		"\n" +
		`<!DOCTYPE doc SYSTEM "doc.dtd">` + "\n" +
		"\n" +
		`<doc>Hello, world!<!-- Comment 1 --></doc>` + "\n" +
		"\n" +
		`<?pi-without-data     ?>` + "\n" +
		"\n" +
		`<!-- Comment 2 -->` + "\n" +
		"\n" +
		`<!-- Comment 3 -->`
	wantNoComments := `<?xml-stylesheet href="doc.xsl"` + "\n" +
		`   type="text/xsl"   ?>` + "\n" +
		`<doc>Hello, world!</doc>` + "\n" +
		`<?pi-without-data?>`
	wantWithComments := `<?xml-stylesheet href="doc.xsl"` + "\n" +
		`   type="text/xsl"   ?>` + "\n" +
		`<doc>Hello, world!<!-- Comment 1 --></doc>` + "\n" +
		`<?pi-without-data?>` + "\n" +
		`<!-- Comment 2 -->` + "\n" +
		`<!-- Comment 3 -->`
	doc := newDocumentFromString(t, input)
	got, err := doc.ExcC14N(nil)
	if err != nil {
		t.Fatalf("etree: unexpected ExcC14N error: %v", err)
	}
	checkStrEq(t, string(got), wantNoComments)
	got, err = doc.ExcC14N(&ExcC14NSettings{WithComments: true})
	if err != nil {
		t.Fatalf("etree: unexpected ExcC14N error: %v", err)
	}
	checkStrEq(t, string(got), wantWithComments)
}

// TestExcC14NW3CExample3_2 checks example 3.2 (whitespace in character
// content) of the W3C Canonical XML 1.0 recommendation. Whitespace within
// character content is preserved.
func TestExcC14NW3CExample3_2(t *testing.T) {
	input := `<doc>` + "\n" +
		`   <clean>   </clean>` + "\n" +
		`   <dirty>   A   B   </dirty>` + "\n" +
		`   <mixed>` + "\n" +
		`      A` + "\n" +
		`      <clean>   </clean>` + "\n" +
		`      B` + "\n" +
		`      <dirty>   A   B   </dirty>` + "\n" +
		`      C` + "\n" +
		`   </mixed>` + "\n" +
		`</doc>`
	doc := newDocumentFromString(t, input)
	got, err := doc.ExcC14N(nil)
	if err != nil {
		t.Fatalf("etree: unexpected ExcC14N error: %v", err)
	}
	checkStrEq(t, string(got), input)
}

// TestExcC14NW3CExample3_3 checks example 3.3 (start and end tags,
// namespace processing, and attribute sorting) of the W3C Canonical XML
// 1.0 recommendation, adapted for exclusive canonicalization: namespace
// declarations that are not visibly utilized are omitted, and the default
// namespace is rendered only where its binding changes. The attribute
// provided by the DTD in the original example (attr="default" on e9) is
// written explicitly, since etree does not process DTD default attributes.
func TestExcC14NW3CExample3_3(t *testing.T) {
	input := `<doc>` + "\n" +
		`   <e1   />` + "\n" +
		`   <e2   ></e2>` + "\n" +
		`   <e3   name = "elem3"   id="elem3"   />` + "\n" +
		`   <e4   name="elem4"   id="elem4"   ></e4>` + "\n" +
		`   <e5 a:attr="out" b:attr="sorted" attr2="all" attr="I'm"` + "\n" +
		`      xmlns:b="http://www.ietf.org"` + "\n" +
		`      xmlns:a="http://www.w3.org"` + "\n" +
		`      xmlns="http://example.org"/>` + "\n" +
		`   <e6 xmlns="" xmlns:a="http://www.w3.org">` + "\n" +
		`      <e7 xmlns="http://www.ietf.org">` + "\n" +
		`         <e8 xmlns="" xmlns:a="http://www.w3.org">` + "\n" +
		`            <e9 xmlns="" xmlns:a="http://www.ietf.org" attr="default"/>` + "\n" +
		`         </e8>` + "\n" +
		`      </e7>` + "\n" +
		`   </e6>` + "\n" +
		`</doc>`
	want := `<doc>` + "\n" +
		`   <e1></e1>` + "\n" +
		`   <e2></e2>` + "\n" +
		`   <e3 id="elem3" name="elem3"></e3>` + "\n" +
		`   <e4 id="elem4" name="elem4"></e4>` + "\n" +
		`   <e5 xmlns="http://example.org" xmlns:a="http://www.w3.org" xmlns:b="http://www.ietf.org" attr="I'm" attr2="all" b:attr="sorted" a:attr="out"></e5>` + "\n" +
		`   <e6>` + "\n" +
		`      <e7 xmlns="http://www.ietf.org">` + "\n" +
		`         <e8 xmlns="">` + "\n" +
		`            <e9 attr="default"></e9>` + "\n" +
		`         </e8>` + "\n" +
		`      </e7>` + "\n" +
		`   </e6>` + "\n" +
		`</doc>`
	doc := newDocumentFromString(t, input)
	got, err := doc.ExcC14N(nil)
	if err != nil {
		t.Fatalf("etree: unexpected ExcC14N error: %v", err)
	}
	checkStrEq(t, string(got), want)
}

// TestExcC14NEscaping checks the character reference escaping rules for
// text content and attribute values.
func TestExcC14NEscaping(t *testing.T) {
	input := `<doc a="&lt;&amp;&quot;&#x9;&#xA;&#xD;">&lt;&amp;&gt;&#xD;</doc>`
	want := `<doc a="&lt;&amp;&quot;&#x9;&#xA;&#xD;">&lt;&amp;&gt;&#xD;</doc>`
	doc := newDocumentFromString(t, input)
	got, err := doc.ExcC14N(nil)
	if err != nil {
		t.Fatalf("etree: unexpected ExcC14N error: %v", err)
	}
	checkStrEq(t, string(got), want)
}

// TestExcC14NInclusivePrefixes checks the InclusiveNamespaces PrefixList
// behavior: listed prefixes are rendered wherever they are in scope, even
// when not visibly utilized.
func TestExcC14NInclusivePrefixes(t *testing.T) {
	input := `<root xmlns="urn:default" xmlns:p="urn:p" xmlns:q="urn:q">` +
		`<sub><child p:x="1"/></sub></root>`
	want := `<sub xmlns="urn:default" xmlns:q="urn:q">` +
		`<child xmlns:p="urn:p" p:x="1"></child></sub>`
	doc := newDocumentFromString(t, input)
	sub := doc.FindElement("//sub")
	if sub == nil {
		t.Fatal("etree: failed to locate sub element")
	}
	settings := &ExcC14NSettings{InclusivePrefixes: []string{"q", "#default"}}
	got, err := sub.ExcC14N(settings)
	if err != nil {
		t.Fatalf("etree: unexpected ExcC14N error: %v", err)
	}
	checkStrEq(t, string(got), want)
}

// TestExcC14NErrors checks that inputs forbidden by the canonicalization
// specification produce errors.
func TestExcC14NErrors(t *testing.T) {
	cases := []struct {
		input string
		err   error
	}{
		{`<doc xmlns:p="relative/uri"><p:a/></doc>`, ErrC14NRelativeNamespace},
		{`<doc xmlns:p="urn:x"><p:a/></doc>`, nil},
		{`<doc><p:a/></doc>`, ErrC14NUnboundPrefix},
		{`<doc xmlns:xml="urn:wrong"/>`, ErrC14NForbiddenBinding},
		{`<doc xmlns:p="http://www.w3.org/XML/1998/namespace"/>`, ErrC14NForbiddenBinding},
		{`<doc xmlns:p="http://www.w3.org/2000/xmlns/"/>`, ErrC14NForbiddenBinding},
	}
	for i, c := range cases {
		doc := newDocumentFromString(t, c.input)
		_, err := doc.ExcC14N(nil)
		if !errors.Is(err, c.err) {
			t.Errorf("etree: case %d: unexpected error. Got: %v. Wanted: %v\n", i, err, c.err)
		}
	}
}

// TestExcC14NDocumentLevel checks the canonicalization of content outside
// the root element: top-level character data and directives are dropped,
// and top-level processing instructions and comments are separated from
// their neighbors by newline characters.
func TestExcC14NDocumentLevel(t *testing.T) {
	cases := []struct {
		input    string
		comments bool
		want     string
	}{
		{`<!--c--><a/>`, false, `<a></a>`},
		{`<!--c--><a/>`, true, `<!--c-->` + "\n" + `<a></a>`},
		{`<a/><!--c--><?p i?>`, false, `<a></a>` + "\n" + `<?p i?>`},
		{`<!--c--><a/><!--d-->`, false, `<a></a>`},
		{`<?p1?><!--c--><a/>`, false, `<?p1?>` + "\n" + `<a></a>`},
		{`<?p1?><a/><?p2?>`, false, `<?p1?>` + "\n" + `<a></a>` + "\n" + `<?p2?>`},
		{`<?xml version="1.0"?>` + "\n\n" + `<a/>` + "\n", false, `<a></a>`},
	}
	for i, c := range cases {
		doc := newDocumentFromString(t, c.input)
		got, err := doc.ExcC14N(&ExcC14NSettings{WithComments: c.comments})
		if err != nil {
			t.Fatalf("etree: case %d: unexpected ExcC14N error: %v", i, err)
		}
		if string(got) != c.want {
			t.Errorf("etree: case %d: unexpected result.\nGot:\n%s\nWanted:\n%s\n", i, got, c.want)
		}
	}
}
