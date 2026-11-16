from __future__ import annotations

from contextlib import contextmanager, suppress
from functools import singledispatch
from typing import TYPE_CHECKING, Any, Callable, Generic, Iterator, Literal, TypeVar

from attrs import Factory, define

from ._compat import TypeAlias
from .fns import Predicate

if TYPE_CHECKING:
    from .converters import BaseConverter

TargetType: TypeAlias = Any
UnstructuredValue: TypeAlias = Any
StructuredValue: TypeAlias = Any

StructureHook: TypeAlias = Callable[[UnstructuredValue, TargetType], StructuredValue]
UnstructureHook: TypeAlias = Callable[[StructuredValue], UnstructuredValue]

Hook = TypeVar("Hook", StructureHook, UnstructureHook)
HookFactory: TypeAlias = Callable[[TargetType], Hook]


@define
class _DispatchNotFound:
    """A dummy object to help signify a dispatch not found."""


@define
class FunctionDispatch:
    """
    FunctionDispatch is similar to functools.singledispatch, but
    instead dispatches based on functions that take the type of the
    first argument in the method, and return True or False.

    objects that help determine dispatch should be instantiated objects.

    :param converter: A converter to be used for factories that require converters.

    ..  versionchanged:: 24.1.0
        Support for factories that require converters, hence this requires a
        converter when creating.
    """

    _converter: BaseConverter
    _handler_pairs: list[tuple[Predicate, Callable[[Any, Any], Any], bool, bool]] = (
        Factory(list)
    )

    def register(
        self,
        predicate: Predicate,
        func: Callable[..., Any],
        is_generator=False,
        takes_converter=False,
    ) -> None:
        self._handler_pairs.insert(0, (predicate, func, is_generator, takes_converter))

    def dispatch(self, typ: Any) -> Callable[..., Any] | None:
        """
        Return the appropriate handler for the object passed.
        """
        for can_handle, handler, is_generator, takes_converter in self._handler_pairs:
            # can handle could raise an exception here
            # such as issubclass being called on an instance.
            # it's easier to just ignore that case.
            try:
                ch = can_handle(typ)
            except Exception:  # noqa: S112
                continue
            if ch:
                if is_generator:
                    if takes_converter:
                        return handler(typ, self._converter)
                    return handler(typ)

                return handler
        return None

    def get_num_fns(self) -> int:
        return len(self._handler_pairs)

    def copy_to(self, other: FunctionDispatch, skip: int = 0) -> None:
        other._handler_pairs = self._handler_pairs[:-skip] + other._handler_pairs


