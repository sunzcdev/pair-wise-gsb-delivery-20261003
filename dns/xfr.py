# Copyright (C) Dnspython Contributors, see LICENSE for text of ISC license

# Copyright (C) 2003-2017 Nominum, Inc.
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

import enum
from collections.abc import Callable
from typing import Any, cast

import dns.edns
import dns.exception
import dns.message
import dns.name
import dns.rcode
import dns.rdata
import dns.rdataset
import dns.rdatatype
import dns.rdtypes
import dns.rdtypes.ANY
import dns.rdtypes.ANY.SOA
import dns.serial
import dns.transaction
import dns.tsig


class TransferError(dns.exception.DNSException):
    """A zone transfer response got a non-zero rcode."""

    def __init__(self, rcode):
        message = f"Zone transfer error: {dns.rcode.to_text(rcode)}"
        super().__init__(message)
        self.rcode = rcode


class SerialWentBackwards(dns.exception.FormError):
    """The current serial number is less than the serial we know."""


class UseTCP(dns.exception.DNSException):
    """This IXFR cannot be completed with UDP."""


class IXFRFormError(dns.exception.FormError):
    """An IXFR response was malformed.

    All IXFR structure and content diagnostic errors derive from this class,
    and are also :class:`dns.exception.FormError` instances so existing code
    catching ``FormError`` keeps working.  Nothing is committed when one of
    these is raised; the transfer's writer transaction is rolled back and the
    zone is left in its prior state.
    """


class IXFRSerialError(IXFRFormError):
    """The SOA serial numbers of an IXFR response did not connect properly.

    RFC 1995 requires the deletion set of each difference to start with the
    serial already known to the receiver, and each following addition set to
    introduce a newer serial.  The first and the final SOA must carry the
    newest serial and be identical.
    """


class IXFRDeleteNotExact(IXFRFormError):
    """An IXFR deletion set removed an RR that was not present.

    A well-formed IXFR only deletes records currently in the zone, so a delete
    of a missing RR indicates a corrupt or out-of-sync transfer rather than a
    harmless no-op.
    """


class IXFRDuplicateAdd(IXFRFormError):
    """An IXFR addition set added an RR that was already present.

    A well-formed IXFR only adds records that are not already in the zone.
    Duplicate additions are merged harmlessly by a plain add(), but reporting
    them makes a corrupt transfer diagnosable.
    """


class _Mode(enum.Enum):
    # We are still consuming the RFC 1995 incremental difference sequence.
    IXFR = enum.auto()
    # A non-SOA RR arrived where the second SOA of an IXFR was required, so
    # the server answered an IXFR query with an AXFR-style full zone; the
    # original (empty) incremental transaction has been rolled back and a
    # replacement writer is consuming the full zone.
    AXFR_FALLBACK = enum.auto()
    # A genuine AXFR transfer (either requested directly or after the mode
    # above has been entered).
    AXFR = enum.auto()


