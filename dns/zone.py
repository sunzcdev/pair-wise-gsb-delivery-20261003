# Copyright (C) Dnspython Contributors, see LICENSE for text of ISC license

# Copyright (C) 2003-2007, 2009-2011 Nominum, Inc.
#
# Permission to use, copy, modify, and distribute this software and its
# documentation for any purpose with or without fee is hereby granted,
# provided that the above copyright notice and this permission notice
# appear in all copies.
#
# THE SOFTWARE IS PROVIDED "AS IS" AND NOMINUM DISCLAIMS ALL WARRANTIES
# WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
# MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL NOMINUM BE LIABLE FOR
# ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
# WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
# ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT
# OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.

"""DNS Zones."""

import dataclasses
import enum
import io
import os
import struct
from collections.abc import Callable, Iterable, Iterator, MutableMapping
from typing import Any, BinaryIO, TextIO, cast

import dns._file_util
import dns.exception
import dns.immutable
import dns.name
import dns.node
import dns.rdata
import dns.rdataclass
import dns.rdataset
import dns.rdatatype
import dns.rdtypes.ANY.SOA
import dns.rdtypes.ANY.ZONEMD
import dns.rrset
import dns.tokenizer
import dns.transaction
import dns.zonefile
from dns.zonetypes import DigestHashAlgorithm, DigestScheme, _digest_hashers


class BadZone(dns.exception.DNSException):
    """The DNS zone is malformed."""


class NoSOA(BadZone):
    """The DNS zone has no SOA RR at its origin."""


class NoNS(BadZone):
    """The DNS zone has no NS RRset at its origin."""


class UnknownOrigin(BadZone):
    """The DNS zone's origin is unknown."""


class UnsupportedDigestScheme(dns.exception.DNSException):
    """The zone digest's scheme is unsupported."""


class UnsupportedDigestHashAlgorithm(dns.exception.DNSException):
    """The zone digest's origin is unsupported."""


class NoDigest(dns.exception.DNSException):
    """The DNS zone has no ZONEMD RRset at its origin."""


class DigestVerificationFailure(dns.exception.DNSException):
    """The ZONEMD digest failed to verify."""


class DigestStatus(enum.Enum):
    """The verification status of a single ZONEMD record.

    The status distinguishes records that actually verified the zone from
    records that could not do so, and keeps the reasons the RFC 8976
    verification procedure treats as disqualifiers (a duplicate
    scheme/hash-algorithm tuple, a serial mismatch) separate from records
    that were never usable (an unsupported scheme or hash algorithm).
    """

    #: The record's digest matched a digest computed over the zone.
    VALID = "valid"
    #: The record's digest did not match the digest of the zone contents.
    INVALID = "invalid"
    #: The record's serial did not match the zone's SOA serial (RFC 8976
    #: section 4 step 5.1).
    SERIAL_MISMATCH = "serial mismatch"
    #: Another ZONEMD record had the same scheme and hash algorithm, which
    #: disqualifies every record sharing the tuple (RFC 8976 section 4
    #: step 4).
    DUPLICATE = "duplicate"
    #: The record's scheme is not supported, so no digest could be computed.
    UNSUPPORTED_SCHEME = "unsupported scheme"
    #: The record's hash algorithm is not supported, so no digest could be
    #: computed.
    UNSUPPORTED_HASH_ALGORITHM = "unsupported hash algorithm"
    #: Computing the digest over the zone failed for another reason, e.g. an
    #: rdata that could not be converted to canonical form.  The record is not
    #: dropped and the zone is not modified; the digest simply cannot verify.
    COMPUTATION_ERROR = "digest computation error"


class DigestExclusionReason(enum.Enum):
    """Why an RRset was not covered by the ZONEMD digest.

    RFC 8976 section 2.3 excludes the apex ZONEMD RRset, as well as any
    RRSIG RRset that covers it, from the digested zone data.  Everything
    else in the zone is covered.
    """

    #: The apex ZONEMD RRset (RFC 8976 section 2.3).
    APEX_ZONEMD = "apex ZONEMD RRset"
    #: An RRSIG RRset covering the apex ZONEMD RRset (RFC 8976 section 2.3).
    APEX_ZONEMD_RRSIG = "RRSIG RRset covering the apex ZONEMD RRset"


@dataclasses.dataclass(frozen=True)
class ExcludedRRset:
    """An RRset that is present in the zone but not covered by the digest.

    :param reason: Why the RRset is excluded.
    :type reason: :py:class:`DigestExclusionReason`
    :param rdtype: The RRset's rdata type.
    :type rdtype: :py:class:`dns.rdatatype.RdataType`
    :param covers: The rdata type covered by the RRset; this is
        :py:data:`dns.rdatatype.NONE` except for RRSIG RRsets.
    :type covers: :py:class:`dns.rdatatype.RdataType`
    :param rr_count: The number of resource records (rdata items) in the
        RRset.
    :type rr_count: int
    """

    reason: DigestExclusionReason
    rdtype: dns.rdatatype.RdataType
    covers: dns.rdatatype.RdataType
    rr_count: int


@dataclasses.dataclass(frozen=True)
class DigestRecordResult:
    """The verification result for a single ZONEMD record.

    :param zonemd: The ZONEMD record that was checked.
    :type zonemd: :py:class:`dns.rdtypes.ANY.ZONEMD.ZONEMD`
    :param status: What happened when the record was checked.
    :type status: :py:class:`DigestStatus`
    :param computed_digest: The digest computed over the zone for the
        record's scheme and hash algorithm, or ``None`` if no digest could
        be computed (e.g. because the scheme or hash algorithm is
        unsupported, or the tuple was disqualified before digesting).
    :type computed_digest: bytes or None
    """

    zonemd: dns.rdtypes.ANY.ZONEMD.ZONEMD
    status: DigestStatus
    computed_digest: bytes | None = None

    @property
    def valid(self) -> bool:
        """Whether this record verified the zone.

        :rtype: bool
        """

        return self.status == DigestStatus.VALID


@dataclasses.dataclass(frozen=True)
class DigestVerificationResult:
    """A reviewable summary of a ZONEMD verification.

    Every ZONEMD record at the zone apex gets its own
    :py:class:`DigestRecordResult`, including records whose scheme or hash
    algorithm is unsupported.  The summary also reports how many resource
    records the digest actually covered and which RRsets were deliberately
    excluded.

    :param serial: The SOA serial of the zone, or ``None`` if the zone has
        no SOA.
    :type serial: int or None
    :param records: The per-ZONEMD-record results, in apex RRset order.
    :type records: list of :py:class:`DigestRecordResult`
    :param covered_rr_count: The number of resource records fed to the
        digest hash.
    :type covered_rr_count: int
    :param covered_rdataset_count: The number of RRsets fed to the digest
        hash.
    :type covered_rdataset_count: int
    :param excluded_rrsets: The RRsets present in the zone but not covered
        by the digest, with the reason for each exclusion.
    :type excluded_rrsets: list of :py:class:`ExcludedRRset`
    """

    serial: int | None
    records: list[DigestRecordResult]
    covered_rr_count: int
    covered_rdataset_count: int
    excluded_rrsets: list[ExcludedRRset]

    @property
    def valid(self) -> bool:
        """Whether at least one ZONEMD record verified the zone.

        Unsupported, duplicate, mismatched, and non-matching records never
        make a result valid; a valid record does.

        :rtype: bool
        """

        return any(record.valid for record in self.records)

    @property
    def excluded_rr_count(self) -> int:
        """The number of resource records not covered by the digest.

        :rtype: int
        """

        return sum(rrset.rr_count for rrset in self.excluded_rrsets)


def _validate_name(
    name: dns.name.Name,
    origin: dns.name.Name | None,
    relativize: bool,
) -> dns.name.Name:
    # This name validation code is shared by Zone and Version
    if origin is None:
        # This should probably never happen as other code (e.g.
        # _rr_line) will notice the lack of an origin before us, but
        # we check just in case!
        raise KeyError("no zone origin is defined")
    if name.is_absolute():
        if not name.is_subdomain(origin):
            raise KeyError("name parameter must be a subdomain of the zone origin")
        if relativize:
            name = name.relativize(origin)
    else:
        # We have a relative name.  Make sure that the derelativized name is
        # not too long.
        try:
            abs_name = name.derelativize(origin)
        except dns.name.NameTooLong:
            # We map dns.name.NameTooLong to KeyError to be consistent with
            # the other exceptions above.
            raise KeyError("relative name too long for zone")
        if not relativize:
            # We have a relative name in a non-relative zone, so use the
            # derelativized name.
            name = abs_name
    return name


