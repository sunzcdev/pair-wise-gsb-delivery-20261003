"""
Explainable resolution traces for ``$dynamicRef`` during schema collection.

The routines here never invoke a validator and never retain an instance: the
schema itself is walked as a basic tree, with one node per physical schema
location, so the resolution state accumulated while handling one sibling
field can never leak into the next one.

Every encountered ``$dynamicRef`` produces a `DynamicRefTrace` describing:

* the static target -- the resource location the reference points at before
  any dynamic scope is replayed,
* the dynamic-scope candidates -- each same-named anchor reachable through
  the resources enclosing the reference, and
* the resource location actually adopted once the dynamic scope is replayed.

The tree threads a real `referencing.Resolver` through every edge. Its
``previous`` chain is exactly the dynamic scope a validator sees, so the
replay produces the same binding a validator would, including the rule that
leaving a reference restores the enclosing scope.
"""

from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING
from urllib.parse import urldefrag, urljoin

from attrs import define

import referencing
import referencing.exceptions
import referencing.jsonschema

if TYPE_CHECKING:
    from referencing._core import Resolver


@define(frozen=True)
class SchemaNode:
    """
    A single physical location in the collected schema tree.

    Equal-looking schemas found at different locations -- including
    same-named anchors inside different nested resources -- are distinct
    nodes, so resolution state is always bound to a location rather than to
    a schema value.
    """

    schema: object
    pointer: str
    resource_uri: str
    parent: "SchemaNode | None" = None
    ref: "str | None" = None
    back_edge: bool = False
    children: tuple["SchemaNode", ...] = ()

    @property
    def is_resource_root(self):
        """Whether this node opens a new (possibly nested) resource."""
        return self.parent is None or (
            self.resource_uri != self.parent.resource_uri
        )

    def path(self):
        """The nodes from the tree root down to this node."""
        nodes = []
        node = self
        while node is not None:
            nodes.append(node)
            node = node.parent
        nodes.reverse()
        return nodes

    def iter_nodes(self):
        """Iterate over the node and its descendants without recursing."""
        pending = [self]
        while pending:
            node = pending.pop()
            yield node
            pending.extend(reversed(node.children))


@define(frozen=True)
class StaticTarget:
    """
    Where a reference resolves before the dynamic scope is replayed.
    """

    uri: str
    fragment: str
    pointer: str
    dynamic_anchor: "str | None"


@define(frozen=True)
class DynamicScopeCandidate:
    """
    A same-named anchor considered while replaying the dynamic scope.

    Only dynamic anchors may move the final binding; a plain ``$anchor``
    with the same name is reported on the static target but never appears as
    a dynamic-scope candidate.
    """

    resource_uri: str
    anchor: str


@define(frozen=True)
class DynamicRefTrace:
    """
    An explanation of how one ``$dynamicRef`` was resolved.
    """

    ref: str
    node: SchemaNode
    static_target: StaticTarget
    candidates: tuple[DynamicScopeCandidate, ...]
    adopted_resource_uri: str
    adopted_pointer: str
    adopted_via: "str | None"

    @property
    def anchor(self):
        """The plain-name fragment the dynamic reference looks for."""
        fragment = self.static_target.fragment
        if fragment and not fragment.startswith("/"):
            return fragment
        return None

    @property
    def adopted_dynamically(self):
        """Whether the binding moved away from the static target."""
        return self.adopted_via is not None


@define(frozen=True)
class RefResolutionError(Exception):
    """
    A reference could not be resolved while collecting the schema tree.
    """

    ref: str
    node: SchemaNode
    cause: Exception

    def __str__(self) -> str:
        return (
            f"Failed to resolve {self.ref!r} at {self.node.pointer} "
            f"within {self.node.resource_uri!r}: {self.cause}"
        )


@define(frozen=True)
class _Frame:
    """
    The state active while one node subtree is collected.

    Frames are immutable. Entering a nested resource or following a
    reference derives a new frame and leaving it drops the frame, so a
    sibling field always starts from the enclosing state and cannot inherit
    an earlier sibling's resolution state.

    ``active`` holds the schema identities reached through reference edges
    on the current reference chain; a reference back into one of them is a
    back edge and terminates that chain instead of looping.
    """

    resolver: "Resolver"
    resource: referencing.Resource
    active: frozenset = frozenset()


