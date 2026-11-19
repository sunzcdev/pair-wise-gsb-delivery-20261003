# Copyright (C) Dnspython Contributors, see LICENSE for text of ISC license

"""Structural validation of zone transaction content.

This module provides an explicit commit-time validator that inspects the
*complete* current view of a :py:class:`dns.transaction.Transaction`, rather
than only the records that a transaction touched.  It is intended to be run
before a transaction is committed (and thus before a versioned zone publishes
a new version) so that a zone whose structure violates the enabled rules is
never published.

The validator is strictly read-only.  It never deletes or rewrites a
conflicting record in order to "fix" the zone; instead it reports every
problem it finds as a :py:class:`Violation`, naming both the owner name and
the offending RRset.

Rules are opt-in through the :py:class:`Rule` enumeration.  Only the rules
documented below are supported; configuring any other rule is rejected.
"""

import dataclasses
import enum
from collections.abc import Iterable, Iterator

import dns.exception
import dns.name
import dns.rdataset
import dns.rdatatype
import dns.transaction


class UnknownRule(dns.exception.DNSException):
    """An unknown validation rule was configured."""


class UnknownOrigin(dns.exception.DNSException):
    """The transaction has no origin, so its apex cannot be determined."""


class Rule(enum.Enum):
    """Structural rules that a :py:class:`CommitValidator` may enforce.

    .. attribute:: APEX_SOA

       The zone apex must contain exactly one SOA RRset, and no other name in
       the zone may contain an SOA RRset.

    .. attribute:: APEX_NS

       The zone apex must contain an NS RRset.

    .. attribute:: CNAME

       A CNAME RRset is exclusive; it may not coexist with any other RRset at
       the same name other than an RRSIG RRset that covers CNAME, and it must
       contain a single rdata.  A CNAME at the zone apex is also reported, as
       an apex node must carry the zone's SOA and NS RRsets.

    .. attribute:: DELEGATION_BOUNDARY

       A non-apex name that carries an NS RRset is a delegation point and
       marks a zone cut.  Only the NS RRset, DS RRsets, and DNSSEC denial /
       signature RRsets (NSEC, NSEC3, and RRSIG) may exist at the cut.  Names
       below a cut are outside the zone's authority: the only authoritative
       data permitted there is in-bailiwick glue, i.e. an A or AAAA RRset
       whose owner is referenced by an NS rdata of the enclosing cut.  Glue is
       never treated as ordinary authoritative data.
    """

    APEX_SOA = "apex-soa"
    APEX_NS = "apex-ns"
    CNAME = "cname"
    DELEGATION_BOUNDARY = "delegation-boundary"

    @classmethod
    def make(cls, rule: "Rule | str") -> "Rule":
        """Convert *rule* into a :py:class:`Rule`.

        :param rule: A rule, or its text form (e.g. ``"cname"``).
        :type rule: :py:class:`Rule` or str
        :raises UnknownRule: if *rule* is not a supported rule.
        :rtype: :py:class:`Rule`
        """
        if isinstance(rule, Rule):
            return rule
        if isinstance(rule, str):
            try:
                return cls(rule)
            except ValueError:
                pass
        raise UnknownRule(f"unknown zone check rule: {rule!r}")


# Every rule that this module knows how to enforce.
ALL_RULES = frozenset(Rule)

# Address RR types that may appear as in-bailiwick glue beneath a zone cut.
_GLUE_RDTYPES = frozenset({dns.rdatatype.A, dns.rdatatype.AAAA})


class CommitCheckFailure(dns.exception.DNSException):
    """A transaction failed its commit-time structural validation.

    The :py:attr:`report` attribute holds the :py:class:`ValidationReport`
    that describes every structural problem found.  The transaction is rolled
    back when this exception is raised, so no invalid version is published.
    """

    msg = "zone transaction failed structural validation"

    def __init__(self, report: "ValidationReport"):
        self.report = report
        super().__init__(str(report))