@dataclasses.dataclass(frozen=True)
class ZoneStyle(dns.node.NodeStyle):
    """Zone text styles.

    A :py:class:`dns.zone.ZoneStyle` is also a :py:class:`dns.name.NameStyle`,
    :py:class:`dns.rdata.RdataStyle`, :py:class:`dns.rdataset.RdatasetStyle`,
    and :py:class:`dns.node.NodeStyle`.
    See those classes for a description of their options.

    :param bool sorted: If ``True`` (the default), the file will be written
        with the names sorted in DNSSEC order from least to greatest.
        Otherwise the names will be written in whatever order they happen
        to have in the zone's dictionary.
    :param nl: The end of line string.  If ``None`` (the default), the
        output will use the platform's native end-of-line marker (i.e. LF
        on POSIX, CRLF on Windows).
    :type nl: str or ``None``
    :param bool want_origin: If ``True``, emit a ``$ORIGIN`` line at the
        start of the output.  If ``False`` (the default), do not emit one.
    :param bool want_unicode_directive: If ``True`` and the zone has a
        non-empty ``unicode`` attribute, then emit a ``$UNICODE`` line in
        the output.  This directive is not standard, but allows dnspython
        and other aware software to read and write Unicode zonefiles without
        changing the rendering of names and TXT-like records.
    """

    sorted: bool = True
    nl: str | None = None
    want_origin: bool = False
    want_unicode_directive: bool = True