@define(init=False)
class MultiStrategyDispatch(Generic[Hook]):
    """
    MultiStrategyDispatch uses a combination of exact-match dispatch,
    singledispatch, and FunctionDispatch.

    :param fallback_factory: A hook factory to be called when a hook cannot be
        produced.
    :param converter: A converter to be used for factories that require converters.

    .. versionchanged:: 23.2.0
        Fallbacks are now factories.
    .. versionchanged:: 24.1.0
        Support for factories that require converters, hence this requires a
        converter when creating.
    .. versionchanged:: NEXT
        Generated hooks now record the types they depend on. Registering a hook
        for a type only invalidates cached hooks that actually depend on that
        type, in both the structuring and unstructuring directions.
    """

    _fallback_factory: HookFactory[Hook]
    _direct_dispatch: dict[TargetType, Hook]
    _function_dispatch: FunctionDispatch
    _single_dispatch: Any
    dispatch: Callable[[TargetType, BaseConverter], Hook]
    #: Exact-type hook cache, replacing the previous ``functools.lru_cache``.
    _cache: dict[TargetType, Hook]
    #: Types whose cached hooks depend on a given type (child -> {parents}).
    _dependents: dict[TargetType, set[TargetType]]
    #: Stack of types whose hooks are currently being generated.
    _generating: list[TargetType]

    def __init__(
        self, fallback_factory: HookFactory[Hook], converter: BaseConverter
    ) -> None:
        self._fallback_factory = fallback_factory
        self._direct_dispatch = {}
        self._function_dispatch = FunctionDispatch(converter)
        self._single_dispatch = singledispatch(_DispatchNotFound)
        self._cache = {}
        self._dependents = {}
        self._generating = []
        self.dispatch = self._dispatch

    def _record_dependency(self, dependent: TargetType, dependency: TargetType) -> None:
        """Record that a hook for ``dependency`` is used while generating the
        hook for ``dependent``.

        Types are only recorded when a hook is actually being generated, and
        unhashable types are silently ignored (they simply can't be cached).
        """
        if dependent is not dependency:
            with suppress(TypeError):
                # An unhashable type can neither be cached nor invalidated.
                self._dependents.setdefault(dependency, set()).add(dependent)

    def _record_generation_edges(self, typ: TargetType) -> None:
        """Record the dependency edges implied by looking up ``typ`` while a
        hook is being generated.

        Class hooks are registered with _singledispatch_, which dispatches by
        the method resolution order: a hook registered for any base class also
        serves its subclasses. Recording every base class therefore ensures that
        re-registering a superclass hook invalidates the generated hooks of its
        subclasses, while hooks for unrelated classes stay cached.
        """
        parent = self._generating[-1]
        self._record_dependency(parent, typ)
        try:
            mro = typ.__mro__
        except AttributeError:
            return
        for base in mro:
            if base is not object:
                self._record_dependency(parent, base)

    def _dispatch(self, typ: TargetType) -> Hook:
        """Return the hook for ``typ``, caching the result.

        On a cache miss the type is pushed onto the generation stack, so hooks
        fetched by a hook factory are recorded as dependencies. Dependencies are
        recorded on cache hits too, since an already generated hook is still part
        of the parent type's behavior.
        """
        if self._generating:
            self._record_generation_edges(typ)
        cached = self._cache.get(typ)
        if cached is not None:
            return cached
        with self._generating_type(typ):
            hook = self.dispatch_without_caching(typ)
        with suppress(TypeError):
            # Unhashable types can't be cached anyway.
            self._cache[typ] = hook
        return hook

    @contextmanager
    def _generating_type(self, typ: TargetType) -> Iterator[None]:
        # Cache clears can happen while a hook is being generated (factories may
        # register hooks themselves), so restore the stack on exit instead of
        # assuming it survived unchanged.
        previous: list[TargetType] = self._generating
        self._generating = [*previous, typ]
        try:
            yield
        finally:
            self._generating = previous

    def dispatch_without_caching(self, typ: TargetType) -> Hook:
        """Dispatch on the type but without caching the result.

        When called while a hook is being generated, the looked-up type (and
        its base classes, since _singledispatch_ dispatches by MRO) is recorded
        as a dependency of the generated hook.
        """
        if self._generating:
            self._record_generation_edges(typ)
        try:
            dispatch = self._single_dispatch.dispatch(typ)
            if dispatch is not _DispatchNotFound:
                return dispatch
        except Exception:  # noqa: S110
            pass

        direct_dispatch = self._direct_dispatch.get(typ)
        if direct_dispatch is not None:
            return direct_dispatch

        res = self._function_dispatch.dispatch(typ)
        return res if res is not None else self._fallback_factory(typ)

    def register_cls_list(self, cls_and_handler, direct: bool = False) -> None:
        """Register a class to direct or singledispatch."""
        registered = []
        for cls, handler in cls_and_handler:
            if direct:
                # The generated hook itself replaces any stale direct entry;
                # keep it so the next lookup doesn't regenerate the same hook.
                self._direct_dispatch[cls] = handler
            else:
                self._single_dispatch.register(cls, handler)
                # Direct entries shadow singledispatch; they are lazily
                # regenerated, so drop them to let the new handler win.
                self.clear_direct()
            registered.append(cls)
        if len(registered) == 1:
            # A single exact type changed: drop its cached hook and the hooks
            # transitively depending on it, keeping everything else intact.
            self.invalidate(registered[0], keep_direct=direct)
        else:
            # Multiple types at once: conservatively reset everything.
            self.clear_cache()

    def register_func_list(
        self,
        pred_and_handler: list[
            tuple[Predicate, Any]
            | tuple[Predicate, Any, bool]
            | tuple[Predicate, Callable[[Any, BaseConverter], Any], Literal["extended"]]
        ],
    ):
        """
        Register a predicate function to determine if the handler
        should be used for the type.

        :param pred_and_handler: The list of predicates and their associated
            handlers. If a handler is registered in `extended` mode, it's a
            factory that requires a converter.

        Predicates may match any number of types, so all cached hooks are
        invalidated conservatively.
        """
        for tup in pred_and_handler:
            if len(tup) == 2:
                func, handler = tup
                self._function_dispatch.register(func, handler)
            else:
                func, handler, is_gen = tup
                if is_gen == "extended":
                    self._function_dispatch.register(
                        func, handler, is_generator=is_gen, takes_converter=True
                    )
                else:
                    self._function_dispatch.register(func, handler, is_generator=is_gen)
        self.clear_cache()

    def clear_direct(self) -> None:
        """Clear the direct dispatch."""
        self._direct_dispatch.clear()

    def invalidate(
        self, typ: TargetType, *, keep_direct: bool = False
    ) -> set[TargetType]:
        """Invalidate the cached hook for ``typ`` and every hook depending on it.

        The transitive closure of dependent types is recomputed lazily the next
        time their hooks are requested; cached hooks unrelated to ``typ`` are
        left untouched. The invalidated types are returned.

        :param keep_direct: Whether to keep the direct-dispatch entry for
            ``typ``. This is used when a freshly generated hook is being
            registered directly, so it must not be discarded immediately.

        .. versionadded:: NEXT
        """
        affected: set[TargetType] = set()
        pending = [typ]
        while pending:
            current = pending.pop()
            if current in affected:
                continue
            affected.add(current)
            try:
                self._cache.pop(current, None)
                if not (keep_direct and current is typ):
                    self._direct_dispatch.pop(current, None)
            except TypeError:
                # Unhashable types are never present in either mapping.
                continue
            pending.extend(self._dependents.get(current, ()))
        return affected

    def clear_cache(self) -> None:
        """Clear all caches."""
        self._direct_dispatch.clear()
        self._cache.clear()
        self._dependents.clear()
        self._generating.clear()

    def get_num_fns(self) -> int:
        return self._function_dispatch.get_num_fns()

    def copy_to(self, other: MultiStrategyDispatch, skip: int = 0) -> None:
        self._function_dispatch.copy_to(other._function_dispatch, skip=skip)
        for cls, fn in self._single_dispatch.registry.items():
            other._single_dispatch.register(cls, fn)
        other.clear_cache()
