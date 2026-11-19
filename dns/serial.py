# Copyright (C) Dnspython Contributors, see LICENSE for text of ISC license

"""Serial number arithmetic from RFC 1982, and SOA serial number planning.

:py:class:`Serial` implements the circular whole-number arithmetic of RFC
1982.  :py:func:`compare` and :py:func:`is_update` expose the same ordering
directly, including the "undefined" result for values exactly half the serial
number space apart (RFC 1982 section 3.2).

The :py:class:`SerialPolicy` classes and :py:func:`plan_serial` plan the next
SOA serial for a zone.  Planning is a pure computation and never modifies a
zone.  The resulting :py:class:`SerialPlan` remembers the serial it was made
from; :py:meth:`SerialPlan.apply` writes it to an open transaction only after
checking that the zone's SOA serial is still the one the plan was based on.
"""

import dataclasses
import datetime
from typing import TYPE_CHECKING, cast

import dns.exception
import dns.name
import dns.rdataset
import dns.rdatatype
import dns.rdtypes.ANY.SOA

if TYPE_CHECKING:
    import dns.transaction
    import dns.zone

#: An SOA serial is a 32-bit unsigned integer (RFC 1035 section 3.3.13).
SERIAL_BITS = 32

#: The number of distinct serial numbers.
SERIAL_MODULUS = 2**SERIAL_BITS

#: The largest serial number.
MAX_SERIAL = SERIAL_MODULUS - 1

#: Half the serial number space.  Values exactly this far apart cannot be
#: ordered (RFC 1982 section 3.2).
HALF_SERIAL = 2 ** (SERIAL_BITS - 1)


class SerialError(dns.exception.DNSException):
    """Base class for serial number planning errors."""


class BadSerial(SerialError):
    """A serial number is not a 32-bit unsigned integer."""


class BadSerialPolicy(SerialError):
    """A serial policy is unknown, or its parameters are invalid."""


class SerialNotUpdated(SerialError):
    """A candidate serial would not be seen as an update by a secondary."""


class SerialComparisonUndefined(SerialError):
    """The serial comparison is undefined (RFC 1982 section 3.2).

    This occurs when the two serials are exactly half the serial number space
    apart, so neither may be considered greater than the other.  It is
    distinct from :py:exc:`SerialNotUpdated` because the answer is unknown
    rather than negative.
    """


class SerialExhausted(SerialError):
    """A date-style serial policy has used all of its counters for the day."""


class SerialChanged(SerialError):
    """The zone's SOA serial changed after a plan was made."""


class Serial:
    """A serial number with RFC 1982 circular arithmetic.

    The comparison operators order values using serial number arithmetic
    rather than ordinary integer arithmetic, so e.g. ``Serial(0) >
    Serial(2**32 - 1)`` is true and adding one to ``2**32 - 1`` yields zero.
    Two values exactly half the number space apart compare as neither greater
    nor less than each other; the ordering operators then return ``False``.
    """

    def __init__(self, value: int, bits: int = 32):
        self.value = value % 2**bits
        self.bits = bits

    def __repr__(self):
        return f"dns.serial.Serial({self.value}, {self.bits})"

    def __eq__(self, other):
        if isinstance(other, int):
            other = Serial(other, self.bits)
        elif not isinstance(other, Serial) or other.bits != self.bits:
            return NotImplemented
        return self.value == other.value

    def __hash__(self):
        return hash(self.value)

    def __ne__(self, other):
        if isinstance(other, int):
            other = Serial(other, self.bits)
        elif not isinstance(other, Serial) or other.bits != self.bits:
            return NotImplemented
        return self.value != other.value

    def __lt__(self, other):
        if isinstance(other, int):
            other = Serial(other, self.bits)
        elif not isinstance(other, Serial) or other.bits != self.bits:
            return NotImplemented
        return compare(self.value, other.value, self.bits) == -1

    def __le__(self, other):
        return self == other or self < other

    def __gt__(self, other):
        if isinstance(other, int):
            other = Serial(other, self.bits)
        elif not isinstance(other, Serial) or other.bits != self.bits:
            return NotImplemented
        return compare(self.value, other.value, self.bits) == 1

    def __ge__(self, other):
        return self == other or self > other

    def __add__(self, other):
        v = self.value
        if isinstance(other, Serial):
            delta = other.value
        elif isinstance(other, int):
            delta = other
        else:
            raise ValueError
        if abs(delta) > (2 ** (self.bits - 1) - 1):
            raise ValueError
        v += delta
        v = v % 2**self.bits
        return Serial(v, self.bits)

    def __iadd__(self, other):
        v = self.value
        if isinstance(other, Serial):
            delta = other.value
        elif isinstance(other, int):
            delta = other
        else:
            raise ValueError
        if abs(delta) > (2 ** (self.bits - 1) - 1):
            raise ValueError
        v += delta
        v = v % 2**self.bits
        self.value = v
        return self

    def __sub__(self, other):
        v = self.value
        if isinstance(other, Serial):
            delta = other.value
        elif isinstance(other, int):
            delta = other
        else:
            raise ValueError
        if abs(delta) > (2 ** (self.bits - 1) - 1):
            raise ValueError
        v -= delta
        v = v % 2**self.bits
        return Serial(v, self.bits)

    def __isub__(self, other):
        v = self.value
        if isinstance(other, Serial):
            delta = other.value
        elif isinstance(other, int):
            delta = other
        else:
            raise ValueError
        if abs(delta) > (2 ** (self.bits - 1) - 1):
            raise ValueError
        v -= delta
        v = v % 2**self.bits
        self.value = v
        return self


