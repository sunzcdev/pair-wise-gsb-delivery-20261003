"""
Validation errors, and some surrounding helpers.
"""

from __future__ import annotations

from collections import defaultdict, deque
from collections.abc import Mapping, Sequence
from pprint import pformat
from textwrap import dedent, indent
from typing import TYPE_CHECKING, Any, ClassVar
import inspect
import json
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
    )

    from jsonschema import _types

WEAK_MATCHES: frozenset[str] = frozenset(["anyOf", "oneOf"])
STRONG_MATCHES: frozenset[str] = frozenset()

_JSON_PATH_COMPATIBLE_PROPERTY_PATTERN = re.compile("^[a-zA-Z][a-zA-Z0-9_]*$")

_unset = _utils.Unset()

#: Markers used by the safe error serialization below.
_TRUNCATED = "<truncated>"
_TRUNCATED_DEPTH = "<truncated: max depth reached>"
_TRUNCATED_SIZE = "<truncated: size limit>"
_TRUNCATED_KEY = "__truncated__"

#: Schema keywords whose values are annotations (i.e. may carry data).
_SCHEMA_ANNOTATION_KEYWORDS = frozenset(
    {
        "const",
        "default",
        "deprecated",
        "description",
        "enum",
        "examples",
        "readOnly",
        "title",
        "writeOnly",
    },
)

#: Schema keywords which describe the structure of a schema rather than data.
_SCHEMA_STRUCTURAL_KEYWORDS = frozenset(
    {
        "$anchor",
        "$defs",
        "$dynamicRef",
        "$id",
        "$ref",
        "$schema",
        "additionalItems",
        "additionalProperties",
        "allOf",
        "anyOf",
        "contains",
        "contentEncoding",
        "contentMediaType",
        "contentSchema",
        "definitions",
        "dependencies",
        "dependentRequired",
        "dependentSchemas",
        "else",
        "exclusiveMaximum",
        "exclusiveMinimum",
        "format",
        "if",
        "items",
        "maxContains",
        "maxItems",
        "maxLength",
        "maxProperties",
        "maximum",
        "minContains",
        "minItems",
        "minLength",
        "minProperties",
        "minimum",
        "multipleOf",
        "not",
        "oneOf",
        "pattern",
        "patternProperties",
        "prefixItems",
        "properties",
        "propertyNames",
        "required",
        "then",
        "type",
        "unevaluatedItems",
        "unevaluatedProperties",
        "uniqueItems",
    },
)

_PATH_SELECTORS = frozenset(
    {
        "additionalItems",
        "additionalProperties",
        "allOf",
        "anyOf",
        "contains",
        "dependencies",
        "dependentSchemas",
        "else",
        "if",
        "items",
        "not",
        "oneOf",
        "prefixItems",
        "propertyNames",
        "then",
        "unevaluatedItems",
        "unevaluatedProperties",
    },
)

_PROPERTY_CONTAINERS = frozenset(
    {
        "$defs",
        "definitions",
        "dependencies",
        "dependentSchemas",
        "patternProperties",
        "properties",
    },
)

_TRUTHY_POLICY_WORDS = frozenset({"mask", "redact", "sensitive", "true"})

_BUILTIN_CAUSE_TYPES = frozenset(
    {
        ArithmeticError,
        AttributeError,
        BufferError,
        EOFError,
        Exception,
        ImportError,
        IndexError,
        KeyError,
        LookupError,
        MemoryError,
        NameError,
        NotImplementedError,
        OSError,
        RuntimeError,
        StopIteration,
        TypeError,
        ValueError,
    },
)


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

    def to_safe_dict(self, **kwargs: Any) -> dict[str, Any]:
        """
        Return a safe serialization of this error without modifying it.

        See `serialize_error` for the accepted arguments.
        """
        return serialize_error(self, **kwargs)

    def to_safe_json(self, **kwargs: Any) -> str:
        """
        Return a safe JSON serialization of this error.

        The error itself is never modified.

        See `dump_error` for the accepted arguments.
        """
        return dump_error(self, **kwargs)


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


def _qualified_name(value: Any) -> str:
    """
    Return a stable ``module.Name`` label for a type or an instance.
    """
    cls = value if isinstance(value, type) else type(value)
    module = getattr(cls, "__module__", "")
    name = getattr(
        cls,
        "__qualname__",
        getattr(cls, "__name__", "Exception"),
    )
    if module and module not in {"builtins", "__main__"}:
        return f"{module}.{name}"
    return name


def _try(call: Callable[[], Any], default: Any = None) -> Any:
    """
    Call something, returning ``default`` if it raises.
    """
    try:
        return call()
    except Exception:  # noqa: BLE001
        return default


def _is_container(value: Any) -> bool:
    """
    Return whether a value is a container whose contents could be walked.
    """
    if isinstance(value, (str, bytes, bytearray)):
        return False
    return isinstance(value, (Mapping, Sequence, set, frozenset))


def _is_sequence(value: Any) -> bool:
    """
    Return whether a value is a non-string sequence.
    """
    if isinstance(value, (str, bytes, bytearray)):
        return False
    return isinstance(value, Sequence)


def _parse_json_path(path: str) -> tuple[Any, ...]:
    """
    Parse the subset of JSON paths which is useful here.
    """
    if path == "$":
        return ()

    name_pattern = r"[A-Za-z0-9_@:$+-]+"
    result: list[Any] = []
    index = 1
    while index < len(path):
        if path.startswith("..", index):
            index += 2
            result.append("**")
            match = re.match(name_pattern, path[index:])
            if match is not None:
                result.append(match.group(0))
                index += len(match.group(0))
        elif path[index] == ".":
            index += 1
            if index < len(path) and path[index] == "*":
                result.append("*")
                index += 1
                continue
            match = re.match(name_pattern, path[index:])
            if match is None:
                raise ValueError(f"Invalid JSON path: {path!r}")
            result.append(match.group(0))
            index += len(match.group(0))
        elif path[index] == "[":
            end = path.find("]", index)
            if end == -1:
                raise ValueError(f"Invalid JSON path: {path!r}")
            token = path[index + 1 : end]
            if token == "*":
                result.append("*")
            elif len(token) >= 2 and token[0] in {"'", '"'}:
                result.append(
                    token[1:-1]
                    .replace(r"\\", "\\")
                    .replace(r"\'", "'")
                    .replace(r"\"", '"'),
                )
            elif token.isdigit():
                result.append(int(token))
            else:
                result.append(token)
            index = end + 1
        else:
            raise ValueError(f"Invalid JSON path: {path!r}")
    return tuple(result)


def _parse_json_pointer(pointer: str) -> tuple[str, ...]:
    """
    Parse a JSON pointer into a tuple of unescaped segments.
    """
    if pointer == "":
        return ()
    if not pointer.startswith("/"):
        raise ValueError(f"Invalid JSON pointer: {pointer!r}")
    return tuple(
        part.replace("~1", "/").replace("~0", "~")
        for part in pointer.split("/")[1:]
    )


def _normalize_path(path: Any) -> tuple[Any, ...]:
    """
    Normalize any supported path representation to a tuple of segments.
    """
    if isinstance(path, str):
        candidate = path.strip()
        if not candidate:
            return ()
        if candidate.startswith("$"):
            return _parse_json_path(candidate)
        if candidate.startswith("/"):
            return _parse_json_pointer(candidate)
        if "." in candidate:
            return tuple(candidate.split("."))
        return (candidate,)
    if isinstance(path, Mapping):
        return tuple(path)
    return tuple(path)


