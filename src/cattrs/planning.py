"""Preparation-only conversion planning.

This module produces a :class:`ConversionPlan` describing *how* a converter
would (un)structure a type, without ever running a field conversion hook or
constructing a structured object. Plans answer the question "which fields have
conversion support?" before a model is deployed.

Planning is preparation, not a pure operation: selecting a hook may require
running registered *hook factories*, and those factories can have side
effects (for example, the built-in generating factories register additional
direct hooks). Every hook-factory invocation is therefore recorded in the
plan's :attr:`ConversionPlan.factory_invocations`, so the side effects are
explicit rather than hidden.
"""

from __future__ import annotations

import sys
from collections.abc import Callable, Mapping, Sequence
from inspect import get_annotations
from typing import Any, ForwardRef, TypeVar

from attrs import define

from ._compat import (
    ANIES,
    NoneType,
    fields,
    get_args,
    get_final_base,
    get_newtype_base,
    get_origin,
    has,
    is_annotated,
    is_literal,
    is_mapping,
    is_typeddict,
    is_union_type,
)
from ._generics import deep_copy_with
from .cols import is_namedtuple
from .dispatch import ResolvedHook
from .errors import StructureHandlerNotFoundError
from .fns import (
    Predicate,
    default_structure_fallback_factory,
    default_unstructure_fallback_factory,
)
from .gen._generics import generate_mapping
from .typealiases import is_type_alias

__all__ = [
    "ConversionPlan",
    "FactoryInvocation",
    "HookSource",
    "TypeEdge",
    "TypeNode",
    "plan_structure",
    "plan_unstructure",
]


@define(frozen=True)
class HookSource:
    """The hook ultimately selected for a type position.

    :param kind: Where the hook came from: ``"singledispatch"`` for a hook
        registered for a concrete class, ``"direct"`` for an exact-type
        generated hook, ``"function"`` for a predicate-registered hook,
        ``"factory"`` for a hook produced by a predicate-registered factory,
        and ``"fallback"`` for the converter fallback factory.
    :param hook: The hook callable that would be used. It is never invoked by
        the planner.
    :param handler: The registered handler or factory behind the hook.
    :param predicate: The matching predicate, when the source is function
        dispatch.
    :param custom: Whether the handler was registered by user code rather than
        being a converter built-in.
    :param missing: Whether the type has no real handler: structure planning
        resolves to the default raising fallback, meaning structuring would
        fail.
    """

    kind: str
    hook: Callable[..., Any]
    handler: Callable[..., Any]
    predicate: Predicate | None
    custom: bool
    missing: bool


@define(frozen=True)
class FactoryInvocation:
    """A record of a hook factory being run during preparation.

    :param type: The type position the factory was asked to handle.
    :param factory: The factory callable that was invoked.
    :param hook: The hook produced by the factory.
    :param custom: Whether the factory was registered by user code.
    """

    type: Any
    factory: Callable[..., Any]
    hook: Callable[..., Any]
    custom: bool


@define(frozen=True)
class TypeNode:
    """A type position in the conversion graph.

    :param type: The exact type, preserving generic arguments (for example
        ``list[int]`` rather than ``list``).
    :param source: The hook selected for this position.
    :param unverified: ``True`` for ``Any`` and unresolved ``TypeVar``
        positions: these accept any input, so they are not verified concrete
        types and are not traversed further.
    """

    type: Any
    source: HookSource
    unverified: bool


@define(frozen=True)
class TypeEdge:
    """A directed edge between two type positions in the conversion graph.

    :param parent: The parent type.
    :param child: The child type, preserving generic arguments.
    :param label: Where the child lives: a field/key name, a collection
        parameter label such as ``"[key]"`` or ``"[value]"``, a tuple index,
        or a union member label.
    """

    parent: Any
    child: Any
    label: str


@define
class ConversionPlan:
    """The full preparation plan for converting a root type.

    :param root: The type the plan was requested for, with generic arguments
        preserved.
    :param nodes: Every distinct type position reachable from the root, keyed
        by the type itself.
    :param edges: The traversed parent-child edges.
    :param back_edges: Edges whose target was already being visited; these are
        the recursive edges that stop the graph from expanding forever.
    :param missing_handlers: Types that would fail to convert because only the
        default fallback applies.
    :param unverified_types: ``Any`` and unresolved ``TypeVar`` positions,
        which are accepted as-is rather than validated.
    :param factory_invocations: Every hook factory executed while preparing
        the plan.
    """

    root: Any
    nodes: dict[Any, TypeNode]
    edges: list[TypeEdge]
    back_edges: list[TypeEdge]
    missing_handlers: list[Any]
    unverified_types: list[Any]
    factory_invocations: list[FactoryInvocation]

    @property
    def fields(self) -> dict[Any, dict[str, Any]]:
        """The field types reachable from each structured type.

        Only fields present in the graph are included, so this answers which
        fields have conversion support for each model type.
        """
        result: dict[Any, dict[str, Any]] = {}
        for edge in self.edges:
            if edge.label and not edge.label.startswith(("[", "<")):
                result.setdefault(edge.parent, {})[edge.label] = edge.child
        return result


