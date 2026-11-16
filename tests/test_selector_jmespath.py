from __future__ import annotations

from typing import Any, cast

import pytest

from parsel import JSONQuerySource, Selector
from parsel.selector import _NOT_SET


class TestJMESPath:
    def test_json_has_html(self) -> None:
        """Sometimes the information is returned in a json wrapper"""
        data = """
        {
            "content": [
                {
                    "name": "A",
                    "value": "a"
                },
                {
                    "name": {
                        "age": 18
                    },
                    "value": "b"
                },
                {
                    "name": "C",
                    "value": "c"
                },
                {
                    "name": "<a>D</a>",
                    "value": "<div>d</div>"
                }
            ],
            "html": "<div><a>a<br>b</a>c</div><div><a>d</a>e<b>f</b></div>"
        }
        """
        sel = Selector(text=data)
        assert (
            sel.jmespath("html").get()
            == "<div><a>a<br>b</a>c</div><div><a>d</a>e<b>f</b></div>"
        )
        assert sel.jmespath("html").xpath("//div/a/text()").getall() == ["a", "b", "d"]
        assert sel.jmespath("html").css("div > b").getall() == ["<b>f</b>"]
        assert cast("int", sel.jmespath("content").jmespath("name.age").get()) == 18

    def test_html_has_json(self) -> None:
        html_text = """
        <div>
            <h1>Information</h1>
            <content>
            {
              "user": [
                        {
                                  "name": "A",
                                  "age": 18
                        },
                        {
                                  "name": "B",
                                  "age": 32
                        },
                        {
                                  "name": "C",
                                  "age": 22
                        },
                        {
                                  "name": "D",
                                  "age": 25
                        }
              ],
              "total": 4,
              "status": "ok"
            }
            </content>
        </div>
        """
        sel = Selector(text=html_text)
        assert sel.xpath("//div/content/text()").jmespath("user[*].name").getall() == [
            "A",
            "B",
            "C",
            "D",
        ]
        assert sel.xpath("//div/content").jmespath("user[*].name").getall() == [
            "A",
            "B",
            "C",
            "D",
        ]
        assert cast("int", sel.xpath("//div/content").jmespath("total").get()) == 4

    def test_html_without_json(self) -> None:
        sel = Selector(text="<div><content>not json</content><empty/></div>")
        assert sel.xpath("//content/text()").jmespath("a").getall() == []
        assert sel.xpath("//empty").jmespath("a").getall() == []

    def test_jmestpath_with_re(self) -> None:
        html_text = """
            <div>
                <h1>Information</h1>
                <content>
                {
                  "user": [
                            {
                                      "name": "A",
                                      "age": 18
                            },
                            {
                                      "name": "B",
                                      "age": 32
                            },
                            {
                                      "name": "C",
                                      "age": 22
                            },
                            {
                                      "name": "D",
                                      "age": 25
                            }
                  ],
                  "total": 4,
                  "status": "ok"
                }
                </content>
            </div>
            """
        sel = Selector(text=html_text)
        assert sel.xpath("//div/content/text()").jmespath("user[*].name").re(
            r"(\w+)"
        ) == ["A", "B", "C", "D"]
        assert sel.xpath("//div/content").jmespath("user[*].name").re(r"(\w+)") == [
            "A",
            "B",
            "C",
            "D",
        ]

        with pytest.raises(TypeError):
            sel.xpath("//div/content").jmespath("user[*].age").re(r"(\d+)")

        assert sel.xpath("//div/content").jmespath("unavailable").re(r"(\d+)") == []

        assert (
            sel.xpath("//div/content").jmespath("unavailable").re_first(r"(\d+)")
            is None
        )

        assert sel.xpath("//div/content").jmespath("user[*].age.to_string(@)").re(
            r"(\d+)"
        ) == ["18", "32", "22", "25"]

    @pytest.mark.parametrize(
        ("text", "root"),
        [
            ("{}", {}),
            ('{"a": "b"}', {"a": "b"}),
            ("[]", []),
            ('["a"]', ["a"]),
        ],
    )
    def test_json_types(self, text: str, root: Any) -> None:
        for input_type in (None, "json"):
            selector = Selector(text=text, type=input_type, root=_NOT_SET)
            assert selector.type == "json"
            assert selector._text == text
            assert selector.root == root

        selector = Selector(text=None, root=root)
        assert selector.type == "json"
        assert selector._text is None
        assert selector.root == root

    @pytest.mark.parametrize(
        ("text", "root"),
        [
            ('""', ""),
            ("0", 0),
            ("1", 1),
            ("true", True),
            ("false", False),
            ("null", None),
        ],
    )
    def test_json_scalar_types(self, text: str, root: Any) -> None:
        """Scalar JSON values are only handled as JSON on request, since they
        are also valid text and HTML."""
        selector = Selector(text=text, type="json", root=_NOT_SET)
        assert selector.type == "json"
        assert selector._text == text
        assert selector.root == root

        selector = Selector(text=text, root=_NOT_SET)
        assert selector.type == "html"
        # libxml2 may or may not wrap the text into a <p> element.
        assert selector.xpath("string(//body)").get() == text


