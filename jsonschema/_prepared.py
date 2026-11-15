"""
Reusable preparation of a schema for validating many instances.

Preparing a schema walks it exactly once, precompiling the regular
expressions which the ``pattern`` and ``patternProperties`` keywords
will actually use, along with the location (resource URI and JSON
pointer) where each was found. Validating many instances against the
resulting `PreparedValidator` then reuses the compiled expressions
instead of recompiling -- and potentially re-reporting configuration
errors in -- the same expressions for every instance.
"""

from __future__ import annotations

from collections.abc import Iterator, Mapping
from types import MappingProxyType
from typing import Any, NamedTuple
from urllib.parse import urldefrag, urljoin
import copy
import re

from jsonschema_specifications import REGISTRY as SPECIFICATIONS
import referencing.jsonschema

from jsonschema.exceptions import ValidationError
from jsonschema.validators import (
    _REMOTE_WARNING_REGISTRY,
    extend,
    validator_for,
)

__all__ = [
    "PatternLocation",
    "PreparationError",
    "PreparedValidator",
    "prepare",
]


# Keywords whose values are themselves subschemas. ``items`` (pre
# 2020-12) and ``extends`` (draft 3) may also hold arrays of subschemas.
_IN_VALUE = frozenset({
    "additionalItems",
    "additionalProperties",
    "contains",
    "else",
    "extends",
    "if",
    "items",
    "not",
    "propertyNames",
    "then",
    "unevaluatedItems",
    "unevaluatedProperties",
})
# Keywords whose values are mappings of names to subschemas.
# ``dependencies`` (pre 2019-09) may also hold arrays of strings, which
# are not schemas and are skipped.
_IN_SUBVALUES = frozenset({
    "$defs",
    "definitions",
    "dependencies",
    "dependentSchemas",
    "properties",
})
# Keywords whose values are arrays of subschemas.
_IN_SUBARRAY = frozenset({"allOf", "anyOf", "oneOf", "prefixItems"})
# Keywords holding references to other (possibly external) resources.
_REFERENCE_KEYS = frozenset({"$dynamicRef", "$recursiveRef", "$ref"})


class PatternLocation(NamedTuple):
    """
    The location of a regular expression within a schema.

    Attributes:

        uri:

            the URI of the schema resource containing the expression,
            or the empty string for an unidentified root resource

        pointer:

            a JSON pointer (RFC 6901) to the expression within that
            resource
    """

    uri: str
    pointer: str

    def __str__(self) -> str:
        return f"{self.uri}#{self.pointer}"


class PreparationError(Exception):
    """
    An error discovered while preparing a schema for reuse.

    Raised when a ``pattern`` or ``patternProperties`` value in the
    schema is not a valid regular expression. The offending expression
    and its location are available as attributes.
    """

    def __init__(
        self,
        pattern: str,
        uri: str,
        pointer: str,
        error: re.error,
    ) -> None:
        self.pattern = pattern
        self.uri = uri
        self.pointer = pointer
        self.error = error
        location = PatternLocation(uri=uri, pointer=pointer)
        super().__init__(f"invalid pattern {pattern!r} at {location}: {error}")


def _escape(segment: object) -> str:
    return str(segment).replace("~", "~0").replace("/", "~1")


