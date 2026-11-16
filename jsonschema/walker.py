"""
Dialect-aware traversal of every subschema within a JSON Schema.

Unlike a naive recursive walk over every nested mapping, the traversal here
follows the structural semantics of each supported dialect. Only locations
which actually hold a subschema are visited, so arbitrary data which merely
happens to live inside a schema -- such as ``default`` or ``examples``
values, or an object appearing inside an ``enum`` -- is never mistaken for
part of the contract.
"""

from __future__ import annotations

from collections.abc import Iterator, Mapping, Sequence
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any
from urllib.parse import urldefrag, urljoin

from attrs import frozen
import referencing
import referencing.exceptions
import referencing.jsonschema

from jsonschema.validators import (
    Draft201909Validator,
    Draft202012Validator,
    Draft3Validator,
    Draft4Validator,
    Draft6Validator,
    Draft7Validator,
    validator_for,
)

if TYPE_CHECKING:
    from referencing._core import Resolver

    from jsonschema.protocols import Validator


def _escape(segment: str) -> str:
    return segment.replace("~", "~0").replace("/", "~1")


@frozen
class _Dialect:
    """
    Structural information describing where subschemas may appear.

    The keyword sets describe the shape of each keyword's value:

    * ``single`` -- the value is itself a (possibly boolean) subschema,
    * ``arrays`` -- the value is an array of subschemas,
    * ``maps`` -- the value is an object whose values are subschemas,
    * ``special`` -- dialect-dependent shapes handled explicitly,
    * ``refs`` -- the value is a reference string, not an inline schema.
    """

    id_keyword: str
    fragment_only_id_is_anchor: bool
    anchor_keywords: tuple[str, ...]
    single: frozenset[str]
    arrays: frozenset[str]
    maps: frozenset[str]
    special: frozenset[str]
    refs: frozenset[str]


_DRAFT3 = _Dialect(
    id_keyword="id",
    fragment_only_id_is_anchor=True,
    anchor_keywords=(),
    single=frozenset({"additionalItems", "additionalProperties"}),
    arrays=frozenset({"extends"}),
    maps=frozenset({"definitions", "patternProperties", "properties"}),
    special=frozenset({"dependencies", "disallow", "items", "type"}),
    refs=frozenset({"$ref"}),
)

_DRAFT4 = _Dialect(
    id_keyword="id",
    fragment_only_id_is_anchor=False,
    anchor_keywords=(),
    single=frozenset(
        {
            "additionalItems",
            "additionalProperties",
            "contains",
            "not",
        },
    ),
    arrays=frozenset({"allOf", "anyOf", "oneOf"}),
    maps=frozenset({"definitions", "patternProperties", "properties"}),
    special=frozenset({"dependencies", "items"}),
    refs=frozenset({"$ref"}),
)

_DRAFT6 = _Dialect(
    id_keyword="$id",
    fragment_only_id_is_anchor=False,
    anchor_keywords=(),
    single=frozenset(
        {
            "additionalItems",
            "additionalProperties",
            "contains",
            "not",
            "propertyNames",
        },
    ),
    arrays=frozenset({"allOf", "anyOf", "oneOf"}),
    maps=frozenset({"definitions", "patternProperties", "properties"}),
    special=frozenset({"dependencies", "items"}),
    refs=frozenset({"$ref"}),
)

_DRAFT7 = _Dialect(
    id_keyword="$id",
    fragment_only_id_is_anchor=False,
    anchor_keywords=(),
    single=frozenset(
        {
            "additionalItems",
            "additionalProperties",
            "contains",
            "else",
            "if",
            "not",
            "propertyNames",
            "then",
        },
    ),
    arrays=frozenset({"allOf", "anyOf", "oneOf"}),
    maps=frozenset({"definitions", "patternProperties", "properties"}),
    special=frozenset({"dependencies", "items"}),
    refs=frozenset({"$ref"}),
)

_DRAFT201909 = _Dialect(
    id_keyword="$id",
    fragment_only_id_is_anchor=False,
    anchor_keywords=("$anchor",),
    single=frozenset(
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
    ),
    arrays=frozenset({"allOf", "anyOf", "oneOf"}),
    maps=frozenset(
        {
            "$defs",
            "definitions",
            "dependentSchemas",
            "patternProperties",
            "properties",
        },
    ),
    special=frozenset({"items"}),
    refs=frozenset({"$recursiveRef", "$ref"}),
)