def compare(a: int, b: int, bits: int = SERIAL_BITS) -> int | None:
    """Compare two serial numbers using RFC 1982 serial arithmetic.

    The serial number space is circular, and only half of it may be used for
    ordering at any one time, so this is *not* an ordinary integer comparison.

    The result describes how the serial number *a* relates to *b*:

    * ``1`` if *a* is greater than *b*,
    * ``0`` if *a* and *b* are equal,
    * ``-1`` if *a* is less than *b*, or
    * ``None`` if the comparison is undefined because the two values are
      exactly half the serial number space apart (RFC 1982 section 3.2).

    :param int a: The first serial number.
    :param int b: The second serial number.
    :param int bits: The size of the serial number space in bits.
    :rtype: int or ``None``
    """
    modulus = 2**bits
    half = 2 ** (bits - 1)
    a %= modulus
    b %= modulus
    if a == b:
        return 0
    difference = (a - b) % modulus
    if difference < half:
        return 1
    if difference > half:
        return -1
    return None


def is_update(candidate: int, current: int, bits: int = SERIAL_BITS) -> bool:
    """Is *candidate* a serial number which updates *current*?

    The answer is ``True`` only if *candidate* is strictly greater than
    *current* in the circular serial number order, i.e. only if a secondary
    would consider the zone to have changed.  A plain integer greater-than
    test is not equivalent and must not be used instead; for example
    ``0`` updates ``2**32 - 1``, while an ordinary integer comparison says
    otherwise.

    :param int candidate: The proposed new serial number.
    :param int current: The present serial number.
    :param int bits: The size of the serial number space in bits.
    :raises SerialComparisonUndefined: If the two values are exactly half the
        serial number space apart, so neither may be considered greater.
    :rtype: bool
    """
    order = compare(candidate, current, bits)
    if order is None:
        raise SerialComparisonUndefined(
            f"serials {candidate} and {current} are half of the {bits}-bit "
            "serial number space apart, so neither is greater"
        )
    return order > 0


def _require_serial(value: int, what: str) -> int:
    # An SOA serial must be a uint32; anything else cannot be stored in the
    # SOA record, and silently reducing it would be a lie.
    if isinstance(value, bool) or not isinstance(value, int):
        raise BadSerial(f"{what} must be an integer, not {value!r}")
    if value < 0 or value > MAX_SERIAL:
        raise BadSerial(
            f"{what} must be between 0 and {MAX_SERIAL} inclusive, not {value}"
        )
    return value


def _require_update(candidate: int, current: int) -> int:
    # Verify with serial arithmetic, and deliberately go through compare() so
    # that a stray ordinary comparison cannot creep in.
    order = compare(candidate, current)
    if order is None:
        raise SerialComparisonUndefined(
            f"serials {candidate} and {current} are half of the 32-bit serial "
            "number space apart, so neither is greater and a secondary could "
            "not tell which is newer"
        )
    if order <= 0:
        raise SerialNotUpdated(
            f"serial {candidate} is not an update of serial {current}; "
            "secondaries would ignore it"
        )
    return candidate


class SerialPolicy:
    """Base class for SOA serial number planning policies.

    A policy maps the current SOA serial number to the next one.  Policies are
    used by :py:func:`plan_serial`, which checks the result with RFC 1982
    serial arithmetic, so a subclass only has to compute a candidate; a
    candidate which a secondary would not see as an update can never escape
    the planning API.
    """

    def next_serial(self, current: int, now: datetime.datetime) -> int:
        """Return the serial number which follows *current*.

        :param int current: The present SOA serial number.
        :param now: The time date-style policies should base their date on.
        :type now: :py:class:`datetime.datetime`
        :rtype: int
        """
        raise NotImplementedError  # pragma: no cover


