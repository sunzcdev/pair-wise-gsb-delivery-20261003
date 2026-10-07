import io, json, subprocess, os, sys
from lxml import etree

CASES = []

def add(name, xml, exclusive=True, with_comments=False, inclusive=None, subtree_path=None):
    CASES.append(dict(name=name, xml=xml, exclusive=exclusive, with_comments=with_comments,
                      inclusive=inclusive or [], subtree=subtree_path))

# W3C REC Appendix B Example 1 input
ex1 = (b'<n0:local xmlns:n0="http://www.w3.org/" xmlns:n3="ftp://example.org">\n'
       b'  <n1:elem2 xmlns:n1="http://example.net" xml:lang="en">\n'
       b'    <n3:stuff xmlns:n3="ftp://example.org"></n3:stuff>\n'
       b'  </n1:elem2>\n'
       b'  <n2:elem3 xmlns:n2="http://example.com/test">\n'
       b'    <n4:stuff xmlns:n4="http://example.com/test" ></n4:stuff>\n'
       b'  </n2:elem3>\n'
       b'  <n1:elem4 xmlns:n1="http://example.net/"></n1:elem4>\n'
       b'</n0:local>')
add("w3c-ex1", ex1)
add("w3c-ex2", ex1, inclusive=["n1","n2"])
add("w3c-ex1-c", ex1, with_comments=True)

add("default", b'<root xmlns="urn:def"><x xmlns="">y</x></root>')
add("subtree-child", b'<root xmlns:a="urn:a"><wrap><a:child attr="v"/></wrap></root>', subtree_path="0.0")
add("subtree-wrap", b'<root xmlns:a="urn:a"><wrap><a:child attr="v"/></wrap></root>', subtree_path="0")
add("subtree-default", b'<root xmlns="urn:def"><child><x/></child></root>', subtree_path="0")
add("incl-subtree", b'<root xmlns:a="urn:a"><wrap><inner><a:z/></inner></wrap></root>',
    inclusive=["a"], subtree_path="0")
add("rebound", b'<a:r xmlns:a="urn:1"><a:x xmlns:a="urn:2"><a:y/></a:x></a:r>')
add("attrsort", b'<r xmlns="urn:z" xmlns:a="urn:a" xmlns:b="urn:b" z="0" a:x="1" b:y="2" a:a="3"/>')
add("xmlprefix", b'<r xml:lang="en"><x xml:space="preserve"/></r>')
add("escape", b'<r a="1&#x9;2&#xA;3&#xD;4">x &amp; y &lt; &gt; z\rq</r>')
add("comments", b'<r><!-- c1 --><x><!-- c2 --></x></r>')
add("comments-on", b'<r><!-- c1 --><x><!-- c2 --></x></r>', with_comments=True)
add("cdata", b'<r><![CDATA[a < b & c]]></r>')
add("docpi", b'<?pi target?><r/>', with_comments=True)
add("docpi-multi", b'<?app inst?><?app2 x?><r/>', with_comments=True)
add("doccomment", b'<?app inst?><!--c--><r/><!--d-->', with_comments=True)
add("crcdata", b'<r>a&#xD;b</r>')
add("picomment-inner", b'<r><?p v?><!--c--></r>', with_comments=True)
add("attrprefix", b'<r xmlns:a="urn:a" a:k="1"/>')
add("siblings", b'<r xmlns:a="urn:a"><w><a:x/></w><z><a:y/></z></r>')
add("inclinused", b'<r xmlns:a="urn:a"><x/></r>', inclusive=["a"])
add("emptydefault", b'<r xmlns="urn:d"><x xmlns=""><y/></x></r>')
add("rd1", b'<r xmlns:a="urn:1"><w xmlns:a="urn:2"><a:x/></w></r>')
add("rd2", b'<a:r xmlns:a="urn:1"><w xmlns:a="urn:2"><a:x/></w></a:r>')
add("rd3", b'<r xmlns:a="urn:1"><w xmlns:a="urn:2"><a:x xmlns:a="urn:3"/></w></r>')
add("hashdefault", b'<r xmlns="urn:d"><x><y/></x></r>', inclusive=["#default"])
add("xsi", b'<r xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="t"/>')
add("i4", b'<r><w xmlns:a="urn:a"><a:x/></w></r>', inclusive=["a"])
add("s1", b'<r xmlns:a="urn:a" a:b="1" xml:lang="en" z="2"/>')
add("s2", b'<r xmlns:a="urn:a" xmlns:h="http://x" h:b="1" a:a="2" n="3"/>')
add("s3", b'<r xmlns:xml="http://www.w3.org/XML/1998/namespace" xml:lang="en"/>')
add("pi-escape", b'<r><?pi a&#xD;b&#xA;c&#x9;d&e<f?></r>', with_comments=True)
add("comment-escape", b'<r><!--a&#xD;b&#xA;c&#x9;d&e<f--></r>', with_comments=True)
add("default-prefixed-mix", b'<a:r xmlns:a="urn:a" xmlns="urn:d"><x xmlns="urn:d2"/></a:r>')
add("attrsort2", b'<r xmlns:a="urn:a" xmlns:b="urn:b" a:z="1" b:z="2"/>')
add("mixed-text", b'<r> a <x/> b </r>')
add("whitespace-leaf", b'<r>  </r>')
add("unicode", '<r xml:lang="zh">\u00e9\u00e8</r>'.encode())
add("xml-style", b'<?xml-stylesheet href="x" type="t/xsl"?><r/>', with_comments=True)
add("cr-comment", b'<r><!--a&#xD;b--></r>', with_comments=True)
add("literal-cr-comment", b'<r><!--a\rb--></r>', with_comments=True)
add("pi-inside-text", b'<r>a<?p v?>b</r>', with_comments=True)
add("incl-rebind", b'<a:r xmlns:a="urn:1"><w><a:x xmlns:a="urn:2"/></w></a:r>', inclusive=["a"])
add("pi-cr-tab", b"<r><?pi a\rb&#x9;c?></r>", with_comments=True)
add("crlfattr", b'<r a="x&#xD;&#xA;y&#x9;z"/>')
add("n12", b'<?a?>\n<!--b-->\n<?c?>\n<r/>', with_comments=True)
add("incl-empty-unused", b'<r xmlns:a="urn:a"/>', inclusive=["a"])
add("default-unbind-deep", b'<r xmlns="urn:d"><x xmlns=""><y/></x></r>')
add("pi-leading-ws", b"<r><?pi    hello world?></r>", with_comments=True)
add("pi-empty", b"<r><?pi?></r>", with_comments=True)
add("comment-pad", b"<r><!--hello--></r>", with_comments=True)
add("no-comment-default", b'<!--c--><r/>', with_comments=False)
add("xml-decl", b'<?xml version="1.0"?><r/>', with_comments=True)
add("doctype", b'<!DOCTYPE r [<!ELEMENT r EMPTY>]><r/>')
add("multi-ns-same-elem", b'<r xmlns:a="urn:a" xmlns:b="urn:b" xmlns:c="urn:c" a:x="1" c:y="2" b:z="3"/>')
add("nested-default", b'<r xmlns="urn:d1"><x xmlns="urn:d2"><y/></x></r>')
add("xml-lang-sort", b'<r a="1" xml:lang="en" b="2"/>')
add("branch-redeclare", b'<r xmlns:a="urn:a"><x><a:u/></x><y><a:v/></y></r>')
add("intermediate-rebind", b'<r xmlns:a="urn:1"><w xmlns:a="urn:2"><u/><v><a:x/></v></w></r>')
add("c14n-text-entities", b'<r>&lt;&gt;&amp;&quot;&apos;&#x9;&#xA;&#xD;</r>')

