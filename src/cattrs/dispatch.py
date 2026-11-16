from __future__ import annotations

from functools import lru_cache, singledispatch
from typing import TYPE_CHECKING, Any, Callable, Generic, Literal, TypeVar

from attrs import Factory, define

from ._compat import TypeAlias
from .errors import AmbiguousHookError
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

#: Which mechanism a rule is registered with. The ordering reflects dispatch
#: precedence: ``singledispatch`` is consulted first, then exact (direct)
#: matches, then predicate rules, and finally the fallback.
HookRuleKind: TypeAlias = Literal["singledispatch", "exact", "predicate", "fallback"]
#: Human-readable labels for :data:`HookRuleKind` values.
_RULE_KIND_LABELS: dict[str, str] = {
    "singledispatch": "singledispatch class rule",
    "exact": "exact type rule",
    "predicate": "predicate rule",
    "fallback": "fallback factory",
}


@define
class _DispatchNotFound:
    """A dummy object to help signify a dispatch not found."""


@define
class HookCandidate:
    """A single rule examined while resolving a hook for a type.

    Candidates are produced by diagnostic resolution (``explain``) and never by
    normal dispatch. They describe *why* a rule was or was not selected; the
    underlying hook is exposed on :attr:`handler` for inspection but is never
    invoked while building the report.

    :param kind: How the rule is registered: ``"singledispatch"``, ``"exact"``,
        ``"predicate"`` or ``"fallback"``.
    :param matched: Whether the rule matches the requested type.
    :param handler: The registered hook, or the hook factory for a factory rule
        (the factory is not invoked during resolution).
    :param predicate: The predicate callable, for predicate rules.
    :param registered_for: The class (for ``singledispatch`` rules) or exact
        type (for exact rules) the hook was registered for.
    :param is_factory: Whether :attr:`handler` is a factory that would normally
        be called to produce the actual hook.
    :param takes_converter: Whether the factory additionally receives the
        converter when producing its hook.
    :param predicate_error: The exception raised while evaluating the
        predicate, if any. Such a rule is treated as not matching, but the
        original error is retained instead of being silently swallowed.
    :param shadow_reason: Why this candidate lost to another one, or ``None``
        for the adopted candidate and for rules that did not match.

    .. versionadded:: NEXT
    """

    kind: HookRuleKind
    matched: bool
    handler: Any = None
    predicate: Predicate | None = None
    registered_for: Any = None
    is_factory: bool = False
    takes_converter: bool = False
    predicate_error: BaseException | None = None
    shadow_reason: str | None = None

    @property
    def is_shadowed(self) -> bool:
        """Whether the candidate matched but lost to a higher-priority rule."""
        return self.matched and self.shadow_reason is not None