class Zone(dns.transaction.TransactionManager):
    """A DNS zone.

    A ``Zone`` is a mapping from names to nodes.  The zone object may be
    treated like a Python dictionary, e.g. ``zone[name]`` will retrieve
    the node associated with that name.  The *name* may be a
    ``dns.name.Name object``, or it may be a string.  In either case,
    if the name is relative it is treated as relative to the origin of
    the zone.
    """

    node_factory: Callable[[], dns.node.Node] = dns.node.Node
    map_factory: Callable[[], MutableMapping[dns.name.Name, dns.node.Node]] = dict
    # We only require the version types as "Version" to allow for flexibility, as
    # only the version protocol matters
    writable_version_factory: Callable[["Zone", bool], "Version"] | None = None
    immutable_version_factory: Callable[["Version"], "Version"] | None = None

    __slots__ = ["rdclass", "origin", "nodes", "relativize"]

    def __init__(
        self,
        origin: dns.name.Name | str | None,
        rdclass: dns.rdataclass.RdataClass = dns.rdataclass.IN,
        relativize: bool = True,
    ):
        """Initialize a zone object.

        :param origin: The origin of the zone.  It may be a
            :py:class:`dns.name.Name`, a ``str``, or ``None``.  If ``None``,
            then the zone's origin will be set by the first ``$ORIGIN`` line
            in a zone file.
        :type origin: :py:class:`dns.name.Name`, str, or ``None``
        :param rdclass: The zone's rdata class; the default is class IN.
        :type rdclass: :py:class:`dns.rdataclass.RdataClass`
        :param bool relativize: Whether domain names are relativized to the
            zone's origin.  The default is ``True``.
        """

        if origin is not None:
            if isinstance(origin, str):
                origin = dns.name.from_text(origin)
            elif not isinstance(origin, dns.name.Name):
                raise ValueError("origin parameter must be convertible to a DNS name")
            if not origin.is_absolute():
                raise ValueError("origin parameter must be an absolute name")
        self.origin: dns.name.Name | None = origin
        self.rdclass = rdclass
        self.nodes: MutableMapping[dns.name.Name, dns.node.Node] = self.map_factory()
        self.relativize = relativize
        self.unicode: set[str] = set()

    def __eq__(self, other):
        """Two zones are equal if they have the same origin, class, and
        nodes.

        :rtype: bool
        """

        if not isinstance(other, Zone):
            return False
        if (
            self.rdclass != other.rdclass
            or self.origin != other.origin
            or self.nodes != other.nodes
        ):
            return False
        return True

    def __ne__(self, other):
        """Are two zones not equal?

        :rtype: bool
        """

        return not self.__eq__(other)

    def _validate_name(self, name: dns.name.Name | str) -> dns.name.Name:
        # Note that any changes in this method should have corresponding changes
        # made in the Version _validate_name() method.
        if isinstance(name, str):
            name = dns.name.from_text(name, None)
        elif not isinstance(name, dns.name.Name):
            raise KeyError("name parameter must be convertible to a DNS name")
        return _validate_name(name, self.origin, self.relativize)

    def __getitem__(self, key):
        key = self._validate_name(key)
        return self.nodes[key]

    def __setitem__(self, key, value):
        key = self._validate_name(key)
        self.nodes[key] = value

    def __delitem__(self, key):
        key = self._validate_name(key)
        del self.nodes[key]

    def __iter__(self):
        return self.nodes.__iter__()

    def keys(self):
        return self.nodes.keys()

    def values(self):
        return self.nodes.values()

    def items(self):
        return self.nodes.items()

    def get(self, key):
        key = self._validate_name(key)
        return self.nodes.get(key)

    def __contains__(self, key):
        key = self._validate_name(key)
        return key in self.nodes

    def find_node(
        self, name: dns.name.Name | str, create: bool = False
    ) -> dns.node.Node:
        """Find a node in the zone, possibly creating it.

        :param name: The name of the node to find.  The value may be a
            :py:class:`dns.name.Name` or a ``str``.  If absolute, the name
            must be a subdomain of the zone's origin.  If ``zone.relativize``
            is ``True``, then the name will be relativized.
        :type name: :py:class:`dns.name.Name` or str
        :param bool create: If ``True``, the node will be created if it does
            not exist.
        :raises KeyError: if the name is not known and *create* was not
            specified, or if the name was not a subdomain of the origin.
        :rtype: :py:class:`dns.node.Node`
        """

        name = self._validate_name(name)
        node = self.nodes.get(name)
        if node is None:
            if not create:
                raise KeyError
            node = self.node_factory()
            self.nodes[name] = node
        return node

    def get_node(
        self, name: dns.name.Name | str, create: bool = False
    ) -> dns.node.Node | None:
        """Get a node in the zone, possibly creating it.

        This method is like :py:meth:`find_node`, except it returns ``None``
        instead of raising an exception if the node does not exist and
        creation has not been requested.

        :param name: The name of the node to find.  The value may be a
            :py:class:`dns.name.Name` or a ``str``.  If absolute, the name
            must be a subdomain of the zone's origin.  If ``zone.relativize``
            is ``True``, then the name will be relativized.
        :type name: :py:class:`dns.name.Name` or str
        :param bool create: If ``True``, the node will be created if it does
            not exist.
        :rtype: :py:class:`dns.node.Node` or ``None``
        """

        try:
            node = self.find_node(name, create)
        except KeyError:
            node = None
        return node

    def delete_node(self, name: dns.name.Name | str) -> None:
        """Delete the specified node if it exists.

        It is not an error if the node does not exist.

        :param name: The name of the node to delete.  The value may be a
            :py:class:`dns.name.Name` or a ``str``.  If absolute, the name
            must be a subdomain of the zone's origin.  If ``zone.relativize``
            is ``True``, then the name will be relativized.
        :type name: :py:class:`dns.name.Name` or str
        """

        name = self._validate_name(name)
        if name in self.nodes:
            del self.nodes[name]

    def find_rdataset(
        self,
        name: dns.name.Name | str,
        rdtype: dns.rdatatype.RdataType | str,
        covers: dns.rdatatype.RdataType | str = dns.rdatatype.NONE,
        create: bool = False,
    ) -> dns.rdataset.Rdataset:
        """Look for an rdataset with the specified name and type in the zone,
        and return an rdataset encapsulating it.

        The rdataset returned is not a copy; changes to it will change
        the zone.

        :param name: The name of the node to find.  The value may be a
            :py:class:`dns.name.Name` or a ``str``.  If absolute, the name
            must be a subdomain of the zone's origin.  If ``zone.relativize``
            is ``True``, then the name will be relativized.
        :type name: :py:class:`dns.name.Name` or str
        :param rdtype: The rdata type desired.
        :type rdtype: :py:class:`dns.rdatatype.RdataType` or str
        :param covers: The covered type.  Usually :py:attr:`dns.rdatatype.NONE`,
            but if *rdtype* is :py:attr:`dns.rdatatype.SIG` or
            :py:attr:`dns.rdatatype.RRSIG`, then this is the rdata type the
            SIG/RRSIG covers.  The library treats the SIG and RRSIG types as
            a family of types, e.g. RRSIG(A), RRSIG(NS), RRSIG(SOA).
        :type covers: :py:class:`dns.rdatatype.RdataType` or str
        :param bool create: If ``True``, the node will be created if it does
            not exist.
        :raises KeyError: if the name or type are not found and *create* was
            not specified, or if the name was not a subdomain of the origin.
        :rtype: :py:class:`dns.rdataset.Rdataset`
        """

        name = self._validate_name(name)
        rdtype = dns.rdatatype.RdataType.make(rdtype)
        covers = dns.rdatatype.RdataType.make(covers)
        node = self.find_node(name, create)
        return node.find_rdataset(self.rdclass, rdtype, covers, create)

    def get_rdataset(
        self,
        name: dns.name.Name | str,
        rdtype: dns.rdatatype.RdataType | str,
        covers: dns.rdatatype.RdataType | str = dns.rdatatype.NONE,
        create: bool = False,
    ) -> dns.rdataset.Rdataset | None:
        """Look for an rdataset with the specified name and type in the zone.

        This method is like :py:meth:`find_rdataset`, except it returns
        ``None`` instead of raising an exception if the rdataset does not
        exist and creation has not been requested.

        The rdataset returned is not a copy; changes to it will change
        the zone.

        :param name: The name of the node to find.  The value may be a
            :py:class:`dns.name.Name` or a ``str``.  If absolute, the name
            must be a subdomain of the zone's origin.  If ``zone.relativize``
            is ``True``, then the name will be relativized.
        :type name: :py:class:`dns.name.Name` or str
        :param rdtype: The rdata type desired.
        :type rdtype: :py:class:`dns.rdatatype.RdataType` or str
        :param covers: The covered type.  Usually :py:attr:`dns.rdatatype.NONE`,
            but if *rdtype* is :py:attr:`dns.rdatatype.SIG` or
            :py:attr:`dns.rdatatype.RRSIG`, then this is the rdata type the
            SIG/RRSIG covers.  The library treats the SIG and RRSIG types as
            a family of types, e.g. RRSIG(A), RRSIG(NS), RRSIG(SOA).
        :type covers: :py:class:`dns.rdatatype.RdataType` or str
        :param bool create: If ``True``, the node will be created if it does
            not exist.
        :rtype: :py:class:`dns.rdataset.Rdataset` or ``None``
        """

        try:
            rdataset = self.find_rdataset(name, rdtype, covers, create)
        except KeyError:
            rdataset = None
        return rdataset

    def delete_rdataset(
        self,
        name: dns.name.Name | str,
        rdtype: dns.rdatatype.RdataType | str,
        covers: dns.rdatatype.RdataType | str = dns.rdatatype.NONE,
    ) -> None:
        """Delete the rdataset matching *rdtype* and *covers*, if it
        exists at the node specified by *name*.

        It is not an error if the node does not exist, or if there is no
        matching rdataset at the node.  If the node has no rdatasets after
        the deletion, it will itself be deleted.

        :param name: The name of the node.  The value may be a
            :py:class:`dns.name.Name` or a ``str``.  If absolute, the name
            must be a subdomain of the zone's origin.  If ``zone.relativize``
            is ``True``, then the name will be relativized.
        :type name: :py:class:`dns.name.Name` or str
        :param rdtype: The rdata type desired.
        :type rdtype: :py:class:`dns.rdatatype.RdataType` or str
        :param covers: The covered type.  Usually :py:attr:`dns.rdatatype.NONE`,
            but if *rdtype* is :py:attr:`dns.rdatatype.SIG` or
            :py:attr:`dns.rdatatype.RRSIG`, then this is the rdata type the
            SIG/RRSIG covers.  The library treats the SIG and RRSIG types as
            a family of types, e.g. RRSIG(A), RRSIG(NS), RRSIG(SOA).
        :type covers: :py:class:`dns.rdatatype.RdataType` or str
        """

        name = self._validate_name(name)
        rdtype = dns.rdatatype.RdataType.make(rdtype)
        covers = dns.rdatatype.RdataType.make(covers)
        node = self.get_node(name)
        if node is not None:
            node.delete_rdataset(self.rdclass, rdtype, covers)
            if len(node) == 0:
                self.delete_node(name)

    def replace_rdataset(
        self, name: dns.name.Name | str, replacement: dns.rdataset.Rdataset
    ) -> None:
        """Replace an rdataset at name.

        It is not an error if there is no rdataset matching *replacement*.

        Ownership of the *replacement* object is transferred to the zone;
        in other words, this method does not store a copy of *replacement*
        at the node, it stores *replacement* itself.  If the node does not
        exist, it is created.

        :param name: The name of the node.  The value may be a
            :py:class:`dns.name.Name` or a ``str``.  If absolute, the name
            must be a subdomain of the zone's origin.  If ``zone.relativize``
            is ``True``, then the name will be relativized.
        :type name: :py:class:`dns.name.Name` or str
        :param replacement: The replacement rdataset.
        :type replacement: :py:class:`dns.rdataset.Rdataset`
        """

        if replacement.rdclass != self.rdclass:
            raise ValueError("replacement.rdclass != zone.rdclass")
        node = self.find_node(name, True)
        node.replace_rdataset(replacement)

    def find_rrset(
        self,
        name: dns.name.Name | str,
        rdtype: dns.rdatatype.RdataType | str,
        covers: dns.rdatatype.RdataType | str = dns.rdatatype.NONE,
    ) -> dns.rrset.RRset:
        """Look for an rdataset with the specified name and type in the zone,
        and return an RRset encapsulating it.

        This method is less efficient than the similar
        :py:meth:`find_rdataset` because it creates an RRset instead of
        returning the matching rdataset.  It may be more convenient for some
        uses since it returns an object which binds the owner name to the
        rdataset.

        This method may not be used to create new nodes or rdatasets;
        use :py:meth:`find_rdataset` instead.

        :param name: The name of the node to find.  The value may be a
            :py:class:`dns.name.Name` or a ``str``.  If absolute, the name
            must be a subdomain of the zone's origin.  If ``zone.relativize``
            is ``True``, then the name will be relativized.
        :type name: :py:class:`dns.name.Name` or str
        :param rdtype: The rdata type desired.
        :type rdtype: :py:class:`dns.rdatatype.RdataType` or str
        :param covers: The covered type.  Usually :py:attr:`dns.rdatatype.NONE`,
            but if *rdtype* is :py:attr:`dns.rdatatype.SIG` or
            :py:attr:`dns.rdatatype.RRSIG`, then this is the rdata type the
            SIG/RRSIG covers.  The library treats the SIG and RRSIG types as
            a family of types, e.g. RRSIG(A), RRSIG(NS), RRSIG(SOA).
        :type covers: :py:class:`dns.rdatatype.RdataType` or str
        :raises KeyError: if the name or type are not found, or if the name
            was not a subdomain of the origin.
        :rtype: :py:class:`dns.rrset.RRset`
        """

        vname = self._validate_name(name)
        rdtype = dns.rdatatype.RdataType.make(rdtype)
        covers = dns.rdatatype.RdataType.make(covers)
        rdataset = self.nodes[vname].find_rdataset(self.rdclass, rdtype, covers)
        rrset = dns.rrset.RRset(vname, self.rdclass, rdtype, covers)
        rrset.update(rdataset)
        return rrset

    def get_rrset(
        self,
        name: dns.name.Name | str,
        rdtype: dns.rdatatype.RdataType | str,
        covers: dns.rdatatype.RdataType | str = dns.rdatatype.NONE,
    ) -> dns.rrset.RRset | None:
        """Look for an rdataset with the specified name and type in the zone,
        and return an RRset encapsulating it.

        This method is less efficient than the similar :py:meth:`get_rdataset`
        because it creates an RRset instead of returning the matching
        rdataset.  It may be more convenient for some uses since it returns
        an object which binds the owner name to the rdataset.

        This method may not be used to create new nodes or rdatasets;
        use :py:meth:`get_rdataset` instead.

        :param name: The name of the node to find.  The value may be a
            :py:class:`dns.name.Name` or a ``str``.  If absolute, the name
            must be a subdomain of the zone's origin.  If ``zone.relativize``
            is ``True``, then the name will be relativized.
        :type name: :py:class:`dns.name.Name` or str
        :param rdtype: The rdata type desired.
        :type rdtype: :py:class:`dns.rdatatype.RdataType` or str
        :param covers: The covered type.  Usually :py:attr:`dns.rdatatype.NONE`,
            but if *rdtype* is :py:attr:`dns.rdatatype.SIG` or
            :py:attr:`dns.rdatatype.RRSIG`, then this is the rdata type the
            SIG/RRSIG covers.  The library treats the SIG and RRSIG types as
            a family of types, e.g. RRSIG(A), RRSIG(NS), RRSIG(SOA).
        :type covers: :py:class:`dns.rdatatype.RdataType` or str
        :rtype: :py:class:`dns.rrset.RRset` or ``None``
        """

        try:
            rrset = self.find_rrset(name, rdtype, covers)
        except KeyError:
            rrset = None
        return rrset

    def iterate_rdatasets(
        self,
        rdtype: dns.rdatatype.RdataType | str = dns.rdatatype.ANY,
        covers: dns.rdatatype.RdataType | str = dns.rdatatype.NONE,
    ) -> Iterator[tuple[dns.name.Name, dns.rdataset.Rdataset]]:
        """Return a generator which yields ``(name, rdataset)`` tuples for
        all rdatasets in the zone which have the specified *rdtype* and
        *covers*.  If *rdtype* is :py:attr:`dns.rdatatype.ANY` (the default),
        then all rdatasets will be matched.

        :param rdtype: The rdata type desired.
        :type rdtype: :py:class:`dns.rdatatype.RdataType` or str
        :param covers: The covered type.  Usually :py:attr:`dns.rdatatype.NONE`,
            but if *rdtype* is :py:attr:`dns.rdatatype.SIG` or
            :py:attr:`dns.rdatatype.RRSIG`, then this is the rdata type the
            SIG/RRSIG covers.  The library treats the SIG and RRSIG types as
            a family of types, e.g. RRSIG(A), RRSIG(NS), RRSIG(SOA).
        :type covers: :py:class:`dns.rdatatype.RdataType` or str
        """

        rdtype = dns.rdatatype.RdataType.make(rdtype)
        covers = dns.rdatatype.RdataType.make(covers)
        for name, node in self.items():
            for rds in node:
                if rdtype == dns.rdatatype.ANY or (
                    rds.rdtype == rdtype and rds.covers == covers
                ):
                    yield (name, rds)

    def iterate_rdatas(
        self,
        rdtype: dns.rdatatype.RdataType | str = dns.rdatatype.ANY,
        covers: dns.rdatatype.RdataType | str = dns.rdatatype.NONE,
    ) -> Iterator[tuple[dns.name.Name, int, dns.rdata.Rdata]]:
        """Return a generator which yields ``(name, ttl, rdata)`` tuples for
        all rdatas in the zone which have the specified *rdtype* and *covers*.
        If *rdtype* is :py:attr:`dns.rdatatype.ANY` (the default), then all
        rdatas will be matched.

        :param rdtype: The rdata type desired.
        :type rdtype: :py:class:`dns.rdatatype.RdataType` or str
        :param covers: The covered type.  Usually :py:attr:`dns.rdatatype.NONE`,
            but if *rdtype* is :py:attr:`dns.rdatatype.SIG` or
            :py:attr:`dns.rdatatype.RRSIG`, then this is the rdata type the
            SIG/RRSIG covers.  The library treats the SIG and RRSIG types as
            a family of types, e.g. RRSIG(A), RRSIG(NS), RRSIG(SOA).
        :type covers: :py:class:`dns.rdatatype.RdataType` or str
        """

        rdtype = dns.rdatatype.RdataType.make(rdtype)
        covers = dns.rdatatype.RdataType.make(covers)
        for name, node in self.items():
            for rds in node:
                if rdtype == dns.rdatatype.ANY or (
                    rds.rdtype == rdtype and rds.covers == covers
                ):
                    for rdata in rds:
                        yield (name, rds.ttl, rdata)

    def to_file(
        self,
        f: str | os.PathLike | BinaryIO | TextIO,
        sorted: bool = True,
        relativize: bool = True,
        nl: str | bytes | None = None,
        want_comments: bool = False,
        want_origin: bool = False,
        style: ZoneStyle | None = None,
    ) -> None:
        """Write a zone to a file.

        :param f: A file object, a ``str`` filename, or an ``os.PathLike``.
            If not a file object, it is treated as the name of a file to
            open.
        :param bool sorted: If ``True`` (the default), the file will be
            written with the names sorted in DNSSEC order from least to
            greatest.  Otherwise the names will be written in whatever order
            they happen to have in the zone's dictionary.
        :param bool relativize: If ``True`` (the default), domain names in
            the output will be relativized to the zone's origin if possible.
        :param nl: The end of line string.  If ``None`` (the default), the
            output will use the platform's native end-of-line marker (i.e. LF
            on POSIX, CRLF on Windows).
        :type nl: str, bytes, or ``None``
        :param bool want_comments: If ``True``, emit end-of-line comments as
            part of writing the file.  If ``False`` (the default), do not
            emit them.
        :param bool want_origin: If ``True``, emit a ``$ORIGIN`` line at the
            start of the file.  If ``False`` (the default), do not emit one.
        :param style: If specified, the style overrides the other parameters.
        :type style: :py:class:`dns.zone.ZoneStyle` or ``None``
        """
        if style is None:
            kw = {}
            kw["sorted"] = sorted
            kw["relativize"] = relativize
            if relativize:
                assert self.origin is not None
                kw["origin"] = self.origin
            kw["nl"] = nl
            kw["want_comments"] = want_comments
            kw["want_origin"] = want_origin
            style = ZoneStyle.from_keywords(kw)
        return self.to_styled_file(style, f)

    def _write_line(self, output, l, l_b, nl, nl_b):
        try:
            bout = cast(BinaryIO, output)
            bout.write(l_b)
            bout.write(nl_b)
        except TypeError:  # textual mode
            tout = cast(TextIO, output)
            tout.write(l)
            tout.write(nl)

    def to_styled_file(
        self,
        style: ZoneStyle,
        f: str | os.PathLike | BinaryIO | TextIO,
    ) -> None:
        """Write a zone to a styled file.

        :param style: The style to apply.
        :type style: :py:class:`dns.zone.ZoneStyle`
        :param f: A file object, a ``str`` filename, or an ``os.PathLike``.
            If not a file object, it is treated as the name of a file to
            open.
        """

        # Apply style items we learned from $UNICODE when we loaded the zone (if any).
        if style.want_unicode_directive:
            idna_codec: dns.name.IDNACodec | None = style.idna_codec
            if "2008" in self.unicode:
                idna_codec = dns.name.IDNA_2008_Practical
            elif "2003" in self.unicode:
                idna_codec = dns.name.IDNA_2003_Practical
            if "TXT" in self.unicode:
                txt_is_utf8 = True
            else:
                txt_is_utf8 = style.txt_is_utf8
            style = style.replace(idna_codec=idna_codec, txt_is_utf8=txt_is_utf8)
        with dns._file_util.maybe_open(f, "wb") as output:
            # must be in this way, f.encoding may contain None, or even
            # attribute may not be there
            file_enc = getattr(f, "encoding", None)
            if file_enc is None:
                file_enc = "utf-8"

            if style.nl is None:
                # binary mode, '\n' is not enough
                nl_b = os.linesep.encode(file_enc)
                nl = "\n"
            elif isinstance(style.nl, str):
                nl_b = style.nl.encode(file_enc)
                nl = style.nl
            else:
                nl_b = style.nl
                nl = style.nl.decode()
            assert nl is not None
            assert nl_b is not None

            if style.want_unicode_directive and len(self.unicode) > 0:
                l = "$UNICODE " + " ".join(sorted(self.unicode))
                l_b = l.encode(file_enc)
                self._write_line(output, l, l_b, nl, nl_b)
            if style.want_origin:
                assert self.origin is not None
                # Ensure we don't relativize the origin to the origin in $ORIGIN!
                origin_style = style.replace(origin=None)
                l = "$ORIGIN " + self.origin.to_styled_text(origin_style)
                l_b = l.encode(file_enc)
                self._write_line(output, l, l_b, nl, nl_b)
            if style.default_ttl is not None:
                l = f"$TTL {style.default_ttl}"
                l_b = l.encode(file_enc)
                self._write_line(output, l, l_b, nl, nl_b)

            if style.sorted:
                names = list(self.keys())
                names.sort()
            else:
                names = self.keys()
            for n in names:
                l = self[n].to_styled_text(style, n)
                l_b = l.encode(file_enc)
                self._write_line(output, l, l_b, nl, nl_b)

    def to_text(
        self,
        sorted: bool = True,
        relativize: bool = True,
        nl: str | None = None,
        want_comments: bool = False,
        want_origin: bool = False,
        style: ZoneStyle | None = None,
    ) -> str:
        """Return a zone's text as though it were written to a file.

        :param bool sorted: If ``True`` (the default), the output will be
            written with the names sorted in DNSSEC order from least to
            greatest.  Otherwise the names will be written in whatever order
            they happen to have in the zone's dictionary.
        :param bool relativize: If ``True`` (the default), domain names in
            the output will be relativized to the zone's origin if possible.
        :param nl: The end of line string.  If ``None`` (the default), the
            output will use the platform's native end-of-line marker (i.e. LF
            on POSIX, CRLF on Windows).
        :type nl: str or ``None``
        :param bool want_comments: If ``True``, emit end-of-line comments as
            part of the output.  If ``False`` (the default), do not emit them.
        :param bool want_origin: If ``True``, emit a ``$ORIGIN`` line at the
            start of the output.  If ``False`` (the default), do not emit one.
        :param style: If specified, the style overrides the other parameters.
        :type style: :py:class:`dns.zone.ZoneStyle` or ``None``
        :rtype: str
        """
        temp_buffer = io.StringIO()
        self.to_file(
            temp_buffer, sorted, relativize, nl, want_comments, want_origin, style
        )
        return_value = temp_buffer.getvalue()
        temp_buffer.close()
        return return_value

    def to_styled_text(self, style: ZoneStyle):
        """Return a zone's styled text as though it were written to a file.

        See the documentation for :py:class:`dns.zone.ZoneStyle` for a description
        of the style parameters.

        :param style: The style to apply.
        :type style: :py:class:`dns.zone.ZoneStyle`
        :rtype: str
        """

        temp_buffer = io.StringIO()
        self.to_styled_file(style, temp_buffer)
        return_value = temp_buffer.getvalue()
        temp_buffer.close()
        return return_value

    def check_origin(self) -> None:
        """Do some simple checking of the zone's origin.

        :raises dns.zone.NoSOA: if there is no SOA RRset.
        :raises dns.zone.NoNS: if there is no NS RRset.
        :raises KeyError: if there is no origin node.
        """
        if self.relativize:
            name = dns.name.empty
        else:
            assert self.origin is not None
            name = self.origin
        if self.get_rdataset(name, dns.rdatatype.SOA) is None:
            raise NoSOA
        if self.get_rdataset(name, dns.rdatatype.NS) is None:
            raise NoNS

    def get_soa(
        self, txn: dns.transaction.Transaction | None = None
    ) -> dns.rdtypes.ANY.SOA.SOA:
        """Get the zone SOA rdata.

        :raises dns.zone.NoSOA: if there is no SOA RRset.
        :rtype: :py:class:`dns.rdtypes.ANY.SOA.SOA`
        """
        if self.relativize:
            origin_name = dns.name.empty
        else:
            if self.origin is None:
                # get_soa() has been called very early, and there must not be
                # an SOA if there is no origin.
                raise NoSOA
            origin_name = self.origin
        soa_rds: dns.rdataset.Rdataset | None
        if txn:
            soa_rds = txn.get(origin_name, dns.rdatatype.SOA)
        else:
            soa_rds = self.get_rdataset(origin_name, dns.rdatatype.SOA)
        if soa_rds is None:
            raise NoSOA
        else:
            soa = cast(dns.rdtypes.ANY.SOA.SOA, soa_rds[0])
            return soa

    def _apex_name(self) -> dns.name.Name:
        if self.relativize:
            return dns.name.empty
        assert self.origin is not None
        return self.origin

    def _is_excluded_apex_zonemd(
        self, name: dns.name.Name, rdataset: dns.rdataset.Rdataset
    ) -> bool:
        # RFC 8976 section 2.3: the apex ZONEMD RRset, and any RRSIG RRset
        # covering it, are not part of the digested zone data.
        return name == self._apex_name() and dns.rdatatype.ZONEMD in (
            rdataset.rdtype,
            rdataset.covers,
        )

    def _digest_coverage(
        self,
    ) -> tuple[int, int, list[ExcludedRRset]]:
        """Walk the zone in the same order the digest does, counting the
        resource records that are actually covered and collecting the RRsets
        that RFC 8976 excludes from the digest.
        """
        covered_rr_count = 0
        covered_rdataset_count = 0
        excluded: list[ExcludedRRset] = []
        for name, node in sorted(self.items()):
            for rdataset in sorted(node, key=lambda rds: (rds.rdtype, rds.covers)):
                if self._is_excluded_apex_zonemd(name, rdataset):
                    if rdataset.rdtype == dns.rdatatype.RRSIG:
                        reason = DigestExclusionReason.APEX_ZONEMD_RRSIG
                    else:
                        reason = DigestExclusionReason.APEX_ZONEMD
                    excluded.append(
                        ExcludedRRset(
                            reason,
                            rdataset.rdtype,
                            rdataset.covers,
                            len(rdataset),
                        )
                    )
                    continue
                covered_rdataset_count += 1
                covered_rr_count += len(rdataset)
        return covered_rr_count, covered_rdataset_count, excluded

    def _compute_digest(
        self,
        hash_algorithm: DigestHashAlgorithm,
        scheme: DigestScheme = DigestScheme.SIMPLE,
    ) -> bytes:
        hashinfo = _digest_hashers.get(hash_algorithm)
        if not hashinfo:
            raise UnsupportedDigestHashAlgorithm
        if scheme != DigestScheme.SIMPLE:
            raise UnsupportedDigestScheme

        hasher = hashinfo()
        for name, node in sorted(self.items()):
            rrnamebuf = name.to_digestable(self.origin)
            for rdataset in sorted(node, key=lambda rds: (rds.rdtype, rds.covers)):
                if self._is_excluded_apex_zonemd(name, rdataset):
                    continue
                rrfixed = struct.pack(
                    "!HHI", rdataset.rdtype, rdataset.rdclass, rdataset.ttl
                )
                rdatas = [rdata.to_digestable(self.origin) for rdata in rdataset]
                for rdata in sorted(rdatas):
                    rrlen = struct.pack("!H", len(rdata))
                    hasher.update(rrnamebuf + rrfixed + rrlen + rdata)
        return hasher.digest()

    def compute_digest(
        self,
        hash_algorithm: DigestHashAlgorithm,
        scheme: DigestScheme = DigestScheme.SIMPLE,
    ) -> dns.rdtypes.ANY.ZONEMD.ZONEMD:
        serial = self.get_soa().serial
        digest = self._compute_digest(hash_algorithm, scheme)
        return dns.rdtypes.ANY.ZONEMD.ZONEMD(
            self.rdclass, dns.rdatatype.ZONEMD, serial, scheme, hash_algorithm, digest
        )

    def check_digest(
        self, zonemd: dns.rdtypes.ANY.ZONEMD.ZONEMD | None = None
    ) -> DigestVerificationResult:
        """Check the zone's ZONEMD records and return a reviewable summary.

        Unlike :py:meth:`verify_digest`, this never raises on a failed
        verification; the outcome of every ZONEMD record is reported in the
        returned :py:class:`DigestVerificationResult`, including records with
        an unsupported scheme or hash algorithm, which get a status of their
        own rather than being silently skipped.

        The zone is only read; no record is added, removed, or modified, and
        no record is dropped from the digest in order to force a match.

        :param zonemd: A specific ZONEMD record to check.  If ``None``, every
            ZONEMD record at the zone apex is checked.
        :type zonemd: :py:class:`dns.rdtypes.ANY.ZONEMD.ZONEMD` or None
        :rtype: :py:class:`DigestVerificationResult`
        :raises dns.zone.NoDigest: if *zonemd* is ``None`` and the zone apex
            has no ZONEMD RRset.
        """
        digests: dns.rdataset.Rdataset | list[dns.rdtypes.ANY.ZONEMD.ZONEMD]
        if zonemd:
            digests = [zonemd]
        else:
            assert self.origin is not None
            rds = self.get_rdataset(self.origin, dns.rdatatype.ZONEMD)
            if rds is None:
                raise NoDigest
            digests = rds
        try:
            serial: int | None = self.get_soa().serial
        except NoSOA:
            # Without an SOA there is no serial for a ZONEMD serial to match.
            serial = None
        (
            covered_rr_count,
            covered_rdataset_count,
            excluded,
        ) = self._digest_coverage()
        # RFC 8976 section 4 step 4: a (scheme, hash algorithm) tuple that
        # occurs more than once disqualifies every ZONEMD RR that has it.
        duplicated: set[tuple[int, int]] = set()
        seen: set[tuple[int, int]] = set()
        for digest in digests:
            key = (digest.scheme, digest.hash_algorithm)
            if key in seen:
                duplicated.add(key)
            seen.add(key)

        def get_status(
            record: dns.rdtypes.ANY.ZONEMD.ZONEMD,
        ) -> tuple[DigestStatus, bytes | None]:
            try:
                computed = self._compute_digest(record.hash_algorithm, record.scheme)
            except UnsupportedDigestScheme:
                return DigestStatus.UNSUPPORTED_SCHEME, None
            except UnsupportedDigestHashAlgorithm:
                return DigestStatus.UNSUPPORTED_HASH_ALGORITHM, None
            except Exception:
                # Any other failure while digesting the zone (for example an
                # rdata that cannot be canonicalized) is reported rather than
                # silently swallowed.  The offending record is not dropped and
                # the zone is not modified, so the digest cannot verify.
                return DigestStatus.COMPUTATION_ERROR, None
            if computed == record.digest:
                return DigestStatus.VALID, computed
            return DigestStatus.INVALID, computed

        records: list[DigestRecordResult] = []
        for digest in digests:
            if (digest.scheme, digest.hash_algorithm) in duplicated:
                status = DigestStatus.DUPLICATE
                computed: bytes | None = None
            elif serial is not None and digest.serial != serial:
                # RFC 8976 section 4 step 5.1.  The apex ZONEMD RRset is not
                # part of what the digest covers, so its serial is only tied
                # to the zone's by this explicit comparison.
                status = DigestStatus.SERIAL_MISMATCH
                computed = None
            else:
                status, computed = get_status(digest)
            records.append(DigestRecordResult(digest, status, computed))
        return DigestVerificationResult(
            serial,
            records,
            covered_rr_count,
            covered_rdataset_count,
            excluded,
        )

    def verify_digest(
        self, zonemd: dns.rdtypes.ANY.ZONEMD.ZONEMD | None = None
    ) -> None:
        result = self.check_digest(zonemd)
        if result.valid:
            return
        raise DigestVerificationFailure

    # TransactionManager methods

    def reader(self) -> "Transaction":
        return Transaction(self, False, Version(self, 1, self.nodes, self.origin))

    def writer(self, replacement: bool = False) -> "Transaction":
        txn = Transaction(self, replacement)
        txn._setup_version()
        return txn

    def origin_information(
        self,
    ) -> tuple[dns.name.Name | None, bool, dns.name.Name | None]:
        effective: dns.name.Name | None
        if self.relativize:
            effective = dns.name.empty
        else:
            effective = self.origin
        return (self.origin, self.relativize, effective)

    def get_class(self):
        return self.rdclass

    # Transaction methods

    def _end_read(self, txn):
        pass

    def _end_write(self, txn):
        pass

    def _commit_version(self, txn, version, origin):
        self.nodes = version.nodes
        if self.origin is None:
            self.origin = origin

    def _get_next_version_id(self) -> int:
        # Versions are ephemeral and all have id 1
        return 1