class _PatternCollector:
    """
    Walk schema resources, collecting and compiling their patterns.

    Identical expressions, whether repeated within one resource or
    shared across referenced resources, are compiled only once, while
    each occurrence's location is recorded individually.
    """

    def __init__(self, id_of) -> None:
        self._id_of = id_of
        self.patterns: dict[str, re.Pattern] = {}
        self.locations: dict[str, list[PatternLocation]] = {}

    def walk_resource(self, contents: Any, uri: str) -> Iterator[str]:
        """
        Walk one resource, returning references found along the way.
        """
        references: list[str] = []
        self._walk(contents, uri, "", references)
        return iter(references)

    def _collect(self, pattern: str, uri: str, pointer: str) -> None:
        self.locations.setdefault(pattern, []).append(
            PatternLocation(uri=uri, pointer=pointer),
        )
        if pattern not in self.patterns:
            try:
                self.patterns[pattern] = re.compile(pattern)
            except re.error as error:
                raise PreparationError(
                    pattern=pattern,
                    uri=uri,
                    pointer=pointer,
                    error=error,
                ) from error

    def _walk(
        self,
        schema: Any,
        uri: str,
        pointer: str,
        references: list[str],
    ) -> None:
        if not isinstance(schema, Mapping):
            return

        pattern = schema.get("pattern")
        if isinstance(pattern, str):
            self._collect(pattern, uri, f"{pointer}/pattern")

        pattern_properties = schema.get("patternProperties")
        if isinstance(pattern_properties, Mapping):
            for key, subschema in pattern_properties.items():
                child = f"{pointer}/patternProperties/{_escape(key)}"
                if isinstance(key, str):
                    self._collect(key, uri, child)
                self._walk_child(subschema, uri, child, references)

        for key, value in schema.items():
            if key in ("pattern", "patternProperties"):
                continue
            if key in _REFERENCE_KEYS:
                if isinstance(value, str):
                    references.append(value)
            elif key in _IN_VALUE:
                if isinstance(value, Mapping):
                    child = f"{pointer}/{_escape(key)}"
                    self._walk_child(value, uri, child, references)
                elif isinstance(value, list):
                    for i, each in enumerate(value):
                        child = f"{pointer}/{_escape(key)}/{i}"
                        self._walk_child(each, uri, child, references)
            elif key in _IN_SUBVALUES:
                if isinstance(value, Mapping):
                    for name, each in value.items():
                        child = f"{pointer}/{_escape(key)}/{_escape(name)}"
                        self._walk_child(each, uri, child, references)
            elif key in _IN_SUBARRAY:
                if isinstance(value, list):
                    for i, each in enumerate(value):
                        child = f"{pointer}/{_escape(key)}/{i}"
                        self._walk_child(each, uri, child, references)

    def _walk_child(
        self,
        subschema: Any,
        uri: str,
        pointer: str,
        references: list[str],
    ) -> None:
        if not isinstance(subschema, Mapping):
            return  # boolean (or invalid) subschemas contain no patterns
        if self._id_of(subschema):
            # A subschema which starts a new resource. It is walked
            # separately, rooted at its own URI.
            return
        self._walk(subschema, uri, pointer, references)


def _pattern(validator, patrn, instance, schema):
    if not validator.is_type(instance, "string"):
        return
    patterns = getattr(validator, "_prepared_patterns", None)
    compiled = patterns.get(patrn) if patterns is not None else None
    if compiled is not None:
        if not compiled.search(instance):
            yield ValidationError(f"{instance!r} does not match {patrn!r}")
    elif not re.search(patrn, instance):
        yield ValidationError(f"{instance!r} does not match {patrn!r}")


def _patternProperties(validator, patternProperties, instance, schema):
    if not validator.is_type(instance, "object"):
        return
    patterns = getattr(validator, "_prepared_patterns", None) or {}
    for pattern, subschema in patternProperties.items():
        compiled = patterns.get(pattern)
        for k, v in instance.items():
            if compiled is not None:
                matched = compiled.search(k)
            else:
                matched = re.search(pattern, k)
            if matched:
                yield from validator.descend(
                    v,
                    subschema,
                    path=k,
                    schema_path=pattern,
                )


