"""Tests for diagnostic hook resolution (``explain_*_hook``)."""

from typing import Any, List

import pytest

from cattrs import AmbiguousHookError, BaseConverter, Converter
from cattrs.dispatch import (
    FunctionDispatch,
    HookCandidate,
    HookResolutionReport,
    MultiStrategyDispatch,
)


class Base:
    pass


class Sub(Base):
    pass


class Other:
    pass


def test_report_structure_and_default_contract(converter_cls):
    converter = converter_cls()
    report = converter.explain_structure_hook(int)

    assert isinstance(report, HookResolutionReport)
    assert report.type is int
    # The fallback is always present and always a factory.
    assert report.candidates[-1].kind == "fallback"
    assert report.candidates[-1].is_factory
    assert report.adopted in report.candidates
    # Fallback is only adopted when nothing else matches.
    assert report.adopted is not report.candidates[-1]


def test_candidate_order_follows_dispatch_precedence(converter_cls):
    converter = converter_cls()
    predicate_handler = lambda o: "p"  # noqa: E731
    converter.register_unstructure_hook_func(
        lambda t: issubclass(t, Base), predicate_handler
    )
    exact_handler = lambda o: "e"  # noqa: E731
    converter.register_unstructure_hook(Sub, exact_handler)

    report = converter.explain_unstructure_hook(Sub)
    kinds = [candidate.kind for candidate in report.candidates]
    assert kinds.index("singledispatch") < kinds.index("predicate")
    assert kinds.index("predicate") < kinds.index("fallback")

    assert report.adopted.kind == "singledispatch"
    assert report.adopted.handler is exact_handler

    shadowed = report.shadowed_candidates
    assert predicate_handler in {candidate.handler for candidate in shadowed}
    assert all(candidate.shadow_reason is not None for candidate in shadowed)
    # The fallback is shadowed too, but not counted among rule shadows.
    fallback = report.candidates[-1]
    assert fallback.is_shadowed
    assert "singledispatch" in fallback.shadow_reason


def test_singledispatch_registered_for_resolves_mro_class(converter_cls):
    converter = converter_cls()
    handler = lambda o: "base"  # noqa: E731
    converter.register_unstructure_hook(Base, handler)

    report = converter.explain_unstructure_hook(Sub)
    single = next(
        candidate
        for candidate in report.candidates
        if candidate.kind == "singledispatch"
    )
    assert single.registered_for is Base
    assert single.matched
    assert single.handler is handler


def test_predicate_candidates_in_registration_order(converter_cls):
    converter = converter_cls()
    converter.register_unstructure_hook_func(
        lambda t: issubclass(t, Base), lambda o: "base"
    )
    converter.register_unstructure_hook_func(
        lambda t: issubclass(t, Sub), lambda o: "sub"
    )

    report = converter.explain_unstructure_hook(Sub)
    predicates = [
        candidate
        for candidate in report.candidates
        if candidate.kind == "predicate" and candidate.matched
    ]
    # Most recently registered predicate is consulted first and adopted.
    assert len(predicates) == 2
    assert report.adopted is predicates[0]
    assert predicates[0].handler(None) == "sub"
    assert predicates[1].is_shadowed
    assert "earlier-registered" in predicates[1].shadow_reason


def test_strict_mode_rejects_same_priority_ambiguity(converter_cls):
    converter = converter_cls()
    converter.register_unstructure_hook_func(
        lambda t: issubclass(t, Base), lambda o: "base"
    )
    converter.register_unstructure_hook_func(
        lambda t: issubclass(t, Sub), lambda o: "sub"
    )

    # Non-strict resolution is deterministic (insertion order).
    report = converter.explain_unstructure_hook(Sub)
    assert report.adopted.handler(None) == "sub"

    with pytest.raises(AmbiguousHookError) as exc_info:
        converter.explain_unstructure_hook(Sub, strict=True)
    error = exc_info.value
    assert error.type_ is Sub
    assert len(error.candidates) == 2
    assert all(isinstance(candidate, HookCandidate) for candidate in error.candidates)


