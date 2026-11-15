"""
Validation errors, and some surrounding helpers.
"""

from __future__ import annotations

from collections import defaultdict, deque
from collections.abc import Mapping
from pprint import pformat
from textwrap import dedent, indent
from typing import TYPE_CHECKING, Any, ClassVar
import re
import warnings

from attrs import define
from referencing.exceptions import Unresolvable as _Unresolvable

from jsonschema import _utils

if TYPE_CHECKING:
    from collections.abc import (
        Callable,
        Iterable,
        MutableMapping,
        Sequence,
    )

    from jsonschema import _types

WEAK_MATCHES: frozenset[str] = frozenset(["anyOf", "oneOf"])
STRONG_MATCHES: frozenset[str] = frozenset()

_JSON_PATH_COMPATIBLE_PROPERTY_PATTERN = re.compile("^[a-zA-Z][a-zA-Z0-9_]*$")

_unset = _utils.Unset()


def _pretty(thing: Any, prefix: str):
    """
    Format something for an error message as prettily as we currently can.
    """
    return indent(pformat(thing, width=72, sort_dicts=False), prefix).lstrip()


def __getattr__(name):
    if name == "RefResolutionError":
        warnings.warn(
            _RefResolutionError._DEPRECATION_MESSAGE,
            DeprecationWarning,
            stacklevel=2,
        )
        return _RefResolutionError
    raise AttributeError(f"module {__name__} has no attribute {name}")


class _Error(Exception):
    _word_for_schema_in_error_message: ClassVar[str]
    _word_for_instance_in_error_message: ClassVar[str]

    def __init__(
        self,
        message: str,
        validator: str = _unset,  # type: ignore[assignment]
        path: Iterable[str | int] = (),
        cause: Exception | None = None,
        context=(),
        validator_value: Any = _unset,
        instance: Any = _unset,
        schema: Mapping[str, Any] | bool = _unset,  # type: ignore[assignment]
        schema_path: Iterable[str | int] = (),
        parent: _Error | None = None,
        type_checker: _types.TypeChecker = _unset,  # type: ignore[assignment]
    ) -> None:
        super().__init__(
            message,
            validator,
            path,
            cause,
            context,
            validator_value,
            instance,
            schema,
            schema_path,
            parent,
        )
        self.message = message
        self.path = self.relative_path = deque(path)
        self.schema_path = self.relative_schema_path = deque(schema_path)
        self.context = list(context)
        self.cause = self.__cause__ = cause
        self.validator = validator
        self.validator_value = validator_value
        self.instance = instance
        self.schema = schema
        self.parent = parent
        self._type_checker = type_checker

        for error in context:
            error.parent = self

    def __repr__(self) -> str:
        return f"<{self.__class__.__name__}: {self.message!r}>"

    def __str__(self) -> str:
        essential_for_verbose = (
            self.validator,
            self.validator_value,
            self.instance,
            self.schema,
        )
        if any(m is _unset for m in essential_for_verbose):
            return self.message

        schema_path = _utils.format_as_index(
            container=self._word_for_schema_in_error_message,
            indices=list(self.relative_schema_path)[:-1],
        )
        instance_path = _utils.format_as_index(
            container=self._word_for_instance_in_error_message,
            indices=self.relative_path,
        )
        prefix = 16 * " "

        return dedent(
            f"""\
            {self.message}

            Failed validating {self.validator!r} in {schema_path}:
                {_pretty(self.schema, prefix=prefix)}

            On {instance_path}:
                {_pretty(self.instance, prefix=prefix)}
            """.rstrip(),
        )

    @classmethod
    def create_from(cls, other: _Error):
        return cls(**other._contents())

    @property
    def absolute_path(self) -> Sequence[str | int]:
        parent = self.parent
        if parent is None:
            return self.relative_path

        path = deque(self.relative_path)
        path.extendleft(reversed(parent.absolute_path))
        return path

    @property
    def absolute_schema_path(self) -> Sequence[str | int]:
        parent = self.parent
        if parent is None:
            return self.relative_schema_path

        path = deque(self.relative_schema_path)
        path.extendleft(reversed(parent.absolute_schema_path))
        return path

    @property
    def json_path(self) -> str:
        path = "$"
        for elem in self.absolute_path:
            if isinstance(elem, int):
                path += "[" + str(elem) + "]"
            elif _JSON_PATH_COMPATIBLE_PROPERTY_PATTERN.match(elem):
                path += "." + elem
            else:
                escaped_elem = elem.replace("\\", "\\\\").replace("'", r"\'")
                path += "['" + escaped_elem + "']"
        return path

    def _set(
        self,
        type_checker: _types.TypeChecker | None = None,
        **kwargs: Any,
    ) -> None:
        if type_checker is not None and self._type_checker is _unset:
            self._type_checker = type_checker

        for k, v in kwargs.items():
            if getattr(self, k) is _unset:
                setattr(self, k, v)

    def _contents(self):
        attrs = (
            "message",
            "cause",
            "context",
            "validator",
            "validator_value",
            "path",
            "schema_path",
            "instance",
            "schema",
            "parent",
        )
        return {attr: getattr(self, attr) for attr in attrs}

    def _matches_type(self) -> bool:
        try:
            # We ignore this as we want to simply crash if this happens
            expected = self.schema["type"]  # type: ignore[index]
        except (KeyError, TypeError):
            return False

        if isinstance(expected, str):
            return self._type_checker.is_type(self.instance, expected)

        return any(
            self._type_checker.is_type(self.instance, expected_type)
            for expected_type in expected
        )


