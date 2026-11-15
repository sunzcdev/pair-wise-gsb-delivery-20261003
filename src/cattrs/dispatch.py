from __future__ import annotations

from functools import lru_cache, singledispatch
from typing import TYPE_CHECKING, Any, Callable, Generic, Literal, TypeVar

from attrs import Factory, define

from ._compat import TypeAlias
from .diagnostics import (
    DIRECT_DISPATCH,
    DISPATCH_ERRORED,
    FALLBACK,
    HookCandidate,
    HookResolutionReport,
    PREDICATE_DISPATCH,
    PREDICATE_ERRORED,
    RULE_DID_NOT_MATCH,
    SHADOWED_BY_EARLIER_RULE,
    SHADOWED_BY_HIGHER_PRIORITY,
    SINGLE_DISPATCH,
)
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

    def inspect(self, typ: Any) -> list[HookCandidate]:
        """Describe every predicate rule for ``typ`` without producing hooks.

        Predicates are evaluated in dispatch (registration) order. A raising
        predicate is captured on its candidate instead of being skipped, and
        neither handlers nor hook factories are ever invoked.
        """
        candidates: list[HookCandidate] = []
        for order, (predicate, handler, is_generator, _) in enumerate(
            self._handler_pairs
        ):
            try:
                matches = bool(predicate(typ))
            except Exception as exc:
                candidates.append(
                    HookCandidate(
                        tier=PREDICATE_DISPATCH,
                        rule=predicate,
                        hook=None,
                        matches=False,
                        order=order,
                        is_factory=bool(is_generator),
                        shadowed_by=PREDICATE_ERRORED,
                        error=exc,
                    )
                )
                continue
            candidates.append(
                HookCandidate(
                    tier=PREDICATE_DISPATCH,
                    rule=predicate,
                    hook=None if is_generator else handler,
                    matches=matches,
                    order=order,
                    is_factory=bool(is_generator),
                    shadowed_by=None if matches else RULE_DID_NOT_MATCH,
                )
            )
        return candidates

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
    dispatch: Callable[[TargetType, BaseConverter], Hook]

    def __init__(
        self, fallback_factory: HookFactory[Hook], converter: BaseConverter
    ) -> None:
        self._fallback_factory = fallback_factory
        self._direct_dispatch = {}
        self._function_dispatch = FunctionDispatch(converter)
        self._single_dispatch = singledispatch(_DispatchNotFound)
        self.dispatch = lru_cache(maxsize=None)(self.dispatch_without_caching)

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
        return res if res is not None else self._fallback_factory(typ)

    def explain(
        self, typ: TargetType, reject_ambiguity: bool = False
    ) -> HookResolutionReport:
        """Explain how a hook would be resolved for ``typ``.

        The returned report lists every candidate in dispatch order, marks the
        rule that normal dispatch would adopt and explains why every other
        candidate is shadowed.

        This never invokes a registered hook or hook factory; only predicates
        are evaluated, and their exceptions are captured in the report.

        :param reject_ambiguity: When true, raise
            :class:`~cattrs.diagnostics.AmbiguousHookError` if several
            same-priority predicate rules match ``typ``.
        """
        candidates: list[HookCandidate] = []

        # Tier 1: exact-class singledispatch, MRO order (first match wins).
        handler_to_cls = {
            handler: cls
            for cls, handler in self._single_dispatch.registry.items()
            if handler is not _DispatchNotFound
        }
        try:
            winning_handler = self._single_dispatch.dispatch(typ)
        except Exception as exc:
            winning_handler = None
            candidates.append(
                HookCandidate(
                    tier=SINGLE_DISPATCH,
                    rule=typ,
                    hook=None,
                    matches=False,
                    order=0,
                    shadowed_by=DISPATCH_ERRORED,
                    error=exc,
                )
            )
        else:
            if winning_handler is not _DispatchNotFound:
                order = 0
                for base in getattr(typ, "__mro__", ()):
                    handler = self._single_dispatch.registry.get(base)
                    if handler is None or handler is _DispatchNotFound:
                        continue
                    candidates.append(
                        HookCandidate(
                            tier=SINGLE_DISPATCH,
                            rule=base,
                            hook=handler,
                            matches=True,
                            order=order,
                            shadowed_by=None
                            if handler is winning_handler
                            else SHADOWED_BY_EARLIER_RULE,
                        )
                    )
                    order += 1
                if not any(
                    candidate.hook is winning_handler for candidate in candidates
                ):
                    # singledispatch can also resolve via virtual subclasses,
                    # which do not appear in the type's MRO.
                    candidates.append(
                        HookCandidate(
                            tier=SINGLE_DISPATCH,
                            rule=handler_to_cls.get(winning_handler),
                            hook=winning_handler,
                            matches=True,
                            order=order,
                        )
                    )

        # Tier 2: direct (generated) exact-match hooks.
        direct_hook = self._direct_dispatch.get(typ)
        candidates.append(
            HookCandidate(
                tier=DIRECT_DISPATCH,
                rule=typ,
                hook=direct_hook,
                matches=direct_hook is not None,
                order=0,
                shadowed_by=None if direct_hook is not None else RULE_DID_NOT_MATCH,
            )
        )

        # Tier 3: predicate rules, registration (first-match) order.
        candidates.extend(self._function_dispatch.inspect(typ))

        # Tier 4: the fallback factory, never invoked by diagnostics.
        candidates.append(
            HookCandidate(
                tier=FALLBACK,
                rule=self._fallback_factory,
                hook=None,
                matches=True,
                order=0,
                is_factory=True,
            )
        )

        # The first matching, non-errored candidate mirrors
        # `dispatch_without_caching`; everything below it is shadowed.
        adopted: HookCandidate | None = None
        for candidate in candidates:
            if adopted is None:
                if candidate.matches and candidate.error is None:
                    adopted = candidate
                continue
            if candidate.error is None and candidate.shadowed_by is None:
                candidate.shadowed_by = (
                    SHADOWED_BY_EARLIER_RULE
                    if candidate.tier == adopted.tier
                    else SHADOWED_BY_HIGHER_PRIORITY
                )

        report = HookResolutionReport(
            type=typ, candidates=candidates, adopted=adopted
        )
        if reject_ambiguity:
            report.ensure_unambiguous()
        return report

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
        other.clear_cache()
