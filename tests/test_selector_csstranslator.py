"""
Selector tests for cssselect backend
"""

from __future__ import annotations

import gc
import pickle
import random
import weakref
from abc import ABC, abstractmethod
from typing import Any

import pytest
from cssselect import __version__ as CSSSELECT_VERSION_STRING
from cssselect.parser import SelectorSyntaxError
from cssselect.xpath import ExpressionError
from cssselect.xpath import XPathExpr as OriginalXPathExpr
from packaging.version import Version

from parsel import Selector, css2xpath
from parsel.csstranslator import (
    GenericTranslator,
    HTMLTranslator,
    TranslatorProtocol,
    XPathExpr,
)

CSSSELECT_VERSION = Version(CSSSELECT_VERSION_STRING)

HTMLBODY = """
<html>
<body>
<div>
 <a id="name-anchor" name="foo"></a>
 <a id="tag-anchor" rel="tag" href="http://localhost/foo">link</a>
 <a id="nofollow-anchor" rel="nofollow" href="https://example.org"> link</a>
 <p id="paragraph">
   lorem ipsum text
   <b id="p-b">hi</b> <em id="p-em">there</em>
   <b id="p-b2">guy</b>
   <input type="checkbox" id="checkbox-unchecked" />
   <input type="checkbox" id="checkbox-disabled" disabled="" />
   <input type="text" id="text-checked" checked="checked" />
   <input type="hidden" />
   <input type="hidden" disabled="disabled" />
   <input type="checkbox" id="checkbox-checked" checked="checked" />
   <input type="checkbox" id="checkbox-disabled-checked"
          disabled="disabled" checked="checked" />
   <fieldset id="fieldset" disabled="disabled">
     <input type="checkbox" id="checkbox-fieldset-disabled" />
     <input type="hidden" />
   </fieldset>
 </p>
 <map name="dummymap">
   <area shape="circle" coords="200,250,25" href="foo.html" id="area-href" />
   <area shape="default" id="area-nohref" />
 </map>
</div>
<div class="cool-footer" id="foobar-div" foobar="ab bc cde">
    <span id="foobar-span">foo ter</span>
</div>
</body></html>
"""


class TestTranslatorBase(ABC):
    @property
    @abstractmethod
    def tr_cls(self) -> type[TranslatorProtocol]:
        raise NotImplementedError

    def c2x(self, css: str) -> str:
        return self.tr_cls().css_to_xpath(css)

    @pytest.mark.parametrize(
        ("css", "xpath"),
        [
            ("::attr(name)", "descendant-or-self::*/@name"),
            ("a::attr(href)", "descendant-or-self::a/@href"),
            (
                "a ::attr(img)",
                "descendant-or-self::a/descendant-or-self::*/@img",
            ),
            ("a > ::attr(class)", "descendant-or-self::a/*/@class"),
            (
                r"a::attr(\:foo)",
                "descendant-or-self::a/attribute::*[name() = ':foo']",
            ),
            (
                r"a::attr(foo\:bar)",
                "descendant-or-self::a/attribute::*[name() = 'foo:bar']",
            ),
        ],
    )
    def test_attr_function(self, css: str, xpath: str) -> None:
        assert self.c2x(css) == xpath, css

    @pytest.mark.parametrize(
        ("css", "exc"),
        [
            ("::attr(12)", ExpressionError),
            ("::attr(34test)", ExpressionError),
            ("::attr(@href)", SelectorSyntaxError),
        ],
    )
    def test_attr_function_exception(self, css: str, exc: type[Exception]) -> None:
        with pytest.raises(exc):
            self.c2x(css)

    @pytest.mark.parametrize(
        ("css", "xpath"),
        [
            ("::text", "descendant-or-self::text()"),
            ("p::text", "descendant-or-self::p/text()"),
            ("p ::text", "descendant-or-self::p/descendant-or-self::text()"),
            ("#id::text", "descendant-or-self::*[@id = 'id']/text()"),
            ("p#id::text", "descendant-or-self::p[@id = 'id']/text()"),
            (
                "p#id ::text",
                "descendant-or-self::p[@id = 'id']/descendant-or-self::text()",
            ),
            ("p#id > ::text", "descendant-or-self::p[@id = 'id']/*/text()"),
            (
                "p#id ~ ::text",
                "descendant-or-self::p[@id = 'id']/following-sibling::*/text()",
            ),
            ("a[href]::text", "descendant-or-self::a[@href]/text()"),
            (
                "a[href] ::text",
                "descendant-or-self::a[@href]/descendant-or-self::text()",
            ),
            (
                "p::text, a::text",
                "descendant-or-self::p/text() | descendant-or-self::a/text()",
            ),
        ],
    )
    def test_text_pseudo_element(self, css: str, xpath: str) -> None:
        assert self.c2x(css) == xpath, css

    @pytest.mark.parametrize(
        ("css", "exc"),
        [
            ("::attribute(12)", ExpressionError),
            ("::text()", ExpressionError),
            ("::attr(@href)", SelectorSyntaxError),
        ],
    )
    def test_pseudo_function_exception(self, css: str, exc: type[Exception]) -> None:
        with pytest.raises(exc):
            self.c2x(css)

    @pytest.mark.parametrize(
        ("css", "exc"),
        [
            ("::text-node", ExpressionError),
        ],
    )
    def test_unknown_pseudo_element(self, css: str, exc: type[Exception]) -> None:
        with pytest.raises(exc):
            self.c2x(css)

    @pytest.mark.parametrize(
        ("css", "exc"),
        [
            (":text", ExpressionError),
            (":attribute(name)", ExpressionError),
        ],
    )
    def test_unknown_pseudo_class(self, css: str, exc: type[Exception]) -> None:
        with pytest.raises(exc):
            self.c2x(css)