# These classes used to be in dns.versioned, but have moved here so we can use
# the copy-on-write transaction mechanism for both kinds of zones.  In a
# regular zone, the version only exists during the transaction, and the nodes
# are regular dns.node.Nodes.

# A node with a version id.


class VersionedNode(dns.node.Node):  # lgtm[py/missing-equals]
    __slots__ = ["id"]

    def __init__(self):
        super().__init__()
        # A proper id will get set by the Version
        self.id = 0


@dns.immutable.immutable
class ImmutableVersionedNode(VersionedNode):
    def __init__(self, node):
        super().__init__()
        self.id = node.id
        self.rdatasets = tuple(
            [dns.rdataset.ImmutableRdataset(rds) for rds in node.rdatasets]
        )

    def find_rdataset(
        self,
        rdclass: dns.rdataclass.RdataClass,
        rdtype: dns.rdatatype.RdataType,
        covers: dns.rdatatype.RdataType = dns.rdatatype.NONE,
        create: bool = False,
    ) -> dns.rdataset.Rdataset:
        if create:
            raise TypeError("immutable")
        return super().find_rdataset(rdclass, rdtype, covers, False)

    def get_rdataset(
        self,
        rdclass: dns.rdataclass.RdataClass,
        rdtype: dns.rdatatype.RdataType,
        covers: dns.rdatatype.RdataType = dns.rdatatype.NONE,
        create: bool = False,
    ) -> dns.rdataset.Rdataset | None:
        if create:
            raise TypeError("immutable")
        return super().get_rdataset(rdclass, rdtype, covers, False)

    def delete_rdataset(
        self,
        rdclass: dns.rdataclass.RdataClass,
        rdtype: dns.rdatatype.RdataType,
        covers: dns.rdatatype.RdataType = dns.rdatatype.NONE,
    ) -> None:
        raise TypeError("immutable")

    def replace_rdataset(self, replacement: dns.rdataset.Rdataset) -> None:
        raise TypeError("immutable")

    def is_immutable(self) -> bool:
        return True


