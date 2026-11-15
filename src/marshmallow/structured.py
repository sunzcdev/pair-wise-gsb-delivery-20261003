"""Typed, structured representation of validation errors.

The :class:`~marshmallow.exceptions.ValidationError` ``messages`` structure
only keeps strings and integers as container keys. The type information that
produced those keys is lost: an integer can equally be a ``List``/``Tuple``
position or a real ``Dict`` key, and the literal ``"key"``/``"value"`` markers
used for mapping entries are indistinguishable from ordinary field names.

This module keeps that information as a list of :class:`PathSegment` objects.
Every segment carries an explicit ``kind`` so the path can never be guessed
from the final, flattened string representation.

JSON object keys are always strings, so arbitrary mapping keys (integers,
tuples, etc.) cannot be represented directly. :func:`encode_key` wraps such
keys in an unambiguous, type-tagged envelope instead of coercing them with
``str()`` (which would make distinct keys collide).
"""

from __future__ import annotations

import dataclasses
import typing

if typing.TYPE_CHECKING:
    from marshmallow.types import ErrorMessageValue

__all__ = [
    "PathSegment",
    "StructuredError",
    "encode_key",
    "to_jsonable",
]

#: Segment kind for a field selected on a schema (by its data key).
FIELD = "field"
#: Segment kind for a positional element of a ``List``.
INDEX = "index"
#: Segment kind for a positional element of a ``Tuple``.
POSITION = "position"
#: Segment kind for a key of a mapping (``Dict``).
MAPPING_KEY = "mapping_key"
#: Segment kind for the value associated with a mapping key.
MAPPING_VALUE = "mapping_value"
#: Segment kind for a schema-level (``_schema``) error.
SCHEMA = "schema"


@dataclasses.dataclass(frozen=True, slots=True)
class PathSegment:
    """A single, typed step in the location of a validation error.

    Use the named constructors rather than instantiating this class directly so
    that every segment carries an explicit, machine-readable ``kind``.
    """

    kind: str
    value: typing.Any = None

    @classmethod
    def field(cls, name: str) -> "PathSegment":
        """A field selected on a schema (by its data key)."""
        return cls(FIELD, name)

    @classmethod
    def index(cls, position: int) -> "PathSegment":
        """A positional element of a :class:`~marshmallow.fields.List`."""
        return cls(INDEX, position)

    @classmethod
    def position(cls, position: int) -> "PathSegment":
        """A positional element of a :class:`~marshmallow.fields.Tuple`."""
        return cls(POSITION, position)

    @classmethod
    def mapping_key(cls, key: typing.Any) -> "PathSegment":
        """A key of a mapping (:class:`~marshmallow.fields.Dict`)."""
        return cls(MAPPING_KEY, key)

    @classmethod
    def mapping_value(cls) -> "PathSegment":
        """The value associated with a mapping key."""
        return cls(MAPPING_VALUE, None)

    @classmethod
    def schema(cls) -> "PathSegment":
        """A schema-level error (stored under ``_schema``)."""
        return cls(SCHEMA, None)

    @property
    def is_mapping_key(self) -> bool:
        return self.kind == MAPPING_KEY

    def to_jsonable(self) -> dict[str, typing.Any]:
        return {"kind": self.kind, "value": encode_key(self.value)}


@dataclasses.dataclass(frozen=True, slots=True)
class StructuredError:
    """A validation error located by a typed ``path``.

    :param path: Ordered, typed segments locating the error. An empty path
        means a top-level schema error.
    :param messages: The message subtree found at ``path``: a string, a list
        of messages, or a (possibly nested) mapping of further sub-errors.
    """

    path: tuple[PathSegment, ...]
    messages: ErrorMessageValue

    def to_jsonable(self) -> dict[str, typing.Any]:
        return {
            "path": [segment.to_jsonable() for segment in self.path],
            "messages": to_jsonable(self.messages),
        }


def encode_key(key: typing.Any) -> typing.Any:
    """Return a JSON-safe representation of a mapping key.

    Strings, ``None`` and booleans/numbers are representable as JSON values
    and are returned untouched. Any other key (tuple, custom object, ...) is
    wrapped in a tagged envelope carrying both its type name and a
    best-effort encoding, so distinct non-string keys can never collapse into
    the same ``str()`` representation::

        {"__marshmallow_key__": True, "type": "tuple", "value": [...]}
    """
    if key is None or isinstance(key, (str, int, float, bool)):
        return key
    return {
        "__marshmallow_key__": True,
        "type": type(key).__name__,
        "value": to_jsonable(key),
    }


def to_jsonable(value: typing.Any) -> typing.Any:
    """Recursively convert error data into JSON-serializable primitives.

    Mappings whose keys are all JSON-compatible object keys (strings) keep
    their dict shape. A mapping containing any other key is encoded as an
    ordered list of ``{"key": ..., "value": ...}`` pairs so key type
    information is preserved instead of coercing every key to a string.
    """
    if isinstance(value, PathSegment):
        return value.to_jsonable()
    if isinstance(value, StructuredError):
        return value.to_jsonable()
    if isinstance(value, dict):
        if all(isinstance(key, str) for key in value):
            return {key: to_jsonable(val) for key, val in value.items()}
        return [
            {"key": encode_key(key), "value": to_jsonable(val)}
            for key, val in value.items()
        ]
    if isinstance(value, (list, tuple)):
        return [to_jsonable(item) for item in value]
    if isinstance(value, (str, int, float, bool)) or value is None:
        return value
    return {
        "__marshmallow_key__": True,
        "type": type(value).__name__,
        "value": repr(value),
    }
