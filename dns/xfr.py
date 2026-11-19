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
import dns.rdataclass
import dns.rdataset
import dns.rdatatype
import dns.rdtypes
import dns.rdtypes.ANY
import dns.rdtypes.ANY.NS
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


class TransferFormError(dns.exception.FormError):
    """A zone transfer response was structurally invalid.

    This is a :py:exc:`dns.exception.FormError`, so code which catches that
    keeps working.  When it (or one of its subclasses) is raised nothing has
    been committed, and the transaction used by the transfer is rolled back,
    so the zone keeps the content it had before the transfer started.
    """


class ZoneIdentityError(TransferFormError):
    """A zone transfer response did not describe the zone being transferred.

    The response's question section, owner names, or record class did not
    match the zone which the :py:class:`Inbound` was created for.
    """


class OutOfZoneName(ZoneIdentityError):
    """An owner name in a zone transfer response is not in the zone.

    RFC 5936 section 2.2.2 says an AXFR response contains the zone's data, so
    a name which is not at or below the zone origin means the response does
    not belong to this zone.  (An :py:class:`Inbound` created with
    *allow_out_of_zone* set to ``True`` ignores such records instead.)
    """


class ClassMismatch(ZoneIdentityError):
    """A record in a zone transfer response has the wrong class."""


class DuplicateEnd(TransferFormError):
    """A zone transfer response ended more than once.

    This is raised when a message arrives after the closing SOA, or when data
    is found in an answer section after the closing SOA.
    """


class TransferAborted(dns.exception.DNSException):
    """The zone transfer has already ended, one way or another.

    A failed :py:class:`Inbound` cannot be fed more messages.  Create a new
    :py:class:`Inbound` for the same transaction manager to start a new
    transfer; the failed one leaves no partial data behind.
    """


class InboundPhase(enum.Enum):
    """The phases of an inbound zone transfer.

    The phases of a transfer proceed in order::

        START_SOA -> ZONE_IDENTITY -> DATA -> END_SOA -> DONE

    * ``START_SOA``: the leading SOA of the transfer has not been seen yet.
    * ``ZONE_IDENTITY``: the leading SOA has been accepted, and the transfer's
      origin and class are known, but no zone data has arrived yet.
    * ``DATA``: zone data is being received into the pending transaction.
    * ``END_SOA``: the trailing SOA has been matched against the leading SOA,
      after which the transaction is committed.
    * ``DONE``: the transfer closed and, if there was anything to write, the
      new zone version was committed.
    * ``FAILED``: the transfer was rejected or abandoned.  Nothing was
      committed, and the :py:class:`Inbound` may no longer be used.

    The phase is available as :py:attr:`Inbound.phase`, and the sequence of
    phases entered is available as :py:attr:`Inbound.history`, so a caller can
    tell an incomplete transfer from a complete one, and a failed transfer
    from a transfer which is merely still in progress, without inspecting the
    zone.
    """

    START_SOA = enum.auto()
    ZONE_IDENTITY = enum.auto()
    DATA = enum.auto()
    END_SOA = enum.auto()
    DONE = enum.auto()
    FAILED = enum.auto()


