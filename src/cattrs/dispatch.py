from __future__ import annotations

from functools import lru_cache, singledispatch
from typing import TYPE_CHECKING, Any, Callable, Generic, Literal, NamedTuple, TypeVar

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

#: Where a resolved hook ultimately came from.
HookSourceKind: TypeAlias = Literal[
    "singledispatch", "direct", "function", "factory", "fallback"
]


class ResolvedHook(NamedTuple):
    """The outcome of resolving a hook for a type, without exposing the
    dispatch internals.

    :param hook: The hook that would be used to (un)structure the type.
    :param kind: Where the hook came from.
    :param handler: The registered handler or factory that produced the hook;
        for the fallback, the fallback factory itself.
    :param predicate: The matching predicate for function-dispatch entries.
    :param generated: Whether the handler was a hook factory (and so was
        invoked to produce the hook) rather than a directly registered hook.
    """

    hook: Callable[..., Any]
    kind: HookSourceKind
    handler: Callable[..., Any]
    predicate: Predicate | None
    generated: bool


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
    # An optional recorder invoked whenever a hook factory is run. It receives
    # the type, the factory and the produced hook. Used by the planning API to
    # honestly report hook-factory side effects during preparation.
    factory_recorder: (
        Callable[[TargetType, Callable[..., Any], Callable[..., Any]], None] | None
    ) = None

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
                        hook = handler(typ, self._converter)
                    else:
                        hook = handler(typ)
                    if self.factory_recorder is not None:
                        self.factory_recorder(typ, handler, hook)
                    return hook

                return handler
        return None

    def resolve(
        self, typ: Any
    ) -> tuple[Callable[..., Any], Predicate, bool, bool] | None:
        """Resolve the matching handler pair for ``typ``.

        Returns a tuple of ``(handler, predicate, is_generator,
        takes_converter)``, or ``None`` if no predicate matches. Factory
        handlers are invoked (and recorded through ``factory_recorder``),
        mirroring :meth:`dispatch`.
        """
        for can_handle, handler, is_generator, takes_converter in self._handler_pairs:
            try:
                ch = can_handle(typ)
            except Exception:  # noqa: S112
                continue
            if ch:
                if is_generator:
                    if takes_converter:
                        hook = handler(typ, self._converter)
                    else:
                        hook = handler(typ)
                    if self.factory_recorder is not None:
                        self.factory_recorder(typ, handler, hook)
                    return hook, can_handle, True, takes_converter
                return handler, can_handle, False, takes_converter
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
    #: The singledispatch registry entries present after the converter's
    #: built-in class hooks were registered. Later registrations are custom.
    _builtin_single_dispatch: frozenset[Callable[..., Any]]
    dispatch: Callable[[TargetType, BaseConverter], Hook]

    def __init__(
        self, fallback_factory: HookFactory[Hook], converter: BaseConverter
    ) -> None:
        self._fallback_factory = fallback_factory
        self._direct_dispatch = {}
        self._function_dispatch = FunctionDispatch(converter)
        self._single_dispatch = singledispatch(_DispatchNotFound)
        self._builtin_single_dispatch = frozenset()
        self.dispatch = lru_cache(maxsize=None)(self.dispatch_without_caching)

    def snapshot_builtin_single_dispatch(self) -> None:
        """Freeze the current singledispatch registry as the built-in set."""
        self._builtin_single_dispatch = frozenset(
            self._single_dispatch.registry.values()
        )

    def dispatch_without_caching(self, typ: TargetType) -> Hook:
        """Dispatch on the type but without caching the result."""
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
        if res is not None:
            return res
        # The fallback itself is a hook factory; record it so the planning API
        # can attribute a missing handler to the exact type that reaches it.
        recorder = self._function_dispatch.factory_recorder
        try:
            hook = self._fallback_factory(typ)
        except BaseException:
            if recorder is not None:
                recorder(typ, self._fallback_factory, None)
            raise
        if recorder is not None:
            recorder(typ, self._fallback_factory, hook)
        return hook

    def resolve(self, typ: TargetType) -> ResolvedHook:
        """Resolve the hook for ``typ`` and describe where it came from.

        Unlike :meth:`dispatch`, this never caches, and it returns the
        selected handler alongside its source. Hook factories selected by the
        function dispatch are still invoked (and reported through the function
        dispatch's ``factory_recorder``), because a hook cannot be selected
        without producing it.
        """
        try:
            hook = self._single_dispatch.dispatch(typ)
            if hook is not _DispatchNotFound:
                return ResolvedHook(hook, "singledispatch", hook, None, False)
        except Exception:  # noqa: S110
            pass

        direct_hook = self._direct_dispatch.get(typ)
        if direct_hook is not None:
            return ResolvedHook(
                direct_hook, "direct", direct_hook, None, False
            )

        resolved = self._function_dispatch.resolve(typ)
        if resolved is not None:
            hook, predicate, is_generator, _takes_converter = resolved
            if is_generator:
                return ResolvedHook(hook, "factory", hook, predicate, True)
            return ResolvedHook(hook, "function", hook, predicate, False)

        # The fallback itself is a hook factory, so record its invocation.
        recorder = self._function_dispatch.factory_recorder
        try:
            hook = self._fallback_factory(typ)
        except BaseException:
            if recorder is not None:
                recorder(typ, self._fallback_factory, None)
            raise
        if recorder is not None:
            recorder(typ, self._fallback_factory, hook)
        return ResolvedHook(hook, "fallback", self._fallback_factory, None, True)

    def set_factory_recorder(
        self,
        recorder: (
            Callable[[TargetType, Callable[..., Any], Callable[..., Any]], None] | None
        ),
    ) -> None:
        """Install (or remove) a recorder for invoked hook factories."""
        self._function_dispatch.factory_recorder = recorder

    def register_cls_list(self, cls_and_handler, direct: bool = False) -> None:
        """Register a class to direct or singledispatch."""
        for cls, handler in cls_and_handler:
            if direct:
                self._direct_dispatch[cls] = handler
            else:
                self._single_dispatch.register(cls, handler)
                self.clear_direct()
        self.dispatch.cache_clear()

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
        self.dispatch.cache_clear()

    def clear_direct(self) -> None:
        """Clear the direct dispatch."""
        self._direct_dispatch.clear()

    def clear_cache(self) -> None:
        """Clear all caches."""
        self._direct_dispatch.clear()
        self.dispatch.cache_clear()

    def get_num_fns(self) -> int:
        return self._function_dispatch.get_num_fns()

    def copy_to(self, other: MultiStrategyDispatch, skip: int = 0) -> None:
        self._function_dispatch.copy_to(other._function_dispatch, skip=skip)
        for cls, fn in self._single_dispatch.registry.items():
            other._single_dispatch.register(cls, fn)
        # Preserve the built-in hook set so the planning API still recognizes
        # built-in class hooks after copying.
        other._builtin_single_dispatch = self._builtin_single_dispatch
        other.clear_cache()
