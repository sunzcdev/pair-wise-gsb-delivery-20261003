"""
Static linting for JSON Schema documents.

The functions in this module intentionally do not participate in instance
validation.  They inspect schema documents themselves, so callers can find
keywords which their chosen dialect or extension vocabularies do not
recognize without changing what validation accepts or rejects.
"""

from __future__ import annotations

from collections.abc import Iterable, Mapping
from difflib import get_close_matches
from functools import lru_cache
from typing import Any
from urllib.parse import urldefrag, urljoin

from attrs import define, field
from jsonschema_specifications import REGISTRY as SPECIFICATIONS
import referencing
import referencing.exceptions
import referencing.jsonschema

_REFERENCE_KEYWORDS = frozenset({"$ref", "$dynamicRef", "$recursiveRef"})

_SCHEMA_VALUE_KEYWORDS = frozenset(
    {
        "additionalItems",
        "additionalProperties",
        "contains",
        "contentSchema",
        "else",
        "if",
        "not",
        "propertyNames",
        "then",
        "unevaluatedItems",
        "unevaluatedProperties",
    },
)

_SCHEMA_ARRAY_KEYWORDS = frozenset(
    {"allOf", "anyOf", "oneOf", "prefixItems"},
)

_SCHEMA_MAP_KEYWORDS = frozenset(
    {
        "$defs",
        "definitions",
        "dependentSchemas",
        "patternProperties",
        "properties",
    },
)

_META_SCHEMA_VALUE_KEYWORDS = _SCHEMA_VALUE_KEYWORDS | {"extends", "items"}

_META_SCHEMA_MAP_KEYWORDS = frozenset(
    {"$defs", "definitions", "dependentSchemas"},
)

_RESOLUTION_ERRORS = (
    referencing.exceptions.CannotDetermineSpecification,
    referencing.exceptions.NoSuchResource,
    referencing.exceptions.Unresolvable,
    referencing.exceptions.Unretrievable,
)


def _as_keyword_set(value: Iterable[str] | str) -> frozenset[str]:
    if isinstance(value, str):
        return frozenset({value})
    return frozenset(value)


def _as_prefix_tuple(value: Iterable[str] | str) -> tuple[str, ...]:
    if isinstance(value, str):
        return (value,)
    return tuple(value)


@define(frozen=True)
class ExtensionVocabulary:
    """
    Keywords and keyword prefixes declared by a caller.

    ``keywords`` contains complete keyword names which should be accepted.
    ``prefixes`` contains namespace prefixes; any keyword beginning with one
    of them is accepted.
    """

    keywords: frozenset[str] = field(
        factory=frozenset,
        converter=_as_keyword_set,
    )
    prefixes: tuple[str, ...] = field(
        factory=tuple,
        converter=_as_prefix_tuple,
    )


@define(frozen=True)
class LintIssue:
    """
    A single static diagnostic found in a schema document.

    ``resource`` identifies the schema resource (after resolving ``$id`` or
    ``id``) and ``pointer`` is a JSON Pointer within that resource.  A
    ``suggestion`` is only a hint -- linting never modifies the schema.
    """

    resource: str
    pointer: str
    keyword: str
    suggestion: str | None = None

    @property
    def code(self) -> str:
        """Return a stable machine-readable diagnostic code."""
        return "unknown-keyword"

    @property
    def message(self) -> str:
        """Return a human-readable description of the issue."""
        message = f"Unknown keyword {self.keyword!r}"
        if self.suggestion is not None:
            message += f". Did you mean {self.suggestion!r}?"
        return message

    def __str__(self) -> str:
        location = self.resource or "<root>"
        pointer = self.pointer or "/"
        return f"{location}{pointer}: {self.message}"


@define(frozen=True)
class _Dialect:
    cls: type[Any]
    keywords: frozenset[str]
    id_of: Any
    specification: Any


@define
class _Context:
    default_dialect: _Dialect
    registry: referencing.Registry[Any]
    follow_references: bool
    extension_keywords: frozenset[str]
    extension_prefixes: tuple[str, ...]
    issues: dict[tuple[str, str, str], LintIssue] = field(factory=dict)
    linted_resources: set[str] = field(factory=set)