def _is_typevar(t: Any) -> bool:
    return isinstance(t, TypeVar)


def _parameters_of(origin: Any) -> tuple[Any, ...]:
    """The declared type parameters of a generic alias origin.

    Builtin generic aliases (``list[int]``) don't always expose
    ``__parameters__`` on the origin, in which case there are no named
    parameters and positional labels are used.
    """
    parameters = getattr(origin, "__parameters__", None)
    return tuple(parameters) if parameters else ()


def _substitute(t: Any, mapping: Mapping[str, Any]) -> Any:
    if not mapping:
        return t
    if _is_typevar(t):
        return mapping.get(t.__name__, t)
    try:
        return deep_copy_with(t, mapping)
    except Exception:  # noqa: BLE001 - never let substitution crash planning
        return t


def _field_children(
    t: Any, mapping: Mapping[str, Any]
) -> Sequence[tuple[str, Any]]:
    """Yield ``(label, child_type)`` pairs for a structured/container type."""
    if is_annotated(t):
        return [("<annotated>", _substitute(get_args(t)[0], mapping))]

    if is_union_type(t):
        return [
            (f"<member {i}>", _substitute(a, mapping))
            for i, a in enumerate(get_args(t))
            if a is not NoneType
        ]

    if is_literal(t) or is_type_alias(t):
        return []

    newtype_base = get_newtype_base(t)
    if newtype_base is not None:
        return [("<newtype>", _substitute(newtype_base, mapping))]

    final_base = get_final_base(t)
    if final_base is not None:
        return [("<final>", _substitute(final_base, mapping))]

    origin = get_origin(t)
    if origin is None:
        return _class_children(t, mapping)

    # Generic alias: preserve the arguments and recurse into them (including
    # unresolved TypeVars, recorded as unverified), plus the generic class's
    # own structured fields.
    parameters = _parameters_of(origin)
    children: list[tuple[str, Any]] = []
    for i, a in enumerate(get_args(t)):
        if a is Ellipsis:
            continue
        if i < len(parameters):
            label = f"[{parameters[i].__name__}]"
        elif is_mapping(origin) and len(get_args(t)) == 2:
            label = "[key]" if i == 0 else "[value]"
        else:
            label = f"[{i}]"
        children.append((label, _substitute(a, mapping)))
    if has(origin):
        children.extend(_class_children(origin, mapping))
    elif is_typeddict(origin):
        children.extend(_typeddict_children(origin, mapping))
    elif is_namedtuple(origin):
        children.extend(_namedtuple_children(origin, mapping))
    return children


def _class_children(
    cl: Any, mapping: Mapping[str, Any]
) -> list[tuple[str, Any]]:
    if is_typeddict(cl):
        return _typeddict_children(cl, mapping)
    if is_namedtuple(cl):
        return _namedtuple_children(cl, mapping)
    if has(cl):
        _resolve_string_annotations(cl)
        out: list[tuple[str, Any]] = []
        for attribute in fields(cl):
            if attribute.type is None:
                # No type metadata: cannot reason about this position.
                continue
            out.append((attribute.name, _substitute(attribute.type, mapping)))
        return out
    return []


def _resolve_string_annotations(cl: Any) -> None:
    """Resolve PEP 563 stringified annotations on an attrs/dataclass in place.

    Function-local classes cannot resolve self-references through the module
    globals; those strings are left untouched rather than crashing planning.
    """
    try:
        from attrs import resolve_types

        resolve_types(cl)
    except Exception:  # noqa: BLE001 - planning must not fail on odd classes
        localns = _local_namespace(cl)
        for attribute in fields(cl):
            if isinstance(attribute.type, str):
                try:
                    attribute.type = _evaluate_forwardref(
                        attribute.type, cl, localns
                    )
                except Exception:  # noqa: BLE001
                    pass


def _local_namespace(owner: Any) -> dict[str, Any]:
    """Best-effort namespace including the owning class for self-references."""
    module = sys.modules.get(getattr(owner, "__module__", None))
    ns = dict(getattr(module, "__dict__", {}))
    ns[getattr(owner, "__name__", "_")] = owner
    return ns