@dataclasses.dataclass(frozen=True)
class IncrementPolicy(SerialPolicy):
    """A fixed-increment serial policy.

    The next serial is the current serial plus *step*, modulo ``2**32``, so a
    serial of ``4294967295`` is followed by ``0``, which RFC 1982 considers to
    be greater than ``4294967295``.

    :param int step: The increment.  It must be at least 1 and at most
        ``2**31 - 1``.  A larger step would be either unorderable (exactly
        half the number space) or actually backwards in serial number order,
        so it is rejected instead of being allowed to produce a value which
        secondaries ignore.
    """

    step: int = 1

    def __post_init__(self):
        step = self.step
        if isinstance(step, bool) or not isinstance(step, int):
            raise BadSerialPolicy(f"increment step must be an integer, not {step!r}")
        if step < 1 or step > HALF_SERIAL - 1:
            raise BadSerialPolicy(
                f"increment step must be between 1 and {HALF_SERIAL - 1} "
                f"inclusive, not {step}"
            )

    def next_serial(self, current: int, now: datetime.datetime) -> int:
        candidate = (current + self.step) % SERIAL_MODULUS
        return _require_update(candidate, current)


def _utc_date(now: datetime.datetime) -> datetime.date:
    # A naive datetime is taken to be UTC already.
    if now.tzinfo is not None:
        now = now.astimezone(datetime.timezone.utc)
    return now.date()


def _date_serial_base(day: datetime.date) -> int:
    # YYYYMMDDnn with an nn of 00.
    return (day.year * 10000 + day.month * 100 + day.day) * 100


def _serial_date(value: int) -> datetime.date | None:
    # A date-style serial is YYYYMMDDnn, i.e. ten decimal digits.  Anything
    # which is not ten digits, or which does not carry a real date in its
    # first eight, is not a date-style serial.
    if value < 1_000_000_000:
        return None
    date_part, _ = divmod(value, 100)
    year, rest = divmod(date_part, 10000)
    month, day = divmod(rest, 100)
    try:
        return datetime.date(year, month, day)
    except ValueError:
        return None


@dataclasses.dataclass(frozen=True)
class DatePolicy(SerialPolicy):
    """A date-style (``YYYYMMDDnn``) serial policy.

    The next serial is ten decimal digits: the date in UTC followed by a
    two-digit counter, e.g. ``2026111900`` for the first update on
    2026-11-19.  If the current serial already carries today's date, then the
    counter is incremented; otherwise the counter starts at zero.

    A counter of 99 is the last one available for a day, so asking for another
    update on the same day raises :py:exc:`SerialExhausted` rather than
    producing an eleven-digit number which looks like a date-style serial but
    is not one, or a value which is no longer an update.

    If the candidate is not an update of the current serial in the RFC 1982
    circular order, for example because the current serial carries a date in
    the future, or because the current serial is far ahead after having been
    incremented by a fixed policy, planning fails with
    :py:exc:`SerialNotUpdated` rather than emitting a value which secondaries
    would ignore.
    """

    def next_serial(self, current: int, now: datetime.datetime) -> int:
        today = _utc_date(now)
        # The serial has to be exactly YYYYMMDDnn, so the year must have four
        # digits (a clock set before 1000 A.D. is not something we can encode)
        # and the whole number must fit in 32 bits.
        if today.year < 1000:
            raise BadSerialPolicy(
                f"a date-style serial for {today.isoformat()} is not of the "
                "form YYYYMMDDnn"
            )
        base = _date_serial_base(today)
        if base > MAX_SERIAL:
            raise BadSerialPolicy(
                f"a date-style serial for {today.isoformat()} does not fit in "
                "a 32-bit serial number"
            )
        if _serial_date(current) == today:
            counter = current % 100
            if counter >= 99:
                raise SerialExhausted(
                    f"serial {current} already uses the last counter value "
                    f"{counter} for {today.isoformat()}; at most 100 updates "
                    "per day can be numbered"
                )
            candidate = current + 1
        else:
            candidate = base
        return _require_update(candidate, current)


