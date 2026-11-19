# Copyright (C) Dnspython Contributors, see LICENSE for text of ISC license

"""Zone differencing.

This module computes the difference between two zones and represents it as
a :py:class:`dns.zonediff.ZoneDiffPlan`, an applicable plan rather than a
textual list of additions and deletions.  A plan captures the identity of
the zone it was computed from (its origin and class), the SOA serial number
of the source zone, and, for every changed RRset, the old value as a
precondition.  Plans are applied inside a transaction; if any precondition
is not satisfied, the whole plan is rejected before anything is changed.
"""

import dataclasses
from collections.abc import Iterator
from typing import Any

import dns.exception
import dns.name
import dns.rdataclass
import dns.rdataset
import dns.rdatatype
import dns.transaction


class PreconditionFailed(dns.exception.DNSException):
    """A zone diff plan precondition was not satisfied.

    The plan was computed from a zone with a specific origin, class, SOA
    serial number, and set of RRset values, and can only be applied to a
    zone that still matches all of those.  This exception is raised before
    any change is made, so a failed application never leaves partial
    results behind.
    """


def _canonical_name(name: dns.name.Name, origin: dns.name.Name) -> dns.name.Name:
    """Normalize *name* for comparison: make it absolute relative to
    *origin* and put it in canonical (downcased) form."""
    return name.derelativize(origin).canonicalize()


def _mutable_copy(rdataset: dns.rdataset.Rdataset) -> dns.rdataset.Rdataset:
    new = dns.rdataset.Rdataset(
        rdataset.rdclass, rdataset.rdtype, rdataset.covers, rdataset.ttl
    )
    new.update(rdataset)
    return new


def _immutable_copy(
    rdataset: dns.rdataset.Rdataset | None,
) -> dns.rdataset.Rdataset | None:
    if rdataset is None:
        return None
    return dns.rdataset.ImmutableRdataset(_mutable_copy(rdataset))


def _digest_set(rdataset: dns.rdataset.Rdataset, origin: dns.name.Name) -> set[bytes]:
    # to_digestable() canonicalizes case and resolves relative names against
    # the origin, so rdata comparison is insensitive to name case and to how
    # names were relativized in the source zones.
    return {rdata.to_digestable(origin) for rdata in rdataset}


def _rdatasets_equivalent(
    a: dns.rdataset.Rdataset | None,
    b: dns.rdataset.Rdataset | None,
    origin: dns.name.Name,
) -> bool:
    """Compare two rdatasets for diffing purposes.

    Unlike ``Rdataset.__eq__``, the TTL is significant: a change of TTL
    alone is a real change and must not look like "no change".
    """
    if a is None or b is None:
        return a is None and b is None
    return (
        a.rdclass == b.rdclass
        and a.rdtype == b.rdtype
        and a.covers == b.covers
        and a.ttl == b.ttl
        and _digest_set(a, origin) == _digest_set(b, origin)
    )


@dataclasses.dataclass(frozen=True)
class RrsetChange:
    """A single RRset change in a :py:class:`ZoneDiffPlan`.

    ``name`` is the absolute, canonical (downcased) owner name.  ``before``
    is the value the RRset is expected to have when the plan is applied, or
    ``None`` if the RRset is expected not to exist.  ``after`` is the value
    the RRset will have after the plan is applied, or ``None`` if the RRset
    is to be deleted.
    """

    name: dns.name.Name
    rdtype: dns.rdatatype.RdataType
    covers: dns.rdatatype.RdataType
    before: dns.rdataset.Rdataset | None
    after: dns.rdataset.Rdataset | None

    def __str__(self) -> str:
        rdtype = dns.rdatatype.to_text(self.rdtype)
        if self.covers != dns.rdatatype.NONE:
            rdtype = f"{rdtype}({dns.rdatatype.to_text(self.covers)})"
        head = f"{self.name} {rdtype}:"

        def fmt(rds: dns.rdataset.Rdataset) -> str:
            rdatas = "; ".join(sorted(str(r) for r in rds))
            return f"ttl={rds.ttl} rdatas=[{rdatas}]"

        if self.before is None:
            assert self.after is not None
            return f"{head} add {fmt(self.after)}"
        if self.after is None:
            return f"{head} delete {fmt(self.before)}"
        return f"{head} old {fmt(self.before)} new {fmt(self.after)}"