def _evaluate_forwardref(
    ref: str, owner: Any, localns: dict[str, Any] | None = None
) -> Any:
    module = sys.modules.get(getattr(owner, "__module__", None))
    globalns = getattr(module, "__dict__", {})
    if localns is None:
        localns = _local_namespace(owner)
    return eval(ref, globalns, localns)  # noqa: S307 - resolving own annotations


def _safe_annotations(cl: Any) -> dict[str, Any]:
    try:
        annotations = get_annotations(cl, eval_str=True)
    except Exception:  # noqa: BLE001
        annotations = getattr(cl, "__annotations__", {})
    return {
        name: _maybe_evaluate_forwardref(t, cl)
        for name, t in annotations.items()
        if not isinstance(t, str)
    }


def _maybe_evaluate_forwardref(t: Any, owner: Any) -> Any:
    """Evaluate a ``ForwardRef`` (left behind by PEP 563) if possible."""
    if not isinstance(t, ForwardRef):
        return t
    try:
        module = sys.modules.get(getattr(owner, "__module__", None))
        globalns = getattr(module, "__dict__", {})
        localns = _local_namespace(owner)
        return t._evaluate(globalns, localns, recursive_guard=set())
    except Exception:  # noqa: BLE001
        return t


def _typeddict_children(
    cl: Any, mapping: Mapping[str, Any]
) -> list[tuple[str, Any]]:
    return [
        (name, _substitute(t, mapping))
        for name, t in _safe_annotations(cl).items()
    ]


def _namedtuple_children(
    cl: Any, mapping: Mapping[str, Any]
) -> list[tuple[str, Any]]:
    return [
        (name, _substitute(t, mapping))
        for name, t in _safe_annotations(cl).items()
    ]


def _generic_mapping(t: Any) -> Mapping[str, Any]:
    """Build the TypeVar-to-argument mapping for a generic position."""
    try:
        return generate_mapping(t)
    except Exception:  # noqa: BLE001
        return {}


def plan_structure(converter: Any, type: Any) -> ConversionPlan:
    """Prepare a structuring plan for ``type`` without structuring anything.

    The plan lists every reachable type position, the hook that would handle
    it, the recursive edges of the type graph and any missing handlers. Hook
    factories needed to select hooks are executed and recorded; the produced
    hooks themselves are never called.
    """
    return _plan(converter, type, structure=True)


def plan_unstructure(converter: Any, type: Any) -> ConversionPlan:
    """Prepare an unstructuring plan for ``type`` without unstructuring it.

    See :func:`plan_structure` for the semantics.
    """
    return _plan(converter, type, structure=False)