_DRAFT202012 = _Dialect(
    id_keyword="$id",
    fragment_only_id_is_anchor=False,
    anchor_keywords=("$anchor", "$dynamicAnchor"),
    single=frozenset(
        {
            "additionalProperties",
            "contains",
            "contentSchema",
            "else",
            "if",
            "items",
            "not",
            "propertyNames",
            "then",
            "unevaluatedItems",
            "unevaluatedProperties",
        },
    ),
    arrays=frozenset({"allOf", "anyOf", "oneOf", "prefixItems"}),
    maps=frozenset(
        {
            "$defs",
            "definitions",
            "dependentSchemas",
            "patternProperties",
            "properties",
        },
    ),
    special=frozenset(),
    refs=frozenset({"$dynamicRef", "$ref"}),
)

_DIALECTS: dict[type[Validator], _Dialect] = {
    Draft3Validator: _DRAFT3,
    Draft4Validator: _DRAFT4,
    Draft6Validator: _DRAFT6,
    Draft7Validator: _DRAFT7,
    Draft201909Validator: _DRAFT201909,
    Draft202012Validator: _DRAFT202012,
}


def _is_schema(value: Any) -> bool:
    return value is True or value is False or isinstance(value, Mapping)


@dataclass(frozen=True)
class SchemaNode:
    """
    A single subschema location found while walking a schema.

    Attributes:

        schema:
            the subschema itself (a mapping, or a boolean schema),

        uri:
            the URI of the resource the subschema belongs to. A resource
            begins at the document root and at every embedded ``$id`` /
            ``id``; nested subschemas without their own identifier share the
            URI of their enclosing resource,

        pointer:
            a JSON Pointer relative to the beginning of the resource at
            :attr:`uri`, identifying this subschema's location within it,

        validator:
            the validator class applicable to the subschema's dialect,

        is_resource_root:
            whether the node is the root of a (possibly embedded) resource,

        ref:
            if the node is a reference keyword location, the raw reference
            string, otherwise `None`.
    """

    schema: Any
    uri: str
    pointer: str
    validator: type[Validator]
    is_resource_root: bool
    ref: str | None = None

    @property
    def absolute_uri(self) -> str:
        """
        The resource URI together with the pointer, when one is present.
        """
        if self.pointer:
            return f"{self.uri}#{self.pointer}"
        return self.uri


@dataclass(frozen=True)
class ReferenceEdge:
    """
    A reference (``$ref`` and friends) discovered while walking.
    """

    source: SchemaNode
    ref: str
    target: SchemaNode
    is_back_edge: bool