class ValidationError(_Error):
    """
    An instance was invalid under a provided schema.
    """

    _word_for_schema_in_error_message = "schema"
    _word_for_instance_in_error_message = "instance"


class SchemaError(_Error):
    """
    A schema was invalid under its corresponding metaschema.
    """

    _word_for_schema_in_error_message = "metaschema"
    _word_for_instance_in_error_message = "schema"


@define(slots=False)
class _RefResolutionError(Exception):  # noqa: PLW1641
    """
    A ref could not be resolved.
    """

    _DEPRECATION_MESSAGE = (
        "jsonschema.exceptions.RefResolutionError is deprecated as of version "
        "4.18.0. If you wish to catch potential reference resolution errors, "
        "directly catch referencing.exceptions.Unresolvable."
    )

    _cause: Exception

    def __eq__(self, other):
        if self.__class__ is not other.__class__:
            return (
                NotImplemented  # pragma: no cover -- uncovered but deprecated
            )
        return self._cause == other._cause

    def __str__(self) -> str:
        return str(self._cause)


class _WrappedReferencingError(
    _RefResolutionError,
    _Unresolvable,
):  # pragma: no cover -- partially uncovered but to be removed
    def __init__(self, cause: _Unresolvable):
        object.__setattr__(self, "_wrapped", cause)

    def __eq__(self, other):
        if other.__class__ is self.__class__:
            return self._wrapped == other._wrapped
        elif other.__class__ is self._wrapped.__class__:
            return self._wrapped == other
        return NotImplemented

    def __getattr__(self, attr):
        return getattr(self._wrapped, attr)

    def __hash__(self):
        return hash(self._wrapped)

    def __repr__(self):
        return f"<WrappedReferencingError {self._wrapped!r}>"

    def __str__(self):
        return f"{self._wrapped.__class__.__name__}: {self._wrapped}"


class UndefinedTypeCheck(Exception):
    """
    A type checker was asked to check a type it did not have registered.
    """

    def __init__(self, type: str) -> None:
        self.type = type

    def __str__(self) -> str:
        return f"Type {self.type!r} is unknown to this type checker"


class UnknownType(Exception):
    """
    A validator was asked to validate an instance against an unknown type.
    """

    def __init__(self, type, instance, schema):
        self.type = type
        self.instance = instance
        self.schema = schema

    def __str__(self):
        prefix = 16 * " "

        return dedent(
            f"""\
            Unknown type {self.type!r} for validator with schema:
                {_pretty(self.schema, prefix=prefix)}

            While checking instance:
                {_pretty(self.instance, prefix=prefix)}
            """.rstrip(),
        )