class Inbound:
    """
    State machine for zone transfers.

    A transfer is fed one :py:class:`dns.message.Message` at a time with
    :py:meth:`process_message`, and it is not complete until the closing SOA
    of a full AXFR has been matched against the opening SOA.  Until then the
    new zone content lives only in a writer transaction, and the zone's
    readers keep seeing the previous version.

    The transfer's progress is visible as :py:attr:`phase`, the current
    :py:class:`InboundPhase`, and as :py:attr:`history`, the list of phases
    entered in order.  The :py:attr:`complete`, :py:attr:`failed`, and
    :py:attr:`committed` properties summarize the outcome.  A transfer which
    raises an exception is marked :py:attr:`InboundPhase.FAILED` and its
    transaction is rolled back before the exception leaves
    :py:meth:`process_message`, so a failure never leaves a partly replaced
    zone.  A failed :py:class:`Inbound` cannot be reused; create a new one
    with the same transaction manager to try again.

    ``serial_number`` is the SOA serial of the closed transfer (or ``None``
    while it is open), ``rrset_count`` is the number of record sets accepted
    so far (including the opening and closing SOA), and ``message_count`` is
    the number of messages processed.
    """

    def __init__(
        self,
        txn_manager: dns.transaction.TransactionManager,
        rdtype: dns.rdatatype.RdataType = dns.rdatatype.AXFR,
        serial: int | None = None,
        is_udp: bool = False,
        raise_on_serial_went_backwards: bool = True,
        transaction_setup: Callable[[dns.transaction.Transaction], None] | None = None,
        allow_out_of_zone: bool = False,
        raise_on_duplicate_end: bool = True,
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

        :param allow_out_of_zone: If ``True``, records whose owner name is not
            at or below the zone origin are ignored instead of raising
            :py:exc:`dns.xfr.OutOfZoneName`.  The default is ``False``, as
            RFC 5936 section 2.2.2 says an AXFR response contains only the
            zone's own data.
        :type allow_out_of_zone: bool

        :param raise_on_duplicate_end: If ``True`` (the default), data
            arriving after the closing SOA raises
            :py:exc:`dns.xfr.DuplicateEnd`.  If ``False``, such data is
            ignored, which is useful for transports which cannot stop reading
            promptly.
        :type raise_on_duplicate_end: bool
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
        # Names of out-of-zone glue targets seen so far, which are ignored
        # rather than rejected if the server includes their glue records.
        self._glue_names: set[dns.name.Name] = set()
        # Owner names of out-of-zone records, which are checked for being
        # glue at the end of the transfer.
        self._out_of_zone_seen: list[dns.name.Name] = []
        try:
            self.rdclass: dns.rdataclass.RdataClass | None = txn_manager.get_class()
        except NotImplementedError:
            # Some transaction managers (e.g. the one used by dns.query.xfr()
            # when it only generates messages) have no class; in that case the
            # class check is skipped.
            self.rdclass = None
        self.soa_rdataset: dns.rdataset.Rdataset | None = None
        self.done = False
        self.expecting_SOA = False
        self.delete_mode = False
        self.raise_on_serial_went_backwards = raise_on_serial_went_backwards
        self.transaction_setup = transaction_setup
        self.allow_out_of_zone = allow_out_of_zone
        self.raise_on_duplicate_end = raise_on_duplicate_end
        self._phase = InboundPhase.START_SOA
        # The phases which have been entered, in order.
        self.history: list[InboundPhase] = [InboundPhase.START_SOA]
        # The number of record sets accepted into the transaction so far
        # (including the opening and closing SOA), and the number of messages
        # which have been processed.
        self.rrset_count = 0
        self.message_count = 0
        # The SOA serial number of the version the transfer produced, once it
        # has closed.
        self.serial_number: int | None = None
        # Set when the closing SOA has been seen but the message containing it
        # has not finished, so that anything else in that message is reported
        # as a duplicate end rather than being applied.
        self._saw_final_soa = False
        self._final_soa_name: dns.name.Name = origin
        self._final_soa_rdataset: dns.rdataset.Rdataset | None = None
        self._committed = False

    @property
    def phase(self) -> InboundPhase:
        """The phase the transfer is in.  See :py:class:`InboundPhase`."""
        return self._phase

    def _set_phase(self, phase: InboundPhase) -> None:
        if phase is self._phase:
            return
        self._phase = phase
        self.history.append(phase)

    @property
    def complete(self) -> bool:
        """Is the transfer complete?

        A transfer is complete only when it has closed properly, i.e. when
        :py:attr:`phase` is :py:attr:`InboundPhase.DONE` and the trailing SOA
        matched the opening SOA.  A failed transfer is not complete.
        """
        return self.phase == InboundPhase.DONE

    @property
    def failed(self) -> bool:
        """Did the transfer fail?

        This is ``True`` if :py:attr:`phase` is
        :py:attr:`InboundPhase.FAILED`, in which case nothing was committed
        and the :py:class:`Inbound` can no longer process messages.
        """
        return self.phase == InboundPhase.FAILED

    @property
    def committed(self) -> bool:
        """Was a new zone version committed by this transfer?"""
        return self._committed

    def _abort(self) -> None:
        """Roll back the pending transaction and mark the transfer failed."""
        if self.phase == InboundPhase.FAILED:
            return
        self._set_phase(InboundPhase.FAILED)
        if self.txn is not None:
            txn, self.txn = self.txn, None
            try:
                txn.rollback()
            except Exception:  # pragma: no cover
                # Rollback is documented as not failing, and if it somehow
                # does we must still let the original error propagate.
                pass

    def _start_writer(
        self, replacement: bool | None = None
    ) -> dns.transaction.Transaction:
        if replacement is None:
            replacement = not self.incremental
        txn = self.txn_manager.writer(replacement)
        if self.transaction_setup is not None:
            try:
                self.transaction_setup(txn)
            except Exception:
                # Don't leak the writer if the caller's setup rejects it.
                txn.rollback()
                raise
        return txn

    def _check_tsig(self, message: dns.message.Message) -> None:
        # RFC 8945 section 5.3.1 lets the messages in the middle of a signed
        # transfer be unsigned, as the next TSIG covers them, but the first and
        # the last message must have a TSIG.  This has to be checked before the
        # transaction is committed, or unauthenticated data ends up in the zone.
        if message.request_mac and not message.had_tsig:
            raise dns.exception.FormError("missing TSIG")

    def _check_question(self, message: dns.message.Message) -> None:
        # We don't require a question section, but if it is present it should
        # be for the zone and transfer type being received.  RFC 5936 section
        # 2.2.1 requires the first message to repeat the question, and allows
        # later messages to have an empty one, but a later message which does
        # have a question must still describe the zone being transferred.
        if len(message.question) == 0:
            return
        question = message.question[0]
        if question.name != self.origin:
            raise ZoneIdentityError("wrong question name")
        if question.rdtype != self.rdtype:
            raise ZoneIdentityError("wrong question rdatatype")
        if self.rdclass is not None and question.rdclass != self.rdclass:
            raise ClassMismatch(
                f"question class {dns.rdataclass.to_text(question.rdclass)} is "
                f"not the zone's class {dns.rdataclass.to_text(self.rdclass)}"
            )

    def _check_class(self, rrset: dns.rdataset.Rdataset) -> None:
        """Check that an rdataset is of the zone's class."""
        if self.rdclass is None:
            # The transaction manager did not say what class it holds, so
            # there is nothing to compare against.
            return
        if rrset.rdclass != self.rdclass:
            raise ClassMismatch(
                f"record class {dns.rdataclass.to_text(rrset.rdclass)} is not "
                f"the zone's class {dns.rdataclass.to_text(self.rdclass)}"
            )

    def _rrset_in_zone(self, rrset: dns.rrset.RRset) -> bool:
        """Is *rrset*'s owner name at or below the zone origin?"""
        # With relativization, names in the zone are relative to the effective
        # origin (the empty name) and out-of-zone names stay absolute, so this
        # single test covers both relativized and non-relativized transfers.
        return rrset.name.is_subdomain(self.origin)

    def _accept_rrset(self, rrset: dns.rrset.RRset) -> bool:
        """Should a data record be stored?

        The class has already been checked by the caller.

        A record outside the zone is never stored, as it cannot be part of the
        zone.  Whether it is an error is decided at the end of the transfer
        (:py:meth:`_check_out_of_zone`), because whether an out-of-zone record
        is undelegated glue does not depend on the order in which the records
        arrive.

        :returns: ``True`` if the record is to be stored, and ``False`` if it
            is out of the zone and must be ignored.
        """
        if self._rrset_in_zone(rrset):
            return True
        if not self.allow_out_of_zone:
            self._out_of_zone_seen.append(rrset.name)
        return False

    def _remember_glue(self, rrset: dns.rrset.RRset) -> None:
        """Note the out-of-zone names which are glue for an in-zone NS.

        A record for such a name, if the server includes it in the transfer,
        is glue for a delegation in this zone and not an attempt to put
        another zone's data in this one.  The name test is the same one used
        for ordinary records, so it works for both relativized and
        non-relativized transfers.
        """
        if rrset.rdtype != dns.rdatatype.NS or not self._rrset_in_zone(rrset):
            return
        for rdata in rrset:
            target = cast(dns.rdtypes.ANY.NS.NS, rdata).target
            if not target.is_subdomain(self.origin):
                self._glue_names.add(target)

    def _scan_for_glue(self, rrsets) -> None:
        """Find the out-of-zone glue targets of the rrsets in one message.

        Glue normally follows the delegation which needs it, but nothing in
        RFC 5936 requires that, so the whole message is scanned before any of
        it is processed.
        """
        for rrset in rrsets:
            self._remember_glue(rrset)

    def _check_out_of_zone(self) -> None:
        """Reject out-of-zone records which are not undelegated glue.

        This runs just before the commit, when every delegation in the
        transfer has been seen, so a record for a name served by an in-zone
        delegation's NS target is recognized as glue regardless of where it
        appeared in the response.
        """
        for name in self._out_of_zone_seen:
            if name in self._glue_names:
                # Out-of-zone glue for a delegation in this zone.  Zones have
                # no place to keep it, and RFC 5936 section 2.2.2 permits a
                # server to include it, so it is ignored rather than treated
                # as data for some other zone.
                continue
            raise OutOfZoneName(f"{name} is not in zone {self.origin}")

    def _start_soa(self, message: dns.message.Message) -> int:
        """Process the opening SOA of the transfer, returning the index of
        the first answer RRset after it."""
        #
        # This is the first message.  We're expecting an SOA at the origin.
        #
        self._check_tsig(message)
        if not message.answer:
            raise TransferFormError("no answer in first message")
        rrset = message.answer[0]
        if rrset.name != self.origin:
            # The leading RRset must be the SOA at the origin; anything else
            # means the response is for some other zone or is not a transfer.
            raise ZoneIdentityError("first RRset is not at the zone origin")
        if rrset.rdtype != dns.rdatatype.SOA:
            raise TransferFormError("first RRset is not an SOA")
        self._check_class(rrset)
        self.soa_rdataset = rrset.copy()  # pyright: ignore
        self.rrset_count = 1
        # The opening SOA established the zone the response is for.  Reaching
        # this phase without an error means the response's origin and class
        # matched ours.
        self._set_phase(InboundPhase.ZONE_IDENTITY)
        if self.incremental:
            assert self.soa_rdataset is not None
            soa = cast(dns.rdtypes.ANY.SOA.SOA, self.soa_rdataset[0])
            if soa.serial == self.serial:
                #
                # We're already up-to-date.
                #
                self.done = True
                self._set_phase(InboundPhase.DONE)
                self.serial_number = soa.serial
            elif dns.serial.Serial(soa.serial) < self.serial:
                # It went backwards!
                if self.raise_on_serial_went_backwards:
                    raise SerialWentBackwards
                self.done = True
                self._set_phase(InboundPhase.DONE)
                self.serial_number = soa.serial
            else:
                if self.is_udp and len(message.answer) == 1:
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
        return 1

    def _start_writer_if_needed(self) -> None:
        if self.txn is None and not self.done:
            self.txn = self._start_writer()

    def _handle_rrset(self, rrset: dns.rrset.RRset) -> None:
        """Handle one RRset of the answer section after the opening SOA.

        The closing SOA ends the data phase, but the transaction is not
        committed here: the caller commits once the whole message has been
        consumed, so data following the closing SOA in the same message is
        detected (as a duplicated end) before anything is committed.
        """
        name = rrset.name
        rdataset = rrset
        if self.done or self._saw_final_soa:
            #
            # The transfer has already ended (either because an IXFR turned
            # out to be up to date, or because the closing SOA was seen), so
            # anything else means it ended more than once.  This has to be
            # detected before the transaction is committed, or the zone would
            # be replaced by a version missing the records after the end.
            #
            if self.raise_on_duplicate_end:
                raise DuplicateEnd("answers after final SOA")
            return
        assert self.txn is not None  # for mypy
        # The class is checked for every record, including the closing SOA and
        # records which are otherwise ignored, so that a response mixing
        # classes is reported as such rather than silently partially applied.
        self._check_class(rdataset)
        if rdataset.rdtype == dns.rdatatype.SOA and name == self.origin:
            #
            # Every time we see an origin SOA delete_mode inverts
            #
            if self.incremental:
                self.delete_mode = not self.delete_mode
            #
            # If this SOA Rdataset is equal to the first we saw
            # then we're finished. If this is an IXFR we also
            # check that we're seeing the record in the expected
            # part of the response.
            #
            if rdataset == self.soa_rdataset and (
                (not self.incremental) or self.delete_mode
            ):
                #
                # This is the final SOA.
                #
                soa = cast(dns.rdtypes.ANY.SOA.SOA, rdataset[0])
                if self.expecting_SOA:
                    # We got an empty IXFR sequence!
                    raise TransferFormError("empty IXFR sequence")
                if self.incremental and self.serial != soa.serial:
                    raise TransferFormError("unexpected end of IXFR sequence")
                self._saw_final_soa = True
                self._final_soa_name = name
                self._final_soa_rdataset = rdataset
                self.serial_number = soa.serial
                self.rrset_count += 1
                self._set_phase(InboundPhase.END_SOA)
            else:
                #
                # This is not the final SOA
                #
                self.expecting_SOA = False
                soa = cast(dns.rdtypes.ANY.SOA.SOA, rdataset[0])
                if self.incremental:
                    if self.delete_mode:
                        # This is the start of an IXFR deletion set
                        if soa.serial != self.serial:
                            raise TransferFormError("IXFR base serial mismatch")
                    else:
                        # This is the start of an IXFR addition set
                        self.serial = soa.serial
                        self.txn.replace(name, rdataset)
                else:
                    # We saw a non-final SOA for the origin in an AXFR.
                    raise TransferFormError("unexpected origin SOA in AXFR")
            return
        if rdataset.rdtype == dns.rdatatype.SOA:
            # An SOA anywhere but at the origin cannot be zone content.
            raise TransferFormError("non-origin SOA in zone transfer")
        if self.expecting_SOA:
            #
            # We made an IXFR request and were expecting the second SOA RR,
            # but saw something else, so this must be an AXFR response.
            #
            self.incremental = False
            self.expecting_SOA = False
            self.delete_mode = False
            self.txn.rollback()
            self.txn = self._start_writer(replacement=True)
            # Nothing from the incremental phase is applied to the full zone,
            # so the identity bookkeeping for it is discarded too.  Only the
            # opening SOA remains relevant.
            self.rrset_count = 1
            self._out_of_zone_seen.clear()
            self._glue_names.clear()
            #
            # Note we are falling through into the code below
            # so whatever rdataset this was gets written.
            #
        if not self._accept_rrset(rrset):
            # The record is out of the zone, so it is not stored.  Whether it
            # should have been rejected outright is decided at the end of the
            # transfer, when all the delegations are known.
            return
        # Add or remove the data
        if self.delete_mode:
            self.txn.delete_exact(name, rdataset)
        else:
            self.txn.add(name, rdataset)
        self.rrset_count += 1
        if self.phase == InboundPhase.ZONE_IDENTITY:
            self._set_phase(InboundPhase.DATA)

    def _commit(self, message: dns.message.Message) -> None:
        """Commit the transaction, closing the transfer."""
        assert self.txn is not None
        assert self._final_soa_rdataset is not None
        self._check_out_of_zone()
        # RFC 8945 section 5.3.1: the last message of a signed transfer must be
        # signed.  This is checked before the commit, so unauthenticated data
        # never ends up in the zone.
        self._check_tsig(message)
        self.txn.replace(self._final_soa_name, self._final_soa_rdataset)
        self.txn.commit()
        self.txn = None
        self.done = True
        self._committed = True
        self._set_phase(InboundPhase.DONE)

    def process_message(self, message: dns.message.Message) -> bool:
        """Process one message in the transfer.

        The message should have the same relativization as was specified when
        the `dns.xfr.Inbound` was created.  The message should also have been
        created with `one_rr_per_rrset=True` because order matters.

        If the request is TSIG-signed, i.e. the message was parsed with a
        nonempty *request_mac*, then the first and the last message of the
        transfer must have a TSIG, and nothing is committed otherwise.

        The message is checked against the zone and the phase the transfer is
        in.  A record outside the zone, a record of the wrong class, an extra
        answer section, or a message after the closing SOA is rejected, and
        the pending transaction is rolled back.  A rejection marks the
        transfer :py:attr:`InboundPhase.FAILED`; the same :py:class:`Inbound`
        cannot be fed more messages, but a new one created for the same
        transaction manager starts a new, independent transfer.

        If the transfer has already closed, a further message is rejected
        with :py:exc:`dns.xfr.DuplicateEnd` (or ignored, if
        *raise_on_duplicate_end* is ``False``).

        :raises dns.xfr.TransferError: if the message's rcode is not NOERROR.
        :raises dns.xfr.ZoneIdentityError: if the response does not describe
            the zone being transferred, including an out-of-zone owner name
            (:py:exc:`dns.xfr.OutOfZoneName`) or a wrong record class
            (:py:exc:`dns.xfr.ClassMismatch`).
        :raises dns.xfr.DuplicateEnd: if the message arrives after the
            closing SOA.
        :raises dns.xfr.TransferAborted: if the transfer already failed.
        :returns: ``True`` if the transfer is complete, and ``False``
            otherwise.
        """
        if self.phase == InboundPhase.FAILED:
            raise TransferAborted("zone transfer has failed")
        if self.done:
            if self.raise_on_duplicate_end:
                raise DuplicateEnd("message after final SOA")
            self.message_count += 1
            return True
        try:
            return self._process_message(message)
        except Exception:
            self._abort()
            raise

    def _process_message(self, message: dns.message.Message) -> bool:
        self.message_count += 1
        rcode = message.rcode()
        if rcode != dns.rcode.NOERROR:
            raise TransferError(rcode)
        self._check_question(message)
        answer_index = 0
        if self.soa_rdataset is None:
            answer_index = self._start_soa(message)
        if not self.done:
            self._start_writer_if_needed()
        #
        # Process the answer section (other than the initial SOA in
        # the first message).
        #
        answer = message.answer[answer_index:]
        self._scan_for_glue(answer)
        for rrset in answer:
            self._handle_rrset(rrset)
        if self._saw_final_soa:
            #
            # The closing SOA matched the opening SOA and nothing followed it,
            # so the new zone version can now be committed.  This happens only
            # after the whole message has been checked, so a duplicate end or
            # trailing junk cannot leave a partially applied version behind.
            #
            self._commit(message)
        if self.is_udp and not self.done:
            #
            # This is a UDP IXFR and we didn't get to done, and we didn't
            # get the proper "truncated" response
            #
            raise TransferFormError("unexpected end of UDP IXFR")
        return self.done

    #
    # Inbounds are context managers.
    #

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        # Leaving the context with the transfer still open abandons it, whether
        # because of an exception anywhere in the stream handling or because
        # the caller simply stopped feeding messages.  Either way the pending
        # transaction is rolled back, so the zone is not partly replaced.
        if self.phase not in (InboundPhase.DONE, InboundPhase.FAILED):
            self._abort()
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
