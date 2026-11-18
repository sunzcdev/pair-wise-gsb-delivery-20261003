"""
h2/ping
~~~~~~~

Optional bookkeeping for outstanding HTTP/2 PING probes.

The tools in this module are deliberately passive: they never schedule
timers, never touch the network and never emit frames. A caller registers
the probes it sent (or intends to send), feeds in the acknowledgements it
received, and asks the bookkeeping object which probes have expired. Time is
only ever advanced explicitly by the caller.

This makes it possible to drive keepalive logic from any event loop, or from
a test, without h2 spawning background threads of its own.
"""
from __future__ import annotations

import collections
import enum
import time
from typing import TYPE_CHECKING, NamedTuple

from .exceptions import DuplicatePingProbeError, TooManyPingProbesError

if TYPE_CHECKING:  # pragma: no cover
    from collections.abc import Callable

#: The number of bytes in the identity carried by an HTTP/2 PING frame.
PING_IDENTITY_LENGTH = 8

#: The largest numeric identity that can be encoded in eight bytes.
_MAX_IDENTITY = 2**64 - 1


class PingProbeState(enum.Enum):
    """
    The lifecycle states a single PING probe can be in.

    A probe starts out :attr:`PENDING`, becomes :attr:`ACKED` when a matching
    PING acknowledgment is observed, :attr:`TIMED_OUT` when the caller notices
    that its deadline has passed, and :attr:`ABANDONED` when the connection
    closes before the probe was resolved.
    """

    PENDING = "pending"
    ACKED = "acked"
    TIMED_OUT = "timed_out"
    ABANDONED = "abandoned"


class PingProbe(NamedTuple):
    """
    An immutable snapshot describing one PING probe.

    Snapshots are returned by :meth:`PingProbeManager.snapshot`,
    :meth:`PingProbeManager.acknowledge`, :meth:`PingProbeManager.poll` and
    :meth:`PingProbeManager.close`, so inspecting a probe can never mutate the
    manager's internal state.
    """

    opaque_data: bytes
    """The eight-byte identity carried in the PING frame."""

    state: PingProbeState
    """The current lifecycle state of the probe."""

    sent_at: float
    """The monotonic timestamp at which the probe was registered."""

    deadline: float
    """The monotonic timestamp at which the probe times out."""

    resolved_at: float | None = None
    """
    The monotonic timestamp at which the probe left the ``PENDING`` state, or
    ``None`` while it is still outstanding.
    """

    @property
    def rtt(self) -> float | None:
        """
        The observed round-trip time, or ``None`` if the probe never completed.

        For acknowledged probes this is the delay between registration and the
        matching acknowledgment. Timed out probes report the timeout interval,
        and abandoned probes report ``None`` because the connection closed
        before any answer could arrive.
        """
        if self.state is PingProbeState.ABANDONED or self.resolved_at is None:
            return None
        return self.resolved_at - self.sent_at


class _Probe:
    """
    The mutable internal representation of a single PING probe.
    """

    __slots__ = ("deadline", "resolved_at", "sent_at", "state")

    def __init__(self, sent_at: float, deadline: float) -> None:
        self.sent_at = sent_at
        self.deadline = deadline
        self.state = PingProbeState.PENDING
        self.resolved_at: float | None = None

    def snapshot(self, opaque_data: bytes) -> PingProbe:
        return PingProbe(
            opaque_data=opaque_data,
            state=self.state,
            sent_at=self.sent_at,
            deadline=self.deadline,
            resolved_at=self.resolved_at,
        )