class Version:
    def __init__(
        self,
        zone: Zone,
        id: int,
        nodes: MutableMapping[dns.name.Name, dns.node.Node] | None = None,
        origin: dns.name.Name | None = None,
    ):
        self.zone = zone
        self.id = id
        if nodes is not None:
            self.nodes = nodes
        else:
            self.nodes = zone.map_factory()
        self.origin = origin

    def _validate_name(self, name: dns.name.Name) -> dns.name.Name:
        return _validate_name(name, self.origin, self.zone.relativize)

    def get_node(self, name: dns.name.Name) -> dns.node.Node | None:
        name = self._validate_name(name)
        return self.nodes.get(name)

    def get_rdataset(
        self,
        name: dns.name.Name,
        rdtype: dns.rdatatype.RdataType,
        covers: dns.rdatatype.RdataType,
    ) -> dns.rdataset.Rdataset | None:
        node = self.get_node(name)
        if node is None:
            return None
        return node.get_rdataset(self.zone.rdclass, rdtype, covers)

    def keys(self):
        return self.nodes.keys()

    def items(self):
        return self.nodes.items()


class WritableVersion(Version):
    def __init__(self, zone: Zone, replacement: bool = False):
        # The zone._versions_lock must be held by our caller in a versioned
        # zone.
        id = zone._get_next_version_id()
        super().__init__(zone, id)
        if not replacement:
            # We copy the map, because that gives us a simple and thread-safe
            # way of doing versions, and we have a garbage collector to help
            # us.  We only make new node objects if we actually change the
            # node.
            self.nodes.update(zone.nodes)
        # We have to copy the zone origin as it may be None in the first
        # version, and we don't want to mutate the zone until we commit.
        self.origin = zone.origin
        self.changed: set[dns.name.Name] = set()

    def _maybe_cow_with_name(
        self, name: dns.name.Name
    ) -> tuple[dns.node.Node, dns.name.Name]:
        name = self._validate_name(name)
        node = self.nodes.get(name)
        if node is None or name not in self.changed:
            new_node = self.zone.node_factory()
            if hasattr(new_node, "id"):
                # We keep doing this for backwards compatibility, as earlier
                # code used new_node.id != self.id for the "do we need to CoW?"
                # test.  Now we use the changed set as this works with both
                # regular zones and versioned zones.
                #
                # We ignore the type error as this is safe but checkers have trouble.
                new_node.id = self.id  # type: ignore
            if node is not None:
                # moo!  copy on write!
                new_node.rdatasets.extend(node.rdatasets)
            self.nodes[name] = new_node
            self.changed.add(name)
            return (new_node, name)
        else:
            return (node, name)

    def _maybe_cow(self, name: dns.name.Name) -> dns.node.Node:
        return self._maybe_cow_with_name(name)[0]

    def delete_node(self, name: dns.name.Name) -> None:
        name = self._validate_name(name)
        if name in self.nodes:
            del self.nodes[name]
            self.changed.add(name)

    def put_rdataset(
        self, name: dns.name.Name, rdataset: dns.rdataset.Rdataset
    ) -> None:
        node = self._maybe_cow(name)
        node.replace_rdataset(rdataset)

    def delete_rdataset(
        self,
        name: dns.name.Name,
        rdtype: dns.rdatatype.RdataType,
        covers: dns.rdatatype.RdataType,
    ) -> None:
        node, name = self._maybe_cow_with_name(name)
        node.delete_rdataset(self.zone.rdclass, rdtype, covers)
        if len(node) == 0:
            del self.nodes[name]


