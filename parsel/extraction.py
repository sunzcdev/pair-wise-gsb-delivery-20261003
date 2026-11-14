"""Declarative extraction of structured records from selectors.

This module provides a small declarative layer on top of
:class:`parsel.selector.Selector`: a :class:`RecordSchema` describes the
fields of a record, and each :class:`Field` says how to extract one field
(with a CSS, XPath or JMESPath query), its cardinality (single value or
list of values) and its default policy. Extracting a schema against a
selector yields a :class:`RecordResult` with the extracted values and one
:class:`FieldProblem` entry per field-level issue, so that e.g. a missing
value, an empty text and a JSON ``null`` are reported as distinct problems.
"""

from __future__ import annotations

import typing
from dataclasses import dataclass
from dataclasses import field as dataclass_field
from typing import TYPE_CHECKING, Any, Literal

import jmespath
from cssselect import SelectorError
from jmespath.exceptions import JMESPathError
from lxml import etree

from .csstranslator import GenericTranslator, HTMLTranslator
from .selector import Selector, SelectorList

if TYPE_CHECKING:
    from collections.abc import Callable, Iterable, Mapping

__all__ = [
    "Field",
    "FieldDefinitionError",
    "FieldProblem",
    "FieldResult",
    "ProblemCode",
    "RecordResult",
    "RecordSchema",
]

_NOT_SET: Any = object()


class FieldDefinitionError(ValueError):
    """Raised when a field definition is invalid.

    Invalid queries are rejected when the field is defined, i.e. before any
    record is scanned.
    """


class ProblemCode:
    """Codes of the field-level problems reported in extraction results."""

    #: The query matched no node.
    MISSING = "missing"
    #: A matched node serialized to an empty string.
    EMPTY = "empty"
    #: A matched node holds a JSON ``null``.
    NULL = "null"
    #: A single-value field matched more than one node.
    MULTIPLE = "multiple"
    #: The query failed at run time.
    ERROR = "error"


@dataclass(frozen=True)
class FieldProblem:
    """A single field-level problem found while extracting a record."""

    #: Name of the field the problem belongs to.
    field: str
    #: One of the :class:`ProblemCode` codes.
    code: str
    #: Human-readable description of the problem.
    message: str


@dataclass
class FieldResult:
    """The extraction outcome of one field: its value and its problems."""

    #: The extracted value, or the field default policy fallback.
    value: Any
    #: Problems found while extracting the field, empty if none.
    problems: list[FieldProblem] = dataclass_field(default_factory=list)

    @property
    def ok(self) -> bool:
        """``True`` if the field was extracted without any problem."""
        return not self.problems


def _validate_css(query: str) -> None:
    try:
        HTMLTranslator().css_to_xpath(query)
        GenericTranslator().css_to_xpath(query)
    except SelectorError as exc:
        raise FieldDefinitionError(f"Invalid CSS query {query!r}: {exc}")


def _validate_xpath(query: str) -> None:
    try:
        etree.XPath(
            query,
            namespaces=dict(Selector._default_namespaces),
            smart_strings=False,
        )
    except etree.XPathError as exc:
        raise FieldDefinitionError(f"Invalid XPath query {query!r}: {exc}")


def _validate_jmespath(query: str) -> None:
    try:
        jmespath.compile(query)
    except JMESPathError as exc:
        raise FieldDefinitionError(f"Invalid JMESPath query {query!r}: {exc}")


_VALIDATORS: dict[str, Callable[[str], None]] = {
    "css": _validate_css,
    "xpath": _validate_xpath,
    "jmespath": _validate_jmespath,
}