def _looks_like_path_string(part: Any) -> bool:
    """
    Return whether a string looks like a path rather than a field name.
    """
    if not isinstance(part, str):
        return False
    return part.startswith(("$", "/")) or "." in part


def _path_collection(paths: Any) -> tuple[tuple[Any, ...], ...]:
    """
    Normalize one path or many paths into a tuple of paths.
    """
    if paths is None:
        return ()
    if isinstance(paths, (str, bytes)):
        return (_normalize_path(paths),)
    if isinstance(paths, Mapping):
        return (tuple(paths),)
    if isinstance(paths, tuple):
        if not paths:
            return ()
        if any(isinstance(part, (tuple, list, Mapping)) for part in paths):
            return tuple(_normalize_path(path) for path in paths)
        return (_normalize_path(paths),)

    collected = list(paths)
    if not collected:
        return ()
    if (
        all(isinstance(part, (str, int)) for part in collected)
        and not any(_looks_like_path_string(part) for part in collected)
    ):
        return (_normalize_path(collected),)
    return tuple(_normalize_path(path) for path in collected)


def _field_collection(fields: Any) -> tuple[str, ...]:
    """
    Normalize a collection of field names, or a single field name.
    """
    if fields is None:
        return ()
    if isinstance(fields, (str, bytes)):
        return (str(fields),)
    if isinstance(fields, Mapping):
        return tuple(str(field) for field in fields)
    return tuple(str(field) for field in fields)


def _schema_to_instance_pattern(
    path: tuple[Any, ...],
) -> tuple[Any, ...]:
    """
    Translate a schema path into a pattern matching instance paths.
    """
    result: list[Any] = []
    index = 0
    while index < len(path):
        part = path[index]
        if part in {"properties", "patternProperties"}:
            if index + 1 < len(path):
                result.append(path[index + 1])
                index += 2
            else:
                index += 1
        elif part in _PATH_SELECTORS:
            if part == "prefixItems" and index + 1 < len(path):
                result.append(path[index + 1])
                index += 2
            else:
                result.append("*")
                index += 1
        elif part in {"definitions", "$defs"}:
            index += 2 if index + 1 < len(path) else 1
        elif part in {"allOf", "anyOf", "oneOf", "not", "if", "then", "else"}:
            index += 2 if index + 1 < len(path) else 1
        elif part in _SCHEMA_ANNOTATION_KEYWORDS:
            break
        else:
            result.append(part)
            index += 1
    return tuple(result) if result else tuple(path)


def _fields_from_schema_path(
    path: tuple[Any, ...],
) -> tuple[str, ...]:
    """
    Find the last field name selected by a schema path.
    """
    field: str | None = None
    for index, part in enumerate(path[:-1]):
        if part in _PROPERTY_CONTAINERS:
            field = str(path[index + 1])
    return () if field is None else (field,)


def _patterns_from_schema_path(
    path: tuple[Any, ...],
) -> tuple[re.Pattern[str], ...]:
    """
    Find the regular expressions selected by ``patternProperties``.
    """
    patterns: list[re.Pattern[str]] = []
    for index, part in enumerate(path[:-1]):
        if part != "patternProperties":
            continue
        compiled = _try(
            lambda pattern=path[index + 1]: re.compile(str(pattern)),
        )
        if compiled is not None:
            patterns.append(compiled)
    return tuple(patterns)


def _segment_equal(left: Any, right: Any) -> bool:
    """
    Compare two path segments.
    """
    if left is right:
        return True
    return _try(lambda: bool(left == right), False)


def _glob_match(pattern: tuple[Any, ...], path: tuple[Any, ...]) -> bool:
    """
    Match a path against a pattern which may contain ``*`` and ``**``.
    """
    memo: dict[tuple[int, int], bool] = {}

    def match(pattern_index: int, path_index: int) -> bool:
        key = (pattern_index, path_index)
        if key in memo:
            return memo[key]
        if pattern_index == len(pattern):
            found = path_index == len(path)
        elif pattern[pattern_index] == "**":
            found = match(pattern_index + 1, path_index) or (
                path_index < len(path)
                and match(pattern_index, path_index + 1)
            )
        elif path_index == len(path):
            found = False
        elif pattern[pattern_index] == "*" or _segment_equal(
            pattern[pattern_index],
            path[path_index],
        ):
            found = match(pattern_index + 1, path_index + 1)
        else:
            found = False
        memo[key] = found
        return found

    return match(0, 0)


def _path_matches(
    pattern: tuple[Any, ...],
    path: tuple[Any, ...],
    *,
    suffix: bool,
    prefix: bool = False,
) -> bool:
    """
    Match an instance path against a pattern.

    ``suffix`` also allows the pattern to match any suffix of the path (so a
    pattern of ``password`` matches ``users[0].password``), and ``prefix``
    allows it to match any prefix (so the same pattern matches values nested
    inside ``password``).
    """
    if _glob_match(pattern, path):
        return True
    if suffix and any(
        _glob_match(pattern, path[index:])
        for index in range(1, len(path))
    ):
        return True
    return prefix and any(
        _glob_match(pattern, path[:index])
        for index in range(1, len(path) + 1)
    )


def _schema_path_matches(
    pattern: tuple[Any, ...],
    path: tuple[Any, ...],
) -> bool:
    """
    Match a schema path against a pattern, or a location within it.
    """
    if _glob_match(pattern, path):
        return True
    return any(
        _glob_match(pattern, path[:index])
        for index in range(len(pattern), len(path))
    )


def _call_field_policy(
    policy: Callable[..., Any],
    name: Any,
    path: tuple[Any, ...],
    value: Any,
) -> bool:
    """
    Call a field policy, adapting to the arguments it accepts.
    """
    candidate = getattr(policy, "is_sensitive", policy)
    try:
        parameters = inspect.signature(candidate).parameters
    except (TypeError, ValueError):
        return bool(candidate(name, path, value))

    if any(
        parameter.kind is inspect.Parameter.VAR_POSITIONAL
        for parameter in parameters.values()
    ):
        return bool(candidate(name, path, value))

    positional = [
        parameter
        for parameter in parameters.values()
        if parameter.kind
        in {
            inspect.Parameter.POSITIONAL_ONLY,
            inspect.Parameter.POSITIONAL_OR_KEYWORD,
        }
    ]
    named = {
        "field": name,
        "instance": value,
        "instance_path": path,
        "key": name,
        "name": name,
        "path": path,
        "value": value,
    }
    names = [parameter.name.lower() for parameter in positional]
    if names and all(name in named for name in names):
        return bool(candidate(*(named[name] for name in names)))
    if not positional:
        return bool(candidate())
    if len(positional) == 1:
        parameter_name = names[0]
        if parameter_name in {"path", "instance_path"}:
            return bool(candidate(path))
        if parameter_name in {"value", "instance"}:
            return bool(candidate(value))
        return bool(candidate(name))
    if len(positional) == 2:
        if names[0] in {"path", "instance_path"}:
            return bool(candidate(path, value))
        return bool(candidate(name, path))
    return bool(candidate(name, path, value))