def _collect_meta_schema_keywords(
    node: Any,
    resolver: referencing.Resolver[Any] | None,
    seen: set[int],
    keywords: set[str],
) -> None:
    if not isinstance(node, Mapping):
        return

    marker = id(node)
    if marker in seen:
        return
    seen.add(marker)

    properties = node.get("properties")
    if isinstance(properties, Mapping):
        keywords.update(
            each for each in properties if isinstance(each, str)
        )

    if resolver is not None:
        for keyword in _REFERENCE_KEYWORDS:
            ref = node.get(keyword)
            if not isinstance(ref, str):
                continue
            try:
                resolved = resolver.lookup(ref)
            except _RESOLUTION_ERRORS:
                continue
            _collect_meta_schema_keywords(
                resolved.contents,
                resolver,
                seen,
                keywords,
            )

    for keyword in _META_SCHEMA_VALUE_KEYWORDS:
        if keyword in node:
            value = node[keyword]
            if keyword == "items" and isinstance(value, (list, tuple)):
                for each in value:
                    _collect_meta_schema_keywords(
                        each,
                        resolver,
                        seen,
                        keywords,
                    )
            else:
                _collect_meta_schema_keywords(
                    value,
                    resolver,
                    seen,
                    keywords,
                )

    for keyword in _SCHEMA_ARRAY_KEYWORDS:
        value = node.get(keyword)
        if isinstance(value, (list, tuple)):
            for each in value:
                _collect_meta_schema_keywords(
                    each,
                    resolver,
                    seen,
                    keywords,
                )

    for keyword in _META_SCHEMA_MAP_KEYWORDS:
        value = node.get(keyword)
        if isinstance(value, Mapping):
            for each in value.values():
                _collect_meta_schema_keywords(
                    each,
                    resolver,
                    seen,
                    keywords,
                )


@lru_cache(maxsize=None)
def _dialect_for_cls(cls: type[Any]) -> _Dialect:
    meta_schema = cls.META_SCHEMA
    dialect_id = cls.ID_OF(meta_schema) or "urn:unknown-dialect"
    specification = referencing.jsonschema.specification_with(
        dialect_id=dialect_id,
        default=referencing.Specification.OPAQUE,
    )

    resolver = SPECIFICATIONS.resolver_with_root(
        specification.create_resource(meta_schema),
    )
    keywords = set(cls.VALIDATORS)
    _collect_meta_schema_keywords(
        meta_schema,
        resolver,
        set(),
        keywords,
    )
    return _Dialect(
        cls=cls,
        keywords=frozenset(keywords),
        id_of=cls.ID_OF,
        specification=specification,
    )


def _dialect_for_contents(contents: Any, default: _Dialect) -> _Dialect:
    if not isinstance(contents, Mapping):
        return default
    if not isinstance(contents.get("$schema"), str):
        return default

    from jsonschema.validators import validator_for

    cls = validator_for(contents, default=default.cls)
    if cls is default.cls:
        return default
    return _dialect_for_cls(cls)


def _normalize_extensions(
    extensions: Any,
    extra_keywords: Iterable[str],
    extra_prefixes: Iterable[str],
) -> tuple[frozenset[str], tuple[str, ...]]:
    if isinstance(extra_keywords, str):
        extra_keywords = (extra_keywords,)
    if isinstance(extra_prefixes, str):
        extra_prefixes = (extra_prefixes,)

    keywords = {each for each in extra_keywords if isinstance(each, str)}
    prefixes = [each for each in extra_prefixes if isinstance(each, str)]

    def add(extension: Any) -> None:
        if extension is None or extension is False:
            return
        if isinstance(extension, ExtensionVocabulary):
            keywords.update(extension.keywords)
            prefixes.extend(extension.prefixes)
        elif isinstance(extension, str):
            keywords.add(extension)
        elif isinstance(extension, Mapping):
            for namespace, names in extension.items():
                namespace = str(namespace)
                if names is None:
                    prefixes.append(namespace)
                elif names is True:
                    keywords.add(namespace)
                elif names is False:
                    continue
                elif isinstance(names, str):
                    if namespace.endswith(("#", "/", ":", ".", "-")):
                        keywords.add(namespace + names)
                    else:
                        keywords.add(names)
                elif isinstance(names, Iterable):
                    for name in names:
                        name = str(name)
                        if namespace.endswith(("#", "/", ":", ".", "-")):
                            keywords.add(namespace + name)
                        else:
                            keywords.add(name)
        elif isinstance(extension, Iterable):
            for each in extension:
                add(each)

    add(extensions)
    return frozenset(keywords), tuple(dict.fromkeys(prefixes))


def _escape(token: str) -> str:
    return token.replace("~", "~0").replace("/", "~1")