class SchemaWalker:
    """
    Walk every subschema of a schema according to its dialect.

    Arguments:

        schema:
            the root schema to traverse, which may be a boolean schema,

        registry:
            an optional `referencing.Registry` containing additional
            resources references may point to,

        follow_refs:
            whether registered references are followed into their target
            resources. When a target has already been visited it is not
            expanded again; instead a back edge recording the cycle is
            produced (see :attr:`back_edges`).
    """

    def __init__(
        self,
        schema: Any,
        registry: referencing.jsonschema.SchemaRegistry | None = None,
        follow_refs: bool = False,
    ) -> None:
        self._root_schema = schema
        self._follow_refs = follow_refs

        from jsonschema.validators import _REMOTE_WARNING_REGISTRY

        if registry is None:
            self._registry = _REMOTE_WARNING_REGISTRY
        else:
            self._registry = _REMOTE_WARNING_REGISTRY.combine(registry)

        self._visited: set[tuple[str, str]] = set()
        self._edges: list[ReferenceEdge] = []
        self._back_edges: list[ReferenceEdge] = []
        # Cache of plain-name anchor -> pointer, keyed by resource URI.
        self._anchor_cache: dict[str, dict[str, str]] = {}

        root_validator = validator_for(schema if _is_schema(schema) else {})
        root_dialect = self._dialect_for(root_validator)
        root_id = self._identifier_of(schema, root_dialect)
        self._root_uri = root_id or ""
        specification = self._specification_for(root_validator)
        resource = specification.create_resource(schema)
        self._root_resolver = self._registry.resolver_with_root(resource)

    def _reset(self) -> None:
        self._visited = set()
        self._edges = []
        self._back_edges = []
        self._anchor_cache = {}

    @property
    def edges(self) -> tuple[ReferenceEdge, ...]:
        """
        Every reference edge seen during the walk.
        """
        return tuple(self._edges)

    @property
    def back_edges(self) -> tuple[ReferenceEdge, ...]:
        """
        Reference edges pointing at an already visited node.

        Following such an edge and re-expanding its target would loop
        forever, so the target is reported here instead of walked again.
        """
        return tuple(self._back_edges)

    @staticmethod
    def _dialect_for(validator: type[Validator]) -> _Dialect:
        return _DIALECTS.get(validator, _DRAFT202012)

    @staticmethod
    def _specification_for(
        validator: type[Validator],
    ) -> referencing.Specification[Any]:
        dialect_id = validator.ID_OF(validator.META_SCHEMA)
        return referencing.jsonschema.specification_with(
            dialect_id=dialect_id or "urn:unknown-dialect",
            default=referencing.Specification.OPAQUE,
        )

    @staticmethod
    def _identifier_of(schema: Any, dialect: _Dialect) -> str | None:
        if not isinstance(schema, Mapping):
            return None
        id = schema.get(dialect.id_keyword)
        if not isinstance(id, str) or not id:
            return None
        if dialect.fragment_only_id_is_anchor and id.startswith("#"):
            return None
        return id.rstrip("#")

    @staticmethod
    def _begins_resource(schema: Any, dialect: _Dialect) -> bool:
        if not isinstance(schema, Mapping):
            return False
        id = schema.get(dialect.id_keyword)
        if not isinstance(id, str) or not id:
            return False
        if dialect.fragment_only_id_is_anchor and id.startswith("#"):
            return False
        return True

    def _anchors_in(
        self,
        uri: str,
        dialect: _Dialect,
        resolver: Resolver[Any],
    ) -> Mapping[str, str]:
        """
        Map plain-name anchors within the resource at ``uri`` to pointers.
        """
        cached = self._anchor_cache.get(uri)
        if cached is not None:
            return cached

        anchors: dict[str, str] = {}
        try:
            resource = resolver.lookup(uri).contents
        except referencing.exceptions.Unresolvable:
            self._anchor_cache[uri] = anchors
            return anchors

        def visit(schema: Any, pointer: str) -> None:
            if not isinstance(schema, Mapping):
                return
            for keyword in dialect.anchor_keywords:
                name = schema.get(keyword)
                if isinstance(name, str):
                    anchors.setdefault(name, pointer or "")
            if dialect.fragment_only_id_is_anchor:
                legacy = schema.get(dialect.id_keyword)
                if isinstance(legacy, str) and legacy.startswith("#"):
                    anchors.setdefault(legacy[1:], pointer or "")
            for child_pointer, child in self._applicator_locations(
                schema,
                pointer,
                dialect,
            ):
                if self._begins_resource(child, dialect):
                    continue
                visit(child, child_pointer)

        visit(resource, "")
        self._anchor_cache[uri] = anchors
        return anchors

    def _applicator_locations(
        self,
        schema: Mapping,
        pointer: str,
        dialect: _Dialect,
    ) -> Iterator[tuple[str, Any]]:
        """
        Yield ``(child_pointer, child_schema)`` for every inline subschema.
        """
        for keyword in dialect.single:
            if keyword in schema:
                value = schema[keyword]
                if _is_schema(value):
                    yield f"{pointer}/{_escape(keyword)}", value

        for keyword in dialect.arrays:
            if keyword in schema and isinstance(
                schema[keyword],
                Sequence,
            ) and not isinstance(schema[keyword], str):
                base = f"{pointer}/{_escape(keyword)}"
                for index, value in enumerate(schema[keyword]):
                    if _is_schema(value):
                        yield f"{base}/{index}", value

        for keyword in dialect.maps:
            if keyword in schema and isinstance(schema[keyword], Mapping):
                base = f"{pointer}/{_escape(keyword)}"
                for name, value in schema[keyword].items():
                    if _is_schema(value):
                        yield f"{base}/{_escape(str(name))}", value

        yield from self._special_locations(schema, pointer, dialect)

    def _special_locations(
        self,
        schema: Mapping,
        pointer: str,
        dialect: _Dialect,
    ) -> Iterator[tuple[str, Any]]:
        if "dependencies" in dialect.special and "dependencies" in schema:
            dependencies = schema["dependencies"]
            if isinstance(dependencies, Mapping):
                base = f"{pointer}/dependencies"
                for name, dependency in dependencies.items():
                    # Array (or string, under draft 3) values are
                    # property-name dependencies, not subschemas.
                    if _is_schema(dependency):
                        yield (
                            f"{base}/{_escape(str(name))}",
                            dependency,
                        )

        if "items" in dialect.special and "items" in schema:
            items = schema["items"]
            base = f"{pointer}/items"
            if isinstance(items, Sequence) and not isinstance(items, str):
                # Tuple form: each element is a positional subschema.
                for index, value in enumerate(items):
                    if _is_schema(value):
                        yield f"{base}/{index}", value
            elif _is_schema(items):
                yield base, items

        if "type" in dialect.special and "type" in schema:
            type_value = schema["type"]
            entries = (
                type_value
                if isinstance(type_value, Sequence)
                and not isinstance(type_value, str)
                else [type_value]
            )
            base = f"{pointer}/type"
            for index, value in enumerate(entries):
                # Draft 3 allows subschemas among the listed types.
                if _is_schema(value):
                    yield f"{base}/{index}", value

        if "disallow" in dialect.special and "disallow" in schema:
            disallow = schema["disallow"]
            entries = (
                disallow
                if isinstance(disallow, Sequence)
                and not isinstance(disallow, str)
                else [disallow]
            )
            base = f"{pointer}/disallow"
            for index, value in enumerate(entries):
                if _is_schema(value):
                    yield f"{base}/{index}", value

    @staticmethod
    def _reference_keyword(schema: Any, dialect: _Dialect) -> str | None:
        if not isinstance(schema, Mapping):
            return None
        for keyword in dialect.refs:
            value = schema.get(keyword)
            if isinstance(value, str):
                return keyword
        return None

    def _resolve_ref(
        self,
        node: SchemaNode,
        ref: str,
        resolver: Resolver[Any],
    ) -> tuple[SchemaNode, Resolver[Any]] | None:
        try:
            resolved = resolver.lookup(ref)
        except referencing.exceptions.Unresolvable:
            return None

        target_uri, fragment = urldefrag(urljoin(node.uri or "", ref))
        target_uri = target_uri.rstrip("#")
        target_validator = validator_for(
            resolved.contents,
            default=node.validator,
        )
        target_dialect = self._dialect_for(target_validator)

        if not fragment:
            target_pointer = ""
        elif fragment.startswith("/"):
            target_pointer = fragment
        else:
            anchors = self._anchors_in(target_uri, target_dialect, resolver)
            target_pointer = anchors.get(fragment, "")

        target = SchemaNode(
            schema=resolved.contents,
            uri=target_uri,
            pointer=target_pointer,
            validator=target_validator,
            is_resource_root=not target_pointer,
        )
        return target, resolved.resolver

    def __iter__(self) -> Iterator[SchemaNode]:
        yield from self.walk()

    def walk(self) -> Iterator[SchemaNode]:
        """
        Lazily yield every subschema, including the root schema.

        Walking the same object again resets the recorded edges and back
        edges, so the walker may safely be iterated more than once.
        """
        self._reset()
        root_validator = validator_for(
            self._root_schema if _is_schema(self._root_schema) else {},
        )
        root_node = SchemaNode(
            schema=self._root_schema,
            uri=self._root_uri,
            pointer="",
            validator=root_validator,
            is_resource_root=True,
        )

        stack: list[
            tuple[SchemaNode, Resolver[Any]],
        ] = [(root_node, self._root_resolver)]
        while stack:
            node, resolver = stack.pop()
            identity = (node.uri, node.pointer)
            if identity in self._visited:
                continue
            self._visited.add(identity)
            yield node

            schema = node.schema
            dialect = self._dialect_for(node.validator)

            ref_keyword = self._reference_keyword(schema, dialect)
            if ref_keyword is not None:
                ref = schema[ref_keyword]
                if not self._follow_refs:
                    continue
                resolution = self._resolve_ref(node, ref, resolver)
                if resolution is None:
                    continue
                target, target_resolver = resolution
                target_identity = (target.uri, target.pointer)
                edge = ReferenceEdge(
                    source=node,
                    ref=ref,
                    target=target,
                    is_back_edge=target_identity in self._visited,
                )
                self._edges.append(edge)
                if edge.is_back_edge:
                    self._back_edges.append(edge)
                    continue
                stack.append((target, target_resolver))
                continue

            if not isinstance(schema, Mapping):
                # Boolean schemas have no inline subschemas.
                continue

            children = list(
                self._applicator_locations(schema, node.pointer, dialect),
            )
            for child_pointer, child in reversed(children):
                if isinstance(child, Mapping):
                    child_validator = validator_for(
                        child,
                        default=node.validator,
                    )
                else:
                    child_validator = node.validator
                child_dialect = self._dialect_for(child_validator)
                embedded_id = self._identifier_of(child, child_dialect)
                if embedded_id is not None:
                    child_uri = urljoin(node.uri, embedded_id).rstrip("#")
                    child_pointer = ""
                    is_resource_root = True
                else:
                    child_uri = node.uri
                    child_pointer = child_pointer
                    is_resource_root = False

                child_node = SchemaNode(
                    schema=child,
                    uri=child_uri,
                    pointer=child_pointer,
                    validator=child_validator,
                    is_resource_root=is_resource_root,
                )
                if is_resource_root:
                    specification = self._specification_for(child_validator)
                    child_resolver = resolver.in_subresource(
                        specification.create_resource(child),
                    )
                else:
                    child_resolver = resolver
                stack.append((child_node, child_resolver))


def walk(
    schema: Any,
    registry: referencing.jsonschema.SchemaRegistry | None = None,
    follow_refs: bool = False,
) -> SchemaWalker:
    """
    Construct a `SchemaWalker` for the given schema.

    The returned object is iterable over its `SchemaNode` objects (each
    iteration walks the schema anew) and also records reference edges and any
    back edges encountered when references are followed.
    """
    return SchemaWalker(
        schema,
        registry=registry,
        follow_refs=follow_refs,
    )
