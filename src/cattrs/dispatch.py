from __future__ import annotations

from functools import singledispatch
from typing import TYPE_CHECKING, Any, Callable, Generic, Literal, TypeVar

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

# Types whose hooks are currently being generated, ordered outermost first.
# A single global stack is shared by every dispatch because generated hooks
# resolve their dependencies lazily (for example, ``Optional[T]`` and unions
# look their member hooks up only when first invoked), outside the frame that
# started the generation. The stack lets those late lookups still register a
# dependency on the hook being built.
_GENERATION_STACK: list[TargetType] = []


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
    """

    _fallback_factory: HookFactory[Hook]
    _direct_dispatch: dict[TargetType, Hook]
    _function_dispatch: FunctionDispatch
    _single_dispatch: Any
    dispatch: Callable[[TargetType], Hook]
    _dispatch_cache: dict[TargetType, Hook]
    # Maps a type to the types of the generated hooks that depend on it.
    # A hook generated for type ``T`` records an edge ``dependency -> T`` for
    # every hook it asks the converter for while being built.
    _dependents: dict[TargetType, list[TargetType]]
    _hits: int
    _misses: int

    def __init__(
        self, fallback_factory: HookFactory[Hook], converter: BaseConverter
    ) -> None:
        self._fallback_factory = fallback_factory
        self._direct_dispatch = {}
        self._function_dispatch = FunctionDispatch(converter)
        self._single_dispatch = singledispatch(_DispatchNotFound)
        self._dispatch_cache = {}
        self._dependents = {}
        self._hits = 0
        self._misses = 0
        self.dispatch = self._cached_dispatch

    def _cached_dispatch(self, typ: TargetType) -> Hook:
        """Dispatch on the type, caching generated hooks and their deps."""
        self._record_dependency(typ)

        cached = self._dispatch_cache.get(typ)
        if cached is not None:
            self._hits += 1
            return cached

        self._misses += 1
        # A factory may register more hooks while running, which can clear the
        # cache. Snapshot the surrounding frames so the global stack can be
        # restored verbatim once generation finishes.
        outer_frames = list(_GENERATION_STACK)
        _GENERATION_STACK[:] = [*outer_frames, typ]
        try:
            hook = self.dispatch_without_caching(typ)
        finally:
            _GENERATION_STACK[:] = outer_frames

        self._dispatch_cache[typ] = hook
        return hook

    def _record_dependency(self, typ: TargetType) -> None:
        """Link ``typ`` to every hook currently being generated.

        Recording the edge on cache hits as well as misses matters: a hook
        generated late (an ``Optional[T]`` whose ``T`` hook already exists)
        must still be invalidated when ``T`` changes. Linking every active
        frame captures transitive dependencies through nested containers,
        generic arguments and recursive types.
        """
        for owner in _GENERATION_STACK:
            if owner == typ:
                continue
            dependents = self._dependents.setdefault(typ, [])
            if owner not in dependents:
                dependents.append(owner)

    def dispatch_without_caching(self, typ: TargetType) -> Hook:
        """Dispatch on the type but without caching the result."""
        self._record_dependency(typ)

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
        affected: list[TargetType] = []
        for cls, handler in cls_and_handler:
            if direct:
                self._direct_dispatch[cls] = handler
            else:
                self._single_dispatch.register(cls, handler)
                self.clear_direct()
                affected.append(cls)
        if direct:
            # Direct entries are consulted before the cache, so cached hooks
            # stay valid and nothing has to be regenerated.
            return
        if affected:
            for cls in affected:
                # Registering a hook for a class also changes what
                # singledispatch resolves its subclasses to.
                self.invalidate(cls)
                self._invalidate_subclasses(cls)
        else:
            self.clear_cache()

    def _invalidate_subclasses(self, cls: TargetType) -> None:
        """Invalidate cached hooks for the registered subclasses of ``cls``."""
        get_subclasses = getattr(cls, "__subclasses__", None)
        if get_subclasses is None:
            return
        for subclass in get_subclasses():
            self.invalidate(subclass)
            self._invalidate_subclasses(subclass)

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
        self.clear_direct()
        self.clear_cache()

    def clear_direct(self) -> None:
        """Clear the direct dispatch."""
        self._direct_dispatch.clear()

    def invalidate(self, typ: TargetType) -> None:
        """Invalidate the cached hook for ``typ`` and every hook depending on it.

        Only hooks that actually use ``typ`` (directly, through a nested
        container, a generic argument or a recursive reference) are
        regenerated; unrelated cached hooks are left untouched. Hooks that
        were already handed out to callers are plain function objects and are
        intentionally not mutated; callers must go through the converter (or
        ``get_*_hook``) again to pick up the new behavior.
        """
        seen: set[int] = set()
        pending = [typ]
        while pending:
            current = pending.pop()
            marker = id(current)
            if marker in seen:
                continue
            seen.add(marker)

            self._dispatch_cache.pop(current, None)
            pending.extend(self._dependents.pop(current, ()))

    def clear_cache(self) -> None:
        """Clear all caches."""
        self._direct_dispatch.clear()
        self._dispatch_cache.clear()
        self._dependents.clear()
        self._hits = 0
        self._misses = 0

    def cache_info(self) -> tuple[int, int, int]:
        """Return ``(hits, misses, current cache size)`` for the hook cache."""
        return self._hits, self._misses, len(self._dispatch_cache)

    def cache_clear(self) -> None:
        """Alias of :meth:`clear_cache` for parity with ``functools.lru_cache``."""
        self.clear_cache()

    def get_num_fns(self) -> int:
        return self._function_dispatch.get_num_fns()

    def copy_to(self, other: MultiStrategyDispatch, skip: int = 0) -> None:
        self._function_dispatch.copy_to(other._function_dispatch, skip=skip)
        for cls, fn in self._single_dispatch.registry.items():
            other._single_dispatch.register(cls, fn)
        other.clear_cache()