@dataclasses.dataclass(frozen=True)
class Violation:
    """A single structural problem found in a transaction view.

    .. attribute:: rule

       The rule that was violated.

       :type: :py:class:`Rule`

    .. attribute:: name

       The owner name of the offending RRset, expressed with the same
       relativity as the transaction view.

       :type: :py:class:`dns.name.Name`

    .. attribute:: rdataset

       The offending RRset, or ``None`` if the problem is the absence of an
       RRset (for example a missing apex SOA).

       :type: :py:class:`dns.rdataset.Rdataset` or ``None``

    .. attribute:: message

       A human readable description of the problem.

       :type: str
    """

    rule: Rule
    name: dns.name.Name
    rdataset: dns.rdataset.Rdataset | None
    message: str

    def __str__(self) -> str:
        if self.rdataset is None:
            return f"{self.rule.value}: {self.message}"
        return f"{self.rule.value}: {self.message} ({self.name} {self.rdataset.rdtype.name})"


class ValidationReport:
    """The result of validating a transaction view.

    A report is an immutable collection of :py:class:`Violation` objects.  It
    is truthy when it contains no violations, and may be iterated or rendered
    as text to see exactly which names and RRsets caused problems.
    """

    def __init__(self, violations: Iterable[Violation] = ()):
        self._violations = tuple(violations)

    @property
    def ok(self) -> bool:
        """Whether the validated view satisfied every enabled rule.

        :rtype: bool
        """
        return len(self._violations) == 0

    @property
    def violations(self) -> tuple[Violation, ...]:
        """The violations found, in a stable order.

        :rtype: tuple[:py:class:`Violation`, ...]
        """
        return self._violations

    def __bool__(self) -> bool:
        return self.ok

    def __iter__(self) -> Iterator[Violation]:
        return iter(self._violations)

    def __len__(self) -> int:
        return len(self._violations)

    def __str__(self) -> str:
        if self.ok:
            return "zone transaction structural validation passed"
        return "zone transaction structural validation failed:\n" + "\n".join(
            f"  {violation}" for violation in self._violations
        )