def make_policy(policy: SerialPolicy | str) -> SerialPolicy:
    """Convert *policy* to a :py:class:`SerialPolicy`.

    The textual forms are:

    * ``"increment"``, or equivalently ``"fixed"``, for a step of one,
    * ``"increment:N"`` or ``"fixed:N"`` for a step of *N*, and
    * ``"date"``, or equivalently ``"datecounter"``, for date-style serials.

    A policy object is returned unchanged.

    :param policy: The policy to use.
    :type policy: :py:class:`SerialPolicy` or str
    :raises BadSerialPolicy: If the text is not a known policy, or if its
        parameters are invalid.
    :rtype: :py:class:`SerialPolicy`
    """
    if isinstance(policy, SerialPolicy):
        return policy
    if not isinstance(policy, str):
        raise BadSerialPolicy(
            f"policy must be a SerialPolicy or str, not {type(policy).__name__}"
        )
    text = policy.strip().lower()
    if text in ("increment", "fixed"):
        return IncrementPolicy()
    name, _, argument = text.partition(":")
    if name in ("increment", "fixed"):
        try:
            step = int(argument, 10)
        except ValueError:
            raise BadSerialPolicy(
                f"invalid increment step {argument!r} in policy {policy!r}"
            ) from None
        return IncrementPolicy(step)
    if text in ("date", "datecounter"):
        return DatePolicy()
    raise BadSerialPolicy(f"unknown serial policy {policy!r}")


@dataclasses.dataclass(frozen=True)
class SerialPlan:
    """A planned next SOA serial number.

    A plan is a pure computation; making one never changes a zone.  Use
    :py:meth:`apply` to write the planned serial to an open transaction, which
    first verifies that the zone's SOA serial is still the one the plan was
    made from.

    :ivar int current: The SOA serial the plan was computed from.
    :ivar int serial: The planned next SOA serial.
    :ivar SerialPolicy policy: The policy which was used.
    """

    current: int
    serial: int
    policy: SerialPolicy

    def __post_init__(self):
        # A plan made by hand must be as trustworthy as one made by
        # plan_serial(), so the same checks apply.
        _require_serial(self.current, "current serial")
        _require_serial(self.serial, "planned serial")
        _require_update(self.serial, self.current)

    @classmethod
    def for_soa(
        cls,
        soa: dns.rdtypes.ANY.SOA.SOA,
        policy: SerialPolicy | str = "increment",
        *,
        now: datetime.datetime | None = None,
    ) -> "SerialPlan":
        """Plan the next serial for the zone whose SOA is *soa*.

        This is a convenience wrapper around :py:func:`plan_serial` for
        callers which already have the SOA rdata, e.g. from a query.

        :param soa: The zone's SOA rdata.
        :type soa: :py:class:`dns.rdtypes.ANY.SOA.SOA`
        :param policy: The planning policy.
        :type policy: :py:class:`SerialPolicy` or str
        :param now: The time to base a date-style serial on.
        :type now: :py:class:`datetime.datetime` or ``None``
        :rtype: :py:class:`SerialPlan`
        """
        return plan_serial(soa.serial, policy, now=now)

    @classmethod
    def for_zone(
        cls,
        zone: "dns.zone.Zone",
        policy: SerialPolicy | str = "increment",
        *,
        txn: "dns.transaction.Transaction | None" = None,
        now: datetime.datetime | None = None,
    ) -> "SerialPlan":
        """Plan the next serial for *zone*.

        The zone's current SOA serial is read, but the zone is not modified.
        If *txn* is given, the serial is read from the transaction instead, so
        that :py:meth:`apply` on the same transaction sees a consistent view.

        :param zone: The zone to plan for.
        :type zone: :py:class:`dns.zone.Zone`
        :param policy: The planning policy.
        :type policy: :py:class:`SerialPolicy` or str
        :param txn: An open transaction to read the serial from.
        :type txn: :py:class:`dns.transaction.Transaction` or ``None``
        :param now: The time to base a date-style serial on.
        :type now: :py:class:`datetime.datetime` or ``None``
        :raises dns.zone.NoSOA: If the zone has no SOA.
        :rtype: :py:class:`SerialPlan`
        """
        return plan_serial(zone.get_soa(txn).serial, policy, now=now)

    def apply(
        self,
        txn: "dns.transaction.Transaction",
        name: "dns.name.Name | str | None" = None,
    ) -> None:
        """Write the planned serial to the SOA in *txn*.

        The transaction's SOA serial must still be :py:attr:`current`; if it
        is not, the plan is stale and :py:exc:`SerialChanged` is raised with
        nothing written.  The planned serial is also re-checked with serial
        number arithmetic before anything is written.

        The owner name of the SOA defaults to the transaction's effective
        origin, which is ``@`` in a relativized zone and the zone origin in an
        absolute one.

        :param txn: An open, writable transaction.
        :type txn: :py:class:`dns.transaction.Transaction`
        :param name: The owner name of the SOA in the transaction's namespace.
        :type name: :py:class:`dns.name.Name`, str, or ``None``
        :raises SerialChanged: If the zone's SOA serial is no longer
            :py:attr:`current`.
        :raises SerialNotUpdated: If the planned serial would not be seen as
            an update of the zone's present serial.
        :raises SerialComparisonUndefined: If the planned serial and the
            zone's present serial are half the serial number space apart.
        :raises KeyError: If there is no SOA rdataset at *name*.
        """
        if name is None:
            _, _, effective = txn.manager.origin_information()
            if effective is None:
                raise SerialError(
                    "cannot determine the SOA owner name because the "
                    "transaction has no zone origin; specify name explicitly"
                )
            name = effective
        elif isinstance(name, str):
            name = dns.name.from_text(name, None)
        rdataset = txn.get(name, dns.rdatatype.SOA)
        if rdataset is None or len(rdataset) == 0:
            raise KeyError(f"no SOA rdataset at {name}")
        soa = cast(dns.rdtypes.ANY.SOA.SOA, rdataset[0])
        if soa.serial != self.current:
            raise SerialChanged(
                f"zone SOA serial is now {soa.serial}, not the serial "
                f"{self.current} the plan was made from; plan again"
            )
        # The plan was validated when it was made, but it may have been
        # constructed directly (its fields are not enforced by the type
        # system), so re-check the serial arithmetic before writing.  In
        # particular this catches a plan whose value has become unorderable
        # relative to the serial now in the zone.
        _require_serial(self.serial, "planned serial")
        _require_update(self.serial, soa.serial)
        rdata = soa.replace(serial=self.serial)
        txn.replace(name, dns.rdataset.from_rdata(rdataset.ttl, rdata))