@dns.immutable.immutable
class ImmutableVersion(Version):
    def __init__(self, version: Version):
        if not isinstance(version, WritableVersion):
            raise ValueError(
                "a dns.zone.ImmutableVersion requires a dns.zone.WritableVersion"
            )
        # We tell super() that it's a replacement as we don't want it
        # to copy the nodes, as we're about to do that with an
        # immutable Dict.
        super().__init__(version.zone, True)
        # set the right id!
        self.id = version.id
        # keep the origin
        self.origin = version.origin
        # Make changed nodes immutable
        for name in version.changed:
            node = version.nodes.get(name)
            # it might not exist if we deleted it in the version
            if node:
                version.nodes[name] = ImmutableVersionedNode(node)
        # We're changing the type of the nodes dictionary here on purpose, so
        # we ignore the mypy error.
        self.nodes = dns.immutable.Dict(
            version.nodes, True, self.zone.map_factory
        )  # pyright: ignore


class Transaction(dns.transaction.Transaction):
    def __init__(
        self,
        zone: Zone,
        replacement: bool,
        version: Version | None = None,
        make_immutable: bool = False,
    ):
        read_only = version is not None
        super().__init__(zone, replacement, read_only)
        self.version = version
        self.make_immutable = make_immutable

    @property
    def zone(self) -> Zone:
        return cast(Zone, self.manager)

    def _setup_version(self):
        assert self.version is None
        factory = self.manager.writable_version_factory  # type: ignore
        if factory is None:
            factory = WritableVersion
        self.version = factory(self.zone, self.replacement)  # pyright: ignore

    def _get_rdataset(self, name, rdtype, covers):
        assert self.version is not None
        return self.version.get_rdataset(name, rdtype, covers)

    def _put_rdataset(self, name, rdataset):
        assert not self.read_only
        assert self.version is not None
        version = cast(WritableVersion, self.version)
        version.put_rdataset(name, rdataset)

    def _delete_name(self, name):
        assert not self.read_only
        assert self.version is not None
        version = cast(WritableVersion, self.version)
        version.delete_node(name)

    def _delete_rdataset(self, name, rdtype, covers):
        assert not self.read_only
        assert self.version is not None
        version = cast(WritableVersion, self.version)
        version.delete_rdataset(name, rdtype, covers)

    def _name_exists(self, name):
        assert self.version is not None
        return self.version.get_node(name) is not None

    def _changed(self):
        if self.read_only:
            return False
        else:
            assert self.version is not None
            version = cast(WritableVersion, self.version)
            return len(version.changed) > 0 or (
                self.replacement and len(self.zone.nodes) > 0
            )

    def _end_transaction(self, commit):
        assert self.zone is not None
        assert self.version is not None
        if self.read_only:
            self.zone._end_read(self)  # pyright: ignore
        elif commit and self._changed():
            if self.make_immutable:
                factory = self.manager.immutable_version_factory  # type: ignore
                if factory is None:
                    factory = ImmutableVersion
                version = factory(self.version)
            else:
                version = self.version
            self.zone._commit_version(  # pyright: ignore
                self, version, self.version.origin
            )

        else:
            # rollback
            self.zone._end_write(self)  # pyright: ignore

    def _set_origin(self, origin):
        assert self.version is not None
        if self.version.origin is None:
            self.version.origin = origin

    def _iterate_rdatasets(self):
        assert self.version is not None
        for name, node in self.version.items():
            for rdataset in node:
                yield (name, rdataset)

    def _iterate_names(self):
        assert self.version is not None
        return self.version.keys()

    def _get_node(self, name):
        assert self.version is not None
        return self.version.get_node(name)

    def _origin_information(
        self,
    ) -> tuple[dns.name.Name | None, bool, dns.name.Name | None]:
        assert self.version is not None
        absolute, relativize, effective = self.manager.origin_information()
        if absolute is None and self.version.origin is not None:
            # No origin has been committed yet, but we've learned one as part of
            # this txn.  Use it.
            absolute = self.version.origin
            if relativize:
                effective = dns.name.empty
            else:
                effective = absolute
        return (absolute, relativize, effective)


