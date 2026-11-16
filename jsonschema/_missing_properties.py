"""
A projection of validation errors onto the individual missing members.

Errors emitted by the ``required`` and ``dependentRequired`` keywords point
at the object which is missing a property, since there is no instance element
to attach a missing property to. This module provides an *independent view*
over such errors which is addressable per missing member, suitable for
interfaces that need to highlight or fix each missing field separately.

The projection never mutates the original `ValidationError` objects: the
errors produced by iterating a validator (and therefore ``best_match``) are
left untouched, and every projected entry keeps a reference to the error it
was derived from.
"""

from __future__ import annotations

from collections.abc import Iterable, Iterator, Mapping
from typing import TYPE_CHECKING
import ast
import re

from attrs import define

if TYPE_CHECKING:
    from jsonschema.exceptions import ValidationError

#: Attribute used to attach structured missing-member information to an error.
MISSING_PROPERTY_ATTRIBUTE = "_missing_property"

#: Keywords which combine errors produced by separate subschemas. Applicator
#: errors group the missing members reported by each of their branches.
_APPLICATORS = frozenset(["anyOf", "oneOf", "allOf"])

_REQUIRED_MESSAGE = re.compile(r"^(.*) is a required property$")
_DEPENDENT_REQUIRED_MESSAGE = re.compile(
    r"^(.*) is a dependency of (.*)$",
)


def escape_json_pointer(token: str) -> str:
    """
    Escape a single property name for use in a JSON Pointer reference token.

    Per RFC 6901, ``~`` is escaped as ``~0`` and ``/`` as ``~1``. The
    tilde must be replaced first so that escaping a slash does not produce
    a token which unescapes incorrectly.
    """
    return token.replace("~", "~0").replace("/", "~1")


def to_json_pointer(path: Iterable[str | int]) -> str:
    """
    Render a path through an instance as an RFC 6901 JSON Pointer.

    Integer elements (array indices) and string elements (object keys) are
    rendered identically once joined, but are kept distinct while building
    the pointer so that a string key such as ``"0"`` is never confused with
    an array index. The root instance is represented by the empty string.
    """
    return "".join("/" + escape_json_pointer(str(element)) for element in path)


@define(frozen=True)
class MissingProperty:
    """
    A single missing member projected from a validation error.

    Attributes:

        parent_path:
            the path (relative to the validated instance) of the object
            which is missing the member, as a tuple of string keys and
            integer array indices

        parent_pointer:
            the JSON Pointer to the parent object, with property names
            escaped per RFC 6901

        name:
            the name of the missing member, kept separate from the parent
            location rather than appended to it, since no instance element
            exists at that path

        pointer:
            a JSON Pointer for where the member *would* live if it were
            present; it must not be mistaken for the path of an existing
            instance element

        trigger:
            the property whose presence triggered the requirement when the
            error comes from ``dependentRequired``, or ``None`` for an
            ordinary ``required`` error

        validator:
            the keyword which produced the underlying error
            (``required`` or ``dependentRequired``)

        error:
            the original `ValidationError` this entry was projected from

    """

    parent_path: tuple[str | int, ...]
    parent_pointer: str
    name: str
    pointer: str
    trigger: str | None
    validator: str
    error: ValidationError

    @classmethod
    def _from(cls, error: ValidationError, name: str, trigger: str | None):
        parent_path = tuple(error.absolute_path)
        parent_pointer = to_json_pointer(parent_path)
        return cls(
            parent_path=parent_path,
            parent_pointer=parent_pointer,
            name=name,
            pointer=parent_pointer + "/" + escape_json_pointer(name),
            trigger=trigger,
            validator=error.validator,
            error=error,
        )


@define(frozen=True)
class MissingPropertyBranch:
    """
    Missing members grouped by the subschema branch which reported them.

    Attributes:

        branch:
            the sequence of ``(keyword, subschema index)`` pairs traversed
            through ``anyOf`` / ``oneOf`` / ``allOf`` to reach the errors.
            It is empty for errors reported directly at the root rather
            than inside a combining keyword.

        properties:
            the projected missing members produced within this branch, in
            the order the underlying errors were produced

    """

    branch: tuple[tuple[str, int | None], ...]
    properties: tuple[MissingProperty, ...]

    @property
    def keyword(self) -> str | None:
        """
        The innermost combining keyword surrounding this branch, if any.
        """
        if not self.branch:
            return None
        return self.branch[-1][0]

    @property
    def index(self) -> int | None:
        """
        The innermost subschema index surrounding this branch, if any.
        """
        if not self.branch:
            return None
        return self.branch[-1][1]