def test_strict_mode_surfaces_latent_ambiguity_behind_class_rule(converter_cls):
    converter = converter_cls()
    converter.register_unstructure_hook(Sub, lambda o: "exact")
    converter.register_unstructure_hook_func(
        lambda t: issubclass(t, Base), lambda o: "base"
    )
    converter.register_unstructure_hook_func(
        lambda t: issubclass(t, Sub), lambda o: "sub"
    )

    # Non-strict: the class rule wins and the predicates are shadowed.
    report = converter.explain_unstructure_hook(Sub)
    assert report.adopted.kind == "singledispatch"

    # Strict: ambiguous predicates are still reported as a problem.
    with pytest.raises(AmbiguousHookError):
        converter.explain_unstructure_hook(Sub, strict=True)


def test_predicate_exceptions_are_retained_not_skipped(converter_cls):
    converter = converter_cls()

    def raising_predicate(_: Any) -> bool:
        raise ValueError("predicate exploded")

    converter.register_unstructure_hook_func(raising_predicate, lambda o: "unreachable")
    converter.register_unstructure_hook_func(
        lambda t: issubclass(t, Sub), lambda o: "sub"
    )

    report = converter.explain_unstructure_hook(Sub)
    errors = report.predicate_errors
    assert len(errors) == 1
    candidate, error = errors[0]
    assert candidate.predicate is raising_predicate
    assert isinstance(error, ValueError)
    assert str(error) == "predicate exploded"
    assert not candidate.matched
    # A healthy predicate still resolves normally.
    assert report.adopted.handler(None) == "sub"


def test_fallback_adopted_when_nothing_matches(converter_cls):
    converter = converter_cls()
    report = converter.explain_unstructure_hook(Other)
    assert report.adopted.kind == "fallback"
    assert report.matching_candidates == [report.adopted]
    assert report.shadowed_candidates == []


def test_hooks_and_factories_are_never_invoked(converter_cls):
    converter = converter_cls()
    hook_calls = 0
    factory_calls = 0

    def hook(_):
        nonlocal hook_calls
        hook_calls += 1

    def factory(_):
        nonlocal factory_calls
        factory_calls += 1
        return hook

    converter.register_unstructure_hook(Other, hook)
    converter.register_unstructure_hook_factory(lambda t: issubclass(t, Base), factory)

    report = converter.explain_unstructure_hook(Sub)
    assert factory_calls == 0
    assert hook_calls == 0
    factory_candidate = next(
        candidate
        for candidate in report.candidates
        if candidate.kind == "predicate" and candidate.matched
    )
    assert factory_candidate.is_factory
    assert factory_candidate.handler is factory

    # Real dispatch still works and invokes the factory exactly once.
    converter.get_unstructure_hook(Sub)
    assert factory_calls == 1


def test_explain_does_not_populate_dispatch_cache(converter_cls):
    converter = converter_cls()
    converter.register_unstructure_hook_func(
        lambda t: issubclass(t, Base), lambda o: "base"
    )
    dispatch = converter._unstructure_func.dispatch
    before = dispatch.cache_info()
    converter.explain_unstructure_hook(Sub)
    assert dispatch.cache_info() == before

    # And normal dispatch does populate it, proving explain bypassed the cache.
    converter.get_unstructure_hook(Sub)
    after = dispatch.cache_info()
    assert after.misses > before.misses


def test_new_registrations_are_reflected_immediately(converter_cls):
    converter = converter_cls()
    converter.register_unstructure_hook_func(
        lambda t: issubclass(t, Base), lambda o: "first"
    )
    first_report = converter.explain_unstructure_hook(Sub)
    assert first_report.adopted.handler(None) == "first"

    converter.register_unstructure_hook_func(
        lambda t: issubclass(t, Sub), lambda o: "second"
    )
    second_report = converter.explain_unstructure_hook(Sub)
    assert second_report.adopted.handler(None) == "second"
    # Previously produced reports keep their own snapshot.
    assert first_report.adopted.handler(None) == "first"


