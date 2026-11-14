from __future__ import annotations

import pytest

from parsel import (
    Field,
    FieldDefinitionError,
    FieldProblem,
    FieldResult,
    ProblemCode,
    RecordResult,
    RecordSchema,
    Selector,
)

HTML = """
<html>
  <body>
    <div class="record" id="r1">
      <h1>First</h1>
      <a href="https://example.com/1">link</a>
      <span class="tag">x</span>
      <span class="tag">y</span>
      <a class="empty" href="">empty link</a>
    </div>
    <div class="record" id="r2">
      <h1>Second</h1>
      <span class="tag">z</span>
    </div>
  </body>
</html>
"""


def first_record() -> Selector:
    return Selector(text=HTML).css("div.record")[0]


class TestFieldDefinition:
    def test_requires_exactly_one_query(self) -> None:
        with pytest.raises(FieldDefinitionError):
            Field()
        with pytest.raises(FieldDefinitionError):
            Field(css="h1", xpath="//h1")
        with pytest.raises(FieldDefinitionError):
            Field(css="h1", jmespath="h1")

    def test_invalid_css_rejected_before_scanning(self) -> None:
        with pytest.raises(FieldDefinitionError, match="Invalid CSS query"):
            Field(css="div[")

    def test_invalid_xpath_rejected_before_scanning(self) -> None:
        with pytest.raises(FieldDefinitionError, match="Invalid XPath query"):
            Field(xpath="//div[")

    def test_invalid_jmespath_rejected_before_scanning(self) -> None:
        with pytest.raises(FieldDefinitionError, match="Invalid JMESPath query"):
            Field(jmespath="foo[")

    def test_invalid_css_rejected_by_schema(self) -> None:
        with pytest.raises(FieldDefinitionError):
            RecordSchema({"bad": Field(css="::bogus")})

    def test_query_attributes(self) -> None:
        field = Field(xpath=".//h1/text()", many=True, default="n/a")
        assert field.query_type == "xpath"
        assert field.query == ".//h1/text()"
        assert field.many is True
        assert field.has_default is True
        assert Field(css="h1").has_default is False

    def test_non_string_query_rejected(self) -> None:
        with pytest.raises(FieldDefinitionError, match="must be a string"):
            Field(css=123)  # type: ignore[arg-type]


class TestSchemaDefinition:
    def test_non_field_value_rejected(self) -> None:
        with pytest.raises(TypeError, match="must be a Field instance"):
            RecordSchema({"a": "h1"})  # type: ignore[dict-item]

    def test_invalid_on_error_rejected(self) -> None:
        with pytest.raises(ValueError, match="on_error"):
            RecordSchema({}, on_error="ignore")  # type: ignore[arg-type]

    def test_accepts_iterable_of_pairs(self) -> None:
        schema = RecordSchema([("a", Field(css="h1::text"))])
        assert list(schema.fields) == ["a"]
        assert schema.on_error == "collect"


