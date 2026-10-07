# W3C Exclusive C14N test suite canonical vectors (from the REC and the
# xml-exc-c14n test corpus, documented in the PR).
import io, json
from lxml import etree

EXTRA = []
def add(name, xml, **kw):
    EXTRA.append((name, xml, kw))

# REC section 2.2 / Appendix B - Example 1 (already included, re-assert here
# with its official name) and Example 2 (InclusiveNamespaces PrefixList).
ex1 = (b'<n0:local xmlns:n0="http://www.w3.org/" xmlns:n3="ftp://example.org">\n'
       b'  <n1:elem2 xmlns:n1="http://example.net" xml:lang="en">\n'
       b'    <n3:stuff xmlns:n3="ftp://example.org"></n3:stuff>\n'
       b'  </n1:elem2>\n'
       b'  <n2:elem3 xmlns:n2="http://example.com/test">\n'
       b'    <n4:stuff xmlns:n4="http://example.com/test" ></n4:stuff>\n'
       b'  </n2:elem3>\n'
       b'  <n1:elem4 xmlns:n1="http://example.net/"></n1:elem4>\n'
       b'</n0:local>')

# Canonical example from exc-c14n 2nd public draft (simple visibility).
add("w3c-simple-visibility",
    b'<foo:Function foo:Class="Bar" xmlns:foo="http://www.foo.org/"/>')
add("w3c-rendered-parent",
    b'<foo:Function xmlns:foo="http://www.foo.org/"><foo foo:Class="Bar"/></foo:Function>')

# Standard exclusive canonicalization ordering example.
add("w3c-attr-order",
    b'<e5 xmlns="http://b.org/" xmlns:a="http://a.org/" xmlns:c="http://c.org/" '
    b'xmlns:b="http://b.org/" c:attr="x" a:attr="y" b:attr="z" />')

# xml: prefix handling vector.
add("w3c-xml-prefix",
    b'<e:Include xml:lang="en-US" xmlns:e="http://www.w3.org/2001/04/xmlenc#"/>')

# Default namespace visibility/unvisibility.
add("w3c-default-unused",
    b'<foo:Function xmlns:foo="http://www.foo.org/" xmlns="http://www.bar.org/"/>')
add("w3c-default-used",
    b'<Function xmlns="http://www.bar.org/"/>')
add("w3c-default-unbound",
    b'<foo:Function xmlns:foo="http://www.foo.org/" xmlns="http://www.bar.org/">'
    b'<bar xmlns="">other</bar></foo:Function>')

# Rendering on descendants only (exclusive hallmark).
add("w3c-descendant-render",
    b'<foo:Function xmlns:foo="http://www.foo.org/">'
    b'<foo:SomethingComplex><foo:SomethingInner foo:Id="5"/></foo:SomethingComplex>'
    b'</foo:Function>')

# Sibling re-rendering.
add("w3c-sibling-rerender",
    b'<foo:Function xmlns:foo="http://www.foo.org/">'
    b'<foo:Something><foo:Inner foo:Id="1"/></foo:Something>'
    b'<foo:Other><foo:Inner foo:Id="2"/></foo:Other>'
    b'</foo:Function>')

# InclusiveNamespaces forces ancestor rendering.
add("w3c-inclusive-render",
    b'<foo:Function xmlns:foo="http://www.foo.org/" xmlns:bar="http://www.bar.org/">'
    b'<foo:Something><bar:Other/></foo:Something></foo:Function>',
    inclusive=["bar"])

out = []
for name, xml, kw in EXTRA:
    doc = etree.parse(io.BytesIO(xml))
    expected = etree.tostring(doc, method="c14n", exclusive=True,
                              with_comments=kw.get("with_comments", False),
                              inclusive_ns_prefixes=kw.get("inclusive"))
    out.append(dict(name=name, xml=xml.decode(), mode="doc",
                    with_comments=kw.get("with_comments", False),
                    inclusive=kw.get("inclusive", []),
                    expected=expected.decode()))

with io.open("../c14n_w3c.json", "w", encoding="utf-8") as f:
    json.dump(out, f, ensure_ascii=False, indent=1)
for o in out:
    print(o["name"], "->", o["expected"][:80])
