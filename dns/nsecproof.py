# Copyright (C) Dnspython Contributors, see LICENSE for text of ISC license

# Permission to use, copy, modify, and distribute this software and its
# documentation for any purpose with or without fee is hereby granted,
# provided that the above copyright notice and this permission notice
# appear in all copies.
#
# THE SOFTWARE IS PROVIDED "AS IS" AND NOMINUM DISCLAIMS ALL WARRANTIES
# WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
# MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL Dnspython BE LIABLE FOR
# ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
# WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
# ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT
# OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.

"""NSEC proof of nonexistence.

This module is a standalone entry point for proving negative answers
(NXDOMAIN and NODATA) from NSEC records, as described in RFC 4035
section 5.4 and updated by RFC 6840 section 4.  It is separate from
NSEC record parsing and from cryptographic signature verification: the
caller is responsible for validating the RRSIGs of the NSEC RRsets
(e.g. with :py:func:`dns.dnssec.validate`) *before* calling
:py:func:`prove`.  No signature verification is performed here, and
NSEC3 (RFC 5155) proofs are not supported.

Names are processed in DNS canonical (DNSSEC) order, including
wrap-around of NSEC intervals at the end of the name space.  NSEC
records owned by names outside the zone are never used as evidence,
and if the available records are insufficient to reach a conclusion,
the result is :py:attr:`Result.INSUFFICIENT` rather than a trusted
claim of nonexistence.
"""

import enum
from collections.abc import Iterable
from typing import TypeVar

import dns.name
import dns.rdatatype
import dns.rrset
import dns.zone
from dns.rdtypes.ANY.NSEC import NSEC

_T = TypeVar("_T")


class Result(enum.Enum):
    """The outcome of an NSEC proof of nonexistence."""

    NXDOMAIN = "nxdomain"
    """The qname provably does not exist."""

    NODATA = "nodata"
    """The qname (or a matching wildcard) provably exists, but provably
    has no data of the qtype."""

    CONFLICT = "conflict"
    """The supplied NSEC records contradict each other, or contradict
    the claim that the answer is negative (e.g. an NSEC bitmap shows
    the qtype, or a CNAME, exists)."""

    INSUFFICIENT = "insufficient"
    """The supplied NSEC records are not sufficient to prove or
    disprove nonexistence.  This is not a trusted statement about
    (non)existence."""

    def __repr__(self):
        return f"<{self.__class__.__name__}.{self.name}>"


class Proof:
    """The result of an NSEC proof of nonexistence for a query.

    A ``Proof`` should be treated as immutable.
    """

    def __init__(
        self,
        result: Result,
        qname: dns.name.Name,
        qtype: dns.rdatatype.RdataType,
        origin: dns.name.Name,
        evidence: Iterable[dns.rrset.RRset],
        reason: str,
        closest_encloser: dns.name.Name | None = None,
        wildcard: dns.name.Name | None = None,
    ):
        self.result = result
        """The :py:class:`Result` of the proof."""

        self.qname = qname
        """The query name, in canonical form."""

        self.qtype = qtype
        """The query type."""

        self.origin = origin
        """The zone origin, in canonical form."""

        self.evidence = tuple(evidence)
        """The NSEC RRsets the conclusion relies upon, in the order
        they were used.  These are the very objects supplied by the
        caller, so they can be checked against the caller's in-memory
        validated RRsets (see :py:meth:`check`)."""

        self.reason = reason
        """A human-readable explanation of the conclusion."""

        self.closest_encloser = closest_encloser
        """The closest encloser determined while proving an NXDOMAIN
        or wildcard NODATA, or ``None`` if not applicable."""

        self.wildcard = wildcard
        """The wildcard name ``*.<closest encloser>`` whose existence
        or nonexistence was proven, or ``None`` if not applicable."""

    def check(self, nsec_rrsets: Iterable[dns.rrset.RRset]) -> bool:
        """Check this proof's conclusion against in-memory NSEC RRsets.

        Every NSEC RRset referenced as evidence must be present in
        *nsec_rrsets*, and recomputing the proof from *nsec_rrsets*
        alone must reach the same result.

        :param nsec_rrsets: the NSEC RRsets to check against.
        :type nsec_rrsets: iterable of :py:class:`dns.rrset.RRset`
        :rtype: bool
        """
        rrsets = list(nsec_rrsets)
        for rrset in self.evidence:
            if rrset not in rrsets:
                return False
        try:
            reproof = prove(self.qname, self.qtype, self.origin, rrsets)
        except (TypeError, ValueError):
            return False
        return reproof.result == self.result

    def __repr__(self):
        return (
            f"<Proof {self.result.name} qname={self.qname} "
            f"qtype={dns.rdatatype.to_text(self.qtype)}>"
        )