class TestExtraction:
    def test_single_css_and_xpath_fields(self) -> None:
        schema = RecordSchema(
            {
                "title": Field(css="h1::text"),
                "href": Field(xpath=".//a[not(@class)]/@href"),
            }
        )
        result = schema.extract(first_record())
        assert isinstance(result, RecordResult)
        assert result.values == {"title": "First", "href": "https://example.com/1"}
        assert result.problems == {}
        assert result.ok
        assert result["title"] == FieldResult(value="First")

    def test_many_field(self) -> None:
        schema = RecordSchema({"tags": Field(css="span.tag::text", many=True)})
        result = schema.extract(first_record())
        assert result.values == {"tags": ["x", "y"]}
        assert result.ok

    def test_missing_single(self) -> None:
        schema = RecordSchema({"price": Field(css="span.price::text")})
        result = schema.extract(first_record())
        assert result.values == {"price": None}
        problems = result.problems["price"]
        assert [p.code for p in problems] == [ProblemCode.MISSING]
        assert not result.ok

    def test_missing_many(self) -> None:
        schema = RecordSchema({"tags": Field(css="b::text", many=True)})
        result = schema.extract(first_record())
        assert result.values == {"tags": []}
        assert [p.code for p in result.problems["tags"]] == [ProblemCode.MISSING]

    def test_missing_with_default(self) -> None:
        schema = RecordSchema({"price": Field(css="span.price::text", default="0")})
        result = schema.extract(first_record())
        assert result.values == {"price": "0"}
        assert [p.code for p in result.problems["price"]] == [ProblemCode.MISSING]

    def test_single_with_multiple_matches_does_not_take_first(self) -> None:
        schema = RecordSchema({"tag": Field(css="span.tag::text")})
        result = schema.extract(first_record())
        assert result.values == {"tag": None}
        assert result.values["tag"] != "x"
        problems = result.problems["tag"]
        assert [p.code for p in problems] == [ProblemCode.MULTIPLE]
        assert "2 nodes" in problems[0].message

    def test_single_with_multiple_matches_uses_default(self) -> None:
        schema = RecordSchema({"tag": Field(css="span.tag::text", default="n/a")})
        result = schema.extract(first_record())
        assert result.values == {"tag": "n/a"}
        assert [p.code for p in result.problems["tag"]] == [ProblemCode.MULTIPLE]

    def test_empty_text(self) -> None:
        schema = RecordSchema({"href": Field(css="a.empty::attr(href)")})
        result = schema.extract(first_record())
        assert result.values == {"href": ""}
        assert [p.code for p in result.problems["href"]] == [ProblemCode.EMPTY]

    def test_empty_item_in_many(self) -> None:
        html = "<div><a href=''>x</a><a href='u'>y</a></div>"
        schema = RecordSchema({"hrefs": Field(css="a::attr(href)", many=True)})
        result = schema.extract(Selector(text=html))
        assert result.values == {"hrefs": ["", "u"]}
        assert [p.code for p in result.problems["hrefs"]] == [ProblemCode.EMPTY]

    def test_json_null_single(self) -> None:
        sel = Selector(text='{"a": [null]}')
        schema = RecordSchema({"a": Field(jmespath="a")})
        result = schema.extract(sel)
        assert result.values == {"a": None}
        assert [p.code for p in result.problems["a"]] == [ProblemCode.NULL]

    def test_json_null_item_in_many(self) -> None:
        sel = Selector(text='{"a": [1, null]}')
        schema = RecordSchema({"a": Field(jmespath="a", many=True)})
        result = schema.extract(sel)
        assert result.values == {"a": [1, None]}
        assert [p.code for p in result.problems["a"]] == [ProblemCode.NULL]

    def test_missing_empty_and_null_are_distinct(self) -> None:
        schema = RecordSchema(
            {
                "missing": Field(css="span.price::text"),
                "empty": Field(css="a.empty::attr(href)"),
            }
        )
        result = schema.extract(first_record())
        codes = {name: [p.code for p in ps] for name, ps in result.problems.items()}
        assert codes == {"missing": ["missing"], "empty": ["empty"]}

        null_result = RecordSchema({"null": Field(jmespath="a")}).extract(
            Selector(text='{"a": [null]}')
        )
        assert [p.code for p in null_result.problems["null"]] == ["null"]

    def test_fields_use_record_node_as_context(self) -> None:
        schema = RecordSchema(
            {
                "title": Field(css="h1::text"),
                "tags": Field(css="span.tag::text", many=True),
            }
        )
        records = Selector(text=HTML).css("div.record")
        results = [schema.extract(record).values for record in records]
        assert results == [
            {"title": "First", "tags": ["x", "y"]},
            {"title": "Second", "tags": ["z"]},
        ]

    def test_field_order_preserved(self) -> None:
        schema = RecordSchema(
            {
                "c": Field(css="h1::text"),
                "a": Field(css="span.tag::text", many=True),
                "b": Field(css="a::attr(href)"),
            }
        )
        result = schema.extract(first_record())
        assert list(result.values) == ["c", "a", "b"]
        assert list(result.fields) == ["c", "a", "b"]
        assert list(schema.fields) == ["c", "a", "b"]

    def test_runtime_error_isolated_by_default(self) -> None:
        # CSS and XPath queries fail on JSON selectors; the failure of one
        # field must not prevent the extraction of the others.
        sel = Selector(text='{"a": 1}')
        schema = RecordSchema(
            {
                "a": Field(jmespath="a"),
                "broken": Field(css="h1::text"),
            }
        )
        result = schema.extract(sel)
        assert result.values == {"a": 1, "broken": None}
        assert "a" not in result.problems
        problems = result.problems["broken"]
        assert [p.code for p in problems] == [ProblemCode.ERROR]
        assert "Cannot use css" in problems[0].message

    def test_runtime_error_uses_default(self) -> None:
        sel = Selector(text='{"a": 1}')
        schema = RecordSchema({"broken": Field(css="h1::text", default="d")})
        result = schema.extract(sel)
        assert result.values == {"broken": "d"}
        assert [p.code for p in result.problems["broken"]] == [ProblemCode.ERROR]

    def test_runtime_error_raise(self) -> None:
        sel = Selector(text='{"a": 1}')
        schema = RecordSchema({"broken": Field(css="h1::text")}, on_error="raise")
        with pytest.raises(ValueError, match="Cannot use css"):
            schema.extract(sel)

    def test_xml_record(self) -> None:
        xml = "<item><name>n</name><tag>x</tag><tag>y</tag></item>"
        schema = RecordSchema(
            {
                "name": Field(xpath="./name/text()"),
                "tags": Field(css="tag::text", many=True),
            }
        )
        result = schema.extract(Selector(text=xml, type="xml"))
        assert result.values == {"name": "n", "tags": ["x", "y"]}
        assert result.ok

    def test_result_problems_skip_clean_fields(self) -> None:
        schema = RecordSchema(
            {
                "title": Field(css="h1::text"),
                "price": Field(css="span.price::text"),
            }
        )
        result = schema.extract(first_record())
        assert list(result.problems) == ["price"]
        assert isinstance(result.problems["price"][0], FieldProblem)
        assert result.problems["price"][0].field == "price"

    def test_old_selector_behavior_unchanged(self) -> None:
        sel = first_record()
        assert sel.css("span.tag::text").get() == "x"
        assert sel.css("span.tag::text").getall() == ["x", "y"]
        assert sel.xpath(".//h1/text()").get() == "First"
        assert sel.css("span.price::text").get() is None