def _pointer(prefix: str, *tokens: str) -> str:
    return prefix + "".join(f"/{_escape(token)}" for token in tokens)


def _root_resource_uri(
    dialect: _Dialect,
    schema: Any,
    base_uri: str,
) -> str:
    if isinstance(schema, Mapping):
        id_value = dialect.id_of(schema)
    else:
        id_value = None
    if isinstance(id_value, str) and id_value:
        uri = urljoin(base_uri, id_value)
    else:
        uri = base_uri
    return urldefrag(uri)[0].rstrip("#")


def _keyword_is_known(
    keyword: str,
    dialect: _Dialect,
    context: _Context,
) -> bool:
    if keyword in dialect.keywords:
        return True
    if keyword in context.extension_keywords:
        return True
    return any(
        keyword.startswith(prefix)
        for prefix in context.extension_prefixes
    )


def _suggest(
    keyword: str,
    dialect: _Dialect,
    context: _Context,
) -> str | None:
    candidates = dialect.keywords | context.extension_keywords
    matches = get_close_matches(
        keyword,
        sorted(candidates),
        n=1,
        cutoff=0.75,
    )
    return matches[0] if matches else None


def _record_unknown(
    resource: str,
    pointer: str,
    keyword: str,
    dialect: _Dialect,
    context: _Context,
) -> None:
    key = (resource, pointer, keyword)
    if key in context.issues:
        return
    context.issues[key] = LintIssue(
        resource=resource,
        pointer=pointer,
        keyword=keyword,
        suggestion=_suggest(keyword, dialect, context),
    )


def _follow_reference(
    ref: str,
    resource: str,
    context: _Context,
) -> None:
    if not context.follow_references:
        return

    document, _ = urldefrag(urljoin(resource, ref))
    document = document.rstrip("#")
    if not document or document == resource:
        return
    if document in context.linted_resources:
        return

    try:
        retrieved = context.registry.get_or_retrieve(document)
    except _RESOLUTION_ERRORS:
        return

    target = retrieved.value
    target_id = target.id()
    if target_id is None:
        target_resource = document
    else:
        target_resource = urldefrag(urljoin(document, target_id))[0]
    target_resource = target_resource.rstrip("#")

    context.registry = retrieved.registry
    target_dialect = _dialect_for_contents(
        target.contents,
        context.default_dialect,
    )
    _walk(
        target.contents,
        target_resource,
        "",
        target_dialect,
        context,
    )


def _walk_keyword_value(
    keyword: str,
    value: Any,
    resource: str,
    pointer: str,
    dialect: _Dialect,
    context: _Context,
) -> None:
    if keyword == "items":
        if isinstance(value, Mapping):
            _walk(
                value,
                resource,
                _pointer(pointer, keyword),
                dialect,
                context,
            )
        elif isinstance(value, (list, tuple)):
            for index, each in enumerate(value):
                _walk(
                    each,
                    resource,
                    _pointer(pointer, keyword, str(index)),
                    dialect,
                    context,
                )
    elif keyword == "extends":
        if isinstance(value, Mapping):
            _walk(
                value,
                resource,
                _pointer(pointer, keyword),
                dialect,
                context,
            )
        elif isinstance(value, (list, tuple)):
            for index, each in enumerate(value):
                _walk(
                    each,
                    resource,
                    _pointer(pointer, keyword, str(index)),
                    dialect,
                    context,
                )
    elif keyword == "dependencies":
        if isinstance(value, Mapping):
            for name, each in value.items():
                if isinstance(each, Mapping):
                    _walk(
                        each,
                        resource,
                        _pointer(pointer, keyword, str(name)),
                        dialect,
                        context,
                    )
    elif keyword in {"type", "disallow"}:
        if isinstance(value, Mapping):
            _walk(
                value,
                resource,
                _pointer(pointer, keyword),
                dialect,
                context,
            )
        elif isinstance(value, (list, tuple)):
            for index, each in enumerate(value):
                if isinstance(each, Mapping):
                    _walk(
                        each,
                        resource,
                        _pointer(pointer, keyword, str(index)),
                        dialect,
                        context,
                    )
    elif keyword in _SCHEMA_VALUE_KEYWORDS:
        _walk(
            value,
            resource,
            _pointer(pointer, keyword),
            dialect,
            context,
        )
    elif keyword in _SCHEMA_ARRAY_KEYWORDS:
        if isinstance(value, (list, tuple)):
            for index, each in enumerate(value):
                _walk(
                    each,
                    resource,
                    _pointer(pointer, keyword, str(index)),
                    dialect,
                    context,
                )
    elif keyword in _SCHEMA_MAP_KEYWORDS:
        if isinstance(value, Mapping):
            for name, each in value.items():
                _walk(
                    each,
                    resource,
                    _pointer(pointer, keyword, str(name)),
                    dialect,
                    context,
                )