def _from_text(
    text: Any,
    origin: dns.name.Name | str | None = None,
    rdclass: dns.rdataclass.RdataClass = dns.rdataclass.IN,
    relativize: bool = True,
    zone_factory: Any = Zone,
    filename: str | None = None,
    allow_include: bool = False,
    check_origin: bool = True,
    idna_codec: dns.name.IDNACodec | None = None,
    allow_directives: bool | Iterable[str] = True,
    transaction_setup: Callable[[dns.transaction.Transaction], None] | None = None,
    rfc2308_ttl: bool = True,
) -> Zone:
    # See the comments for the public APIs from_text() and from_file() for
    # details.

    # 'text' can also be a file, but we don't publish that fact
    # since it's an implementation detail.  The official file
    # interface is from_file().

    if filename is None:
        filename = "<string>"
    zone = zone_factory(origin, rdclass, relativize=relativize)
    with zone.writer(True) as txn:
        if transaction_setup is not None:
            transaction_setup(txn)
        tok = dns.tokenizer.Tokenizer(text, filename, idna_codec=idna_codec)
        reader = dns.zonefile.Reader(
            tok,
            rdclass,
            txn,
            allow_include=allow_include,
            allow_directives=allow_directives,
            rfc2308_ttl=rfc2308_ttl,
        )
        try:
            reader.read()
            zone.unicode = txn.unicode
        except dns.zonefile.UnknownOrigin:
            # for backwards compatibility
            raise UnknownOrigin
    # Now that we're done reading, do some basic checking of the zone.
    if check_origin:
        zone.check_origin()
    return zone