class _Interval:
    """An NSEC interval: the half-open interval (owner, next) in DNS
    canonical order, wrapping around at the end of the name space."""

    __slots__ = ["owner", "next", "rrset", "rdata"]

    def __init__(
        self,
        owner: dns.name.Name,
        next: dns.name.Name,
        rrset: dns.rrset.RRset,
        rdata: NSEC,
    ):
        self.owner = owner
        self.next = next
        self.rrset = rrset
        self.rdata = rdata

    def covers(self, name: dns.name.Name) -> bool:
        """Does this interval cover *name*?"""
        if self.owner < self.next:
            return self.owner < name < self.next
        else:
            # Wrap-around.  Note that next == owner means the interval
            # covers every name except the owner itself.
            return name > self.owner or name < self.next

    def has_type(self, rdtype: dns.rdatatype.RdataType) -> bool:
        """Is *rdtype* present in the NSEC type bitmap?"""
        window = rdtype // 256
        offset = rdtype % 256
        byte = offset // 8
        bit = offset % 8
        for w, bitmap in self.rdata.windows:
            if w == window:
                return byte < len(bitmap) and bool(bitmap[byte] & (0x80 >> bit))
        return False


def _closest_encloser(
    qname: dns.name.Name, nsec_owner: dns.name.Name, origin: dns.name.Name
) -> dns.name.Name:
    """Compute the closest encloser of *qname*.

    *nsec_owner* is the owner of an NSEC interval covering *qname*, and
    is known to exist.  The closest encloser is the longest ancestor of
    *qname* that is a superdomain of (or equal to) *nsec_owner*; any
    existing name between the closest encloser and *qname* would have
    sorted between them in canonical order, contradicting the covering
    NSEC.  The origin always qualifies, as both the qname and the NSEC
    owner are in the zone.
    """
    candidate = qname.parent()
    while not nsec_owner.is_subdomain(candidate):
        candidate = candidate.parent()
    return candidate


def _unique(items: Iterable[_T]) -> list[_T]:
    """Return the items, deduplicated, preserving order."""
    result: list[_T] = []
    for item in items:
        if item not in result:
            result.append(item)
    return result


