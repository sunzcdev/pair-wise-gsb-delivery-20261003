"""Diagnostic helpers explaining how hooks are resolved for a type.

The regular dispatch path (``Converter.structure``/``Converter.unstructure``)
only needs the winning hook, so it stays allocation-free.  When maintainers
need to understand *why* a particular rule won, the helpers in this module
produce a :class:`HookResolutionReport` describing every candidate, the rule
that was adopted and the reason every other rule was shadowed.

Producing a report never invokes a registered hook or hook factory; only
predicates are evaluated, and exceptions raised by predicates are captured in
the report instead of being swallowed.
"""

from __future__ import annotations

from typing import Any, Callable, Final, Literal

from attrs import define

#: The exact-match registry backed by ``functools.singledispatch``.
SINGLE_DISPATCH: Final = "singledispatch"
#: The exact-match registry populated by generated (``direct``) hooks.
DIRECT_DISPATCH: Final = "direct"
#: The ordered list of predicate-based rules.
PREDICATE_DISPATCH: Final = "predicate"
#: The fallback factory used when no registered rule matches.
FALLBACK: Final = "fallback"

#: A rule in a higher-priority tier matched the type.
SHADOWED_BY_HIGHER_PRIORITY: Final = "shadowed_by_higher_priority"
#: An earlier rule of the same tier matched; predicate rules are first-match.
SHADOWED_BY_EARLIER_RULE: Final = "shadowed_by_earlier_rule"
#: The rule did not match the requested type.
RULE_DID_NOT_MATCH: Final = "rule_did_not_match"
#: A predicate raised; the captured exception is attached to the candidate.
PREDICATE_ERRORED: Final = "predicate_errored"
#: A non-predicate dispatch lookup raised; the exception is attached.
DISPATCH_ERRORED: Final = "dispatch_errored"

HookTier = Literal["singledispatch", "direct", "predicate", "fallback"]


class AmbiguousHookError(RuntimeError):
    """Raised when multiple same-priority rules match a type ambiguously.

    Only predicate rules can be ambiguous with one another, since they share
    a priority tier and are otherwise resolved first-match-wins. The report
    describing the candidates is available as :attr:`report`.
    """

    def __init__(self, message: str, type: Any, report: HookResolutionReport):
        super().__init__(message)
        self.type = type
        self.report = report


@define(eq=False)
class HookCandidate:
    """A single rule considered while resolving a hook for a type.

    :param tier: Where the rule lives; one of the module-level tier constants.
    :param rule: The registered class (exact tiers), predicate (predicate
        tier) or fallback factory (fallback tier).
    :param hook: The hook produced by the rule, or ``None`` when no hook is
        produced (a non-matching or errored predicate, or the fallback, whose
        factory is never invoked by diagnostics).
    :param matches: Whether the rule matches the requested type.
    :param order: The zero-based position of this candidate within its tier.
    :param is_factory: Whether the registered handler is a hook factory rather
        than a ready-to-use hook. Factories are never called by diagnostics.
    :param shadowed_by: The reason the rule did not win, or ``None`` for the
        adopted rule.
    :param error: The exception raised by an errored predicate, if any.
    """

    tier: HookTier
    rule: Any
    hook: Callable[..., Any] | None
    matches: bool
    order: int
    is_factory: bool = False
    shadowed_by: Literal[
        "shadowed_by_higher_priority",
        "shadowed_by_earlier_rule",
        "rule_did_not_match",
        "predicate_errored",
        "dispatch_errored",
    ] | None = None
    error: Exception | None = None

    @property
    def adopted(self) -> bool:
        """Whether this is the rule that normal dispatch would adopt."""
        return self.matches and self.shadowed_by is None and self.error is None


@define(eq=False)
class HookResolutionReport:
    """The outcome of explaining hook resolution for a single type.

    :param type: The type the report was produced for.
    :param candidates: Every considered rule, in dispatch order (higher
        priority tiers first, registration order within a tier).
    :param adopted: The winning candidate, or ``None`` when no registered
        rule matches (the fallback would be used by normal dispatch).
    """

    type: Any
    candidates: list[HookCandidate]
    adopted: HookCandidate | None

    @property
    def matching_candidates(self) -> list[HookCandidate]:
        """All candidates whose rule matches the type, in dispatch order."""
        return [candidate for candidate in self.candidates if candidate.matches]

    @property
    def errored_candidates(self) -> list[HookCandidate]:
        """All candidates whose predicate raised, in dispatch order."""
        return [
            candidate
            for candidate in self.candidates
            if candidate.error is not None
        ]

    @property
    def ambiguous(self) -> bool:
        """Whether multiple same-tier predicate rules match the type."""
        matching_predicates = [
            candidate
            for candidate in self.candidates
            if candidate.tier == PREDICATE_DISPATCH and candidate.matches
        ]
        return len(matching_predicates) > 1

    def ensure_unambiguous(self) -> None:
        """Raise :class:`AmbiguousHookError` if predicate rules are tied."""
        if self.ambiguous:
            raise AmbiguousHookError(
                f"Multiple registered predicate rules match {self.type!r}; "
                "resolution is ambiguous. See the attached report for details.",
                self.type,
                self,
            )