class FormatError(Exception):
    """
    Validating a format failed.
    """

    def __init__(self, message, cause=None):
        super().__init__(message, cause)
        self.message = message
        self.cause = self.__cause__ = cause

    def __str__(self):
        return self.message


class ErrorTree:
    """
    ErrorTrees make it easier to check which validations failed.

    Arguments:

        errors:

            the errors to populate the tree with

        instance:

            the instance the tree corresponds to, if known, which
            enables indexing the tree at indices not present in it to
            raise the same error that indexing the instance itself would

    """

    def __init__(
        self,
        errors: Iterable[ValidationError] = (),
        *,
        instance: Any = _unset,
    ):
        self.errors: MutableMapping[str, ValidationError] = {}
        self._contents: MutableMapping[str | int, ErrorTree] = {}
        self._instance = instance

        for error in errors:
            container = self
            for element in error.path:
                container = container._contents.setdefault(
                    element,
                    self.__class__(),
                )
            container.errors[error.validator] = error

            container._instance = error.instance

    def __contains__(self, index: str | int):
        """
        Check whether ``instance[index]`` has any errors.
        """
        return index in self._contents

    def __getitem__(self, index):
        """
        Retrieve the child tree one level down at the given ``index``.

        If the index is not in the instance that this tree corresponds
        to and is not known by this tree, whatever error would be raised
        by ``instance.__getitem__`` will be propagated (usually this is
        some subclass of `LookupError`.
        """
        if index in self:
            return self._contents[index]
        if self._instance is _unset:
            return self.__class__()
        return self.__class__(instance=self._instance[index])

    def __setitem__(self, index: str | int, value: ErrorTree):
        """
        Add an error to the tree at the given ``index``.

        .. deprecated:: v4.20.0

            Setting items on an `ErrorTree` is deprecated without replacement.
            To populate a tree, provide all of its sub-errors when you
            construct the tree.
        """
        warnings.warn(
            "ErrorTree.__setitem__ is deprecated without replacement.",
            DeprecationWarning,
            stacklevel=2,
        )
        self._contents[index] = value

    def __iter__(self):
        """
        Iterate (non-recursively) over the indices in the instance with errors.
        """
        return iter(self._contents)

    def __len__(self):
        """
        Return the `total_errors`.
        """
        return self.total_errors

    def __repr__(self):
        total = len(self)
        errors = "error" if total == 1 else "errors"
        return f"<{self.__class__.__name__} ({total} total {errors})>"

    @property
    def total_errors(self):
        """
        The total number of errors in the entire tree, including children.
        """
        child_errors = sum(len(tree) for _, tree in self._contents.items())
        return len(self.errors) + child_errors


def _escape_json_pointer_token(token: str | int) -> str:
    """
    Escape a single reference token for use in a JSON Pointer (RFC 6901).
    """
    return str(token).replace("~", "~0").replace("/", "~1")