class Inbound:
    """
    State machine for zone transfers.
    """

    def __init__(
        self,
        txn_manager: dns.transaction.TransactionManager,
        rdtype: dns.rdatatype.RdataType = dns.rdatatype.AXFR,
        serial: int | None = None,
        is_udp: bool = False,
        raise_on_serial_went_backwards: bool = True,
        transaction_setup: Callable[[dns.transaction.Transaction], None] | None = None,
        raise_on_delete_missing: bool = True,
        raise_on_duplicate_add: bool = True,
    ):
        """Initialize an inbound zone transfer.

        *txn_manager* is a :py:class:`dns.transaction.TransactionManager`.

        *rdtype* can be `dns.rdatatype.AXFR` or `dns.rdatatype.IXFR`

        *serial* is the base serial number for IXFRs, and is required in
        that case.

        :param is_udp: Whether UDP is being used for this XFR.
        :type is_udp: bool

        :param transaction_setup: If not ``None``, call the object's setup()
            method with the just-created writer transaction.  This lets the caller
            alter the transaction's configuration before it is used, for example adding
            checking policies.
        :type transaction_setup: None or dns.transaction.TransactionSetup

        :param raise_on_delete_missing: If ``True`` (the default), an IXFR
            deletion set that removes an RR which is not present in the zone
            raises :class:`dns.xfr.IXFRDeleteNotExact` instead of silently
            ignoring the missing data.  Set to ``False`` to treat such deletes
            as permissive no-ops.  This only affects genuine IXFR difference
            sequences; an AXFR (including an AXFR returned in answer to an
            IXFR query) replaces the whole zone and never deletes individual
            RRs.
        :type raise_on_delete_missing: bool

        :param raise_on_duplicate_add: If ``True`` (the default), an IXFR
            addition set that adds an RR which is already present in the zone
            raises :class:`dns.xfr.IXFRDuplicateAdd`.  Set to ``False`` to
            keep the traditional set-semantics behavior where the duplicate is
            merged silently.
        :type raise_on_duplicate_add: bool
        """
        self.txn_manager = txn_manager
        self.txn: dns.transaction.Transaction | None = None
        self.rdtype = rdtype
        if rdtype == dns.rdatatype.IXFR:
            if serial is None:
                raise ValueError("a starting serial must be supplied for IXFRs")
            self.incremental = True
        elif rdtype == dns.rdatatype.AXFR:
            if is_udp:
                raise ValueError("is_udp specified for AXFR")
            self.incremental = False
        else:
            raise ValueError("rdtype is not IXFR or AXFR")
        self.serial = serial
        self.is_udp = is_udp
        _, _, origin = txn_manager.origin_information()
        if origin is None:
            raise ValueError("transaction manager must supply an origin for XFRs")
        self.origin = origin
        self.soa_rdataset: dns.rdataset.Rdataset | None = None
        self.done = False
        self.expecting_SOA = False
        self.delete_mode = False
        # Whether the transfer is currently consuming an RFC 1995 incremental
        # difference sequence.  It flips to False exactly once, when (and if)
        # the server answers an IXFR query with an AXFR-style full zone.
        self.mode = _Mode.IXFR if self.incremental else _Mode.AXFR
        self.raise_on_serial_went_backwards = raise_on_serial_went_backwards
        self.raise_on_delete_missing = raise_on_delete_missing
        self.raise_on_duplicate_add = raise_on_duplicate_add
        self.transaction_setup = transaction_setup
        # Diagnostics counters; they survive commit and make malformed but
        # tolerated transfers observable when the corresponding raise flags
        # are disabled.
        self.missing_deletes = 0
        self.duplicate_adds = 0

    def _check_tsig(self, message: dns.message.Message) -> None:
        # RFC 8945 section 5.3.1 lets the messages in the middle of a signed
        # transfer be unsigned, as the next TSIG covers them, but the first and
        # the last message must have a TSIG.  This has to be checked before the
        # transaction is committed, or unauthenticated data ends up in the zone.
        if message.request_mac and not message.had_tsig:
            raise dns.exception.FormError("missing TSIG")

    def _start_writer(self, replacement: bool) -> dns.transaction.Transaction:
        txn = self.txn_manager.writer(replacement)
        if self.transaction_setup is not None:
            self.transaction_setup(txn)
        return txn

    def _switch_to_axfr(self) -> None:
        # An IXFR query was answered with an AXFR-style full zone (RFC 1995
        # section 2: the first record after the leading SOA is not an SOA).
        # Nothing from the incremental phase may leak into the full zone: the
        # incremental writer is discarded before any of its changes could be
        # committed, and a fresh replacement writer owns the whole transfer.
        # The switch happens at most once, and never again once in AXFR mode.
        assert self.txn is not None
        self.txn.rollback()
        self.txn = self._start_writer(True)
        self.incremental = False
        self.mode = _Mode.AXFR_FALLBACK
        self.expecting_SOA = False
        self.delete_mode = False

    def _serial_value(self, rdataset: dns.rdataset.Rdataset) -> int:
        soa = cast(dns.rdtypes.ANY.SOA.SOA, rdataset[0])
        return soa.serial

    def _check_ixfr_soa(
        self, rdataset: dns.rdataset.Rdataset, entering_delete_mode: bool
    ) -> None:
        """Validate the serial continuity of a non-final origin SOA in an IXFR.

        *entering_delete_mode* is ``True`` when this SOA opens a deletion set
        (the zone serial being removed) and ``False`` when it opens an
        addition set (the zone serial being introduced).
        """
        # This helper only runs inside a genuine IXFR difference sequence, for
        # which a base serial is mandatory.
        assert self.serial is not None
        base_serial = self.serial
        serial = self._serial_value(rdataset)
        if entering_delete_mode:
            # RFC 1995: a deletion set starts with the SOA of the version the
            # receiver is expected to already have, so its serial must be the
            # one we have built up to so far (the requested base for the first
            # difference, the previous addition serial thereafter).
            if serial != base_serial:
                raise IXFRSerialError(
                    f"IXFR base serial mismatch: expected {base_serial}, "
                    f"got {serial}"
                )
        else:
            # The addition set must move the serial strictly forward in
            # RFC 1982 serial arithmetic.  Equality means an empty/no-op
            # difference and a backwards value means a corrupt stream; both
            # break the required serial chain.
            if not (dns.serial.Serial(serial) > dns.serial.Serial(base_serial)):
                raise IXFRSerialError(
                    f"IXFR addition serial {serial} did not advance from "
                    f"{base_serial}"
                )

    def _handle_origin_soa(
        self,
        name: dns.name.Name,
        rdataset: dns.rdataset.Rdataset,
        message: dns.message.Message,
    ) -> None:
        assert self.txn is not None
        if self.incremental:
            # Every origin SOA in an IXFR alternates between opening a
            # deletion set and an addition set.
            self.delete_mode = not self.delete_mode
        #
        # If this SOA rdataset is equal to the first one we saw then the
        # transfer is finished.  In an IXFR the closing SOA is always the one
        # that opens a deletion set, so we additionally require delete mode.
        #
        if rdataset == self.soa_rdataset and (
            (not self.incremental) or self.delete_mode
        ):
            # This is the final SOA.
            if self.expecting_SOA:
                # We got an empty IXFR sequence!
                raise IXFRFormError("empty IXFR sequence")
            if self.incremental:
                # The closing SOA must carry exactly the serial the addition
                # sets have built up to; otherwise the differences do not end
                # at the advertised newest serial.
                if self.serial != self._serial_value(rdataset):
                    raise IXFRSerialError("unexpected end of IXFR sequence")
            # The last message must be TSIG-signed when the request was; this
            # has to happen before the commit below.  The check is driven by
            # the final SOA rrset, not a message boundary, so it holds however
            # the response is split into messages.
            self._check_tsig(message)
            self.txn.replace(name, rdataset)
            self.txn.commit()
            self.txn = None
            self.done = True
            return
        #
        # This is not the final SOA.
        #
        self.expecting_SOA = False
        if self.incremental:
            entering_delete_mode = self.delete_mode
            self._check_ixfr_soa(rdataset, entering_delete_mode)
            if not entering_delete_mode:
                # This is the start of an IXFR addition set.  Track the new
                # serial and install the new SOA as zone content.
                self.serial = self._serial_value(rdataset)
                self.txn.replace(name, rdataset)
        else:
            # We saw a non-final SOA for the origin in an AXFR.
            raise dns.exception.FormError("unexpected origin SOA in AXFR")

    def _rrset_exists(
        self, name: dns.name.Name, rdataset: dns.rdataset.Rdataset
    ) -> bool:
        assert self.txn is not None
        existing = self.txn.get(name, rdataset.rdtype, rdataset.covers)
        if existing is None:
            return False
        return bool(existing.intersection(rdataset))

    def _handle_rrset(
        self, name: dns.name.Name, rdataset: dns.rdataset.Rdataset
    ) -> None:
        assert self.txn is not None
        if self.expecting_SOA:
            #
            # We made an IXFR request and were expecting the second SOA RR,
            # but saw something else, so this is an AXFR-style response.  The
            # (still empty) incremental transaction is discarded and a
            # replacement writer takes over before this rrset is written, so
            # no incremental delete/add is ever applied to the full zone.
            #
            self._switch_to_axfr()
        # Ignore glue that is not a subdomain of the origin.  For pathological
        # cases it would be good if we could keep it in some side location, but
        # dnspython zones don't have a place or an API for that, so we just ignore
        # it at this time.
        if not name.is_subdomain(self.origin):
            return
        exists = self._rrset_exists(name, rdataset)
        if self.delete_mode:
            # RFC 1995 deletion sets only remove records that are present.
            if not exists:
                if self.raise_on_delete_missing:
                    raise IXFRDeleteNotExact(
                        f"IXFR deletion set removes a missing RR: {name} "
                        f"{dns.rdatatype.to_text(rdataset.rdtype)}"
                    )
                self.missing_deletes += 1
            # delete() is permissive when an RR is absent; delete_exact()
            # would raise the transaction layer's own DeleteNotExact.
            if self.raise_on_delete_missing:
                self.txn.delete_exact(name, rdataset)
            else:
                self.txn.delete(name, rdataset)
        else:
            # Duplicate additions are set-semantics no-ops at the transaction
            # layer, but a well-formed IXFR does not repeat records, so make
            # the condition diagnosable when requested.
            if self.incremental and exists:
                if self.raise_on_duplicate_add:
                    raise IXFRDuplicateAdd(
                        f"IXFR addition set adds an existing RR: {name} "
                        f"{dns.rdatatype.to_text(rdataset.rdtype)}"
                    )
                self.duplicate_adds += 1
            self.txn.add(name, rdataset)

    def process_message(self, message: dns.message.Message) -> bool:
        """Process one message in the transfer.

        The message should have the same relativization as was specified when
        the `dns.xfr.Inbound` was created.  The message should also have been
        created with `one_rr_per_rrset=True` because order matters.

        The state machine is driven purely by the ordered RRsets across all
        messages, never by message boundaries, so splitting or joining the
        response into different numbers of messages yields the same final
        zone state.

        If the request was TSIG-signed, i.e. the message was parsed with a
        nonempty *request_mac*, then the first and the last message of the
        transfer must have a TSIG, and nothing is committed otherwise.

        Returns `True` if the transfer is complete, and `False` otherwise.
        """
        if self.txn is None:
            # An IXFR starts with an incremental writer; an AXFR starts with a
            # replacement writer.  If an IXFR turns out to be an AXFR-style
            # response, the transaction is swapped once in _switch_to_axfr().
            self.txn = self._start_writer(not self.incremental)
        rcode = message.rcode()
        if rcode != dns.rcode.NOERROR:
            raise TransferError(rcode)
        #
        # We don't require a question section, but if it is present is
        # should be correct.
        #
        if len(message.question) > 0:
            if message.question[0].name != self.origin:
                raise dns.exception.FormError("wrong question name")
            if message.question[0].rdtype != self.rdtype:
                raise dns.exception.FormError("wrong question rdatatype")
        answer_index = 0
        if self.soa_rdataset is None:
            #
            # This is the first message.  We're expecting an SOA at
            # the origin.
            #
            self._check_tsig(message)
            if not message.answer or message.answer[0].name != self.origin:
                raise dns.exception.FormError("No answer or RRset not for zone origin")
            rrset = message.answer[0]
            rdataset = rrset
            if rdataset.rdtype != dns.rdatatype.SOA:
                raise dns.exception.FormError("first RRset is not an SOA")
            answer_index = 1
            self.soa_rdataset = rdataset.copy()  # pyright: ignore
            if self.incremental:
                assert self.soa_rdataset is not None
                soa = cast(dns.rdtypes.ANY.SOA.SOA, self.soa_rdataset[0])
                if soa.serial == self.serial:
                    #
                    # We're already up-to-date.
                    #
                    self.done = True
                elif dns.serial.Serial(soa.serial) < self.serial:
                    # It went backwards!
                    if self.raise_on_serial_went_backwards:
                        raise SerialWentBackwards
                    self.done = True
                else:
                    if self.is_udp and len(message.answer[answer_index:]) == 0:
                        #
                        # There are no more records, so this is the
                        # "truncated" response.  Say to use TCP
                        #
                        raise UseTCP
                    #
                    # Note we're expecting another SOA so we can detect
                    # if this IXFR response is an AXFR-style response.
                    #
                    self.expecting_SOA = True
        #
        # Process the answer section (other than the initial SOA in
        # the first message).
        #
        for rrset in message.answer[answer_index:]:
            name = rrset.name
            rdataset = rrset
            if self.done:
                raise dns.exception.FormError("answers after final SOA")
            if rdataset.rdtype == dns.rdatatype.SOA and name == self.origin:
                self._handle_origin_soa(name, rdataset, message)
            else:
                self._handle_rrset(name, rdataset)
        if self.is_udp and not self.done:
            #
            # This is a UDP IXFR and we didn't get to done, and we didn't
            # get the proper "truncated" response
            #
            raise dns.exception.FormError("unexpected end of UDP IXFR")
        return self.done

    #
    # Inbounds are context managers.
    #

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        if self.txn:
            self.txn.rollback()
        return False