@dataclasses.dataclass(frozen=True)
class ZoneDiffPlan:
    """An applicable plan describing how to turn one zone into another.

    The plan records the identity of the source zone: its ``origin`` (in
    canonical form), its ``rdclass``, and the ``serial`` of its SOA record
    (``None`` if the source zone has no SOA).  ``changes`` is a tuple of
    :py:class:`RrsetChange` objects, each carrying the old value of the
    RRset as a precondition.

    A plan is applied to a zone inside a transaction with
    :py:meth:`apply`, or directly to a transaction manager (e.g. a
    :py:class:`dns.zone.Zone`) with :py:meth:`apply_to`.  All preconditions
    are checked before any change is made; if any of them fails,
    :py:exc:`PreconditionFailed` is raised and the zone is left untouched.
    """

    origin: dns.name.Name
    rdclass: dns.rdataclass.RdataClass
    serial: int | None
    changes: tuple[RrsetChange, ...]

    def __len__(self) -> int:
        return len(self.changes)

    def __iter__(self) -> Iterator[RrsetChange]:
        return iter(self.changes)

    def __str__(self) -> str:
        lines = [
            f"; zone diff plan for {self.origin} class "
            f"{dns.rdataclass.to_text(self.rdclass)} serial {self.serial}"
        ]
        lines.extend(str(change) for change in self.changes)
        return "\n".join(lines)

    def _txn_name(
        self, name: dns.name.Name, origin: dns.name.Name | None, relativize: bool
    ) -> dns.name.Name:
        # Convert an absolute plan name into the form expected by the
        # transaction's manager.
        if relativize:
            assert origin is not None
            return name.relativize(origin)
        return name

    def check_preconditions(self, txn: dns.transaction.Transaction) -> None:
        """Check that *txn*'s zone still matches the source zone this plan
        was computed from.

        :raises dns.zonediff.PreconditionFailed: if the origin, class, SOA
            serial number, or any RRset's old value does not match.
        """
        absolute, relativize, _ = txn._origin_information()
        if absolute is None or absolute.canonicalize() != self.origin:
            raise PreconditionFailed(
                f"plan origin {self.origin} does not match zone origin {absolute}"
            )
        if txn.manager.get_class() != self.rdclass:
            raise PreconditionFailed(
                f"plan class {dns.rdataclass.to_text(self.rdclass)} does not "
                f"match zone class "
                f"{dns.rdataclass.to_text(txn.manager.get_class())}"
            )
        if self.serial is not None:
            soa = txn.get(
                self._txn_name(self.origin, absolute, relativize), dns.rdatatype.SOA
            )
            if soa is None or len(soa) == 0 or soa[0].serial != self.serial:
                actual = soa[0].serial if soa is not None and len(soa) > 0 else None
                raise PreconditionFailed(
                    f"plan serial {self.serial} does not match zone serial {actual}"
                )
        for change in self.changes:
            name = self._txn_name(change.name, absolute, relativize)
            current = txn.get(name, change.rdtype, change.covers)
            if not _rdatasets_equivalent(current, change.before, self.origin):
                raise PreconditionFailed(
                    f"precondition failed for {change.name} "
                    f"{dns.rdatatype.to_text(change.rdtype)}: "
                    f"expected {change.before}, found {current}"
                )

    def apply(self, txn: dns.transaction.Transaction) -> None:
        """Apply the plan within transaction *txn*.

        All preconditions are checked first; if any fails,
        :py:exc:`PreconditionFailed` is raised before anything is changed,
        so the plan is never half applied.
        """
        self.check_preconditions(txn)
        absolute, relativize, _ = txn._origin_information()
        for change in self.changes:
            name = self._txn_name(change.name, absolute, relativize)
            if change.after is None:
                txn.delete(name, change.rdtype, change.covers)
            else:
                txn.replace(name, _mutable_copy(change.after))

    def apply_to(self, manager: dns.transaction.TransactionManager) -> None:
        """Apply the plan to *manager* (e.g. a :py:class:`dns.zone.Zone`)
        in a single transaction.

        If any precondition fails, the transaction is rolled back and the
        zone is left unchanged.
        """
        with manager.writer() as txn:
            self.apply(txn)


def _snapshot(
    zone: Any,
) -> tuple[dns.name.Name, dict, dns.rdataclass.RdataClass]:
    # *zone* is anything with the dns.zone.Zone read interface, i.e. an
    # origin, an rdclass, and items() yielding (name, node) pairs.
    origin = zone.origin
    if origin is None:
        raise ValueError("cannot diff a zone with no origin")
    origin = origin.canonicalize()
    table: dict[tuple[dns.name.Name, int, int], dns.rdataset.Rdataset] = {}
    for name, node in zone.items():
        cname = _canonical_name(name, origin)
        for rds in node:
            table[(cname, rds.rdtype, rds.covers)] = rds
    return origin, table, zone.rdclass


def diff(source: Any, target: Any) -> ZoneDiffPlan:
    """Compute a plan that transforms zone *source* into zone *target*.

    *source* and *target* are zones, e.g. :py:class:`dns.zone.Zone` or
    :py:class:`dns.versioned.Zone` objects.

    Owner names are made absolute relative to the zone origin and
    downcased, and rdata is compared in canonical form, so differences in
    name case or in how names are relativized do not show up as changes.
    TTLs are significant: a TTL-only difference is a change.

    The two zones must have the same origin (ignoring case) and the same
    rdata class.

    :raises ValueError: if either zone has no origin, or if the zones have
        different origins or classes.
    :rtype: :py:class:`ZoneDiffPlan`
    """
    s_origin, s_table, s_rdclass = _snapshot(source)
    t_origin, t_table, t_rdclass = _snapshot(target)
    if s_rdclass != t_rdclass:
        raise ValueError("cannot diff zones with different rdata classes")
    if s_origin != t_origin:
        raise ValueError(
            f"cannot diff zones with different origins: {s_origin} vs {t_origin}"
        )
    changes = []
    for key in sorted(set(s_table) | set(t_table)):
        before = s_table.get(key)
        after = t_table.get(key)
        if before is not None and after is not None and _rdatasets_equivalent(
            before, after, s_origin
        ):
            continue
        changes.append(
            RrsetChange(
                key[0],
                dns.rdatatype.RdataType.make(key[1]),
                dns.rdatatype.RdataType.make(key[2]),
                _immutable_copy(before),
                _immutable_copy(after),
            )
        )
    serial = None
    soa = s_table.get((s_origin, dns.rdatatype.SOA, dns.rdatatype.NONE))
    if soa is not None and len(soa) > 0:
        serial = soa[0].serial
    return ZoneDiffPlan(s_origin, s_rdclass, serial, tuple(changes))