class CommitValidator:
    """An explicit, read-only validator of zone transaction content.

    A validator is configured once with the rules it should enforce and may
    then be attached to many transactions, either directly with
    :py:meth:`attach` or indirectly by passing the validator itself as a
    transaction setup callable (it implements ``__call__``).

    Validation always examines the *whole* current transaction view, including
    records that the transaction did not touch.  This catches problems at
    names related to a change but not themselves modified, such as glue left
    beneath a new delegation or an apex CNAME that collides with the zone's
    SOA and NS RRsets.

    The validator never mutates the transaction.  A failed validation does not
    delete or rewrite anything; the conflicting records remain in the
    transaction view and are reported instead.
    """

    def __init__(self, rules: Iterable[Rule | str] = ALL_RULES):
        """Create a validator enforcing *rules*.

        :param rules: The rules to enforce.  Each element must be a
            :py:class:`Rule` or the text form of one; the default is every
            supported rule.  An empty iterable produces a validator that
            always succeeds.
        :type rules: iterable of :py:class:`Rule` or str
        :raises UnknownRule: if any element names an unsupported rule.
        """
        self._rules = frozenset(Rule.make(rule) for rule in rules)

    @property
    def rules(self) -> frozenset[Rule]:
        """The rules enforced by this validator.

        :rtype: frozenset[:py:class:`Rule`]
        """
        return self._rules

    #
    # Public API
    #

    def validate(self, txn: dns.transaction.Transaction) -> ValidationReport:
        """Validate the complete current view of *txn*.

        :param txn: The transaction whose view should be inspected.  The
            transaction is not modified.
        :type txn: :py:class:`dns.transaction.Transaction`
        :returns: A :py:class:`ValidationReport` describing every violation
            of the enabled rules.
        :rtype: :py:class:`ValidationReport`
        """
        absolute_origin, relativize, apex = txn._origin_information()
        if apex is None:
            # Without an origin we cannot name an apex or reason about
            # delegation boundaries.
            raise UnknownOrigin
        # Sort so that violation ordering, and hence report text, is stable
        # even when the transaction's backing store does not order names.
        view: dict[dns.name.Name, list[dns.rdataset.Rdataset]] = {}
        for name, rdataset in txn.iterate_rdatasets():
            view.setdefault(name, []).append(rdataset)
        violations: list[Violation] = []
        if Rule.APEX_SOA in self._rules:
            violations.extend(self._check_apex_soa(view, apex))
        if Rule.APEX_NS in self._rules:
            violations.extend(self._check_apex_ns(view, apex))
        if Rule.CNAME in self._rules:
            violations.extend(self._check_cname(view, apex))
        if Rule.DELEGATION_BOUNDARY in self._rules:
            violations.extend(
                self._check_delegation(view, apex, absolute_origin, relativize)
            )
        return ValidationReport(violations)

    def check(self, txn: dns.transaction.Transaction) -> None:
        """Validate *txn*, raising if any enabled rule is violated.

        :param txn: The transaction whose view should be inspected.
        :type txn: :py:class:`dns.transaction.Transaction`
        :raises CommitCheckFailure: if one or more violations are found.  The
            report is available on the exception's ``report`` attribute.
        """
        report = self.validate(txn)
        if not report.ok:
            raise CommitCheckFailure(report)

    def attach(self, txn: dns.transaction.Transaction) -> None:
        """Register :py:meth:`check` as a commit check on *txn*.

        The check runs when the transaction is committed, after all edits have
        been applied but before a new version is published.  If validation
        fails, the transaction is rolled back and no version is published.

        :param txn: The transaction to guard.
        :type txn: :py:class:`dns.transaction.Transaction`
        """
        txn.check_commit(self.check)

    def __call__(self, txn: dns.transaction.Transaction) -> None:
        # Allow a validator to be used as a transaction setup callable, in the
        # same style as dns.transaction.TransactionLimiter.
        self.attach(txn)

    #
    # Rule implementations
    #

    def _check_apex_soa(
        self,
        view: dict[dns.name.Name, list[dns.rdataset.Rdataset]],
        apex: dns.name.Name,
    ) -> list[Violation]:
        violations: list[Violation] = []
        apex_soa: dns.rdataset.Rdataset | None = None
        for name, rdatasets in view.items():
            for rdataset in rdatasets:
                if rdataset.rdtype != dns.rdatatype.SOA:
                    continue
                if name == apex:
                    apex_soa = rdataset
                    if len(rdataset) != 1:
                        violations.append(
                            Violation(
                                Rule.APEX_SOA,
                                name,
                                rdataset,
                                "apex SOA RRset must contain exactly one rdata",
                            )
                        )
                else:
                    violations.append(
                        Violation(
                            Rule.APEX_SOA,
                            name,
                            rdataset,
                            "SOA RRsets are only allowed at the zone apex",
                        )
                    )
        if apex_soa is None:
            violations.append(
                Violation(
                    Rule.APEX_SOA,
                    apex,
                    None,
                    "zone apex has no SOA RRset",
                )
            )
        return violations

    def _check_apex_ns(
        self,
        view: dict[dns.name.Name, list[dns.rdataset.Rdataset]],
        apex: dns.name.Name,
    ) -> list[Violation]:
        for rdataset in view.get(apex, ()):
            if rdataset.rdtype == dns.rdatatype.NS and len(rdataset) > 0:
                return []
        return [
            Violation(
                Rule.APEX_NS,
                apex,
                None,
                "zone apex has no NS RRset",
            )
        ]

    def _check_cname(
        self,
        view: dict[dns.name.Name, list[dns.rdataset.Rdataset]],
        apex: dns.name.Name,
    ) -> list[Violation]:
        violations: list[Violation] = []
        for name, rdatasets in view.items():
            cname = self._find_rdataset(rdatasets, dns.rdatatype.CNAME)
            if cname is None:
                continue
            if name == apex:
                violations.append(
                    Violation(
                        Rule.CNAME,
                        name,
                        cname,
                        "CNAME RRset is not allowed at the zone apex",
                    )
                )
            if len(cname) != 1:
                violations.append(
                    Violation(
                        Rule.CNAME,
                        name,
                        cname,
                        "CNAME RRset must contain exactly one rdata",
                    )
                )
            for rdataset in rdatasets:
                if rdataset is cname:
                    continue
                if (
                    rdataset.rdtype == dns.rdatatype.RRSIG
                    and rdataset.covers == dns.rdatatype.CNAME
                ):
                    # The RRSIG RRset covering the CNAME is its signature, not
                    # competing data.
                    continue
                violations.append(
                    Violation(
                        Rule.CNAME,
                        name,
                        rdataset,
                        "RRset coexists with a CNAME RRset at the same name",
                    )
                )
        return violations

    def _check_delegation(
        self,
        view: dict[dns.name.Name, list[dns.rdataset.Rdataset]],
        apex: dns.name.Name,
        absolute_origin: dns.name.Name | None,
        relativize: bool,
    ) -> list[Violation]:
        violations: list[Violation] = []
        # Every non-apex name with an NS RRset is a delegation point.  Cuts are
        # evaluated against the whole view, so delegations added by this
        # transaction affect pre-existing names beneath them even if those
        # names were never touched.
        cuts: dict[dns.name.Name, dns.rdataset.Rdataset] = {}
        for name in sorted(view):
            if name == apex:
                continue
            ns = self._find_rdataset(view[name], dns.rdatatype.NS)
            if ns is not None:
                cuts[name] = ns
        for name in sorted(view):
            cut = self._enclosing_cut(cuts, name)
            if cut is None:
                # The name is authoritative (at or above every cut).
                continue
            is_glue_name = name != cut
            glue_targets = self._glue_targets(cuts[cut], absolute_origin, relativize)
            for rdataset in view[name]:
                if is_glue_name:
                    if rdataset.rdtype in _GLUE_RDTYPES and name in glue_targets:
                        # In-bailiwick glue: the only data permitted beneath a
                        # cut, and it is never treated as authoritative.
                        continue
                    violations.append(
                        Violation(
                            Rule.DELEGATION_BOUNDARY,
                            name,
                            rdataset,
                            "non-glue data beneath delegation point "
                            f"{cut} crosses the zone boundary",
                        )
                    )
                elif not self._allowed_at_cut(rdataset):
                    violations.append(
                        Violation(
                            Rule.DELEGATION_BOUNDARY,
                            name,
                            rdataset,
                            "RRset is not allowed at a delegation point",
                        )
                    )
        return violations

    #
    # Helpers
    #

    @staticmethod
    def _find_rdataset(
        rdatasets: Iterable[dns.rdataset.Rdataset],
        rdtype: dns.rdatatype.RdataType,
    ) -> dns.rdataset.Rdataset | None:
        for rdataset in rdatasets:
            if rdataset.rdtype == rdtype:
                return rdataset
        return None

    @staticmethod
    def _enclosing_cut(
        cuts: dict[dns.name.Name, dns.rdataset.Rdataset],
        name: dns.name.Name,
    ) -> dns.name.Name | None:
        # Return the delegation point that governs *name*: the cut itself when
        # *name* is a delegation point, otherwise the nearest cut that is a
        # superdomain of *name*.
        for cut in sorted(cuts, reverse=True):
            if name == cut or name.is_subdomain(cut):
                return cut
        return None

    @staticmethod
    def _glue_targets(
        ns_rdataset: dns.rdataset.Rdataset,
        absolute_origin: dns.name.Name | None,
        relativize: bool,
    ) -> set[dns.name.Name]:
        # Only in-bailiwick name servers can have glue inside this zone, so
        # out-of-bailiwick NS targets are ignored here.
        targets: set[dns.name.Name] = set()
        for rdata in ns_rdataset:
            target = rdata.target
            # Normally zonefile loading has already relativized in-bailiwick
            # names, but a transaction built by other code may still carry an
            # absolute one; normalize it so the glue comparison works.
            if (
                relativize
                and target.is_absolute()
                and absolute_origin is not None
                and target.is_subdomain(absolute_origin)
            ):
                target = target.relativize(absolute_origin)
            targets.add(target)
        return targets

    @staticmethod
    def _allowed_at_cut(rdataset: dns.rdataset.Rdataset) -> bool:
        # At a delegation point only the NS RRset, DS RRsets, and DNSSEC
        # denial-of-existence / signature RRsets are permitted.  Everything
        # else (including CNAME and ordinary service RRsets) is authoritative
        # data that cannot live at a zone cut.
        return rdataset.rdtype in (
            dns.rdatatype.NS,
            dns.rdatatype.DS,
            dns.rdatatype.NSEC,
            dns.rdatatype.NSEC3,
            dns.rdatatype.RRSIG,
        )


def validate(
    txn: dns.transaction.Transaction,
    rules: Iterable[Rule | str] = ALL_RULES,
) -> ValidationReport:
    """Validate the complete current view of *txn* with *rules*.

    This is a convenience wrapper around :py:class:`CommitValidator` for
    one-off validation.

    :param txn: The transaction whose view should be inspected.
    :type txn: :py:class:`dns.transaction.Transaction`
    :param rules: The rules to enforce; the default is every supported rule.
    :type rules: iterable of :py:class:`Rule` or str
    :raises UnknownRule: if any element names an unsupported rule.
    :rtype: :py:class:`ValidationReport`
    """
    return CommitValidator(rules).validate(txn)