def from_text(
    text: str,
    origin: dns.name.Name | str | None = None,
    rdclass: dns.rdataclass.RdataClass = dns.rdataclass.IN,
    relativize: bool = True,
    zone_factory: Any = Zone,
    filename: str | None = None,
    allow_include: bool = False,
    check_origin: bool = True,
    idna_codec: dns.name.IDNACodec | None = None,
    allow_directives: bool | Iterable[str] = True,
    transaction_setup: Callable[[dns.transaction.Transaction], None] | None = None,
    rfc2308_ttl: bool = True,
) -> Zone:
    """Build a zone object from a zone file format string.

    :param str text: The zone file format input.
    :param origin: The origin of the zone.  If not specified, the first
        ``$ORIGIN`` statement in the zone file will determine the origin.
    :type origin: :py:class:`dns.name.Name`, str, or ``None``
    :param rdclass: The zone's rdata class; the default is class IN.
    :type rdclass: :py:class:`dns.rdataclass.RdataClass`
    :param bool relativize: Whether domain names are relativized to the
        zone's origin.  The default is ``True``.
    :param zone_factory: The zone factory to use.  If ``None``, then
        :py:class:`dns.zone.Zone` will be used.  The value may be any class
        or callable that returns a subclass of :py:class:`dns.zone.Zone`.
    :param filename: The filename to emit when describing where an error
        occurred; the default is ``'<string>'``.
    :type filename: str or ``None``
    :param bool allow_include: If ``True`` (the default), then ``$INCLUDE``
        directives are permitted.  If ``False``, then encountering a
        ``$INCLUDE`` will raise a :py:exc:`SyntaxError`.
    :param bool check_origin: If ``True`` (the default), then sanity checks
        of the origin node will be made by calling
        :py:meth:`dns.zone.Zone.check_origin`.
    :param idna_codec: The IDNA encoder/decoder.  If ``None``, the default
        IDNA encoder/decoder is used.
    :type idna_codec: :py:class:`dns.name.IDNACodec` or ``None``
    :param allow_directives: If ``True`` (the default), directives are
        permitted and *allow_include* controls ``$INCLUDE``.  If ``False``
        or an empty iterable, no directive processing is done.  If a
        non-empty iterable, only the listed directives (including the ``$``)
        are allowed.
    :type allow_directives: bool or Iterable[str]
    :param transaction_setup: If not ``None``, it is called with the just-created
        transaction.  This lets the caller alter the transaction's configuration
        before it is used, for example adding checking policies.
    :type transaction_setup: ``None`` or ``Callable[[dns.transaction.Transaction], None]``.
    :param bool rfc2308_ttl: If ``True``, the default, then RFC 2308 section 4
        is followed and the SOA MINIMUM field is not treated as a source of the
        default TTL; a record with no TTL instead inherits the most recently
        stated TTL, per RFC 1035 section 5.1.  The SOA MINIMUM is still used as a
        last resort if no TTL has been stated at all.  If ``False``, then a master
        file with no ``$TTL`` directive gets its default TTL from the SOA MINIMUM
        field, which is the pre-RFC 2308 behavior.
    :raises dns.zone.NoSOA: if there is no SOA RRset.
    :raises dns.zone.NoNS: if there is no NS RRset.
    :raises KeyError: if there is no origin node.
    :returns: A subclass of :py:class:`dns.zone.Zone`.
    """
    return _from_text(
        text,
        origin,
        rdclass,
        relativize,
        zone_factory,
        filename,
        allow_include,
        check_origin,
        idna_codec,
        allow_directives,
        transaction_setup,
        rfc2308_ttl,
    )


def from_file(
    f: str | os.PathLike | TextIO,
    origin: dns.name.Name | str | None = None,
    rdclass: dns.rdataclass.RdataClass = dns.rdataclass.IN,
    relativize: bool = True,
    zone_factory: Any = Zone,
    filename: str | None = None,
    allow_include: bool = True,
    check_origin: bool = True,
    idna_codec: dns.name.IDNACodec | None = None,
    allow_directives: bool | Iterable[str] = True,
    transaction_setup: Callable[[dns.transaction.Transaction], None] | None = None,
    rfc2308_ttl: bool = True,
) -> Zone:
    """Read a zone file and build a zone object.

    :param f: A file object, a ``str`` filename, or an ``os.PathLike``.  If
        not a file object, it is treated as the name of a file to open.
    :param origin: The origin of the zone.  If not specified, the first
        ``$ORIGIN`` statement in the zone file will determine the origin.
    :type origin: :py:class:`dns.name.Name`, str, or ``None``
    :param rdclass: The zone's rdata class; the default is class IN.
    :type rdclass: :py:class:`dns.rdataclass.RdataClass`
    :param bool relativize: Whether domain names are relativized to the
        zone's origin.  The default is ``True``.
    :param zone_factory: The zone factory to use.  If ``None``, then
        :py:class:`dns.zone.Zone` will be used.  The value may be any class
        or callable that returns a subclass of :py:class:`dns.zone.Zone`.
    :param filename: The filename to emit when describing where an error
        occurred; the default is ``'<string>'``.
    :type filename: str or ``None``
    :param bool allow_include: If ``True`` (the default), then ``$INCLUDE``
        directives are permitted.  If ``False``, then encountering a
        ``$INCLUDE`` will raise a :py:exc:`SyntaxError`.
    :param bool check_origin: If ``True`` (the default), then sanity checks
        of the origin node will be made by calling
        :py:meth:`dns.zone.Zone.check_origin`.
    :param idna_codec: The IDNA encoder/decoder.  If ``None``, the default
        IDNA encoder/decoder is used.
    :type idna_codec: :py:class:`dns.name.IDNACodec` or ``None``
    :param allow_directives: If ``True`` (the default), directives are
        permitted and *allow_include* controls ``$INCLUDE``.  If ``False``
        or an empty iterable, no directive processing is done.  If a
        non-empty iterable, only the listed directives (including the ``$``)
        are allowed.
    :type allow_directives: bool or Iterable[str]
    :param transaction_setup: If not ``None``, it is called with the just-created
        transaction.  This lets the caller alter the transaction's configuration
        before it is used, for example adding checking policies.
    :type transaction_setup: ``None`` or ``Callable[[dns.transaction.Transaction], None]``.
    :param bool rfc2308_ttl: If ``True``, the default, then RFC 2308 section 4
        is followed and the SOA MINIMUM field is not treated as a source of the
        default TTL; a record with no TTL instead inherits the most recently
        stated TTL, per RFC 1035 section 5.1.  The SOA MINIMUM is still used as a
        last resort if no TTL has been stated at all.  If ``False``, then a master
        file with no ``$TTL`` directive gets its default TTL from the SOA MINIMUM
        field, which is the pre-RFC 2308 behavior.
    :raises dns.zone.NoSOA: if there is no SOA RRset.
    :raises dns.zone.NoNS: if there is no NS RRset.
    :raises KeyError: if there is no origin node.
    :returns: A subclass of :py:class:`dns.zone.Zone`.
    """

    if filename is None:
        filename = dns._file_util.as_filename(f)
    with dns._file_util.maybe_open(f, encoding="utf-8") as fp:
        return _from_text(
            fp,
            origin,
            rdclass,
            relativize,
            zone_factory,
            filename,
            allow_include,
            check_origin,
            idna_codec,
            allow_directives,
            transaction_setup,
            rfc2308_ttl,
        )
    assert False  # make mypy happy  lgtm[py/unreachable-statement]


def from_xfr(
    xfr: Any,
    zone_factory: Any = Zone,
    relativize: bool = True,
    check_origin: bool = True,
) -> Zone:
    """Convert the output of a zone transfer generator into a zone object.

    :param xfr: A generator of :py:class:`dns.message.Message` objects,
        typically from :py:func:`dns.query.xfr`.
    :param zone_factory: The zone factory to use.  If ``None``, then
        :py:class:`dns.zone.Zone` will be used.
    :param bool relativize: Whether domain names are relativized to the
        zone's origin.  The default is ``True``.  It is essential that this
        setting matches the one specified to the generator.
    :param bool check_origin: If ``True`` (the default), then sanity checks
        of the origin node will be made by calling
        :py:meth:`dns.zone.Zone.check_origin`.
    :raises dns.zone.NoSOA: if there is no SOA RRset.
    :raises dns.zone.NoNS: if there is no NS RRset.
    :raises KeyError: if there is no origin node.
    :raises ValueError: if no messages are yielded by the generator.
    :returns: A subclass of :py:class:`dns.zone.Zone`.
    """

    z = None
    for r in xfr:
        if z is None:
            if relativize:
                origin = r.origin
            else:
                origin = r.answer[0].name
            rdclass = r.answer[0].rdclass
            z = zone_factory(origin, rdclass, relativize=relativize)
        for rrset in r.answer:
            # Apply the checks that dns.xfr.Inbound and a zone transaction would,
            # as the messages yielded by dns.query.xfr() are not filtered.
            if z.origin is not None:
                name = rrset.name.derelativize(z.origin)
                if not name.is_subdomain(z.origin):
                    # Ignore glue that is not a subdomain of the origin.
                    continue
                if rrset.rdtype == dns.rdatatype.SOA and name != z.origin:
                    raise dns.exception.FormError("non-origin SOA in zone transfer")
            if rrset.rdclass != z.rdclass:
                raise dns.exception.FormError("wrong rdclass in zone transfer")
            znode = z.nodes.get(rrset.name)
            if not znode:
                znode = z.node_factory()
                z.nodes[rrset.name] = znode
            zrds = znode.find_rdataset(rrset.rdclass, rrset.rdtype, rrset.covers, True)
            zrds.update_ttl(rrset.ttl)
            for rd in rrset:
                zrds.add(rd)
    if z is None:
        raise ValueError("empty transfer")
    if check_origin:
        z.check_origin()
    return z