def test_deletion_of_rule_is_reflected(converter_cls):
    converter = converter_cls()
    func_dispatch = converter._unstructure_func._function_dispatch
    converter.register_unstructure_hook_func(
        lambda t: issubclass(t, Sub), lambda o: "sub"
    )
    assert converter.explain_unstructure_hook(Sub).adopted.handler(None) == "sub"

    # Remove the registered predicate pair directly (no public removal API).
    func_dispatch._handler_pairs.clear()
    report = converter.explain_unstructure_hook(Sub)
    assert report.adopted.kind == "fallback"


def test_converters_do_not_pollute_each_other():
    first = BaseConverter()
    second = BaseConverter()
    first.register_unstructure_hook_func(
        lambda t: issubclass(t, Base), lambda o: "first"
    )

    first_report = first.explain_unstructure_hook(Sub)
    second_report = second.explain_unstructure_hook(Sub)
    assert first_report.adopted.handler(None) == "first"
    assert second_report.adopted.kind == "fallback"

    second.register_unstructure_hook_func(
        lambda t: issubclass(t, Base), lambda o: "second"
    )
    assert second.explain_unstructure_hook(Sub).adopted.handler(None) == "second"
    assert first.explain_unstructure_hook(Sub).adopted.handler(None) == "first"


def test_explain_structure_and_unstructure_are_independent(converter_cls):
    converter = converter_cls()
    converter.register_unstructure_hook(Sub, lambda o: "un")
    converter.register_structure_hook(Sub, lambda v, _: Sub())

    un_report = converter.explain_unstructure_hook(Sub)
    st_report = converter.explain_structure_hook(Sub)
    assert un_report.adopted.kind == "singledispatch"
    assert st_report.adopted.kind == "singledispatch"
    assert un_report.adopted is not st_report.adopted


def test_exact_direct_dispatch_candidate():
    # The generated Converter registers specialized collection hooks directly.
    converter = Converter()
    converter.unstructure([1, 2, 3], unstructure_as=List[int])

    report = converter.explain_unstructure_hook(List[int])
    exact = [candidate for candidate in report.candidates if candidate.kind == "exact"]
    assert len(exact) == 1
    assert exact[0].matched
    assert report.adopted is exact[0]


def test_report_renders_readable_summary(converter_cls):
    converter = converter_cls()
    converter.register_unstructure_hook(Sub, lambda o: "exact")
    converter.register_unstructure_hook_func(
        lambda t: issubclass(t, Base), lambda o: "base"
    )
    text = str(converter.explain_unstructure_hook(Sub))
    assert "Hook resolution" in text
    assert "ADOPTED" in text
    assert "SHADOWED" in text
    assert "fallback factory" in text


def test_function_dispatch_explain_keeps_predicate_errors():
    dispatch = FunctionDispatch(BaseConverter())

    def raising(_):
        raise RuntimeError("nope")

    dispatch.register(raising, "error")
    dispatch.register(lambda cls: issubclass(cls, float), "float")

    candidates = dispatch.explain(float)
    assert len(candidates) == 2
    # Registration order puts the healthy predicate first.
    assert candidates[0].matched
    assert candidates[0].handler == "float"
    assert not candidates[1].matched
    assert isinstance(candidates[1].predicate_error, RuntimeError)


def test_multi_strategy_dispatch_explain_strict():
    dispatch = MultiStrategyDispatch(lambda _: lambda: "fallback", BaseConverter())
    dispatch.register_func_list(
        [
            (lambda cls: issubclass(cls, Base), "base"),
            (lambda cls: issubclass(cls, Sub), "sub"),
        ]
    )
    report = dispatch.explain(Sub)
    assert report.adopted.handler == "sub"

    with pytest.raises(AmbiguousHookError):
        dispatch.explain(Sub, strict=True)


def test_fast_conversion_still_works_after_diagnostics(converter_cls):
    converter = converter_cls()
    converter.register_unstructure_hook_func(
        lambda t: issubclass(t, Base), lambda o: "base"
    )
    converter.register_structure_hook(Sub, lambda v, _: Sub())

    converter.explain_unstructure_hook(Sub)
    converter.explain_structure_hook(Sub)

    assert converter.unstructure(Sub()) == "base"
    assert isinstance(converter.structure({}, Sub), Sub)