@define(frozen=True)
class MissingMember:
    """
    A single missing object member, projected from a `ValidationError`.

    Errors for the :kw:`required` and :kw:`dependentRequired` keywords
    point at the parent object as a whole, which makes them awkward to
    consume when each missing member needs to be addressed individually
    (e.g. when rendering one problem per field in a UI). `missing_members`
    splits such errors into one `MissingMember` per absent property.

    Each projection keeps a reference to the `ValidationError` it was
    derived from via `MissingMember.error`, so no information is lost.

    .. note::

        The missing member does *not* exist in the instance, so no
        instance path is fabricated for it. Use `parent_path` (or
        `parent_pointer`) for the location of the containing object --
        which does exist -- and `member` for the name of the absent
        property within it.
    """

    error: ValidationError
    """
    The original `ValidationError` this projection was derived from.
    """

    member: str
    """
    The name of the property missing from the parent object.
    """

    triggered_by: str | None
    """
    For :kw:`dependentRequired`, the property whose presence in the
    instance triggered the dependency, or `None` for :kw:`required`.
    """

    branch: tuple[str | int | None, ...]
    """
    The combinators branch the projection belongs to.

    Empty for errors raised directly; otherwise the indices of the
    subschemas (e.g. within :kw:`anyOf` or :kw:`oneOf`) which were
    descended into to reach the error, outermost first.
    """

    @property
    def parent_path(self) -> tuple[str | int, ...]:
        """
        The absolute path to the parent object missing the member.

        Path segments retain their types, so array indices (`int`) are
        never confused with string object keys.
        """
        return tuple(self.error.absolute_path)

    @property
    def parent_pointer(self) -> str:
        """
        A JSON Pointer (RFC 6901) to the parent object.

        The empty string if the parent object is the instance itself.
        """
        return "".join(
            "/" + _escape_json_pointer_token(segment)
            for segment in self.parent_path
        )

    @property
    def member_token(self) -> str:
        """
        The missing member's name, escaped as a JSON Pointer token.
        """
        return _escape_json_pointer_token(self.member)


def missing_members(
    errors: Iterable[ValidationError],
) -> list[MissingMember]:
    """
    Project :kw:`required` and :kw:`dependentRequired` errors onto members.

    Each error for those keywords is split into one `MissingMember` per
    missing property, each recording the parent object's location, the
    missing member's name, and (for :kw:`dependentRequired`) the property
    whose presence triggered the dependency. Errors for other keywords
    are ignored.

    Errors within the context of combinators like :kw:`anyOf` or
    :kw:`oneOf` are descended into, and the resulting projections are
    grouped by branch: projections sharing a `MissingMember.branch` are
    contiguous in the returned list, so they can be grouped with e.g.
    `itertools.groupby`.

    This is a read-only view over the provided errors -- iterating
    errors or using `best_match` on them is unaffected.

    Arguments:

        errors (collections.abc.Iterable):

            the errors to project, e.g. from
            `jsonschema.protocols.Validator.iter_errors`

    Returns:

        a list of `MissingMember` projections, one per missing member
    """
    projections = []
    for error in errors:
        projections.extend(_project_missing_members(error, branch=()))
    return projections


def _project_missing_members(
    error: _Error,
    branch: tuple[str | int | None, ...],
) -> Iterable[MissingMember]:
    if error.validator in ("required", "dependentRequired"):
        yield from _missing_members_of(error, branch)
    else:
        for suberror in error.context:
            index = suberror.schema_path[0] if suberror.schema_path else None
            yield from _project_missing_members(
                suberror,
                branch=branch + (index,),
            )


def _missing_members_of(
    error: _Error,
    branch: tuple[str | int | None, ...],
) -> Iterable[MissingMember]:
    instance, dependencies = error.instance, error.validator_value
    if instance is _unset or dependencies is _unset:
        return
    if not isinstance(instance, Mapping):
        return

    if error.validator == "required":
        candidates = [
            (property, None)
            for property in dependencies
            if property not in instance
        ]
        expected = "{0!r} is a required property"
    else:  # dependentRequired
        candidates = [
            (each, property)
            for property, dependency in dependencies.items()
            if property in instance
            for each in dependency
            if each not in instance
        ]
        expected = "{0!r} is a dependency of {1!r}"

    # Each error the keywords emit covers exactly one missing member, so
    # attribute the error to the member whose message it carries rather
    # than to every absent property its validator value happens to list.
    missing = dict.fromkeys(
        (member, triggered_by)
        for member, triggered_by in candidates
        if expected.format(member, triggered_by) == error.message
    )
    for member, triggered_by in missing:
        yield MissingMember(
            error=error,
            member=member,
            triggered_by=triggered_by,
            branch=branch,
        )