def _walk(
    schema: Any,
    resource: str,
    pointer: str,
    dialect: _Dialect,
    context: _Context,
) -> None:
    if not isinstance(schema, Mapping):
        if pointer == "":
            context.linted_resources.add(resource)
        return

    id_value = dialect.id_of(schema)
    if isinstance(id_value, str) and id_value:
        absolute = urljoin(resource, id_value)
        candidate, _ = urldefrag(absolute)
        candidate = candidate.rstrip("#")
        if candidate and candidate != resource:
            resource = candidate
            pointer = ""
            dialect = _dialect_for_contents(schema, dialect)

    if pointer == "":
        if resource in context.linted_resources:
            return
        context.linted_resources.add(resource)

    for keyword, value in schema.items():
        if not isinstance(keyword, str):
            continue

        keyword_pointer = _pointer(pointer, keyword)
        if not _keyword_is_known(keyword, dialect, context):
            _record_unknown(
                resource,
                keyword_pointer,
                keyword,
                dialect,
                context,
            )
            continue

        if keyword in _REFERENCE_KEYWORDS and isinstance(value, str):
            _follow_reference(value, resource, context)
        _walk_keyword_value(
            keyword,
            value,
            resource,
            pointer,
            dialect,
            context,
        )


def lint(
    schema: Any,
    *,
    cls: type[Any] | None = None,
    registry: referencing.Registry[Any] | None = None,
    extensions: Any = (),
    extra_keywords: Iterable[str] = (),
    extra_prefixes: Iterable[str] = (),
    follow_references: bool = True,
    base_uri: str = "",
) -> list[LintIssue]:
    """
    Return static diagnostics for unrecognized keywords in a schema.

    This is an opt-in inspection and does not alter validation behavior.  The
    known keyword set comes from the selected dialect (``cls`` or the schema's
    ``$schema``), plus any caller-declared extension vocabulary.

    Arguments:

        schema:

            The schema document to inspect.

        cls:

            The validator class whose dialect should be used.  If omitted, the
            schema's ``$schema`` URI selects it, falling back to the latest
            supported draft.

        registry:

            A `referencing.Registry` containing referenced documents.  When
            provided, registered resources reachable through ``$ref`` are
            linted as well.

        extensions:

            Caller-declared extension keywords.  May be an
            `ExtensionVocabulary`, an iterable of exact keyword names, or a
            mapping whose keys are namespace prefixes and whose values are
            local keyword names.  A mapping value of ``None`` declares the key
            itself as a prefix, and a value of ``True`` declares the key as an
            exact keyword.  Mapping keys not ending in a namespace separator
            (``#``, ``/``, ``:``, ``.``, or ``-``) are treated as vocabulary
            identifiers, with their values as exact keyword names.

        extra_keywords:

            Additional exact keyword names to accept.

        extra_prefixes:

            Additional namespace prefixes to accept.

        follow_references:

            Whether registered documents referenced from the schema should be
            inspected too.  Defaults to ``True``.

        base_uri:

            A base URI to use when the root schema does not declare one.

    Returns:

        A list of `LintIssue` objects sorted by resource, JSON Pointer, and
        keyword.
    """
    if cls is None:
        from jsonschema.validators import validator_for

        cls = validator_for(schema)
    if registry is None:
        registry = referencing.Registry()

    dialect = _dialect_for_cls(cls)
    extension_keywords, extension_prefixes = _normalize_extensions(
        extensions,
        extra_keywords,
        extra_prefixes,
    )
    root_resource = _root_resource_uri(dialect, schema, base_uri)
    registry = registry.with_resource(
        root_resource,
        dialect.specification.create_resource(schema),
    )
    context = _Context(
        default_dialect=dialect,
        registry=registry,
        follow_references=follow_references,
        extension_keywords=extension_keywords,
        extension_prefixes=extension_prefixes,
    )

    _walk(
        schema,
        root_resource,
        "",
        dialect,
        context,
    )
    return sorted(
        context.issues.values(),
        key=lambda issue: (issue.resource, issue.pointer, issue.keyword),
    )