def _truthy_policy(value: Any) -> bool:
    """
    Interpret a mapping entry in a field policy.
    """
    if isinstance(value, str):
        return value.lower() in _TRUTHY_POLICY_WORDS
    return bool(value)


def _mapping_field_policy(
    mapping: Mapping[Any, Any],
    name: Any,
    path: tuple[Any, ...],
    value: Any,
) -> bool:
    """
    Interpret a mapping-based field policy.
    """
    current: Any = mapping
    found_path = True
    for part in path:
        if not isinstance(current, Mapping):
            found_path = False
            break
        if _try(lambda part=part: part in current, False):
            current = current[part]
        elif _try(lambda part=part: str(part) in current, False):
            current = current[str(part)]
        else:
            found_path = False
            break
    if found_path:
        if callable(current):
            return _call_field_policy(current, name, path, value)
        if isinstance(current, Mapping):
            # The path selected a policy for locations *inside* it, rather
            # than declaring the location itself sensitive.
            return False
        return _truthy_policy(current)

    entry = _try(lambda: mapping.get(name, _unset), _unset)
    if entry is _unset:
        for key, candidate in mapping.items():
            if not isinstance(key, str):
                continue
            if _try(lambda key=key: re.fullmatch(key, str(name))) is not None:
                entry = candidate
                break
    if entry is _unset:
        return False
    if callable(entry):
        return _call_field_policy(entry, name, path, value)
    if isinstance(entry, Mapping):
        return bool(entry)
    return _truthy_policy(entry)


def _normalize_field_policy(
    policy: Any,
) -> Callable[..., bool] | None:
    """
    Turn a field policy into a single callable, if one was given.
    """
    if policy is None:
        return None
    if isinstance(policy, RedactionPolicy):
        return policy.is_sensitive_field
    if isinstance(policy, Mapping):
        return lambda name, path, value: _mapping_field_policy(
            policy,
            name,
            path,
            value,
        )
    if callable(policy) or hasattr(policy, "is_sensitive"):
        return lambda name, path, value: _call_field_policy(
            policy,
            name,
            path,
            value,
        )
    fields = frozenset(_field_collection(policy))
    return lambda name, path, value: str(name) in fields


def _combine_field_policies(
    left: Callable[..., bool] | None,
    right: Callable[..., bool] | None,
) -> Callable[..., bool] | None:
    """
    Return a field policy which is the union of two others.
    """
    if left is None:
        return right
    if right is None:
        return left
    return lambda name, path, value: left(
        name,
        path,
        value,
    ) or right(
        name,
        path,
        value,
    )


class RedactionPolicy:
    """
    Describes which instance values a safe serialization must mask.

    Values can be selected by field name, by a path within the schema (for
    example ``("properties", "password")`` or ``"/properties/password"``),
    by an instance path, or by an arbitrary callable.

    Selecting a field in the schema, e.g. ``properties.password``, treats
    ``password`` as sensitive wherever it occurs, and also masks the schema
    annotations (such as :kw:`default` or :kw:`examples`) describing it.

    Arguments:

        sensitive_fields:

            field names whose values must always be masked

        sensitive_schema_paths:

            paths within the schema which identify sensitive fields. They may
            be sequences, JSON pointers, JSON paths, or dotted paths.

        sensitive_paths:

            additional instance paths to mask, in any of the same forms.

        field_policy:

            a callable, a mapping, or an object with an ``is_sensitive``
            method. Callables are passed up to ``(name, path, value)``.

        mask:

            the replacement used for a sensitive value

        redact_field_names:

            whether the names of matching fields are also replaced in the
            safe representation. Instance locations remain available
            separately.

    """

    def __init__(
        self,
        *,
        sensitive_fields: Any = (),
        sensitive_schema_paths: Any = (),
        sensitive_paths: Any = (),
        field_policy: Any = None,
        mask: str = "***",
        redact_field_names: bool = False,
    ) -> None:
        self.mask = mask
        self.redact_field_names = redact_field_names
        self.sensitive_fields = frozenset(
            _field_collection(sensitive_fields),
        )
        self.sensitive_schema_paths = _path_collection(
            sensitive_schema_paths,
        )
        self.sensitive_paths = _path_collection(sensitive_paths)

        schema_fields: set[str] = set()
        schema_patterns: list[re.Pattern[str]] = []
        instance_patterns: list[tuple[Any, ...]] = []
        for path in self.sensitive_schema_paths:
            schema_fields.update(_fields_from_schema_path(path))
            schema_patterns.extend(_patterns_from_schema_path(path))
            instance_patterns.append(_schema_to_instance_pattern(path))
        instance_patterns.extend(self.sensitive_paths)

        self._schema_fields = frozenset(schema_fields)
        self._schema_patterns = tuple(schema_patterns)
        self._instance_patterns = tuple(instance_patterns)
        self._field_policy = _normalize_field_policy(field_policy)

    def with_(
        self,
        *,
        sensitive_fields: Any = (),
        sensitive_schema_paths: Any = (),
        sensitive_paths: Any = (),
        field_policy: Any = None,
        mask: str | None = None,
        redact_field_names: bool | None = None,
    ) -> RedactionPolicy:
        """
        Return a new policy with additional sensitive locations.
        """
        return self.__class__(
            sensitive_fields=self.sensitive_fields
            | frozenset(_field_collection(sensitive_fields)),
            sensitive_schema_paths=(
                self.sensitive_schema_paths
                + _path_collection(sensitive_schema_paths)
            ),
            sensitive_paths=(
                self.sensitive_paths + _path_collection(sensitive_paths)
            ),
            field_policy=_combine_field_policies(
                self._field_policy,
                _normalize_field_policy(field_policy),
            ),
            mask=self.mask if mask is None else mask,
            redact_field_names=(
                self.redact_field_names
                if redact_field_names is None
                else redact_field_names
            ),
        )

    def is_sensitive_field(
        self,
        name: Any,
        path: tuple[Any, ...],
        value: Any,
    ) -> bool:
        """
        Return whether the field at the given location is sensitive.
        """
        name = str(name)
        if name in self.sensitive_fields or name in self._schema_fields:
            return True
        if any(
            pattern.fullmatch(name) for pattern in self._schema_patterns
        ):
            return True
        if self._field_policy is None:
            return False
        try:
            return bool(self._field_policy(name, path, value))
        except Exception:  # noqa: BLE001
            # When in doubt, treat something a caller asked about as
            # sensitive rather than risk writing it into a log.
            return True

    def is_sensitive_field_path(self, path: tuple[Any, ...]) -> bool:
        """
        Return whether any field along the given path is sensitive.

        This is what makes a value *nested inside* a sensitive field (say an
        element of a sensitive array) sensitive as well.
        """
        for index in range(1, len(path) + 1):
            prefix = path[:index]
            name = str(prefix[-1])
            if name in self.sensitive_fields or name in self._schema_fields:
                return True
            if any(
                pattern.fullmatch(name) for pattern in self._schema_patterns
            ):
                return True
            if self._field_policy is None:
                continue
            try:
                if self._field_policy(name, prefix, _unset):
                    return True
            except Exception:  # noqa: BLE001
                # A policy which cannot be probed without a value simply
                # does not mark the enclosing location as sensitive; the
                # real value is still checked once it is known.
                continue
        return False

    def is_sensitive_path(self, path: tuple[Any, ...]) -> bool:
        """
        Return whether the given instance path is sensitive.

        A location inside a sensitive location is sensitive too, so a value
        nested beneath a sensitive field cannot be written to a log.
        """
        return any(
            _path_matches(pattern, path, suffix=True, prefix=True)
            for pattern in self._instance_patterns
        )

    def is_sensitive_schema_path(
        self,
        path: tuple[Any, ...],
    ) -> bool:
        """
        Return whether the given schema path is inside a sensitive field.
        """
        if any(
            _schema_path_matches(pattern, path)
            for pattern in self.sensitive_schema_paths
        ):
            return True
        for index, part in enumerate(path[:-1]):
            if part not in _PROPERTY_CONTAINERS:
                continue
            name = str(path[index + 1])
            if name in self.sensitive_fields or name in self._schema_fields:
                return True
            if any(
                pattern.fullmatch(name)
                for pattern in self._schema_patterns
            ):
                return True
            if self._field_policy is None:
                continue
            instance_hint = _schema_to_instance_pattern(path[: index + 2])
            while (
                instance_hint
                and instance_hint[-1]
                in _SCHEMA_STRUCTURAL_KEYWORDS
                | _SCHEMA_ANNOTATION_KEYWORDS
            ):
                instance_hint = instance_hint[:-1]
            if not instance_hint:
                instance_hint = (name,)
            try:
                if self._field_policy(
                    instance_hint[-1],
                    tuple(instance_hint),
                    _unset,
                ):
                    return True
            except Exception:  # noqa: BLE001
                # As above, do not risk a leak if a policy misbehaves.
                return True
        return False