class Field:
    """Declarative description of how to extract one record field.

    Exactly one of ``css``, ``xpath`` or ``jmespath`` must be given. CSS and
    XPath queries apply to HTML and XML record nodes, JMESPath queries to
    JSON ones. The query is validated when the field is defined, so an
    invalid query raises :class:`FieldDefinitionError` before any record is
    scanned.

    ``many`` sets the cardinality: ``False`` (the default) for a single
    value, ``True`` for a list of values. A single-value field that matches
    more than one node does not silently keep the first match: a
    ``"multiple"`` problem is recorded and the default policy fallback is
    used as value.

    ``default`` is the value used when the query matches nothing, when a
    single-value field matches too many nodes, or when the query fails at
    run time under ``on_error="collect"``. When not set, the fallback is
    ``None`` for single-value fields and ``[]`` for multi-value ones.
    """

    __slots__ = ("css", "default", "jmespath", "many", "xpath")

    def __init__(
        self,
        *,
        css: str | None = None,
        xpath: str | None = None,
        jmespath: str | None = None,
        many: bool = False,
        default: Any = _NOT_SET,
    ) -> None:
        queries = {"css": css, "xpath": xpath, "jmespath": jmespath}
        given = {kind: query for kind, query in queries.items() if query is not None}
        if len(given) != 1:
            raise FieldDefinitionError(
                "A field needs exactly one of the css, xpath or jmespath "
                f"arguments, got {len(given)}"
            )
        kind, query = next(iter(given.items()))
        if not isinstance(query, str):
            raise FieldDefinitionError(
                f"The {kind} query of a field must be a string, "
                f"got {type(query).__name__}"
            )
        _VALIDATORS[kind](query)
        self.css = css
        self.xpath = xpath
        self.jmespath = jmespath
        self.many = many
        self.default = default

    @property
    def query_type(self) -> str:
        """The kind of query of the field: ``"css"``, ``"xpath"`` or ``"jmespath"``."""
        if self.css is not None:
            return "css"
        if self.xpath is not None:
            return "xpath"
        return "jmespath"

    @property
    def query(self) -> str:
        """The query of the field."""
        return typing.cast("str", getattr(self, self.query_type))

    @property
    def has_default(self) -> bool:
        """``True`` if a ``default`` value was set for the field."""
        return self.default is not _NOT_SET

    def fallback(self) -> Any:
        """Return the value to use when the field cannot be extracted."""
        if self.default is not _NOT_SET:
            return self.default
        return [] if self.many else None

    def __repr__(self) -> str:
        return (
            f"{type(self).__name__}({self.query_type}={self.query!r}, "
            f"many={self.many!r})"
        )


def _run_query(spec: Field, selector: Selector) -> SelectorList[Selector]:
    if spec.css is not None:
        return selector.css(spec.css)
    if spec.xpath is not None:
        return selector.xpath(spec.xpath)
    assert spec.jmespath is not None
    return selector.jmespath(spec.jmespath)


def _value_problems(name: str, values: list[Any]) -> list[FieldProblem]:
    problems = []
    if any(value is None for value in values):
        problems.append(
            FieldProblem(
                field=name,
                code=ProblemCode.NULL,
                message="matched a JSON null value",
            )
        )
    if any(isinstance(value, str) and not value for value in values):
        problems.append(
            FieldProblem(
                field=name,
                code=ProblemCode.EMPTY,
                message="matched an empty string",
            )
        )
    return problems


class RecordResult:
    """The outcome of extracting a :class:`RecordSchema` from one record node.

    ``fields`` maps field names to :class:`FieldResult` objects, in the
    order in which the fields were defined in the schema.
    """

    __slots__ = ("fields",)

    def __init__(self, fields: dict[str, FieldResult]) -> None:
        self.fields = fields

    @property
    def values(self) -> dict[str, Any]:
        """Field values, in field definition order."""
        return {name: result.value for name, result in self.fields.items()}

    @property
    def problems(self) -> dict[str, list[FieldProblem]]:
        """Field problems, in field definition order, skipping clean fields."""
        return {
            name: result.problems for name, result in self.fields.items() if result.problems
        }

    @property
    def ok(self) -> bool:
        """``True`` if every field was extracted without any problem."""
        return all(result.ok for result in self.fields.values())

    def __getitem__(self, name: str) -> FieldResult:
        return self.fields[name]

    def __repr__(self) -> str:
        return (
            f"{type(self).__name__}(values={self.values!r}, "
            f"problems={self.problems!r})"
        )


