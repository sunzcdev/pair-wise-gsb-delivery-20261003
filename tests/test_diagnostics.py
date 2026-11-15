from collections.abc import Sized

import attrs
import pytest

from cattrs import AmbiguousHookError, BaseConverter, Converter
from cattrs.diagnostics import (
    DIRECT_DISPATCH,
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


class Base:
    pass


class Sub(Base):
    pass


class Other:
    pass


@pytest.mark.parametrize("converter_cls", [BaseConverter, Converter])
def test_explain_structure_and_unstructure_share_semantics(converter_cls):
    converter = converter_cls()
    converter.register_structure_hook(Base, lambda v, _: Base())
    converter.register_unstructure_hook(Base, lambda o: "base")

    for report in (
        converter.explain_structure_hook(Sub),
        converter.explain_unstructure_hook(Sub),
    ):
        assert isinstance(report, HookResolutionReport)
        assert report.type is Sub
        assert report.adopted is not None
        assert report.adopted.adopted
        assert report.adopted.tier == SINGLE_DISPATCH
        assert report.adopted.rule is Base
        assert [candidate.tier for candidate in report.candidates[:2]] == [
            SINGLE_DISPATCH,
            DIRECT_DISPATCH,
        ]
        assert report.candidates[-1].tier == FALLBACK


def test_exact_class_rule_shadows_base_class_and_predicates():
    converter = BaseConverter()
    exact_hook = lambda o: "exact"
    base_hook = lambda o: "base"
    predicate_hook = lambda o: "predicate"
    converter.register_unstructure_hook(Base, base_hook)
    converter.register_unstructure_hook_func(
        lambda t: issubclass(t, Base), predicate_hook
    )
    converter.register_unstructure_hook(Sub, exact_hook)

    report = converter.explain_unstructure_hook(Sub)

    assert report.adopted.rule is Sub
    assert report.adopted.hook is exact_hook
    single = [c for c in report.candidates if c.tier == SINGLE_DISPATCH]
    assert [c.rule for c in single] == [Sub, Base]
    assert single[0].shadowed_by is None
    assert single[1].shadowed_by is SHADOWED_BY_EARLIER_RULE
    matching_predicate = next(
        c
        for c in report.candidates
        if c.tier == PREDICATE_DISPATCH and c.matches
    )
    assert matching_predicate.shadowed_by is SHADOWED_BY_HIGHER_PRIORITY
    assert report.candidates[-1].shadowed_by is SHADOWED_BY_HIGHER_PRIORITY


def test_predicate_rules_are_first_match_wins():
    """New predicate rules are inserted at the front, so later wins."""
    converter = BaseConverter()
    first = lambda v, t: "first"
    second = lambda v, t: "second"
    converter.register_structure_hook_func(
        lambda t: issubclass(t, Base), first
    )
    converter.register_structure_hook_func(
        lambda t: issubclass(t, Base), second
    )

    report = converter.explain_structure_hook(Sub)

    matching = [
        c
        for c in report.candidates
        if c.tier == PREDICATE_DISPATCH and c.matches
    ]
    assert [c.hook for c in matching] == [second, first]
    assert report.adopted.hook is second
    assert matching[1].shadowed_by is SHADOWED_BY_EARLIER_RULE
    assert report.ambiguous


def test_higher_tier_shadows_matching_predicates():
    converter = BaseConverter()
    base_hook = lambda v, t: "base"
    predicate_hook = lambda v, t: "predicate"
    converter.register_structure_hook(Base, base_hook)
    converter.register_structure_hook_func(
        lambda t: issubclass(t, Base), predicate_hook
    )

    report = converter.explain_structure_hook(Sub)

    assert report.adopted.tier == SINGLE_DISPATCH
    assert report.adopted.hook is base_hook
    matching_predicates = [
        c
        for c in report.candidates
        if c.tier == PREDICATE_DISPATCH and c.matches
    ]
    assert len(matching_predicates) == 1
    assert (
        matching_predicates[0].shadowed_by is SHADOWED_BY_HIGHER_PRIORITY
    )


def test_reject_ambiguity_raises_with_report_attached():
    converter = BaseConverter()
    converter.register_structure_hook_func(
        lambda t: issubclass(t, Base), lambda v, t: "a"
    )
    converter.register_structure_hook_func(
        lambda t: issubclass(t, Base), lambda v, t: "b"
    )

    with pytest.raises(AmbiguousHookError) as exc_info:
        converter.explain_structure_hook(Sub, reject_ambiguity=True)

    error = exc_info.value
    assert error.type is Sub
    assert error.report.ambiguous
    # Without the flag, normal dispatch semantics are preserved.
    assert converter.explain_structure_hook(Sub).adopted is not None


def test_predicate_exception_is_retained_not_skipped_silently():
    converter = BaseConverter()

    def raising_predicate(t):
        raise ValueError("predicate exploded")

    converter.register_unstructure_hook_func(raising_predicate, lambda o: None)

    report = converter.explain_unstructure_hook(Sub)

    errored = report.errored_candidates
    assert len(errored) == 1
    candidate = errored[0]
    assert candidate.tier == PREDICATE_DISPATCH
    assert candidate.shadowed_by is PREDICATE_ERRORED
    assert isinstance(candidate.error, ValueError)
    assert str(candidate.error) == "predicate exploded"
    assert candidate.hook is None
    # The errored rule never becomes the adopted rule.
    assert report.adopted is not candidate
    assert all(c is not candidate for c in report.matching_candidates)
    # Normal dispatch still works and treats the predicate as non-matching.
    converter.unstructure(Sub())


def test_diagnostics_never_invoke_hooks_or_factories():
    converter = BaseConverter()

    def boom_hook(*_):
        raise AssertionError("a hook was invoked")

    def boom_factory(t):
        raise AssertionError("a factory was invoked")

    def boom_fallback(t):
        raise AssertionError("the fallback factory was invoked")

    converter.register_structure_hook(Base, boom_hook)
    converter.register_structure_hook_func(
        lambda t: issubclass(t, Base), boom_hook
    )
    converter.register_structure_hook_factory(
        lambda t: issubclass(t, Other), boom_factory
    )

    custom = BaseConverter(
        structure_fallback_factory=boom_fallback,
    )
    custom.register_structure_hook(Base, boom_hook)

    report = converter.explain_structure_hook(Sub)
    assert report.adopted.hook is boom_hook

    factory_candidate = next(
        c
        for c in report.candidates
        if c.tier == PREDICATE_DISPATCH and c.is_factory
    )
    assert factory_candidate.hook is None

    fallback_report = custom.explain_structure_hook(Other)
    assert fallback_report.adopted.tier == FALLBACK
    assert fallback_report.adopted.is_factory
    assert fallback_report.adopted.hook is None


def test_fallback_is_adopted_when_nothing_matches():
    converter = BaseConverter()
    report = converter.explain_structure_hook(Other)

    assert report.adopted.tier == FALLBACK
    assert report.adopted.rule is converter._structure_func._fallback_factory
    non_fallback = report.candidates[:-1]
    assert all(
        not candidate.matches or candidate.error is not None
        for candidate in non_fallback
    )


def test_report_reflects_registrations_immediately():
    converter = BaseConverter()

    before = converter.explain_unstructure_hook(Sub)
    assert before.adopted.tier == FALLBACK

    hook = lambda o: "registered"
    converter.register_unstructure_hook(Sub, hook)

    after = converter.explain_unstructure_hook(Sub)
    assert after.adopted.tier == SINGLE_DISPATCH
    assert after.adopted.hook is hook

    # Cached dispatch stays consistent with the diagnostic report.
    assert converter.get_unstructure_hook(Sub) is hook


def test_report_reflects_rule_removal_immediately():
    converter = BaseConverter()
    predicate = lambda t: issubclass(t, Base)
    hook = lambda o: "registered"
    converter.register_unstructure_hook_func(predicate, hook)
    assert converter.explain_unstructure_hook(Sub).adopted.hook is hook

    # Remove the rule from the live registry and clear the dispatch cache.
    pairs = converter._unstructure_func._function_dispatch._handler_pairs
    index = next(i for i, pair in enumerate(pairs) if pair[0] is predicate)
    del pairs[index]
    converter._unstructure_func.dispatch.cache_clear()

    report = converter.explain_unstructure_hook(Sub)
    assert all(candidate.rule is not predicate for candidate in report.candidates)
    assert report.adopted.tier == FALLBACK


def test_report_does_not_leak_between_converters():
    first = BaseConverter()
    second = BaseConverter()
    hook = lambda o: "first-only"
    first.register_unstructure_hook(Sub, hook)

    first_report = first.explain_unstructure_hook(Sub)
    second_report = second.explain_unstructure_hook(Sub)

    assert first_report.adopted.hook is hook
    assert second_report.adopted.tier == FALLBACK

    copy = first.copy()
    copy.register_unstructure_hook(Other, lambda o: "other")

    assert (
        first.explain_unstructure_hook(Other).adopted.tier == FALLBACK
    )
    assert copy.explain_unstructure_hook(Sub).adopted.hook is hook


def test_direct_dispatch_candidate_is_reported():
    converter = Converter()
    generated = lambda o: "generated"
    converter._unstructure_func._direct_dispatch[Sub] = generated

    report = converter.explain_unstructure_hook(Sub)

    direct = next(c for c in report.candidates if c.tier == DIRECT_DISPATCH)
    assert direct.matches
    assert direct.hook is generated
    assert report.adopted is direct


def test_generated_factory_rules_are_reported_unbuilt():
    @attrs.define
    class Data:
        value: int

    base = BaseConverter()
    generated = Converter()

    base_report = base.explain_unstructure_hook(Data)
    gen_report = generated.explain_unstructure_hook(Data)

    # BaseConverter registers a ready method; Converter registers a factory.
    assert not base_report.adopted.is_factory
    assert base_report.adopted.hook is not None
    assert gen_report.adopted.is_factory
    assert gen_report.adopted.hook is None
    # The diagnostic run must not populate the generated-hook cache.
    assert generated._unstructure_func._direct_dispatch == {}

    # Ordinary conversion still works independently of diagnostics.
    assert generated.unstructure(Data(1)) == {"value": 1}


def test_non_class_types_do_not_crash_diagnostics():
    converter = BaseConverter()
    for typ in (int, str, list[int], int | None):
        report = converter.explain_structure_hook(typ)
        assert isinstance(report, HookResolutionReport)
        assert report.adopted is not None
        assert all(isinstance(candidate, HookCandidate) for candidate in report.candidates)


def test_singledispatch_virtual_subclass_is_reported():
    class Virtual:
        def __len__(self):
            return 1

    converter = BaseConverter()
    hook = lambda o: "sized"
    converter.register_unstructure_hook(Sized, hook)

    report = converter.explain_unstructure_hook(Virtual)

    assert report.adopted.tier == SINGLE_DISPATCH
    assert report.adopted.rule is Sized
    assert report.adopted.hook is hook
    assert Sized not in Virtual.__mro__


def test_non_matching_predicate_is_marked_not_shadowed():
    converter = BaseConverter()
    converter.register_structure_hook_func(
        lambda t: issubclass(t, Other), lambda v, t: None
    )
    report = converter.explain_structure_hook(Sub)

    predicate = next(
        c for c in report.candidates if c.tier == PREDICATE_DISPATCH
    )
    assert not predicate.matches
    assert predicate.shadowed_by is RULE_DID_NOT_MATCH