def make_query(
    txn_manager: dns.transaction.TransactionManager,
    serial: int | None = 0,
    use_edns: int | bool | None = None,
    ednsflags: int | None = None,
    payload: int | None = None,
    request_payload: int | None = None,
    options: list[dns.edns.Option] | None = None,
    keyring: Any = None,
    keyname: dns.name.Name | None = None,
    keyalgorithm: dns.name.Name | str = dns.tsig.default_algorithm,
) -> tuple[dns.message.QueryMessage, int | None]:
    """Make an AXFR or IXFR query.

    :param txn_manager: The transaction manager for this transfer, typically
        a :py:class:`dns.zone.Zone`.
    :type txn_manager: :py:class:`dns.transaction.TransactionManager`
    :param serial: If ``0``, IXFR is attempted using the most recent serial
        from *txn_manager* (caller must ensure no write transactions are
        active that could invalidate the retrieved serial).  If a serial
        cannot be determined, AXFR is forced.  Other integer values are the
        starting serial to use.  ``None`` forces an AXFR.
    :type serial: int or ``None``

    Please see the documentation for :py:func:`dns.message.make_query` and
    :py:func:`dns.message.Message.use_tsig` for details on the other parameters
    to this function.

    :returns: A ``(query, serial)`` tuple.
    :rtype: tuple
    """
    zone_origin, _, origin = txn_manager.origin_information()
    if zone_origin is None:
        raise ValueError("no zone origin")
    if serial is None:
        rdtype = dns.rdatatype.AXFR
    elif not isinstance(serial, int):
        raise ValueError("serial is not an integer")
    elif serial == 0:
        with txn_manager.reader() as txn:
            rdataset = txn.get(origin, "SOA")
            if rdataset:
                soa = cast(dns.rdtypes.ANY.SOA.SOA, rdataset[0])
                serial = soa.serial
                rdtype = dns.rdatatype.IXFR
            else:
                serial = None
                rdtype = dns.rdatatype.AXFR
    elif serial > 0 and serial < 4294967296:
        rdtype = dns.rdatatype.IXFR
    else:
        raise ValueError("serial out-of-range")
    rdclass = txn_manager.get_class()
    q = dns.message.make_query(
        zone_origin,
        rdtype,
        rdclass,
        use_edns,
        False,
        ednsflags,
        payload,
        request_payload,
        options,
    )
    if serial is not None:
        rdata = dns.rdata.from_text(rdclass, "SOA", f". . {serial} 0 0 0 0")
        rrset = q.find_rrset(
            q.authority, zone_origin, rdclass, dns.rdatatype.SOA, create=True
        )
        rrset.add(rdata, 0)
    if keyring is not None:
        q.use_tsig(keyring, keyname, algorithm=keyalgorithm)
    return (q, serial)


def extract_serial_from_query(query: dns.message.Message) -> int | None:
    """Extract the SOA serial number from query if it is an IXFR and return
    it, otherwise return None.

    *query* is a dns.message.QueryMessage that is an IXFR or AXFR request.

    Raises if the query is not an IXFR or AXFR, or if an IXFR doesn't have
    an appropriate SOA RRset in the authority section.
    """
    if not isinstance(query, dns.message.QueryMessage):
        raise ValueError("query not a QueryMessage")
    question = query.question[0]
    if question.rdtype == dns.rdatatype.AXFR:
        return None
    elif question.rdtype != dns.rdatatype.IXFR:
        raise ValueError("query is not an AXFR or IXFR")
    soa_rrset = query.find_rrset(
        query.authority, question.name, question.rdclass, dns.rdatatype.SOA
    )
    soa = cast(dns.rdtypes.ANY.SOA.SOA, soa_rrset[0])
    return soa.serial