_SUBSCHEMA_IN_VALUE = frozenset(
    {
        "additionalItems",
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
)
_SUBSCHEMA_IN_ARRAY = frozenset(
    {"allOf", "anyOf", "oneOf", "prefixItems"},
)
_SUBSCHEMA_IN_MAP = frozenset(
    {
        "$defs",
        "definitions",
        "dependentSchemas",
        "patternProperties",
        "properties",
    },
)


def _encode_segment(segment):
    return str(segment).replace("~", "~0").replace("/", "~1")


def _join_pointer(pointer, segment):
    if isinstance(segment, tuple):
        for part in segment:
            pointer = _join_pointer(pointer, part)
        return pointer
    return f"{pointer}/{_encode_segment(segment)}"


def _split_ref(ref, base_uri):
    if ref.startswith("#"):
        return base_uri, ref[1:]
    return urldefrag(urljoin(base_uri, ref))


def _is_dynamic_anchor(anchor):
    return isinstance(anchor, referencing.jsonschema.DynamicAnchor)


def _iter_children(schema):
    """Yield ``(pointer segment, child schema)`` per schema location."""
    if not isinstance(schema, Mapping):
        return

    for keyword in _SUBSCHEMA_IN_VALUE:
        if keyword in schema:
            yield keyword, schema[keyword]
    for keyword in _SUBSCHEMA_IN_ARRAY:
        if keyword in schema:
            for index, child in enumerate(schema[keyword]):
                yield (keyword, index), child
    for keyword in _SUBSCHEMA_IN_MAP:
        if keyword in schema:
            for name, child in schema[keyword].items():
                yield (keyword, name), child


def collect(schema, registry=None, *, trace_refs=True):
    """
    Recursively collect ``schema`` as a tree, optionally tracing references.

    Arguments:

        schema:

            the root JSON Schema to collect

        registry (referencing.Registry or None):

            extra registered resources. References to resources which are
            neither embedded nor registered raise `RefResolutionError`.

        trace_refs (bool):

            when true (the default), each encountered ``$dynamicRef`` is
            explained in the returned trace list. The tree is built either
            way.

    Returns:

        a ``(root_node, traces)`` pair where ``traces`` is a tuple of
        `DynamicRefTrace`
    """
    specification = referencing.jsonschema.DRAFT202012
    root_resource = specification.create_resource(schema)
    resolver = (registry or referencing.Registry()).resolver_with_root(
        root_resource,
    )

    root_node = SchemaNode(
        schema=schema,
        pointer="",
        resource_uri=resolver._base_uri,
    )
    collector = _Collector(trace_refs)
    collector._run(
        root_node,
        _Frame(
            resolver=resolver,
            resource=root_resource,
            active=frozenset({id(schema)}),
        ),
    )
    return root_node, tuple(collector.traces)


class _Collector:
    def __init__(self, trace_refs):
        self.trace_refs = trace_refs
        self.traces = []

    @staticmethod
    def _add_child(parent, child):
        object.__setattr__(parent, "children", parent.children + (child,))

    def _run(self, root_node, root_frame):
        stack = [(root_node, root_frame)]
        while stack:
            node, frame = stack.pop()
            self._visit(node, frame, stack)

    def _visit(self, node, frame, stack):
        schema = node.schema
        if isinstance(schema, bool) or not isinstance(schema, Mapping):
            return

        if "$ref" in schema:
            self._follow_reference(node, frame, schema["$ref"], False, stack)
        if "$dynamicRef" in schema:
            self._follow_reference(
                node,
                frame,
                schema["$dynamicRef"],
                True,
                stack,
            )

        specification = frame.resource._specification
        children = list(_iter_children(schema))
        for segment, child_schema in reversed(children):
            child_resource = specification.create_resource(child_schema)
            if child_resource.id() is None:
                child_node = SchemaNode(
                    schema=child_schema,
                    pointer=_join_pointer(node.pointer, segment),
                    resource_uri=node.resource_uri,
                    parent=node,
                )
                self._add_child(node, child_node)
                stack.append((child_node, frame))
                continue

            nested_resolver = frame.resolver.in_subresource(child_resource)
            child_node = SchemaNode(
                schema=child_schema,
                pointer=_join_pointer(node.pointer, segment),
                resource_uri=nested_resolver._base_uri,
                parent=node,
            )
            self._add_child(node, child_node)
            stack.append(
                (
                    child_node,
                    _Frame(
                        resolver=nested_resolver,
                        resource=child_resource,
                        active=frame.active,
                    ),
                ),
            )

    def _follow_reference(self, node, frame, ref, dynamic, stack):
        resolver = frame.resolver
        target_uri, fragment = _split_ref(ref, resolver._base_uri)

        try:
            resolved = resolver.lookup(ref)
        except referencing.exceptions.Unresolvable as error:
            raise RefResolutionError(
                ref=ref,
                node=node,
                cause=error,
            ) from error

        trace = None
        if dynamic:
            trace = self._build_trace(
                node=node,
                resolver=resolver,
                ref=ref,
                target_uri=target_uri,
                fragment=fragment,
            )
            if self.trace_refs:
                self.traces.append(trace)

        if dynamic and trace.adopted_via is not None:
            adopted = resolver._registry.anchor(
                trace.adopted_resource_uri,
                trace.anchor,
            ).value
            adopted_contents = adopted.resource.contents
        else:
            adopted_contents = resolved.contents

        is_back_edge = id(adopted_contents) in frame.active
        child_node = SchemaNode(
            schema=adopted_contents,
            pointer=node.pointer,
            resource_uri=resolved.resolver._base_uri,
            parent=node,
            ref=ref,
            back_edge=is_back_edge,
        )
        self._add_child(node, child_node)
        if is_back_edge:
            return

        target_resource = self._resource_for(
            resolved.resolver,
            adopted_contents,
        )
        stack.append(
            (
                child_node,
                _Frame(
                    resolver=resolved.resolver,
                    resource=target_resource,
                    active=frame.active | {id(adopted_contents)},
                ),
            ),
        )

    def _build_trace(
        self,
        node,
        resolver,
        ref,
        target_uri,
        fragment,
    ):
        registry = resolver._registry
        static_pointer = ""
        static_dynamic_anchor = None

        if fragment.startswith("/"):
            static_pointer = fragment
        elif fragment:
            anchor = self._anchor(registry, target_uri, fragment)
            if anchor is not None and _is_dynamic_anchor(anchor):
                static_dynamic_anchor = fragment

        static_target = StaticTarget(
            uri=target_uri,
            fragment=fragment,
            pointer=static_pointer,
            dynamic_anchor=static_dynamic_anchor,
        )

        scope_candidate_uris = self._dynamic_scope_anchor_uris(
            registry=registry,
            resolver=resolver,
            target_uri=target_uri,
            anchor_name=fragment,
        )
        candidates = tuple(
            DynamicScopeCandidate(resource_uri=uri, anchor=fragment)
            for uri in scope_candidate_uris
        )

        adopted_uri = target_uri
        adopted_pointer = static_pointer
        adopted_via = None
        for uri in scope_candidate_uris:
            adopted_uri = uri
            adopted_pointer = ""
            adopted_via = uri

        return DynamicRefTrace(
            ref=ref,
            node=node,
            static_target=static_target,
            candidates=tuple(candidates),
            adopted_resource_uri=adopted_uri,
            adopted_pointer=adopted_pointer,
            adopted_via=adopted_via,
        )

    def _dynamic_scope_anchor_uris(
        self,
        registry,
        resolver,
        target_uri,
        anchor_name,
    ):
        """
        Replay `DynamicAnchor.resolve` exactly.

        Only resources which define a *dynamic* anchor with the relevant
        name move the binding; a resource without such an anchor is simply
        skipped, leaving the previous (possibly static) binding in place.
        """
        if not anchor_name or anchor_name.startswith("/"):
            return []

        static_anchor = self._anchor(registry, target_uri, anchor_name)
        if static_anchor is None or not _is_dynamic_anchor(static_anchor):
            return []

        candidate_uris = []
        for scope_uri, scope_registry in resolver.dynamic_scope():
            anchor = self._anchor(scope_registry, scope_uri, anchor_name)
            if anchor is None or not _is_dynamic_anchor(anchor):
                continue
            candidate_uris.append(scope_uri)
        return candidate_uris

    @staticmethod
    def _anchor(registry, resource_uri, anchor_name):
        try:
            return registry.anchor(resource_uri, anchor_name).value
        except referencing.exceptions.NoSuchAnchor:
            return None

    @staticmethod
    def _resource_for(resolver, contents):
        try:
            return resolver._registry[resolver._base_uri]
        except referencing.exceptions.NoSuchResource:
            return referencing.jsonschema.DRAFT202012.create_resource(contents)