class _SafeErrorSerializer:
    """
    Walks errors, collects the values which must not be logged, and emits a
    bounded, JSON-compatible representation of them.
    """

    _MAX_SCAN_DEPTH = 64
    #: Last-resort bounds, so that even a caller who disables ``max_depth``
    #: cannot be met with a `RecursionError` on hostile input.
    _MAX_ERROR_DEPTH = 200
    _MAX_VALUE_DEPTH = 200

    def _limit(self, requested: int | None, ceiling: int) -> int:
        """
        Combine a caller-requested limit with a safety ceiling.
        """
        if requested is None:
            return ceiling
        return min(requested, ceiling)

    def _too_deep(self, depth: int, kind: str) -> bool:
        """
        Return whether the given depth exceeds the applicable limit.
        """
        ceiling = (
            self._MAX_ERROR_DEPTH
            if kind == "error"
            else self._MAX_VALUE_DEPTH
        )
        limit = self._limit(self.max_depth, ceiling)
        return depth > limit

    def __init__(
        self,
        policy: RedactionPolicy,
        *,
        max_depth: int | None,
        max_length: int | None,
        max_items: int | None,
        max_context: int | None,
        include_context: bool,
        include_cause: bool,
        include_cause_message: bool,
    ) -> None:
        self.policy = policy
        self.max_depth = max_depth
        self.max_length = max_length
        self.max_items = max_items
        self.max_context = max_context
        self.include_context = include_context
        self.include_cause = include_cause
        self.include_cause_message = include_cause_message

        self.truncation: set[str] = set()
        self.redacted = False
        self._secret_tokens: list[tuple[str, str]] = []
        self._collected: set[int] = set()
        self._seen_errors: set[int] = set()
        self._value_stack: set[int] = set()

    def mark_truncated(self, reason: str) -> None:
        """
        Record that output was cut for the given reason.
        """
        self.truncation.add(reason)

    def _truncate_text(self, text: str) -> str:
        if self.max_length is None or len(text) <= self.max_length:
            return text
        self.mark_truncated("length")
        marker = "... [truncated]"
        if self.max_length <= len(marker):
            return marker[: self.max_length]
        return text[: self.max_length - len(marker)] + marker

    def _remember(self, value: Any) -> None:
        """
        Remember every textual form of a value which must not be logged.
        """
        self.redacted = True
        mask = self.policy.mask
        candidates = {
            _try(lambda: repr(value), ""),
            _try(lambda: str(value), ""),
            _try(
                lambda: json.dumps(
                    value,
                    ensure_ascii=False,
                    default=lambda _: _TRUNCATED,
                ),
                "",
            ),
        }
        if _is_container(value):
            candidates.add(
                _try(
                    lambda: pformat(
                        value,
                        width=72,
                        sort_dicts=False,
                    ),
                    "",
                ),
            )
        for candidate in candidates:
            if not candidate or candidate == mask:
                continue
            if (candidate, mask) not in self._secret_tokens:
                self._secret_tokens.append((candidate, mask))

    def _scan(
        self,
        value: Any,
        *,
        path: tuple[Any, ...],
        schema_path: tuple[Any, ...],
        is_schema: bool,
        force: bool,
        depth: int,
        seen: set[int],
    ) -> bool:
        """
        Walk a value, remembering everything sensitive found within it.

        Returns whether anything sensitive was found.
        """
        if depth > self._MAX_SCAN_DEPTH:
            return force

        container = _is_container(value)
        identity = id(value) if container else None
        if identity is not None:
            if identity in seen:
                return force
            seen.add(identity)
        try:
            sensitive = force
            if is_schema:
                sensitive = (
                    sensitive
                    or self.policy.is_sensitive_schema_path(schema_path)
                )
            else:
                sensitive = sensitive or self.policy.is_sensitive_path(path)
                sensitive = (
                    sensitive or self._field_is_sensitive(path, value)
                )
            if sensitive and is_schema and not container:
                last = schema_path[-1] if schema_path else None
                if last in _SCHEMA_STRUCTURAL_KEYWORDS:
                    sensitive = False
            if sensitive and not (is_schema and container):
                self._remember(value)

            found = sensitive
            if isinstance(value, Mapping):
                for key, child in value.items():
                    if is_schema:
                        child_schema_path = schema_path + (key,)
                        child_sensitive = (
                            sensitive
                            or self.policy.is_sensitive_schema_path(
                                child_schema_path,
                            )
                        )
                        child_force = child_sensitive and (
                            key in _SCHEMA_ANNOTATION_KEYWORDS
                            or key not in _SCHEMA_STRUCTURAL_KEYWORDS
                        )
                        found = self._scan(
                            child,
                            path=path,
                            schema_path=child_schema_path,
                            is_schema=True,
                            force=child_force,
                            depth=depth + 1,
                            seen=seen,
                        ) or found
                    else:
                        found = self._scan(
                            child,
                            path=path + (key,),
                            schema_path=schema_path,
                            is_schema=False,
                            force=sensitive,
                            depth=depth + 1,
                            seen=seen,
                        ) or found
            elif _is_sequence(value):
                for index, child in enumerate(value):
                    child_path = path if is_schema else path + (index,)
                    found = self._scan(
                        child,
                        path=child_path,
                        schema_path=schema_path,
                        is_schema=is_schema,
                        force=sensitive,
                        depth=depth + 1,
                        seen=seen,
                    ) or found
            elif isinstance(value, (set, frozenset)):
                for child in value:
                    found = self._scan(
                        child,
                        path=path,
                        schema_path=schema_path,
                        is_schema=is_schema,
                        force=sensitive,
                        depth=depth + 1,
                        seen=seen,
                    ) or found
        finally:
            if identity is not None:
                seen.discard(identity)

        if found and not sensitive and container:
            self._remember(value)
        return found

    def _field_is_sensitive(
        self,
        path: tuple[Any, ...],
        value: Any,
    ) -> bool:
        """
        Return whether an instance location selects a sensitive field.

        A location nested inside a sensitive field is sensitive too, so an
        element of a sensitive array cannot leak either.
        """
        if self.policy.is_sensitive_field_path(path):
            return True
        return bool(
            path
            and self.policy.is_sensitive_field(path[-1], path, value)
        )

    def _path_is_sensitive(self, error: _Error) -> bool:
        path = tuple(error.absolute_path)
        if self.policy.is_sensitive_path(path):
            return True
        if error.instance is _unset:
            return self._field_is_sensitive(path, _unset)
        return self._field_is_sensitive(path, error.instance)

    def _collect_secrets(self, error: BaseException) -> None:
        """
        Collect every sensitive value reachable from an error's tree.

        This walks the tree iteratively, since error contexts may be deep.
        """
        pending = [error]
        while pending:
            current = pending.pop()
            if id(current) in self._collected:
                continue
            self._collected.add(id(current))
            if not isinstance(current, _Error):
                continue

            path_is_sensitive = self._path_is_sensitive(current)
            absolute_path = tuple(current.absolute_path)
            if current.instance is not _unset:
                self._scan(
                    current.instance,
                    path=absolute_path,
                    schema_path=(),
                    is_schema=False,
                    force=path_is_sensitive,
                    depth=0,
                    seen=set(),
                )
            if (
                current.validator_value is not _unset
                and isinstance(current.validator, str)
                and current.validator in {"const", "enum"}
            ):
                self._scan(
                    current.validator_value,
                    path=absolute_path,
                    schema_path=tuple(current.absolute_schema_path),
                    is_schema=True,
                    force=path_is_sensitive,
                    depth=0,
                    seen=set(),
                )
            if current.schema is not _unset:
                self._scan(
                    current.schema,
                    path=(),
                    schema_path=tuple(current.absolute_schema_path)[:-1],
                    is_schema=True,
                    force=path_is_sensitive,
                    depth=0,
                    seen=set(),
                )
            pending.extend(current.context)

    def _message_still_leaks(self, text: str) -> bool:
        """
        Check a scrubbed message for any remembered value.

        This is deliberately strict: if a value cannot be removed from the
        message (for instance because it is a substring of the mask itself),
        the whole message is replaced by an explicit summary instead.
        """
        return any(token and token in text for token, _ in self._secret_tokens)

    def _safe_failure_message(self, error: _Error | None) -> str:
        if error is None:
            return "validation failed; details omitted"
        category = _reason_category(error)
        validator = getattr(error, "validator", _unset)
        json_path = self._json_path(error)
        if not isinstance(validator, str):
            return f"{category} validation failed at {json_path}"
        return (
            f"{category} validation failed for keyword "
            f"{validator} at {json_path}"
        )

    def _scrub_message(
        self,
        message: Any,
        error: _Error | None,
    ) -> str:
        """
        Remove remembered values from a message, replacing the whole message
        with an explicit summary if anything could still be there.
        """
        if not isinstance(message, str):
            return self._safe_failure_message(error)
        scrubbed = message
        for token, replacement in sorted(
            self._secret_tokens,
            key=lambda pair: len(pair[0]),
            reverse=True,
        ):
            scrubbed = scrubbed.replace(token, replacement)
        if scrubbed != message:
            self.redacted = True
        if self._message_still_leaks(scrubbed):
            self.redacted = True
            scrubbed = self._safe_failure_message(error)
        return self._truncate_text(scrubbed)

    def _safe_key(self, key: Any) -> str:
        """
        Return a JSON-compatible key.

        Keys always become strings, both so that the result survives
        ``json.dumps`` unchanged and so that keys such as ``1`` and ``"1"``
        cannot collapse into a duplicate in the encoded form.
        """
        if isinstance(key, str):
            return self._truncate_text(key)
        if key is True:
            return "true"
        if key is False:
            return "false"
        if key is None:
            return "null"
        if isinstance(key, float):
            return str(self._safe_number(key))
        if isinstance(key, int):
            return str(key)
        if isinstance(key, (bytes, bytearray)):
            return f"<bytes key: {len(key)} bytes>"
        return f"<{type(key).__name__} key>"

    def _safe_number(self, value: float) -> Any:
        """
        Return a float which JSON can represent, or a marker for one it cannot.
        """
        if value != value:  # noqa: PLR0124 -- NaN detection
            return "<nan>"
        if value == float("inf"):
            return "<infinity>"
        if value == float("-inf"):
            return "<-infinity>"
        return value

    def _safe_value(
        self,
        value: Any,
        path: tuple[Any, ...] = (),
        *,
        depth: int = 0,
        schema_path: tuple[Any, ...] = (),
        is_schema: bool = False,
        mask_sensitive: bool = False,
        force_sensitive: bool = False,
    ) -> Any:
        """
        Return a JSON-compatible copy of a value with sensitive parts masked.
        """
        if self._too_deep(depth, "value"):
            self.mark_truncated("depth")
            return _TRUNCATED_DEPTH

        container = _is_container(value)
        identity = id(value) if container else None
        if identity is not None:
            if identity in self._value_stack:
                self.mark_truncated("cycle")
                return _TRUNCATED
            self._value_stack.add(identity)
        try:
            sensitive = force_sensitive
            if is_schema:
                sensitive = (
                    sensitive
                    or self.policy.is_sensitive_schema_path(schema_path)
                )
            else:
                sensitive = sensitive or self.policy.is_sensitive_path(path)
                if not sensitive:
                    sensitive = self._field_is_sensitive(path, value)

            if not is_schema and sensitive:
                self.redacted = True
                return self.policy.mask
            if is_schema and mask_sensitive and sensitive:
                self.redacted = True
                return self.policy.mask

            if isinstance(value, Mapping):
                result: dict[Any, Any] = {}
                items = list(value.items())
                truncated_items = (
                    self.max_items is not None
                    and len(items) > max(0, self.max_items)
                )
                if truncated_items:
                    items = items[: max(0, self.max_items)]
                for key, child in items:
                    if is_schema:
                        child_schema_path = schema_path + (key,)
                        child_sensitive = (
                            sensitive
                            or self.policy.is_sensitive_schema_path(
                                child_schema_path,
                            )
                        )
                        safe_key = self._safe_key(key)
                        if child_sensitive and (
                            key in _SCHEMA_ANNOTATION_KEYWORDS
                            or key not in _SCHEMA_STRUCTURAL_KEYWORDS
                        ):
                            result[safe_key] = self.policy.mask
                            self.redacted = True
                            continue
                        result[safe_key] = self._safe_value(
                            child,
                            path,
                            depth=depth + 1,
                            schema_path=child_schema_path,
                            is_schema=True,
                            mask_sensitive=(
                                mask_sensitive and not child_sensitive
                            ),
                        )
                    else:
                        child_path = path + (key,)
                        safe_key = self._safe_key(key)
                        if self.policy.redact_field_names and (
                            self._field_is_sensitive(child_path, child)
                        ):
                            safe_key = self.policy.mask
                        result[safe_key] = self._safe_value(
                            child,
                            child_path,
                            depth=depth + 1,
                            schema_path=schema_path,
                        )
                if truncated_items:
                    self.mark_truncated("items")
                    result[_TRUNCATED_KEY] = (
                        f"{len(value) - max(0, self.max_items)}"
                        " more item(s)"
                    )
                return result

            if _is_sequence(value):
                result = []
                values = list(value)
                truncated_items = (
                    self.max_items is not None
                    and len(values) > max(0, self.max_items)
                )
                if truncated_items:
                    values = values[: max(0, self.max_items)]
                for index, child in enumerate(values):
                    child_path = path if is_schema else path + (index,)
                    result.append(
                        self._safe_value(
                            child,
                            child_path,
                            depth=depth + 1,
                            schema_path=schema_path,
                            is_schema=is_schema,
                            mask_sensitive=mask_sensitive,
                        ),
                    )
                if truncated_items:
                    self.mark_truncated("items")
                    result.append(
                        {
                            _TRUNCATED_KEY: (
                                f"{len(value) - max(0, self.max_items)}"
                                " more item(s)"
                            ),
                        },
                    )
                return result

            if isinstance(value, (set, frozenset)):
                values = list(value)
                truncated_items = (
                    self.max_items is not None
                    and len(values) > max(0, self.max_items)
                )
                if truncated_items:
                    values = values[: max(0, self.max_items)]
                result = [
                    self._safe_value(
                        child,
                        path,
                        depth=depth + 1,
                        schema_path=schema_path,
                        is_schema=is_schema,
                        mask_sensitive=mask_sensitive,
                    )
                    for child in values
                ]
                if truncated_items:
                    self.mark_truncated("items")
                    result.append(
                        {
                            _TRUNCATED_KEY: (
                                f"{len(value) - max(0, self.max_items)}"
                                " more item(s)"
                            ),
                        },
                    )
                return result

            if isinstance(value, str):
                return self._truncate_text(value)
            if isinstance(value, float):
                return self._safe_number(value)
            if isinstance(value, (bool, int)) or value is None:
                return value
            if isinstance(value, (bytes, bytearray)):
                return self._truncate_text(
                    f"<bytes: {len(value)} bytes>",
                )
            if isinstance(value, BaseException):
                return self._exception_summary(value)["summary"]
            if callable(value):
                return "<callable>"
            return f"<{type(value).__name__} object>"
        finally:
            if identity is not None:
                self._value_stack.discard(identity)

    def _exception_summary(
        self,
        error: BaseException,
    ) -> dict[str, Any]:
        """
        Summarize an exception without expanding unknown ones.
        """
        error_type = _qualified_name(error)
        if isinstance(error, _Error):
            category = _reason_category(error)
            summary = error_type
        elif isinstance(error, FormatError):
            category = "format"
            summary = error_type
        elif isinstance(error, (UnknownType, UndefinedTypeCheck)):
            category = "type"
            summary = error_type
        elif isinstance(error, _RefResolutionError):
            category = "reference"
            summary = error_type
        elif type(error) in _BUILTIN_CAUSE_TYPES:
            category = "python"
            summary = error_type
        else:
            category = "unknown"
            summary = f"Unrecognized exception type: {error_type}"

        result = {
            "type": type(error).__name__,
            "error_type": error_type,
            "category": category,
            "summary": summary,
            "message": summary,
        }
        if (
            self.include_cause_message
            and type(error) in _BUILTIN_CAUSE_TYPES
        ):
            message = _try(lambda: self._scrub_message(str(error), None))
            if isinstance(message, str):
                result["message"] = message
        return result

    def _cause_of(self, error: BaseException) -> BaseException | None:
        if isinstance(error, _RefResolutionError):
            cause = _try(lambda: error._cause)
            if isinstance(cause, BaseException):
                return cause
        cause = _try(lambda: getattr(error, "cause", None))
        if not isinstance(cause, BaseException):
            cause = _try(lambda: getattr(error, "__cause__", None))
        return cause if isinstance(cause, BaseException) else None

    def _cause_summary(self, cause: BaseException | None) -> Any:
        if cause is None or not self.include_cause:
            return None
        return self._exception_summary(cause)

    def _json_path(self, error: _Error) -> str:
        try:
            return error.json_path
        except Exception:  # noqa: BLE001
            return "$"

    def _parent_summary(self, parent: Any) -> Any:
        """
        Describe a parent error without any of its values or message.
        """
        if parent is None:
            return None
        if not isinstance(parent, _Error):
            return {
                "type": type(parent).__name__,
                "error_type": _qualified_name(parent),
            }
        validator = None if parent.validator is _unset else parent.validator
        return {
            "type": type(parent).__name__,
            "error_type": _qualified_name(parent),
            "validator": validator,
            "keyword": validator,
            "path": list(parent.relative_path),
            "json_path": self._json_path(parent),
        }

    def _serialize_error(
        self,
        error: _Error,
        depth: int = 0,
    ) -> dict[str, Any]:
        if self._too_deep(depth, "error"):
            self.mark_truncated("depth")
            return self._truncated_stub(error, "max depth reached")
        identity = id(error)
        if identity in self._seen_errors:
            self.mark_truncated("cycle")
            return self._truncated_stub(
                error,
                "recursive error reference",
            )

        self._seen_errors.add(identity)
        try:
            self._collect_secrets(error)
            validator = (
                None if error.validator is _unset else error.validator
            )
            absolute_path = tuple(error.absolute_path)
            path_is_sensitive = self._path_is_sensitive(error)
            instance = (
                None
                if error.instance is _unset
                else self._safe_value(error.instance, absolute_path)
            )
            validator_value = (
                None
                if error.validator_value is _unset
                else self._safe_value(
                    error.validator_value,
                    absolute_path,
                    schema_path=tuple(error.absolute_schema_path),
                    is_schema=True,
                    mask_sensitive=True,
                    force_sensitive=path_is_sensitive,
                )
            )
            schema = (
                None
                if error.schema is _unset
                else self._safe_value(
                    error.schema,
                    schema_path=tuple(error.absolute_schema_path)[:-1],
                    is_schema=True,
                    force_sensitive=path_is_sensitive,
                )
            )

            context: list[Any] = []
            if self.include_context and error.context:
                children = list(error.context)
                if self.max_context is None:
                    visible = children
                else:
                    visible = children[: max(0, self.max_context)]
                for child in visible:
                    if isinstance(child, _Error):
                        context.append(
                            self._serialize_error(child, depth + 1),
                        )
                    else:
                        context.append(self._serialize_non_error(child))
                if self.max_context is not None and len(children) > max(
                    0,
                    self.max_context,
                ):
                    self.mark_truncated("context")
                    context.append(
                        {
                            _TRUNCATED_KEY: (
                                f"{len(children) - max(0, self.max_context)}"
                                " more error(s)"
                            ),
                        },
                    )

            reason = _reason_category(error)
            message = self._scrub_message(error.message, error)
            return {
                "type": type(error).__name__,
                "error_type": _qualified_name(error),
                "category": _error_category(error),
                "reason": reason,
                "reason_category": reason,
                "keyword": validator,
                "validator": validator,
                "message": message,
                "summary": message,
                "path": list(error.relative_path),
                "instance_path": list(absolute_path),
                "absolute_path": list(absolute_path),
                "json_path": self._json_path(error),
                "schema_path": list(error.relative_schema_path),
                "absolute_schema_path": list(error.absolute_schema_path),
                "instance": instance,
                "validator_value": validator_value,
                "schema": schema,
                "context": context,
                "cause": self._cause_summary(self._cause_of(error)),
                "parent": self._parent_summary(error.parent),
                "redacted": self.redacted,
                "truncated": bool(self.truncation),
                "truncation": sorted(self.truncation),
            }
        finally:
            self._seen_errors.discard(identity)

    def _truncated_stub(self, error: _Error, reason: str) -> dict[str, Any]:
        """
        Return a stable, empty shape for an error which was not expanded.
        """
        reason_category = _reason_category(error)
        return {
            _TRUNCATED_KEY: reason,
            "type": _qualified_name(error),
            "error_type": _qualified_name(error),
            "category": _error_category(error),
            "reason": reason_category,
            "reason_category": reason_category,
            "keyword": None,
            "validator": None,
            "message": f"<truncated: {reason}>",
            "summary": f"<truncated: {reason}>",
            "path": list(error.relative_path),
            "instance_path": list(_try(error.absolute_path, ())),
            "absolute_path": list(_try(error.absolute_path, ())),
            "json_path": self._json_path(error),
            "schema_path": list(error.relative_schema_path),
            "absolute_schema_path": list(
                _try(error.absolute_schema_path, ()),
            ),
            "instance": None,
            "validator_value": None,
            "schema": None,
            "context": [],
            "cause": None,
            "parent": None,
            "redacted": self.redacted,
            "truncated": True,
            "truncation": sorted(self.truncation),
        }

    def _serialize_non_error(
        self,
        error: BaseException,
    ) -> dict[str, Any]:
        error_type = _qualified_name(error)
        if isinstance(error, FormatError):
            category = reason = "format"
            message = self._scrub_message(error.message, None)
        elif isinstance(error, UnknownType):
            category = reason = "type"
            message = self._scrub_message(
                f"Unknown type {error.type!r} for validator",
                None,
            )
        elif isinstance(error, UndefinedTypeCheck):
            category = reason = "type"
            message = self._scrub_message(
                f"Type {error.type!r} is unknown to this type checker",
                None,
            )
        elif isinstance(error, _RefResolutionError):
            category = reason = "reference"
            message = f"{error_type} while resolving a reference"
        else:
            category = reason = "unknown"
            message = f"Unrecognized exception type: {error_type}"

        result: dict[str, Any] = {
            "type": type(error).__name__,
            "error_type": error_type,
            "category": category,
            "reason": reason,
            "reason_category": reason,
            "keyword": None,
            "validator": None,
            "message": message,
            "summary": message,
            "path": [],
            "instance_path": [],
            "absolute_path": [],
            "json_path": "$",
            "schema_path": [],
            "absolute_schema_path": [],
            "instance": None,
            "validator_value": None,
            "schema": None,
            "context": [],
            "cause": self._cause_summary(self._cause_of(error)),
            "parent": None,
            "redacted": self.redacted,
            "truncated": bool(self.truncation),
            "truncation": sorted(self.truncation),
        }
        if isinstance(error, UnknownType):
            result["instance"] = self._safe_value(error.instance)
            result["schema"] = self._safe_value(
                error.schema,
                is_schema=True,
            )
            result["keyword"] = result["validator"] = "type"
        return result

    def serialize(self, error: BaseException) -> dict[str, Any]:
        """
        Serialize an error, of any kind, to a safe mapping.
        """
        if isinstance(error, _Error):
            result = self._serialize_error(error)
        else:
            result = self._serialize_non_error(error)
        self._finalize(result)
        return result

    def _finalize(self, result: dict[str, Any]) -> None:
        """
        Make every nested error report the final redaction and truncation
        state, since a later sibling may reveal secrets or hit a limit.
        """
        pending = [result]
        while pending:
            current = pending.pop()
            if _TRUNCATED_KEY in current:
                continue
            current["redacted"] = self.redacted
            current["truncated"] = bool(self.truncation)
            current["truncation"] = sorted(self.truncation)
            for child in current.get("context", ()):
                if isinstance(child, dict):
                    pending.append(child)