class TestJMESPathDetail:
    def test_hit(self) -> None:
        sel = Selector(text='{"a": 1}')
        detail = sel.jmespath_detail("a")
        assert detail.query == "a"
        assert detail.backend == "jmespath"
        assert detail.matched is True
        assert detail.match_count == 1
        assert detail.values == [1]
        match = detail.matches[0]
        assert match.value == 1
        assert match.value_type == "number"
        assert match.path is None  # JMESPath cannot provide node paths
        assert match.source_index is None
        assert detail.sources == (JSONQuerySource(index=None, is_json=True),)
        assert detail.non_json_sources == ()

    def test_null_and_miss_are_unknown(self) -> None:
        """JMESPath returns None both for a missing key and for an explicit
        JSON null, so both cases must be reported as unknown, not as a
        fabricated hit or miss."""
        sel = Selector(text='{"a": null}')
        for query in ("a", "b"):
            detail = sel.jmespath_detail(query)
            assert detail.matched is None
            assert detail.match_count == 0
            assert detail.values == []
            assert detail.sources[0].is_json is True

    @pytest.mark.parametrize(
        ("text", "value", "value_type"),
        [
            ('{"a": false}', False, "boolean"),
            ('{"a": 0}', 0, "number"),
            ('{"a": ""}', "", "string"),
            ('{"a": []}', [], "array"),
        ],
    )
    def test_falsy_values_kept(
        self, text: str, value: Any, value_type: str
    ) -> None:
        """Falsy values must not be dropped by truthiness checks."""
        detail = Selector(text=text).jmespath_detail("a")
        assert detail.matched is True
        assert detail.match_count == 1
        assert detail.values == [value]
        assert detail.matches[0].value_type == value_type

    def test_list_result_flattened(self) -> None:
        detail = Selector(text='{"a": [1, "x", null]}').jmespath_detail("a")
        assert detail.matched is True
        assert detail.match_count == 3
        assert detail.values == [1, "x", None]
        assert [m.value_type for m in detail.matches] == ["number", "string", "null"]

    def test_object_result(self) -> None:
        detail = Selector(text='{"a": {"b": 1}}').jmespath_detail("a")
        assert detail.matched is True
        assert detail.values == [{"b": 1}]
        assert detail.matches[0].value_type == "object"

    def test_not_json(self) -> None:
        """Non-JSON input is diagnosed separately from a legitimate empty
        result."""
        sel = Selector(text="<html><body><p>Not JSON</p></body></html>")
        detail = sel.jmespath_detail("a")
        assert detail.matched is None
        assert detail.match_count == 0
        assert detail.sources == (JSONQuerySource(index=None, is_json=False),)
        assert detail.non_json_sources == (None,)

    def test_json_null_document(self) -> None:
        """A JSON null document is JSON input."""
        sel = Selector(text="null", type="json")
        detail = sel.jmespath_detail("a")
        assert detail.matched is None
        assert detail.sources[0].is_json is True

    def test_invalid_json_document(self) -> None:
        sel = Selector(text="not json", type="json")
        detail = sel.jmespath_detail("a")
        assert detail.matched is None
        assert detail.sources[0].is_json is False

    def test_html_with_json(self) -> None:
        html_text = '<div><content>{"a": 1}</content></div>'
        detail = Selector(text=html_text).xpath("//content").jmespath_detail("a")
        assert detail.matched is True
        assert detail.values == [1]

    def test_selector_list(self) -> None:
        """Matches carry the index of their source selector."""
        sel = Selector(text='[{"a": 1}, {"a": null}, {"b": 2}]')
        detail = sel.jmespath("[*]").jmespath_detail("a")
        assert detail.matched is True
        assert detail.match_count == 1
        assert detail.values == [1]
        assert detail.matches[0].source_index == 0
        assert [s.index for s in detail.sources] == [0, 1, 2]
        assert all(s.is_json for s in detail.sources)

    def test_selector_list_all_unknown(self) -> None:
        sel = Selector(text='[{"a": null}, {"b": 1}]')
        detail = sel.jmespath("[*]").jmespath_detail("a")
        assert detail.matched is None
        assert detail.match_count == 0

    def test_selector_list_not_json_source(self) -> None:
        """A non-JSON input in the list is diagnosed by index, separately
        from legitimate empty results."""
        html_text = (
            '<div><content>{"a": 1}</content><content>not json</content></div>'
        )
        detail = Selector(text=html_text).xpath("//content").jmespath_detail("a")
        assert detail.matched is True
        assert detail.values == [1]
        assert detail.matches[0].source_index == 0
        assert detail.non_json_sources == (1,)
        assert [s.is_json for s in detail.sources] == [True, False]

    def test_selector_list_empty(self) -> None:
        detail = Selector(text="{}").jmespath("b").jmespath_detail("a")
        assert detail.matched is False
        assert detail.match_count == 0
        assert detail.sources == ()