def _plan(converter: Any, root: Any, *, structure: bool) -> ConversionPlan:
    dispatch = (
        converter._structure_func if structure else converter._unstructure_func
    )
    # Function-dispatch entries owned by the converter itself (built-ins).
    # Entries inserted at the front after initialization are user-registered.
    builtin_count = (
        converter._struct_copy_skip
        if structure
        else converter._unstruct_copy_skip
    )
    default_fallback = (
        default_structure_fallback_factory
        if structure
        else default_unstructure_fallback_factory
    )

    invocations: list[FactoryInvocation] = []

    def recorder(
        typ: Any, factory: Callable[..., Any], hook: Callable[..., Any]
    ) -> None:
        invocations.append(
            FactoryInvocation(typ, factory, hook, _is_custom_factory(dispatch, factory, builtin_count))
        )

    nodes: dict[Any, TypeNode] = {}
    edges: list[TypeEdge] = []
    back_edges: list[TypeEdge] = []
    missing: list[Any] = []
    unverified: list[Any] = []
    # Types on the current DFS ancestry path. An edge to an ancestor is a
    # genuine recursive (back) edge; an edge to a type already fully expanded
    # is a shared position and still recorded as a normal edge.
    on_path: set[Any] = set()
    seen_edges: set[tuple[Any, Any, str]] = set()

    dispatch.set_factory_recorder(recorder)
    try:
        # DFS work items. ``("enter", t, mapping, parent, label)`` visits a
        # type; ``("exit", t)`` removes it from the ancestry path.
        stack: list[tuple[str, Any, Mapping[str, Any], Any, str | None]] = [
            ("enter", root, _generic_mapping(root), None, None)
        ]
        while stack:
            kind, t, mapping, parent, label = stack.pop()
            t = _substitute(t, mapping)

            if kind == "exit":
                on_path.discard(t)
                continue

            if parent is not None:
                edge = TypeEdge(parent, t, label or "")
                edge_key = (parent, t, label or "")
                if edge_key in seen_edges:
                    continue
                seen_edges.add(edge_key)
                if t in on_path:
                    # Recursive reference back into the ancestry: stop here.
                    back_edges.append(edge)
                    continue
                edges.append(edge)

            if t in nodes:
                continue

            is_any = t in ANIES
            is_tvar = _is_typevar(t)
            try:
                resolved: ResolvedHook = dispatch.resolve(t)
            except StructureHandlerNotFoundError:
                # The default raising fallback was invoked for this type. If it
                # was invoked for *this* position it is a genuinely missing
                # handler; otherwise a parent factory propagated a child's
                # failure and we still record the parent as a factory.
                if _last_invocation_is_default_fallback(
                    invocations, t, default_fallback
                ):
                    resolved = ResolvedHook(
                        hook=default_fallback,
                        kind="fallback",
                        handler=default_fallback,
                        predicate=None,
                        generated=True,
                    )
                else:
                    resolved = ResolvedHook(
                        hook=default_fallback,
                        kind="factory",
                        handler=default_fallback,
                        predicate=None,
                        generated=True,
                    )
            except Exception:  # noqa: BLE001
                # A factory could not produce a hook (for example an
                # unresolvable PEP 563 forward reference on a function-local
                # class). Record the position as an unresolvable factory and
                # keep planning the rest of the graph.
                resolved = ResolvedHook(
                    hook=default_fallback,
                    kind="factory",
                    handler=default_fallback,
                    predicate=None,
                    generated=True,
                )
            source = _build_source(
                resolved, dispatch, builtin_count, default_fallback, structure
            )

            nodes[t] = TypeNode(t, source, unverified=is_any or is_tvar)
            if source.missing and t not in missing:
                missing.append(t)
            if (is_any or is_tvar) and t not in unverified:
                unverified.append(t)

            if is_any or is_tvar or is_literal(t) or is_type_alias(t):
                # Any/TypeVar accept anything and are not validated; literals
                # and type aliases do not add traversable children here.
                continue

            child_mapping = _generic_mapping(t)
            children = list(_field_children(t, child_mapping))
            if not children:
                continue

            on_path.add(t)
            stack.append(("exit", t, child_mapping, None, None))
            # Push in reverse so declared field order is preserved on pop.
            for child_label, child in reversed(children):
                stack.append(
                    ("enter", child, child_mapping, t, child_label)
                )
    finally:
        dispatch.set_factory_recorder(None)

    return ConversionPlan(
        root=root,
        nodes=nodes,
        edges=edges,
        back_edges=back_edges,
        missing_handlers=missing,
        unverified_types=unverified,
        factory_invocations=invocations,
    )


def _last_invocation_is_default_fallback(
    invocations: Sequence[FactoryInvocation],
    typ: Any,
    default_fallback: Callable[..., Any],
) -> bool:
    """Was the most recent factory call the default fallback for ``typ``?

    The recorder appends an entry (with a ``None`` hook) even when the default
    raising fallback fails, so this works regardless of whether annotations
    were stringified.
    """
    if not invocations:
        return False
    last = invocations[-1]
    return last.type is typ and last.factory is default_fallback


def _is_custom_factory(
    dispatch: Any, factory: Callable[..., Any], builtin_count: int
) -> bool:
    pairs = dispatch._function_dispatch._handler_pairs
    for index, pair in enumerate(pairs):
        if pair[1] is factory:
            # Built-in entries occupy the tail of the pair list; user
            # registrations are inserted at the front.
            return index < len(pairs) - builtin_count
    return False


def _build_source(
    resolved: ResolvedHook,
    dispatch: Any,
    builtin_count: int,
    default_fallback: Callable[..., Any],
    structure: bool,
) -> HookSource:
    if resolved.kind == "fallback":
        return HookSource(
            kind="fallback",
            hook=resolved.hook,
            handler=resolved.handler,
            predicate=None,
            custom=resolved.handler is not default_fallback,
            missing=structure
            and resolved.handler is default_structure_fallback_factory,
        )

    custom = True
    if resolved.kind in {"function", "factory"}:
        custom = _is_custom_factory(dispatch, resolved.handler, builtin_count)
    elif resolved.kind == "direct":
        # Direct entries are generated internally; user hooks registered via
        # register_unstructure_hook/register_structure_hook use singledispatch.
        custom = False
    elif resolved.kind == "singledispatch":
        custom = resolved.hook not in dispatch._builtin_single_dispatch

    return HookSource(
        kind=resolved.kind,
        hook=resolved.hook,
        handler=resolved.handler,
        predicate=resolved.predicate,
        custom=custom,
        missing=False,
    )