@define
class HookResolutionReport:
    """The full diagnostic outcome of resolving a hook for one type.

    The report contains every candidate considered, in the order in which the
    dispatcher consults rules, together with the single :attr:`adopted`
    candidate. Normal (un-)structuring does not construct these reports; use the
    ``explain_*_hook`` methods on a converter to produce one on demand.

    :param type: The type being resolved.
    :param candidates: All rules considered, in dispatch order.
    :param adopted: The candidate that would produce the actual hook. It is
        always present: if nothing matches, the fallback factory is adopted.

    .. versionadded:: NEXT
    """

    type: Any
    candidates: list[HookCandidate]
    adopted: HookCandidate

    @property
    def matching_candidates(self) -> list[HookCandidate]:
        """All candidates whose rule matches the type, in dispatch order."""
        return [candidate for candidate in self.candidates if candidate.matched]

    @property
    def shadowed_candidates(self) -> list[HookCandidate]:
        """The matching candidates that lost to a higher-priority rule."""
        return [candidate for candidate in self.candidates if candidate.is_shadowed]

    @property
    def predicate_errors(self) -> list[tuple[HookCandidate, BaseException]]:
        """Predicates that raised while evaluated, paired with their exception."""
        return [
            (candidate, candidate.predicate_error)
            for candidate in self.candidates
            if candidate.predicate_error is not None
        ]

    def __str__(self) -> str:
        lines = [f"Hook resolution for {self.type!r}:"]
        for index, candidate in enumerate(self.candidates):
            label = _RULE_KIND_LABELS[candidate.kind]
            marker = (
                "ADOPTED"
                if candidate is self.adopted
                else ("SHADOWED" if candidate.is_shadowed else "skipped")
            )
            line = f"  {index}. {label} [{marker}]"
            if candidate.registered_for is not None:
                line += f" registered for {candidate.registered_for!r}"
            if candidate.is_factory:
                line += " (factory)"
            if candidate.predicate_error is not None:
                line += (
                    f", predicate raised "
                    f"{type(candidate.predicate_error).__name__}: "
                    f"{candidate.predicate_error}"
                )
            elif not candidate.matched:
                line += ", predicate did not match"
            if candidate.shadow_reason is not None:
                line += f" - {candidate.shadow_reason}"
            lines.append(line)
        return "\n".join(lines)


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

    def explain(
        self, typ: Any, *, strict: bool = False, already_matched: bool = False
    ) -> list[HookCandidate]:
        """Explain how predicate rules resolve for ``typ`` without invoking them.

        Every registered predicate is evaluated and a :class:`HookCandidate` is
        produced for it, in dispatch order (most recently registered first).
        Exceptions raised by predicates are captured on the candidate instead
        of being silently skipped; such candidates never match.

        No handler or factory is called, so this method has no side effects on
        generated hooks or caches.

        :param typ: The type to resolve.
        :param strict: When true, raise
            :class:`~cattrs.errors.AmbiguousHookError` if more than one
            predicate matches, even if a higher-priority rule elsewhere would
            win the overall resolution.
        :param already_matched: Whether a rule outside the function dispatch (a
            ``singledispatch`` or exact rule) already matches. Matched
            predicate candidates are then marked as shadowed by it.

        .. versionadded:: NEXT
        """
        candidates: list[HookCandidate] = []
        first_match: HookCandidate | None = None
        for predicate, handler, is_generator, takes_converter in self._handler_pairs:
            error: BaseException | None = None
            try:
                matched = bool(predicate(typ))
            except Exception as exc:  # retain the reason instead of skipping
                matched = False
                error = exc
            candidate = HookCandidate(
                kind="predicate",
                matched=matched,
                handler=handler,
                predicate=predicate,
                is_factory=bool(is_generator),
                takes_converter=takes_converter,
                predicate_error=error,
            )
            if matched:
                if first_match is None:
                    first_match = candidate
                elif strict:
                    self._raise_ambiguity(typ, candidates, candidate)
                if already_matched:
                    candidate.shadow_reason = (
                        "shadowed by a higher-priority singledispatch or exact rule"
                    )
                elif first_match is not candidate:
                    candidate.shadow_reason = (
                        "shadowed by an earlier-registered predicate rule"
                    )
            candidates.append(candidate)
        return candidates

    @staticmethod
    def _raise_ambiguity(
        typ: Any, candidates: list[HookCandidate], latest: HookCandidate
    ) -> None:
        matching = [
            candidate for candidate in [*candidates, latest] if candidate.matched
        ]
        raise AmbiguousHookError(
            f"Multiple predicate hooks match {typ!r}; cannot determine a unique "
            f"hook in strict mode.",
            typ,
            matching,
        )

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

    def explain(self, typ: TargetType, *, strict: bool = False) -> HookResolutionReport:
        """Explain how a hook for ``typ`` would be resolved, without producing it.

        The returned :class:`HookResolutionReport` lists every candidate in the
        order in which the dispatcher consults rules (``singledispatch`` class
        rules, exact-type rules, predicate rules and finally the fallback
        factory), marks the adopted candidate and explains why every other
        matching candidate was shadowed.

        Neither hooks nor hook factories are invoked, so this method has no side
        effects and never populates the dispatch cache. Exceptions raised by
        predicates are retained on their candidates instead of being swallowed.

        :param typ: The type to resolve.
        :param strict: When true, raise
            :class:`~cattrs.errors.AmbiguousHookError` if more than one
            predicate rule matches. The check runs even when a
            ``singledispatch`` or exact rule would win, to surface latent
            ambiguity among the predicates.

        .. versionadded:: NEXT
        """
        candidates: list[HookCandidate] = []

        # singledispatch class rules are consulted first.
        single_handler: Any = None
        single_registered_for: Any = None
        try:
            single_handler = self._single_dispatch.dispatch(typ)
        except Exception:  # mirror dispatch_without_caching's behavior
            single_handler = _DispatchNotFound
        if single_handler is not _DispatchNotFound:
            # Recover the class the handler was actually registered for. When
            # `typ` is a class, singledispatch walks its MRO itself.
            mro = typ.__mro__ if isinstance(typ, type) else ()
            for cls in mro:
                if self._single_dispatch.registry.get(cls) is single_handler:
                    single_registered_for = cls
                    break
            single_candidate = HookCandidate(
                kind="singledispatch",
                matched=True,
                handler=single_handler,
                registered_for=single_registered_for,
            )
            candidates.append(single_candidate)

        # Exact (direct) matches come next.
        direct_handler = self._direct_dispatch.get(typ)
        if direct_handler is not None:
            direct_candidate = HookCandidate(
                kind="exact", matched=True, handler=direct_handler, registered_for=typ
            )
            if candidates:
                direct_candidate.shadow_reason = (
                    "shadowed by a higher-priority singledispatch class rule"
                )
            candidates.append(direct_candidate)

        higher_priority_matched = bool(candidates)

        # Predicate rules, in their own dispatch order. May raise in strict mode.
        predicate_candidates = self._function_dispatch.explain(
            typ, strict=strict, already_matched=higher_priority_matched
        )
        candidates.extend(predicate_candidates)

        # The fallback factory always "matches" but is only adopted last.
        fallback_candidate = HookCandidate(
            kind="fallback",
            matched=True,
            handler=self._fallback_factory,
            is_factory=True,
        )
        candidates.append(fallback_candidate)

        adopted = next(
            (candidate for candidate in candidates if candidate.matched),
            fallback_candidate,
        )
        if adopted is not fallback_candidate:
            fallback_candidate.shadow_reason = (
                f"shadowed by a matching {_RULE_KIND_LABELS[adopted.kind]}"
            )

        return HookResolutionReport(typ, candidates, adopted)

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