def _error_category(error: _Error) -> str:
    """
    Return the broad category of a json schema error.
    """
    if isinstance(error, SchemaError):
        return "schema"
    return "validation"


def _reason_category(error: _Error) -> str:
    """
    Return the category of failure which an error describes.

    This is deliberately coarser than the keyword itself, so that consumers
    can group failures without reading any instance values.
    """
    validator = getattr(error, "validator", _unset)
    if not isinstance(validator, str) or validator is _unset:
        return _error_category(error)
    if validator in {"required", "dependentRequired"}:
        return "missing"
    if validator == "type":
        return "type"
    if validator in {"const", "enum"}:
        return "value"
    if validator in {
        "allOf",
        "anyOf",
        "else",
        "if",
        "not",
        "oneOf",
        "then",
    }:
        return "composition"
    if validator.startswith(("min", "max", "exclusive")):
        return "range"
    if validator in {"pattern", "patternProperties", "propertyNames"}:
        return "pattern"
    if validator in {"format", "contentEncoding", "contentMediaType"}:
        return "format"
    if validator in {
        "contains",
        "maxContains",
        "minContains",
        "uniqueItems",
    }:
        return "array"
    if validator in {"unevaluatedItems", "unevaluatedProperties"}:
        return "unevaluated"
    return _error_category(error)