def mark_missing_property(
    error: ValidationError,
    name: str,
    trigger: str | None = None,
) -> ValidationError:
    """
    Attach structured missing-member information to a validation error.

    The error is returned for convenience. Only a private attribute is set;
    all of the error's existing attributes (including its path and message)
    are left unchanged.
    """
    setattr(error, MISSING_PROPERTY_ATTRIBUTE, (name, trigger))
    return error


def _as_literal(text: str) -> str | None:
    """
    Parse a repr()-rendered string back into its value, or ``None``.
    """
    try:
        value = ast.literal_eval(text)
    except (SyntaxError, ValueError):
        return None
    return value if isinstance(value, str) else None


def _missing_property_for(
    error: ValidationError,
) -> MissingProperty | None:
    """
    Project a single error, or return ``None`` if it does not report one.
    """
    structured = getattr(error, MISSING_PROPERTY_ATTRIBUTE, None)
    if structured is not None:
        name, trigger = structured
    else:
        name, trigger = _parse_message(error)
        if name is None:
            return None

    # Never claim a member is missing when the parent object actually has it.
    instance = error.instance
    if isinstance(instance, Mapping) and name in instance:
        return None

    return MissingProperty._from(error, name=name, trigger=trigger)


def _parse_message(error: ValidationError) -> tuple[str | None, str | None]:
    """
    Recover the missing member (and any triggering property) from a message.

    This fallback supports errors built by hand and the pre-2019-09
    ``dependencies`` keyword; errors produced by the library's current
    ``required`` and ``dependentRequired`` validators carry structured
    information and do not rely on message parsing.
    """
    message = error.message

    if error.validator == "required":
        match = _REQUIRED_MESSAGE.match(message)
        if match is None:
            return None, None
        return _as_literal(match.group(1)), None

    if error.validator in ("dependentRequired", "dependencies"):
        match = _DEPENDENT_REQUIRED_MESSAGE.match(message)
        if match is None:
            return None, None
        name = _as_literal(match.group(1))
        trigger = _as_literal(match.group(2))
        if name is None or trigger is None:
            return None, None
        return name, trigger

    return None, None


def _branch_index(error: ValidationError) -> int | None:
    """
    The subschema index an error carries relative to its parent applicator.

    Sub-errors within ``anyOf`` / ``oneOf`` / ``allOf`` have the branch's
    position as the first element of their relative schema path. Hand-built
    errors may lack a schema path, in which case the index is unknown.
    """
    index = error.schema_path[0] if error.schema_path else None
    return index if isinstance(index, int) else None


def _walk(
    errors: Iterable[ValidationError],
    branch: tuple[tuple[str, int | None], ...],
    groups: dict[tuple[tuple[str, int | None], ...], list[MissingProperty]],
) -> None:
    for error in errors:
        projected = _missing_property_for(error)
        if projected is not None:
            groups.setdefault(branch, []).append(projected)

        if not error.context:
            continue

        if error.validator in _APPLICATORS:
            for child in error.context:
                child_branch = (
                    *branch,
                    (error.validator, _branch_index(child)),
                )
                _walk([child], child_branch, groups)
        else:
            _walk(error.context, branch, groups)


def project_missing_properties(
    errors: Iterable[ValidationError],
) -> list[MissingPropertyBranch]:
    """
    Project missing-member errors, grouped by the branch which produced them.

    Errors from ``required`` and ``dependentRequired`` are turned into one
    `MissingProperty` per missing member. Errors nested inside a combining
    keyword (``anyOf``, ``oneOf`` or ``allOf``) are grouped per subschema
    branch; errors reported outside of any combining keyword form a single
    group with an empty branch path.

    The input errors are not modified, and their original iteration order
    within each branch is preserved.
    """
    groups: dict[
        tuple[tuple[str, int | None], ...],
        list[MissingProperty],
    ] = {}
    _walk(errors, (), groups)
    return [
        MissingPropertyBranch(branch=branch, properties=tuple(properties))
        for branch, properties in groups.items()
    ]


def iter_missing_properties(
    errors: Iterable[ValidationError],
) -> Iterator[MissingProperty]:
    """
    Yield every projected missing member, flattened across all branches.
    """
    for group in project_missing_properties(errors):
        yield from group.properties