def prove(
    qname: dns.name.Name | str,
    qtype: dns.rdatatype.RdataType | int | str,
    zone: dns.zone.Zone | dns.name.Name | str,
    nsec_rrsets: Iterable[dns.rrset.RRset],
) -> Proof:
    """Prove a negative answer for a query using NSEC records.

    The NSEC RRsets in *nsec_rrsets* must already have had their
    signatures validated by the caller; no cryptographic verification
    is performed by this function.  NSEC3 is not supported.

    :param qname: the query name.  Must be absolute.
    :type qname: :py:class:`dns.name.Name` or ``str``
    :param qtype: the query type.
    :type qtype: :py:class:`dns.rdatatype.RdataType`, ``int``, or ``str``
    :param zone: the zone the NSEC records belong to, either as a
        :py:class:`dns.zone.Zone` or as its origin name.
    :type zone: :py:class:`dns.zone.Zone`, :py:class:`dns.name.Name`,
        or ``str``
    :param nsec_rrsets: the signature-validated NSEC RRsets to use as
        evidence.  RRsets owned by names outside the zone are ignored.
    :type nsec_rrsets: iterable of :py:class:`dns.rrset.RRset`
    :returns: the proof.
    :rtype: :py:class:`Proof`
    :raises TypeError: if an element of *nsec_rrsets* is not an RRset,
        or *zone* is of an unsupported type.
    :raises ValueError: if *qname* is not absolute, *zone* is a zone
        without an origin, or an element of *nsec_rrsets* is not an
        NSEC RRset.
    """
    if isinstance(qname, str):
        qname = dns.name.from_text(qname)
    if not qname.is_absolute():
        raise ValueError("qname must be absolute")
    qname = qname.canonicalize()
    qtype = dns.rdatatype.RdataType.make(qtype)
    if isinstance(zone, dns.zone.Zone):
        if zone.origin is None:
            raise ValueError("zone has no origin")
        origin = zone.origin
    elif isinstance(zone, dns.name.Name):
        origin = zone
    elif isinstance(zone, str):
        origin = dns.name.from_text(zone)
    else:
        raise TypeError("zone must be a dns.zone.Zone, dns.name.Name, or str")
    origin = origin.canonicalize()

    def conclude(
        result: Result,
        evidence: Iterable[dns.rrset.RRset],
        reason: str,
        closest_encloser: dns.name.Name | None = None,
        wildcard: dns.name.Name | None = None,
    ) -> Proof:
        return Proof(
            result, qname, qtype, origin, evidence, reason, closest_encloser, wildcard
        )

    if not qname.is_subdomain(origin):
        return conclude(
            Result.INSUFFICIENT,
            (),
            f"qname {qname} is not in zone {origin}",
        )

    # Collect the in-zone NSEC intervals.  Records owned by names
    # outside the zone, or pointing outside the zone, can never be
    # evidence for this zone and are ignored.
    intervals: list[_Interval] = []
    for rrset in nsec_rrsets:
        if not isinstance(rrset, dns.rrset.RRset):
            raise TypeError("nsec_rrsets elements must be dns.rrset.RRset")
        if rrset.rdtype != dns.rdatatype.NSEC:
            raise ValueError(f"rrset at {rrset.name} is not an NSEC RRset")
        owner = rrset.name.canonicalize()
        if not owner.is_subdomain(origin):
            continue
        for rdata in rrset:
            assert isinstance(rdata, NSEC)
            next = rdata.next.canonicalize()
            if not next.is_subdomain(origin):
                continue
            intervals.append(_Interval(owner, next, rrset, rdata))

    # The NSEC chain must be consistent: no interval may cover the
    # owner of another NSEC, as NSEC owners exist by definition.
    owners = _unique([interval.owner for interval in intervals])
    for interval in intervals:
        for owner in owners:
            if owner != interval.owner and interval.covers(owner):
                return conclude(
                    Result.CONFLICT,
                    (),
                    f"NSEC at {interval.owner} covers existing name {owner}",
                )

    # Note that the chain consistency check above implies that no
    # interval can cover the qname (or the wildcard) while another
    # NSEC matches it exactly, as an exact match makes the name an
    # NSEC owner, and owners exist by definition.
    exact = [interval for interval in intervals if interval.owner == qname]
    covering = [interval for interval in intervals if interval.covers(qname)]

    if exact:
        # The qname exists, so only NODATA can be proven.  The bitmap
        # must have neither the qtype nor CNAME; a CNAME would have
        # redirected the query, so it must not be ignored.
        for interval in exact:
            if interval.has_type(qtype):
                return conclude(
                    Result.CONFLICT,
                    (),
                    f"NSEC at {qname} shows the qtype exists",
                )
            if interval.has_type(dns.rdatatype.CNAME):
                return conclude(
                    Result.CONFLICT,
                    (),
                    f"NSEC at {qname} shows a CNAME exists",
                )
        return conclude(
            Result.NODATA,
            _unique(interval.rrset for interval in exact),
            f"NSEC at {qname} proves the name exists but has no "
            f"{dns.rdatatype.to_text(qtype)} or CNAME",
        )

    if not covering:
        return conclude(
            Result.INSUFFICIENT,
            (),
            f"no NSEC record covers qname {qname}",
        )

    # The qname does not exist.  NXDOMAIN additionally requires proof
    # that no wildcard could have matched; the relevant wildcard is at
    # the closest encloser, which needs its own evidence.
    closest_encloser = _closest_encloser(qname, covering[0].owner, origin)
    wildcard = dns.name.Name((b"*",) + closest_encloser.labels)
    w_exact = [interval for interval in intervals if interval.owner == wildcard]
    w_covering = [interval for interval in intervals if interval.covers(wildcard)]

    if w_exact:
        # The wildcard exists; this is a wildcard NODATA proof if and
        # only if it has neither the qtype nor a CNAME.
        for interval in w_exact:
            if interval.has_type(qtype):
                return conclude(
                    Result.CONFLICT,
                    (),
                    f"NSEC at wildcard {wildcard} shows the qtype exists",
                    closest_encloser,
                    wildcard,
                )
            if interval.has_type(dns.rdatatype.CNAME):
                return conclude(
                    Result.CONFLICT,
                    (),
                    f"NSEC at wildcard {wildcard} shows a CNAME exists",
                    closest_encloser,
                    wildcard,
                )
        return conclude(
            Result.NODATA,
            _unique(
                [interval.rrset for interval in covering]
                + [interval.rrset for interval in w_exact]
            ),
            f"NSEC at wildcard {wildcard} proves it exists but has no "
            f"{dns.rdatatype.to_text(qtype)} or CNAME",
            closest_encloser,
            wildcard,
        )

    if w_covering:
        return conclude(
            Result.NXDOMAIN,
            _unique(
                [interval.rrset for interval in covering]
                + [interval.rrset for interval in w_covering]
            ),
            f"NSEC records prove {qname} does not exist and no wildcard "
            f"at {closest_encloser} applies",
            closest_encloser,
            wildcard,
        )

    return conclude(
        Result.INSUFFICIENT,
        (),
        f"no NSEC record proves the nonexistence of wildcard {wildcard}",
        closest_encloser,
        wildcard,
    )