class PreparedValidator:
    """
    A validator bound to a frozen schema with precompiled patterns.

    Returned by `prepare`. Validates instances exactly as the validator
    class it was prepared from would, but reuses the regular
    expressions compiled during preparation.

    Attributes:

        patterns:

            a mapping of each regular expression found in the schema to
            its compiled form

        locations:

            a mapping of each regular expression to the locations
            (`PatternLocation`\\ s) where it appears in the schema
    """

    def __init__(self, validator, patterns, locations) -> None:
        self._validator = validator
        self._patterns = MappingProxyType(patterns)
        self._locations = MappingProxyType({
            pattern: tuple(found) for pattern, found in locations.items()
        })

    def __repr__(self) -> str:
        return f"PreparedValidator(schema={self.schema!r})"

    @property
    def validator(self):
        """
        The underlying validator instance.
        """
        return self._validator

    @property
    def schema(self):
        """
        The frozen copy of the schema this validator was prepared from.
        """
        return self._validator.schema

    @property
    def patterns(self) -> Mapping[str, re.Pattern]:
        """
        Each pattern found in the schema, in compiled form.
        """
        return self._patterns

    @property
    def locations(self) -> Mapping[str, tuple[PatternLocation, ...]]:
        """
        The locations where each pattern appears in the schema.
        """
        return self._locations

    def iter_errors(self, instance) -> Iterator[ValidationError]:
        """
        Lazily yield each validation error for the given instance.
        """
        return self._validator.iter_errors(instance)

    def is_valid(self, instance) -> bool:
        """
        Check whether the given instance is valid under the schema.
        """
        return self._validator.is_valid(instance)

    def validate(self, instance, *args, **kwargs) -> None:
        """
        Raise `jsonschema.ValidationError` if the instance is invalid.
        """
        self._validator.validate(instance, *args, **kwargs)


def prepare(
    schema: Mapping[str, Any] | bool,
    cls=None,
    registry=None,
    format_checker=None,
) -> PreparedValidator:
    """
    Prepare a schema for repeatedly validating instances against it.

    The schema is walked once, precompiling the regular expressions
    used by its ``pattern`` and ``patternProperties`` keywords --
    including those in referenced resources present in ``registry`` --
    and recording the URI and JSON pointer of each. Identical
    expressions appearing in multiple places are compiled only once.

    The schema is deep-copied (frozen) during preparation, so later
    mutation of the original schema object does not affect the
    returned `PreparedValidator`. Resources in ``registry`` are used
    as-is and should similarly not be mutated afterwards.

    Arguments:

        schema:

            the schema to prepare

        cls:

            the validator class to use. If unprovided, it is inferred
            from the schema via `jsonschema.validators.validator_for`.

        registry:

            a `referencing.Registry` of additional resources the schema
            may reference

        format_checker:

            a format checker to use when validating

    Raises:

        PreparationError:

            if a ``pattern`` or ``patternProperties`` value in the
            schema is not a valid regular expression. The error
            carries the ``uri`` and ``pointer`` of the offending
            expression.
    """
    if cls is None:
        cls = validator_for(schema)

    frozen = copy.deepcopy(schema)

    specification = referencing.jsonschema.specification_with(
        dialect_id=cls.ID_OF(cls.META_SCHEMA) or "urn:unknown-dialect",
        default=referencing.Specification.OPAQUE,
    )
    resource = specification.create_resource(frozen)
    root_uri = resource.id() or ""

    if registry is None:
        full = _REMOTE_WARNING_REGISTRY
    else:
        full = SPECIFICATIONS.combine(registry)
    full = full.with_resource(root_uri, resource).crawl()
    resources = {uri: full[uri] for uri in full}

    collector = _PatternCollector(id_of=specification.id_of)
    seen: set[str] = set()
    pending = [root_uri]
    while pending:
        uri = pending.pop()
        if uri in seen:
            continue
        seen.add(uri)
        found = resources.get(uri)
        if found is None:
            # Referenced, but not present in the registry. As with
            # unprepared validation, resolution is left to validation
            # time.
            continue
        for ref in collector.walk_resource(found.contents, uri):
            target, _ = urldefrag(urljoin(uri, ref))
            if target not in seen:
                pending.append(target)

    Prepared = extend(
        cls,
        {"pattern": _pattern, "patternProperties": _patternProperties},
    )
    Prepared._prepared_patterns = collector.patterns

    kwargs: dict[str, Any] = {"schema": frozen}
    if registry is not None:
        kwargs["registry"] = registry
    if format_checker is not None:
        kwargs["format_checker"] = format_checker

    return PreparedValidator(
        validator=Prepared(**kwargs),
        patterns=collector.patterns,
        locations=collector.locations,
    )