class TestHTMLTranslator(TestTranslatorBase):
    tr_cls = HTMLTranslator


class TestGenericTranslator(TestTranslatorBase):
    tr_cls = GenericTranslator


@pytest.mark.parametrize("tr_cls", [GenericTranslator, HTMLTranslator])
class TestTranslatorCache:
    def test_cache(self, tr_cls: type[GenericTranslator | HTMLTranslator]) -> None:
        tr = tr_cls()
        assert tr.css_to_xpath("a::text") == tr.css_to_xpath("a::text")
        assert tr._cache.cache_info().hits == 1

    def test_garbage_collection(
        self, tr_cls: type[GenericTranslator | HTMLTranslator]
    ) -> None:
        tr = tr_cls()
        tr.css_to_xpath("a::text")
        ref = weakref.ref(tr)
        del tr
        gc.collect()
        assert ref() is None

    def test_pickle(self, tr_cls: type[GenericTranslator | HTMLTranslator]) -> None:
        tr = pickle.loads(pickle.dumps(tr_cls()))  # noqa: S301
        assert tr.css_to_xpath("a::text") == "descendant-or-self::a/text()"


def test_css2xpath() -> None:
    prefilter = (
        "contains(@class, 'some-class') and "
        if CSSSELECT_VERSION >= Version("1.6.0")
        else ""
    )
    expected_xpath = (
        f"descendant-or-self::*[@class and {prefilter}contains("
        "concat(' ', normalize-space(@class), ' '), ' some-class ')]"
    )
    assert css2xpath(".some-class") == expected_xpath


def test_xpathexpr_join_wrong_type() -> None:
    expr = XPathExpr(path="foo")
    with pytest.raises(ValueError, match="can only join expressions"):
        expr.join("/", OriginalXPathExpr(path="bar"))


class TestCSSSelector:
    sel = Selector(text=HTMLBODY)

    def x(self, *a: Any, **kw: Any) -> list[str]:
        return [v.strip() for v in self.sel.css(*a, **kw).extract() if v.strip()]

    def test_selector_simple(self) -> None:
        for x in self.sel.css("input"):
            assert isinstance(x, self.sel.__class__), x
        assert self.sel.css("input").extract() == [
            x.extract() for x in self.sel.css("input")
        ]

    def test_text_pseudo_element(self) -> None:
        assert self.x("#p-b2") == ['<b id="p-b2">guy</b>']
        assert self.x("#p-b2::text") == ["guy"]
        assert self.x("#p-b2 ::text") == ["guy"]
        assert self.x("#paragraph::text") == ["lorem ipsum text"]
        assert self.x("#paragraph ::text") == ["lorem ipsum text", "hi", "there", "guy"]
        assert self.x("p::text") == ["lorem ipsum text"]
        assert self.x("p ::text") == ["lorem ipsum text", "hi", "there", "guy"]

    def test_attribute_function(self) -> None:
        assert self.x("#p-b2::attr(id)") == ["p-b2"]
        assert self.x(".cool-footer::attr(class)") == ["cool-footer"]
        assert self.x(".cool-footer ::attr(id)") == ["foobar-div", "foobar-span"]
        assert self.x('map[name="dummymap"] ::attr(shape)') == ["circle", "default"]

    def test_nested_selector(self) -> None:
        assert self.sel.css("p").css("b::text").extract() == ["hi", "guy"]
        assert self.sel.css("div").css("area:last-child").extract() == [
            '<area shape="default" id="area-nohref">'
        ]

    def test_pseudoclass_has(self) -> None:
        assert self.x("p:has(b)::text") == ["lorem ipsum text"]