# error cases (expected failure)
ERRORS = [
    ("relative-used", b'<r xmlns:a="rel/x"><a:x/></r>'),
    ("relative-unused", b'<r xmlns:a="rel/x"><x/></r>'),
    ("relative-ancestor-subtree", b'<r xmlns:a="rel/x"><w><x/></w></r>'),
    ("relative-default", b'<r xmlns="rel/x"><x/></r>'),
    ("xmlns-xmlns", b'<r xmlns:xmlns="http://www.w3.org/2000/xmlns/"/>'),
    ("bad-xml-ns", b'<r xmlns:xml="wrong" xml:lang="en"/>'),
]

out = []
for case in CASES:
    doc = etree.parse(io.BytesIO(case["xml"]))
    if case["subtree"] is not None:
        node = doc.getroot()
        for idx in case["subtree"].split("."):
            # find element children only
            count = int(idx)
            for child in node:
                if isinstance(child.tag, str):
                    if count == 0:
                        node = child
                        break
                    count -= 1
        target = node
        out.append(dict(name=case["name"], xml=case["xml"].decode("utf-8", "surrogateescape"),
                        mode="subtree", with_comments=case["with_comments"],
                        inclusive=case["inclusive"],
                        expected=etree.tostring(target, method="c14n", exclusive=True,
                                                with_comments=case["with_comments"],
                                                inclusive_ns_prefixes=case["inclusive"] or None).decode("utf-8","surrogateescape"),
                        path=case["subtree"]))
    else:
        out.append(dict(name=case["name"], xml=case["xml"].decode("utf-8","surrogateescape"),
                        mode="doc", with_comments=case["with_comments"],
                        inclusive=case["inclusive"],
                        expected=etree.tostring(doc, method="c14n", exclusive=True,
                                                with_comments=case["with_comments"],
                                                inclusive_ns_prefixes=case["inclusive"] or None).decode("utf-8","surrogateescape")))

errs = []
for name, xml in ERRORS:
    errs.append(dict(name=name, xml=xml.decode()))

with io.open("../c14n_vectors.json", "w", encoding="utf-8") as f:
    json.dump(dict(cases=out, errors=errs), f, ensure_ascii=False, indent=1)
print("cases:", len(out), "errors:", len(errs))