class PingProbeManager:
    """
    Bookkeeping for a bounded set of outstanding PING probes.

    Each probe has a distinct eight-byte identity, the monotonic timestamp at
    which it was registered, and a deadline. Acknowledging a probe settles
    exactly the probe that carries the acknowledged identity: acknowledgments
    for unknown identities are ignored and never settle a different probe.
    Probes that are never acknowledged can be collected with :meth:`poll`,
    and unresolved probes are given a terminal state when the connection
    closes via :meth:`close`.

    The manager owns no timer of its own. Time is supplied by ``monotonic``
    (which defaults to :func:`time.monotonic`), and callers can either inject
    a fake clock or pass explicit timestamps to :meth:`poll` and
    :meth:`close`.

    :param max_outstanding: The maximum number of probes that may be pending
        at once. Registering one more probe raises
        :class:`TooManyPingProbesError <h2.exceptions.TooManyPingProbesError>`.
        Defaults to ``64``.
    :param default_timeout: The default lifetime of a probe in seconds. When
        ``None``, probes have no deadline and never time out. Defaults to
        ``None``.
    :param max_resolved: The maximum number of settled probes whose snapshots
        are retained for later inspection. Older snapshots are discarded first.
        Defaults to ``64``.
    :param monotonic: A zero-argument callable returning monotonically
        increasing time in seconds. Defaults to :func:`time.monotonic`.

    .. versionadded:: 4.5.0
    """

    def __init__(self, *,
                 max_outstanding: int = 64,
                 default_timeout: float | None = None,
                 max_resolved: int = 64,
                 monotonic: Callable[[], float] | None = None) -> None:
        if max_outstanding < 1:
            msg = "max_outstanding must be at least 1"
            raise ValueError(msg)
        if default_timeout is not None and default_timeout < 0:
            msg = "default_timeout must not be negative"
            raise ValueError(msg)
        if max_resolved < 0:
            msg = "max_resolved must not be negative"
            raise ValueError(msg)

        self.max_outstanding = max_outstanding
        self.default_timeout = default_timeout
        self.max_resolved = max_resolved
        self._clock = monotonic or time.monotonic
        self._next_identity = 0
        self._pending: dict[bytes, _Probe] = {}
        self._resolved: collections.OrderedDict[bytes, PingProbe] = collections.OrderedDict()

    def __len__(self) -> int:
        """
        The number of probes that are still awaiting an acknowledgment.
        """
        return len(self._pending)

    @property
    def pending_probes(self) -> tuple[PingProbe, ...]:
        """
        Snapshots of all probes that are still awaiting an acknowledgment, in
        the order they were registered.
        """
        return tuple(
            probe.snapshot(identity) for identity, probe in self._pending.items()
        )

    @property
    def resolved_probes(self) -> tuple[PingProbe, ...]:
        """
        Snapshots of recently settled probes, oldest first.

        The number retained is bounded by ``max_resolved``; once that many
        probes have settled, the oldest snapshots are dropped.
        """
        return tuple(self._resolved.values())

    def register(self,
                 opaque_data: bytes | None = None, *,
                 timeout: float | None = None) -> bytes:
        """
        Register a new outstanding probe and return its eight-byte identity.

        When ``opaque_data`` is ``None`` an unused identity is allocated.
        Otherwise the supplied identity is validated for length and for
        uniqueness among outstanding probes. Registering an identity that is
        already pending raises
        :class:`DuplicatePingProbeError <h2.exceptions.DuplicatePingProbeError>`
        before any state is changed.

        :param opaque_data: The identity to use, or ``None`` to allocate one.
        :param timeout: The lifetime of this probe in seconds, overriding
            ``default_timeout``. ``None`` falls back to the default.
        :returns: The identity of the registered probe.
        """
        if len(self._pending) >= self.max_outstanding:
            msg = (
                f"Cannot register more than {self.max_outstanding} "
                f"outstanding PING probes"
            )
            raise TooManyPingProbesError(msg)

        if opaque_data is None:
            identity = self._allocate_identity()
        else:
            if not isinstance(opaque_data, bytes) or len(opaque_data) != PING_IDENTITY_LENGTH:
                msg = f"Invalid value for ping data: {opaque_data!r}"
                raise ValueError(msg)
            if opaque_data in self._pending:
                msg = f"A PING probe with data {opaque_data!r} is already pending"
                raise DuplicatePingProbeError(msg)
            identity = opaque_data

        effective_timeout = self.default_timeout if timeout is None else timeout
        if effective_timeout is not None and effective_timeout < 0:
            msg = "PING probe timeout must not be negative"
            raise ValueError(msg)

        sent_at = self._clock()
        if effective_timeout is None:
            deadline = float("inf")
        else:
            deadline = sent_at + effective_timeout

        self._pending[identity] = _Probe(sent_at, deadline)
        return identity

    def acknowledge(self, opaque_data: bytes) -> PingProbe | None:
        """
        Settle the outstanding probe carried by an acknowledged PING.

        Only a probe with exactly this identity is settled. An acknowledgment
        for an identity that is not pending - including one that was already
        settled - returns ``None`` and leaves every other probe untouched.

        :param opaque_data: The identity from the PING acknowledgment frame.
        :returns: A snapshot of the settled probe, or ``None`` if the identity
            is unknown.
        """
        probe = self._pending.pop(opaque_data, None)
        if probe is None:
            return None

        probe.state = PingProbeState.ACKED
        probe.resolved_at = self._clock()
        snapshot = probe.snapshot(opaque_data)
        self._remember(opaque_data, snapshot)
        return snapshot

    def abandon(self, opaque_data: bytes) -> PingProbe | None:
        """
        Give up on a single outstanding probe without an acknowledgment.

        This is useful when sending the PING itself failed. It is not used for
        graceful connection shutdown; :meth:`close` handles that case.

        :param opaque_data: The identity of the probe to abandon.
        :returns: A snapshot of the abandoned probe, or ``None`` if the
            identity is unknown.
        """
        probe = self._pending.pop(opaque_data, None)
        if probe is None:
            return None

        probe.state = PingProbeState.ABANDONED
        probe.resolved_at = self._clock()
        snapshot = probe.snapshot(opaque_data)
        self._remember(opaque_data, snapshot)
        return snapshot

    def poll(self, now: float | None = None) -> list[PingProbe]:
        """
        Collect the probes whose deadlines have passed.

        Each timed out probe is settled exactly once, with state
        :attr:`PingProbeState.TIMED_OUT`, and is reported in registration
        order. No frame is emitted and no acknowledgment is forged: the caller
        decides what a timeout means for the connection.

        :param now: The current monotonic timestamp. Defaults to the value of
            the manager's clock.
        :returns: Snapshots of the probes that timed out during this call.
        """
        current_time = self._clock() if now is None else now
        expired: list[PingProbe] = []

        for identity, probe in list(self._pending.items()):
            if probe.deadline > current_time:
                continue
            del self._pending[identity]
            probe.state = PingProbeState.TIMED_OUT
            probe.resolved_at = probe.deadline
            snapshot = probe.snapshot(identity)
            self._remember(identity, snapshot)
            expired.append(snapshot)

        return expired

    def close(self, now: float | None = None) -> list[PingProbe]:
        """
        Settle every outstanding probe because the connection is closing.

        This is the terminal state for probes that were still pending when the
        connection went away: no acknowledgment will ever arrive, so they are
        recorded as :attr:`PingProbeState.ABANDONED` with no round-trip time.

        :param now: The monotonic timestamp of the closure. Defaults to the
            value of the manager's clock.
        :returns: Snapshots of the abandoned probes, in registration order.
        """
        current_time = self._clock() if now is None else now
        abandoned: list[PingProbe] = []

        for identity, probe in self._pending.items():
            probe.state = PingProbeState.ABANDONED
            probe.resolved_at = current_time
            snapshot = probe.snapshot(identity)
            self._remember(identity, snapshot)
            abandoned.append(snapshot)

        self._pending.clear()
        return abandoned

    def snapshot(self, opaque_data: bytes) -> PingProbe | None:
        """
        Return a snapshot of the probe with the given identity.

        Both pending and recently resolved probes can be inspected. Returns
        ``None`` when the identity is unknown or its snapshot has been dropped.

        :param opaque_data: The eight-byte identity of the probe to inspect.
        :returns: A snapshot of the probe, or ``None`` if it is unknown.
        """
        probe = self._pending.get(opaque_data)
        if probe is not None:
            return probe.snapshot(opaque_data)
        return self._resolved.get(opaque_data)

    def _allocate_identity(self) -> bytes:
        """
        Allocate an identity that is not used by a pending or resolved probe.
        """
        while True:
            self._next_identity += 1
            if self._next_identity > _MAX_IDENTITY:
                self._next_identity = 1
            identity = self._next_identity.to_bytes(PING_IDENTITY_LENGTH, "big")
            if identity not in self._pending and identity not in self._resolved:
                return identity

    def _remember(self, opaque_data: bytes, snapshot: PingProbe) -> None:
        """
        Retain a resolved snapshot, dropping the oldest ones first.
        """
        if self.max_resolved == 0:
            return
        # Re-insert an identity that was resolved before so that ordering still
        # reflects resolution order: assigning to an existing OrderedDict key
        # keeps its original position.
        self._resolved.pop(opaque_data, None)
        self._resolved[opaque_data] = snapshot
        while len(self._resolved) > self.max_resolved:
            self._resolved.popitem(last=False)