class TestCSSSelectorBytes(TestCSSSelector):
    sel = Selector(body=bytes(HTMLBODY, encoding="utf-8"))


NTH_OF_BODY = """
<html><body>
<ul id="first">
  <li class="ad">ad 1</li>
  <li class="item" data-tags="a,b">item 1</li>
  <li class="separator">---</li>
  <li class="item promo">item 2</li>
  <li class="item">item 3</li>
  <li class="item,x" data-tags="a)b">item 4</li>
</ul>
<ul id="second">
  <li class="ad">ad 2</li>
  <li class="item">item 5</li>
  <li class="item">item 6</li>
</ul>
<p>
  <span class="item">span 1</span>
  <span class="item">span 2</span>
</p>
</body></html>
"""


class TestNthChildOf:
    sel = Selector(text=NTH_OF_BODY)

    def texts(self, query: str) -> list[str]:
        return [v.strip() for v in self.sel.css(query).getall()]

    @pytest.mark.parametrize(
        ("query", "expected"),
        [
            # Positional counting only over the siblings that match ``of``.
            (
                "li:nth-child(1 of .item)::text",
                ["item 1", "item 5"],
            ),
            (
                "li:nth-child(2 of .item)::text",
                ["item 2", "item 6"],
            ),
            (
                "li:nth-child(3 of .item)::text",
                ["item 3"],
            ),
            (
                "li:nth-last-child(1 of .item)::text",
                ["item 3", "item 6"],
            ),
            (
                "li:nth-last-child(2 of .item)::text",
                ["item 2", "item 5"],
            ),
            # An+B, odd/even, constants and negative steps.
            (
                "li:nth-child(odd of .item)::text",
                ["item 1", "item 3", "item 5"],
            ),
            (
                "li:nth-child(even of .item)::text",
                ["item 2", "item 6"],
            ),
            (
                "li:nth-child(2n+1 of .item)::text",
                ["item 1", "item 3", "item 5"],
            ),
            (
                "li:nth-child(-n+2 of .item)::text",
                ["item 1", "item 2", "item 5", "item 6"],
            ),
            (
                "li:nth-child(-2n+5 of .item)::text",
                ["item 1", "item 3", "item 5"],
            ),
            (
                "li:nth-last-child(-n+2 of .item)::text",
                ["item 2", "item 3", "item 5", "item 6"],
            ),
            (
                "li:nth-child(n+2 of .item)::text",
                ["item 2", "item 3", "item 6"],
            ),
            (
                "li:nth-child(0 of .item)::text",
                [],
            ),
            (
                "li:nth-child(-1 of .item)::text",
                [],
            ),
            # A selector list: overlapping matches count once.
            (
                "li:nth-child(2 of .item, .promo)::text",
                ["item 2", "item 6"],
            ),
            (
                "li:nth-child(1 of .item, .item)::text",
                ["item 1", "item 5"],
            ),
            # The keyword is case-insensitive.
            (
                "li:nth-child(2 OF .item)::text",
                ["item 2", "item 6"],
            ),
            # The legacy form keeps working.
            (
                "li:nth-child(2)::text",
                ["item 1", "item 5"],
            ),
            (
                "li:nth-last-child(1)::text",
                ["item 4", "item 6"],
            ),
            # The list can be a complex selector, and matched nodes from
            # different parents are counted separately.
            (
                "li:nth-child(1 of ul > .item)::text",
                ["item 1", "item 5"],
            ),
            (
                "#second > li:nth-child(2 of .item)::text",
                ["item 6"],
            ),
            (
                "p > span:nth-child(2 of .item)::text",
                ["span 2"],
            ),
        ],
    )
    def test_nth_of(self, query: str, expected: list[str]) -> None:
        assert self.texts(query) == expected

    def test_overlapping_list_counts_once(self) -> None:
        # item 2 is matched by both selectors; if it counted twice, the
        # third *and* fourth matching sibling would be off by one.
        assert self.texts("li:nth-child(3 of .item, .promo)::text") == ["item 3"]
        assert self.texts("li:nth-child(2 of .item, li.promo)::text") == [
            "item 2",
            "item 6",
        ]

    @pytest.mark.parametrize(
        ("query", "expected"),
        [
            # Commas and parentheses in attribute values are not separators.
            ('li:nth-child(1 of [data-tags="a,b"])::text', ["item 1"]),
            ("li:nth-child(1 of [data-tags='a)b'])::text", ["item 4"]),
            # An escaped comma is part of an identifier, not a separator.
            (r"li:nth-child(1 of .item\,x)::text", ["item 4"]),
            # A comment is not syntax.
            ("li:nth-child(1 of .item /* of .ad, */)::text", ["item 1", "item 5"]),
        ],
    )
    def test_strings_and_escapes(self, query: str, expected: list[str]) -> None:
        assert self.texts(query) == expected

    @pytest.mark.parametrize(
        ("query", "exc"),
        [
            pytest.param(
                "li:nth-of-type(1 of .item)",
                ExpressionError,
                id="nth-of-type",
            ),
            pytest.param(
                "li:nth-last-of-type(1 of .item)",
                ExpressionError,
                id="nth-last-of-type",
            ),
            pytest.param(
                "li:nth-child(1 of )",
                ExpressionError,
                id="empty-list",
            ),
            pytest.param(
                "li:nth-child(bogus of .item)",
                ExpressionError,
                id="bad-series",
            ),
            pytest.param(
                "li:nth-child(1 of .item::text)",
                ExpressionError,
                id="pseudo-element",
            ),
            pytest.param(
                "li:nth-child(1 of :scope > .item)",
                ExpressionError,
                id="scope",
            ),
            pytest.param(
                "li:nth-child(1 of > .item)",
                Exception,
                id="relative-selector",
            ),
        ],
    )
    def test_rejected(self, query: str, exc: type[Exception]) -> None:
        with pytest.raises(exc):
            self.sel.css(query)

    def test_rejected_before_running(self) -> None:
        """Unsupported combinations raise at translation time, i.e. they do
        not depend on the document contents."""
        empty = Selector(text="<html><body></body></html>")
        with pytest.raises(ExpressionError):
            empty.css("li:nth-child(1 of .item::text)")

    def test_pseudo_elements_keep_working(self) -> None:
        assert self.sel.css("li:nth-child(2 of .item)::attr(class)").getall() == [
            "item promo",
            "item",
        ]
        assert self.sel.css("li:nth-last-child(1 of .item)::text").getall() == [
            "item 3",
            "item 6",
        ]

    @pytest.mark.parametrize("tr_cls", [GenericTranslator, HTMLTranslator])
    def test_plain_nth_child_unchanged(
        self, tr_cls: type[GenericTranslator | HTMLTranslator]
    ) -> None:
        assert (
            tr_cls().css_to_xpath("li:nth-child(2n+1)")
            == "descendant-or-self::li[count(preceding-sibling::*) mod 2 = 0]"
        )
        assert (
            tr_cls().css_to_xpath("li:nth-last-child(2n+1)")
            == "descendant-or-self::li[count(following-sibling::*) mod 2 = 0]"
        )

    @pytest.mark.parametrize("query", ["li:nth-child(2)", "li:nth-last-child(2)"])
    def test_plain_nth_child_same_results(self, query: str) -> None:
        plain = self.sel.css(query)
        all_elements = self.sel.css(query.replace("nth-child(2)", "nth-child(2 of *)"))
        assert all_elements.getall() == plain.getall()

    def test_nested_of(self) -> None:
        body = """<ul>
<li class="a">1</li><li class="b">2</li><li class="a">3</li><li class="a">4</li>
</ul>"""
        sel = Selector(text=body)
        # The list of "of" can itself contain an "of" call.
        assert [
            x.get() for x in sel.css("li:nth-child(1 of :nth-child(2n of .a))")
        ] == ['<li class="a">3</li>']
        # Several "of" calls can appear in one query.
        assert [
            x.get()
            for x in sel.css("li:nth-child(1 of .a), li:nth-last-child(1 of .a)")
        ] == ['<li class="a">1</li>', '<li class="a">4</li>']

    def test_inside_other_pseudo_classes(self) -> None:
        assert self.texts("li:not(:nth-child(2 of .item))::text")[:3] == [
            "ad 1",
            "item 1",
            "---",
        ]
        assert self.texts("li:is(:nth-child(2 of .item))::text") == [
            "item 2",
            "item 6",
        ]

    def test_of_in_attribute_string_is_not_a_keyword(self) -> None:
        body = '<ul><li data-x=":nth-child(2 of .a)">x</li></ul>'
        sel = Selector(text=body)
        assert sel.css('li[data-x=":nth-child(2 of .a)"]').getall() == [
            '<li data-x=":nth-child(2 of .a)">x</li>'
        ]

    def test_translation_cache(self) -> None:
        tr = HTMLTranslator()
        first = tr.css_to_xpath("li:nth-child(1 of .item)")
        assert first == tr.css_to_xpath("li:nth-child(1 of .item)")
        assert tr._cache.cache_info().hits == 1

    def test_xml_namespaces(self) -> None:
        body = """<?xml version="1.0" encoding="UTF-8"?>
<root xmlns:h="http://example.com/h">
  <h:item class="a">1</h:item>
  <h:item class="b">2</h:item>
  <h:item class="a">3</h:item>
  <h:item class="a">4</h:item>
</root>"""
        sel = Selector(text=body, type="xml")
        sel.register_namespace("h", "http://example.com/h")
        assert [x.get() for x in sel.css("h|item:nth-child(2 of .a)")] == [
            '<h:item xmlns:h="http://example.com/h" class="a">3</h:item>'
        ]
        assert [x.get() for x in sel.css("h|item:nth-last-child(1 of .a)")] == [
            '<h:item xmlns:h="http://example.com/h" class="a">4</h:item>'
        ]
        assert [x.get() for x in sel.css("h|item:nth-child(1 of h|item.a)")] == [
            '<h:item xmlns:h="http://example.com/h" class="a">1</h:item>'
        ]

    def test_different_parents_counted_separately(self) -> None:
        body = """<div>
<ul id="u1"><li class="a">1</li><li class="a">2</li><li class="a">3</li></ul>
<ul id="u2"><li class="a">4</li><li class="a">5</li></ul>
</div>"""
        sel = Selector(text=body)
        assert [x.get() for x in sel.css("ul > li:nth-child(2 of .a)")] == [
            '<li class="a">2</li>',
            '<li class="a">5</li>',
        ]
        assert [x.get() for x in sel.css("ul > li:nth-last-child(2 of .a)")] == [
            '<li class="a">2</li>',
            '<li class="a">4</li>',
        ]