def plan_serial(
    current: int,
    policy: SerialPolicy | str = "increment",
    *,
    now: datetime.datetime | None = None,
) -> SerialPlan:
    """Plan the next SOA serial number for a zone.

    This function only computes a value; it neither reads nor writes a zone.
    The current serial would normally come from the zone's SOA, e.g.
    ``zone.get_soa().serial`` or ``zone.get_soa(txn).serial``.  The planned
    serial is always verified with RFC 1982 serial number arithmetic, so a
    candidate which a secondary would not see as an update is never returned::

        with zone.writer() as txn:
            plan = dns.serial.plan_serial(zone.get_soa(txn).serial, "date")
            # ... other changes ...
            plan.apply(txn)

    :param int current: The zone's present SOA serial.
    :param policy: The planning policy, either an object or its textual form.
        See :py:func:`make_policy`.
    :type policy: :py:class:`SerialPolicy` or str
    :param now: The time to base a date-style serial on.  It must be a
        :py:class:`datetime.datetime`; a naive value is taken to be UTC.  The
        default is the current time in UTC.
    :type now: :py:class:`datetime.datetime` or ``None``
    :raises BadSerial: If *current* is not a 32-bit unsigned integer.
    :raises BadSerialPolicy: If the policy is unknown or invalid.
    :raises SerialNotUpdated: If the planned serial would not be an update.
    :raises SerialComparisonUndefined: If the planned serial and *current* are
        half the serial number space apart.
    :raises SerialExhausted: If a date-style policy has used all of the
        counters for the day.
    :rtype: :py:class:`SerialPlan`
    """
    _require_serial(current, "current serial")
    policy = make_policy(policy)
    if now is None:
        now = datetime.datetime.now(datetime.timezone.utc)
    elif not isinstance(now, datetime.datetime):
        raise TypeError(f"now must be a datetime, not {now!r}")
    candidate = policy.next_serial(current, now)
    # SerialPlan revalidates both numbers and the update relation, so a custom
    # policy cannot smuggle in a value which secondaries would ignore.
    return SerialPlan(current=current, serial=candidate, policy=policy)


def next_serial(
    current: int,
    policy: SerialPolicy | str = "increment",
    *,
    now: datetime.datetime | None = None,
) -> int:
    """Return the next SOA serial number for *current* under *policy*.

    This is a convenience wrapper around :py:func:`plan_serial` for callers
    which only want the number; use :py:func:`plan_serial` when the planned
    serial will be applied to a transaction, so that a stale plan can be
    detected.

    :param int current: The zone's present SOA serial.
    :param policy: The planning policy, either an object or its textual form.
    :type policy: :py:class:`SerialPolicy` or str
    :param now: The time to base a date-style serial on.
    :type now: :py:class:`datetime.datetime` or ``None``
    :rtype: int
    """
    return plan_serial(current, policy, now=now).serial