def _json_fallback(value: Any) -> Any:
    """
    Serialize values which JSON does not know about without their repr.
    """
    if isinstance(value, BaseException):
        return f"<{_qualified_name(value)}>"
    return f"<{type(value).__name__} object>"


def _shrink_to_size(
    result: dict[str, Any],
    max_bytes: int,
    serializer: _SafeErrorSerializer,
) -> dict[str, Any]:
    """
    Reduce a serialized error until it fits within an encoded byte limit.
    """

    def encoded_size(value: Any) -> int:
        text = json.dumps(
            value,
            ensure_ascii=False,
            default=_json_fallback,
        )
        return len(text.encode("utf-8"))

    if encoded_size(result) <= max_bytes:
        return result
    serializer.mark_truncated("size")
    result = dict(result)

    for key in (
        "schema",
        "validator_value",
        "instance",
        "context",
        "cause",
        "parent",
    ):
        if encoded_size(result) <= max_bytes:
            break
        if result.get(key) is not None:
            result[key] = _TRUNCATED_SIZE

    for key in (
        "absolute_schema_path",
        "schema_path",
        "absolute_path",
        "path",
    ):
        if encoded_size(result) <= max_bytes:
            break
        if result.get(key):
            result[key] = _TRUNCATED_SIZE

    message = result.get("message")
    if isinstance(message, str) and encoded_size(result) > max_bytes:
        low, high = 0, len(message)
        while low < high:
            middle = (low + high + 1) // 2
            result["message"] = message[:middle]
            result["summary"] = message[:middle]
            if encoded_size(result) <= max_bytes:
                low = middle
            else:
                high = middle - 1
        result["message"] = result["summary"] = message[:low] + "..."

    result["redacted"] = serializer.redacted
    result["truncated"] = True
    result["truncation"] = sorted(serializer.truncation)

    if encoded_size(result) > max_bytes:
        fallback = {
            "type": result.get("type"),
            "category": result.get("category"),
            "message": "...",
            "truncated": True,
            "truncation": sorted(serializer.truncation),
        }
        if encoded_size(fallback) <= max_bytes:
            return fallback
        return _smallest_fitting(max_bytes, serializer.truncation)
    return result