def by_relevance(weak=WEAK_MATCHES, strong=STRONG_MATCHES):
    """
    Create a key function that can be used to sort errors by relevance.

    Arguments:
        weak (set):
            a collection of validation keywords to consider to be
            "weak".  If there are two errors at the same level of the
            instance and one is in the set of weak validation keywords,
            the other error will take priority. By default, :kw:`anyOf`
            and :kw:`oneOf` are considered weak keywords and will be
            superseded by other same-level validation errors.

        strong (set):
            a collection of validation keywords to consider to be
            "strong"

    """

    def relevance(error):
        validator = error.validator
        return (  # prefer errors which are ...
            -len(error.path),  # shorter path thereby more general
            validator not in weak,  # for a non-low-priority keyword
            validator in strong,  # for a high priority keyword
            not error._matches_type(),  # at least match the instance's type
        )  # otherwise we'll treat them the same

    return relevance


relevance = by_relevance()
"""
A key function (e.g. to use with `sorted`) which sorts errors by relevance.

Example:

.. code:: python

    sorted(validator.iter_errors(12), key=jsonschema.exceptions.relevance)
"""


def best_match(errors, key=relevance):
    """
    Try to find an error that appears to be the best match among given errors.

    In general, errors that are higher up in the instance (i.e. for which
    `ValidationError.path` is shorter) are considered better matches,
    since they indicate "more" is wrong with the instance.

    If the resulting match is either :kw:`oneOf` or :kw:`anyOf`, the
    *opposite* assumption is made -- i.e. the deepest error is picked
    among the most relevant errors in each separate subschema (preferring
    subschemas which produced fewer errors when tied), since these
    keywords only need to match once, and any other errors may not be
    relevant.

    Arguments:
        errors (collections.abc.Iterable):

            the errors to select from. Do not provide a mixture of
            errors from different validation attempts (i.e. from
            different instances or schemas), since it won't produce
            sensical output.

        key (collections.abc.Callable):

            the key to use when sorting errors. See `relevance` and
            transitively `by_relevance` for more details (the default is
            to sort with the defaults of that function). Changing the
            default is only useful if you want to change the function
            that rates errors but still want the error context descent
            done by this function.

    Returns:
        the best matching error, or ``None`` if the iterable was empty

    .. note::

        This function is a heuristic. Its return value may change for a given
        set of inputs from version to version if better heuristics are added.

    """
    _, best = _most_relevant(errors, key=key)
    if best is None:
        return

    while best.context:
        # Group the errors by the subschema which produced them.
        by_subschema: dict[Any, list[_Error]] = defaultdict(list)
        for error in best.context:
            index = error.schema_path[0] if error.schema_path else None
            by_subschema[index].append(error)

        # Rank each subschema by how deep its most relevant error is,
        # and amongst those equally deep, by how few errors it produced
        # (i.e. how close it was to being valid). Lower ranks are better.
        best_rank, best_in_subschema, tied = None, None, False
        for errors_in_subschema in by_subschema.values():
            error_key, error = _most_relevant(errors_in_subschema, key=key)
            rank = error_key, len(errors_in_subschema)
            if best_rank is None or rank < best_rank:
                best_rank, best_in_subschema, tied = rank, error, False
            elif rank == best_rank:
                tied = True

        # If multiple subschemas rank equally we can't tell which was
        # intended, so we stop here rather than descend into one of them.
        if tied:
            break
        best = best_in_subschema
    return best


def _most_relevant[E: _Error](
    errors: Iterable[E],
    key: Callable[[E], Any],
) -> tuple[Any, E | None]:
    """
    Find the most relevant error along with its key, computing each key once.

    Equally relevant errors are settled by picking the one which appears
    earlier in the instance, which makes the choice independent of the
    order in which the errors happened to be produced.

    Returns ``(None, None)`` if there were no errors.
    """
    best_key, best = None, None
    for error in errors:
        error_key = key(error)
        if (
            best is None
            or error_key > best_key
            or (error_key == best_key and error.path < best.path)
        ):
            best_key, best = error_key, error
    return best_key, best