@pytest.mark.parametrize("type_", ["html", "xml"])
def test_nth_of_matches_reference(type_: str) -> None:
    """Compare against a plain-Python implementation on documents with
    interleaved separators, advertisements and other noise nodes."""
    tags = ["li", "p", "span"]
    classes = ["a", "b", "c"]
    rng = random.Random(0)  # noqa: S311

    def matches(
        index: int, tag_list: list[str], class_list: list[set[str]], name: str
    ) -> bool:
        if name == "*":
            return True
        if name.startswith("."):
            return name[1:] in class_list[index]
        return tag_list[index] == name

    for _ in range(200):
        size = rng.randint(0, 7)
        tag_list = [rng.choice(tags) for _ in range(size)]
        class_list = [set(rng.sample(classes, rng.randint(0, 2))) for _ in range(size)]
        markup = "".join(
            f'<{tag} class="{" ".join(sorted(cls))}">x</{tag}>'
            if cls
            else f"<{tag}>x</{tag}>"
            for tag, cls in zip(tag_list, class_list, strict=True)
        )
        sel = Selector(text=f"<div>{markup}</div>", type=type_)

        names = [
            rng.choice(["*", ".a", ".b", ".c", "li", "p", "span"])
            for _ in range(rng.randint(1, 3))
        ]
        a = rng.choice([0, 1, 2, 3, -1, -2, -3])
        b = rng.randint(-3, 6)
        last = rng.choice([True, False])

        if a == 0:
            series = str(b)
        elif a == 1:
            series = f"n{b:+d}"
        elif a == -1:
            series = f"-n{b:+d}"
        else:
            series = f"{a}n{b:+d}"

        pseudo = "nth-last-child" if last else "nth-child"
        query = f"div > *:{pseudo}({series} of {', '.join(names)})"
        result = [x.get() for x in sel.css(query)]

        matching = [
            i
            for i in range(size)
            if any(matches(i, tag_list, class_list, name) for name in names)
        ]
        ordered = matching[::-1] if last else matching
        expected_indexes = []
        for position, index in enumerate(ordered, start=1):
            if a == 0:
                if position != b:
                    continue
            else:
                difference = position - b
                if difference % a or difference // a < 0:
                    continue
            expected_indexes.append(index)
        expected_indexes.sort()
        expected = [
            f'<{tag_list[i]} class="{" ".join(sorted(class_list[i]))}">x</{tag_list[i]}>'
            if class_list[i]
            else f"<{tag_list[i]}>x</{tag_list[i]}>"
            for i in expected_indexes
        ]
        assert result == expected, query