def _smallest_fitting(
    max_bytes: int,
    truncation: Iterable[str],
) -> dict[str, Any]:
    """
    Return the smallest mapping which still marks truncation and fits a limit.

    An extremely small limit (one smaller than any useful marker) yields an
    empty mapping, since no valid JSON mapping can both fit and say anything
    more.
    """
    for candidate in (
        {
            "truncated": True,
            "truncation": sorted(truncation),
        },
        {"truncated": True},
        {"truncated": 1},
        {},
    ):
        if len(json.dumps(candidate).encode("utf-8")) <= max_bytes:
            return candidate
    return {}


_ALIASES = {
    "context_limit": "max_context",
    "depth": "max_depth",
    "fields": "sensitive_fields",
    "is_sensitive": "field_policy",
    "max_chars": "max_length",
    "max_entries": "max_items",
    "max_string_length": "max_length",
    "paths": "sensitive_paths",
    "policy_fn": "field_policy",
    "redact_fields": "sensitive_fields",
    "redact_paths": "sensitive_paths",
    "schema_paths": "sensitive_schema_paths",
    "size_limit": "max_bytes",
}


def serialize_error(
    error: BaseException,
    policy: RedactionPolicy | None = None,
    *,
    sensitive_fields: Any = (),
    sensitive_schema_paths: Any = (),
    sensitive_paths: Any = (),
    field_policy: Any = None,
    mask: str = "***",
    max_depth: int | None = 10,
    max_length: int | None = 1000,
    max_items: int | None = 100,
    max_context: int | None = 50,
    max_bytes: int | None = None,
    include_context: bool = True,
    include_cause: bool = True,
    include_cause_message: bool = False,
    redact_field_names: bool = False,
    **aliases: Any,
) -> dict[str, Any]:
    """
    Serialize an exception into a bounded, safe, JSON-compatible mapping.

    The original exception is only ever read. In particular its `message`,
    `instance`, `context` and `cause` are left untouched, so that other
    consumers of the error continue to see the original values.

    Values selected by the policy are replaced by ``mask``. The failed
    keyword, the instance location and a category for the reason for failure
    are always kept, so logs remain useful even when every value is masked.

    A parent error's message is scrubbed of every value which was masked
    anywhere in its `context`, and its `parent` is described only by its type,
    keyword and location -- never by its own message or values. A `cause` is
    likewise reduced to its type and an explicit summary, and unknown custom
    exceptions are never expanded with their own ``repr`` or ``str``.

    Arguments:

        error:

            the exception to serialize

        policy:

            an optional `RedactionPolicy`, to which any other policy
            arguments are added

        sensitive_fields:

            field names whose values must be masked

        sensitive_schema_paths:

            schema paths identifying sensitive fields, as sequences, JSON
            pointers, JSON paths, or dotted paths

        sensitive_paths:

            additional instance paths to mask

        field_policy:

            a callable, mapping, or object with an ``is_sensitive`` method

        mask:

            the replacement value for a sensitive value

        max_depth:

            maximum nesting depth for values and nested errors

        max_length:

            maximum length of individual strings

        max_items:

            maximum number of entries serialized from a container

        max_context:

            maximum number of nested errors serialized from ``context``

        max_bytes:

            optional limit for the UTF-8 encoded size of the result

        include_context:

            whether to include nested errors

        include_cause:

            whether to include a summary of the cause

        include_cause_message:

            whether to include a scrubbed message for known built-in causes.
            Unknown custom causes are always reduced to a summary.

        redact_field_names:

            whether to replace matching field names as well as their values

    Returns:

        a JSON-compatible mapping. ``truncated`` is true whenever any limit
        changed the output, and ``truncation`` lists the reasons why.

    """
    unknown_aliases = set(aliases) - set(_ALIASES)
    if unknown_aliases:
        names = ", ".join(sorted(unknown_aliases))
        raise TypeError(f"Unknown safe serialization argument(s): {names}")

    for alias, value in aliases.items():
        target = _ALIASES[alias]
        if target == "max_context":
            max_context = value
        elif target == "max_depth":
            max_depth = value
        elif target == "sensitive_fields":
            sensitive_fields = value
        elif target == "field_policy":
            field_policy = value
        elif target == "max_length":
            max_length = value
        elif target == "max_items":
            max_items = value
        elif target == "sensitive_paths":
            sensitive_paths = value
        elif target == "sensitive_schema_paths":
            sensitive_schema_paths = value
        elif target == "max_bytes":
            max_bytes = value

    if policy is None:
        policy = RedactionPolicy(
            sensitive_fields=sensitive_fields,
            sensitive_schema_paths=sensitive_schema_paths,
            sensitive_paths=sensitive_paths,
            field_policy=field_policy,
            mask=mask,
            redact_field_names=redact_field_names,
        )
    else:
        policy = policy.with_(
            sensitive_fields=sensitive_fields,
            sensitive_schema_paths=sensitive_schema_paths,
            sensitive_paths=sensitive_paths,
            field_policy=field_policy,
            mask=mask if mask != "***" else None,
            redact_field_names=(
                redact_field_names if redact_field_names else None
            ),
        )

    serializer = _SafeErrorSerializer(
        policy,
        max_depth=max_depth,
        max_length=max_length,
        max_items=max_items,
        max_context=max_context,
        include_context=include_context,
        include_cause=include_cause,
        include_cause_message=include_cause_message,
    )
    result = serializer.serialize(error)
    if max_bytes is not None:
        result = _shrink_to_size(result, max_bytes, serializer)
    return result