class RecordSchema:
    """An ordered set of named fields to extract from each record node.

    ``fields`` is a mapping of field names to :class:`Field` objects (or an
    iterable of ``(name, field)`` pairs); extraction results keep this
    order.

    ``on_error`` controls what happens when a field query fails at run time:
    ``"collect"`` (the default) isolates the failure, recording an
    ``"error"`` problem for that field and continuing with the remaining
    fields, while ``"raise"`` propagates the exception.

    Usage example:

    >>> from parsel import Selector
    >>> schema = RecordSchema(
    ...     {
    ...         "name": Field(css="h1::text"),
    ...         "link": Field(css="a::attr(href)", default=""),
    ...         "tags": Field(css="a.tag::text", many=True),
    ...     }
    ... )
    >>> html = "<h1>Example</h1><a class='tag'>news</a><a class='tag'>docs</a>"
    >>> result = schema.extract(Selector(text=html))
    >>> result.values
    {'name': 'Example', 'link': '', 'tags': ['news', 'docs']}
    >>> result.problems
    {'link': [FieldProblem(field='link', code='missing', message="css query 'a::attr(href)' matched no node")]}
    >>> result.ok
    False
    """

    __slots__ = ("_fields", "_on_error")

    def __init__(
        self,
        fields: Mapping[str, Field] | Iterable[tuple[str, Field]],
        *,
        on_error: Literal["collect", "raise"] = "collect",
    ) -> None:
        self._fields = dict(fields)
        for name, field_spec in self._fields.items():
            if not isinstance(field_spec, Field):
                raise TypeError(
                    f"Field {name!r} must be a Field instance, "
                    f"got {type(field_spec).__name__}"
                )
        if on_error not in ("collect", "raise"):
            raise ValueError(
                f"on_error must be 'collect' or 'raise', got {on_error!r}"
            )
        self._on_error = on_error

    @property
    def fields(self) -> dict[str, Field]:
        """A copy of the ordered mapping of field names to field definitions."""
        return dict(self._fields)

    @property
    def on_error(self) -> str:
        """The run-time error policy: ``"collect"`` or ``"raise"``."""
        return self._on_error

    def extract(self, selector: Selector) -> RecordResult:
        """Extract a record from *selector*, used as context for every field."""
        return RecordResult(
            {
                name: self._extract_field(name, field_spec, selector)
                for name, field_spec in self._fields.items()
            }
        )

    def _extract_field(
        self, name: str, field_spec: Field, selector: Selector
    ) -> FieldResult:
        try:
            matches = _run_query(field_spec, selector)
        except Exception as exc:
            if self._on_error == "raise":
                raise
            return FieldResult(
                value=field_spec.fallback(),
                problems=[
                    FieldProblem(
                        field=name,
                        code=ProblemCode.ERROR,
                        message=(
                            f"{field_spec.query_type} query {field_spec.query!r} "
                            f"failed: {exc}"
                        ),
                    )
                ],
            )
        if not matches:
            return FieldResult(
                value=field_spec.fallback(),
                problems=[
                    FieldProblem(
                        field=name,
                        code=ProblemCode.MISSING,
                        message=(
                            f"{field_spec.query_type} query {field_spec.query!r} "
                            "matched no node"
                        ),
                    )
                ],
            )
        if field_spec.many:
            values: Any = matches.getall()
            return FieldResult(
                value=values, problems=_value_problems(name, values)
            )
        if len(matches) > 1:
            return FieldResult(
                value=field_spec.fallback(),
                problems=[
                    FieldProblem(
                        field=name,
                        code=ProblemCode.MULTIPLE,
                        message=(
                            f"{field_spec.query_type} query {field_spec.query!r} "
                            f"matched {len(matches)} nodes, expected at most one"
                        ),
                    )
                ],
            )
        value = matches[0].get()
        return FieldResult(value=value, problems=_value_problems(name, [value]))