def dump_error(
    error: BaseException,
    *,
    indent: int | None = None,
    sort_keys: bool = False,
    max_bytes: int | None = None,
    **kwargs: Any,
) -> str:
    """
    Serialize an exception to a JSON string via `serialize_error`.

    All arguments other than ``indent``, ``sort_keys`` and ``max_bytes`` are
    passed through to `serialize_error`. When ``max_bytes`` is given, the
    returned text is guaranteed to fit within it whenever that is possible.
    """
    if max_bytes is not None:
        kwargs["max_bytes"] = max_bytes

    result = serialize_error(error, **kwargs)
    text = _dump_json(result, indent=indent, sort_keys=sort_keys)
    while (
        max_bytes is not None
        and max_bytes > 1
        and len(text.encode("utf-8")) > max_bytes
    ):
        max_bytes = max(1, max_bytes // 2)
        kwargs["max_bytes"] = max_bytes
        result = serialize_error(error, **kwargs)
        text = _dump_json(result, indent=indent, sort_keys=sort_keys)
    if max_bytes is not None and len(text.encode("utf-8")) > max_bytes:
        text = json.dumps(
            result,
            ensure_ascii=False,
            separators=(",", ":"),
            default=_json_fallback,
        )
    return text


def _dump_json(
    result: Any,
    *,
    indent: int | None,
    sort_keys: bool,
) -> str:
    return json.dumps(
        result,
        ensure_ascii=False,
        indent=indent,
        sort_keys=sort_keys,
        default=_json_fallback,
    )


safe_error = serialize_error
safe_error_dict = serialize_error
safe_serialize = serialize_error
redact_error = serialize_error

dumps_error = dump_error
safe_error_json = dump_error


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
