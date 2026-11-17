"""Explicit acknowledgment coordination for parallel consumers.

When messages are processed by a pool of workers they are usually completed
in a different order than the one in which they were delivered.  An AMQP
``basic.ack`` with ``multiple=True`` (and the equivalent virtual-transport
bookkeeping) acknowledges *every* delivery tag up to the given tag, so it
must never be issued while an earlier delivery is still in flight.

The :class:`AckCoordinator` keeps an ordered record of every delivery, grouped
by the channel (and channel epoch) the delivery was made on, and only exposes
a *contiguous completed prefix* for batched acknowledgment.  Rejections and
requeues have their own terminal states and break the prefix, and deliveries
made on a channel before a reconnect are quarantined as stale so that their
tags can never be used against the replacement channel.

If the actual channel call fails the affected records are moved to an
inspectable *uncertain* state instead of being optimistically discarded, so
the caller can decide whether to retry or to let the broker redeliver.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Callable, Dict, List, Optional

__all__ = (
    'DeliveryState', 'DeliveryRecord', 'AckCoordinator',
    'AckCoordinationError', 'UnknownDeliveryTag', 'StaleDeliveryError',
)


class AckCoordinationError(Exception):
    """Base class for acknowledgment coordination failures."""


class UnknownDeliveryTag(AckCoordinationError):
    """A completion was reported for a delivery that was never tracked."""


class StaleDeliveryError(AckCoordinationError):
    """A delivery belongs to a channel epoch that is no longer current.

    The tag of such a delivery must never be sent to the replacement
    channel because the broker may associate it with a different message.
    """


class DeliveryState(str, Enum):
    """Lifecycle states of a tracked delivery."""

    #: Delivered, processing has not completed yet.
    PENDING = 'PENDING'
    #: Processing completed, waiting to be included in an ack batch.
    ACKED = 'ACKED'
    #: Acknowledged successfully on the channel.
    ACK_SENT = 'ACK_SENT'
    #: The ack call failed; the broker outcome is unknown.
    ACK_UNCERTAIN = 'ACK_UNCERTAIN'
    #: Rejected (without requeue) and the call succeeded.
    REJECTED = 'REJECTED'
    #: The reject call failed; the broker outcome is unknown.
    REJECT_UNCERTAIN = 'REJECT_UNCERTAIN'
    #: Requeued successfully.
    REQUEUED = 'REQUEUED'
    #: The requeue call failed; the broker outcome is unknown.
    REQUEUE_UNCERTAIN = 'REQUEUE_UNCERTAIN'
    #: The owning channel was replaced before the delivery was settled.
    STALE = 'STALE'

    @property
    def is_uncertain(self) -> bool:
        return self in _UNCERTAIN_STATES

    @property
    def is_settled(self) -> bool:
        """True once the broker accepted the ack/reject/requeue call."""
        return self in _SETTLED_STATES

    @property
    def no_longer_active(self) -> bool:
        """True if the record left normal prefix coordination.

        This covers settled records, uncertain records (which still block
        their channel's prefix until retried or forgotten) and stale records
        quarantined after a reconnect -- everything except deliveries that
        are pending or completed locally but not yet sent.
        """
        return self in _INACTIVE_STATES


_UNCERTAIN_STATES = frozenset({
    DeliveryState.ACK_UNCERTAIN,
    DeliveryState.REJECT_UNCERTAIN,
    DeliveryState.REQUEUE_UNCERTAIN,
})

#: States in which a record has left ordinary prefix coordination: settled
#: deliveries, uncertain deliveries (which still block their channel's prefix
#: until retried or forgotten) and stale records quarantined after reconnect.
_INACTIVE_STATES = frozenset({
    DeliveryState.ACK_SENT,
    DeliveryState.ACK_UNCERTAIN,
    DeliveryState.REJECTED,
    DeliveryState.REJECT_UNCERTAIN,
    DeliveryState.REQUEUED,
    DeliveryState.REQUEUE_UNCERTAIN,
    DeliveryState.STALE,
})

#: States recorded after the broker accepted the corresponding channel call.
_SETTLED_STATES = frozenset({
    DeliveryState.ACK_SENT,
    DeliveryState.REJECTED,
    DeliveryState.REQUEUED,
})

#: Completion states that can be requested through :meth:`AckCoordinator.mark`.
_INTENDED_STATES = {
    'ack': DeliveryState.ACKED,
    'reject': DeliveryState.REJECTED,
    'requeue': DeliveryState.REQUEUED,
}

_FAILOVER_STATES = {
    DeliveryState.ACKED: DeliveryState.ACK_UNCERTAIN,
    DeliveryState.REJECTED: DeliveryState.REJECT_UNCERTAIN,
    DeliveryState.REQUEUED: DeliveryState.REQUEUE_UNCERTAIN,
}


@dataclass
class DeliveryRecord:
    """A single delivery tracked by the coordinator.

    Attributes
    ----------
    delivery_tag:
        The tag assigned to the message by its channel.
    channel_key:
        Identifier of the channel that delivered the message.
    epoch:
        Channel epoch.  Every reconnect bumps the epoch, which lets the
        coordinator tell tags from a dead channel apart from live ones.
    message:
        The original message object, kept for inspection and requeue.
    state:
        Current :class:`DeliveryState`.
    error:
        The exception stored when the delivery entered an uncertain state.
    """

    delivery_tag: Any
    channel_key: Any
    epoch: int
    message: Any = None
    state: DeliveryState = DeliveryState.PENDING
    error: Optional[BaseException] = field(default=None, repr=False)

    @property
    def uncertain(self) -> bool:
        return self.state.is_uncertain

    @property
    def settled(self) -> bool:
        """True once the broker call for the delivery succeeded."""
        return self.state in _SETTLED_STATES


class AckCoordinator:
    """Coordinate acknowledgments across channels and worker threads.

    The coordinator is transport agnostic: the actual wire calls are made
    through the ``on_ack`` / ``on_reject`` callbacks, which receive the same
    arguments as ``Channel.basic_ack`` / ``Channel.basic_reject``.

    Arguments
    ---------
    on_ack:
        Callable ``(channel_key, delivery_tag, multiple)`` used to send an
        acknowledgment to the channel.
    on_reject:
        Callable ``(channel_key, delivery_tag, requeue)`` used to reject or
        requeue a delivery on the channel.
    """

    def __init__(self, on_ack: Callable[..., Any],
                 on_reject: Callable[..., Any]):
        self._on_ack = on_ack
        self._on_reject = on_reject
        # channel_key -> current epoch
        self._epochs: Dict[Any, int] = {}
        # channel_key -> ordered live records (pending/completed/uncertain)
        self._channels: Dict[Any, List[DeliveryRecord]] = {}
        # (channel_key, delivery_tag) -> record, for O(1) completion lookups.
        # Tags are only unique within a channel, so the channel is part of
        # the identity.
        self._records: Dict[tuple, DeliveryRecord] = {}
        # records that reached a settled state, retained for inspection
        self._settled: List[DeliveryRecord] = []
        # records orphaned by a channel reconnect, retained for inspection
        self._stale: List[DeliveryRecord] = []

    @staticmethod
    def _key(channel_key: Any, delivery_tag: Any) -> tuple:
        return channel_key, delivery_tag

    # ------------------------------------------------------------------
    # channel / delivery registration
    # ------------------------------------------------------------------
    def register_channel(self, channel_key: Any) -> int:
        """Start tracking a channel and return its current epoch."""
        self._epochs.setdefault(channel_key, 0)
        self._channels.setdefault(channel_key, [])
        return self._epochs[channel_key]

    def revive_channel(self, channel_key: Any) -> int:
        """Mark a channel as reconnected.

        Pending deliveries belonging to the previous epoch are moved to the
        :attr:`stale` quarantine and can no longer be acknowledged through
        this coordinator.  The replacement channel starts a fresh epoch with
        an empty delivery list.
        """
        previous = self._channels.pop(channel_key, [])
        for record in previous:
            self._quarantine_stale(record)
        epoch = self._epochs.get(channel_key, -1) + 1
        self._epochs[channel_key] = epoch
        self._channels[channel_key] = []
        return epoch

    def _quarantine_stale(self, record: DeliveryRecord) -> None:
        record.state = DeliveryState.STALE
        record.error = None
        self._records.pop(
            self._key(record.channel_key, record.delivery_tag), None)
        self._stale.append(record)

    def track(self, channel_key: Any, delivery_tag: Any,
              message: Any = None) -> DeliveryRecord:
        """Register a newly delivered message.

        A tag that was tracked before on the same channel (for example a
        message that was restored and redelivered) is re-registered as a
        fresh delivery: any previous live record is dropped first, while
        settled and stale history is left untouched for inspection.
        """
        record_key = self._key(channel_key, delivery_tag)
        if record_key in self._records:
            self.forget(delivery_tag, channel_key=channel_key)
        epoch = self.register_channel(channel_key)
        record = DeliveryRecord(
            delivery_tag=delivery_tag,
            channel_key=channel_key,
            epoch=epoch,
            message=message,
        )
        self._channels[channel_key].append(record)
        self._records[record_key] = record
        return record

    # ------------------------------------------------------------------
    # completion reporting
    # ------------------------------------------------------------------
    def mark(self, delivery_tag: Any, outcome: str = 'ack',
             channel_key: Any = None) -> DeliveryRecord:
        """Record the local completion of a delivery.

        Parameters
        ----------
        delivery_tag:
            The tag of the completed delivery.
        outcome:
            One of ``'ack'``, ``'reject'`` or ``'requeue'``.
        channel_key:
            Channel that owns the tag.  Required when more than one channel
            is registered because delivery tags are only unique per channel.

        Raises
        ------
            UnknownDeliveryTag: if the tag was never tracked or has already
                been settled/quarantined.
            StaleDeliveryError: if the owning channel has been replaced.
        """
        try:
            intended = _INTENDED_STATES[outcome]
        except KeyError:
            raise ValueError(
                f'Unknown completion outcome {outcome!r}, expected one of '
                f'{sorted(_INTENDED_STATES)}')
        record = self._lookup(delivery_tag, channel_key)
        if record is None:
            if self._is_known_stale_tag(delivery_tag, channel_key):
                raise StaleDeliveryError(
                    f'Delivery tag {delivery_tag!r} belongs to a channel '
                    'epoch that is no longer current')
            raise UnknownDeliveryTag(
                f'Delivery tag {delivery_tag!r} is not tracked')
        if record.state is not DeliveryState.PENDING:
            raise AckCoordinationError(
                f'Delivery tag {delivery_tag!r} already completed with '
                f'state {record.state.name}')
        record.state = intended
        return record

    def _lookup(self, delivery_tag: Any,
                channel_key: Any) -> Optional[DeliveryRecord]:
        if channel_key is not None:
            return self._records.get(self._key(channel_key, delivery_tag))
        matches = [record for record in self._records.values()
                   if record.delivery_tag == delivery_tag]
        if len(matches) > 1:
            raise AckCoordinationError(
                f'Delivery tag {delivery_tag!r} exists on multiple channels; '
                'channel_key is required')
        return matches[0] if matches else None

    def _is_known_stale_tag(self, delivery_tag: Any,
                            channel_key: Any = None) -> bool:
        return any(
            r.delivery_tag == delivery_tag and
            (channel_key is None or r.channel_key == channel_key)
            for r in self._stale)

    # ------------------------------------------------------------------
    # flushing the contiguous completed prefix
    # ------------------------------------------------------------------
    def flush(self, channel_key: Any = None) -> List[DeliveryRecord]:
        """Send the contiguous completed prefix to the channel(s).

        Only records that are completed *and* form an uninterrupted prefix of
        the per-channel delivery order are sent.  A still-pending delivery,
        or one whose channel call previously failed, blocks every later
        delivery on that channel, so a ``multiple`` ack can never jump over
        an unfinished message.

        Returns the records whose broker call succeeded during this call.
        """
        keys = [channel_key] if channel_key is not None else list(
            self._channels)
        flushed: List[DeliveryRecord] = []
        for key in keys:
            flushed.extend(self._flush_channel(key))
        return flushed

    def _flush_channel(self, channel_key: Any) -> List[DeliveryRecord]:
        records = self._channels.get(channel_key)
        if not records:
            return []

        flushed: List[DeliveryRecord] = []
        ack_batch: List[DeliveryRecord] = []

        def send_ack_batch(start_index: int) -> None:
            # ``ack_batch`` holds consecutive ACKED records ending just
            # before ``start_index`` in ``records``.
            if not ack_batch:
                return
            last = ack_batch[-1]
            batch_size = len(ack_batch)
            try:
                self._on_ack(
                    channel_key, last.delivery_tag,
                    multiple=batch_size > 1,
                )
            except BaseException as exc:
                self._fail(ack_batch, exc)
                # Keep the failed batch and everything after it queued so the
                # uncertain records keep blocking the completed prefix.
                self._channels[channel_key] = records[
                    start_index - batch_size:]
                raise
            for record in ack_batch:
                record.state = DeliveryState.ACK_SENT
                self._settle(record)
                flushed.append(record)
            ack_batch.clear()

        for index, record in enumerate(records):
            state = record.state
            if state is DeliveryState.PENDING or state.is_uncertain:
                # The prefix ends here; this record and everything after it
                # remain queued on the channel.
                send_ack_batch(index)
                self._channels[channel_key] = records[index:]
                return flushed
            if state is DeliveryState.ACKED:
                ack_batch.append(record)
                continue
            # A reject/requeue completion breaks the ack prefix: flush any
            # accumulated acks first, then settle it with its own call.
            send_ack_batch(index)
            try:
                self._settle_reject(record, channel_key)
            except BaseException:
                self._channels[channel_key] = records[index:]
                raise
            flushed.append(record)

        send_ack_batch(len(records))
        self._channels[channel_key] = []
        return flushed

    def _settle_reject(self, record: DeliveryRecord,
                       channel_key: Any) -> None:
        requeue = record.state is DeliveryState.REQUEUED
        try:
            self._on_reject(
                channel_key, record.delivery_tag, requeue=requeue,
            )
        except BaseException as exc:
            self._fail([record], exc)
            raise
        # REJECTED / REQUEUED already encode the intended terminal state.
        self._settle(record)

    def _fail(self, records: List[DeliveryRecord],
              exc: BaseException) -> None:
        # The records stay in their channel list so they keep blocking the
        # completed prefix, but they leave the live completion map: a later
        # ``mark`` must report them as unknown rather than allow a second,
        # contradictory outcome.  The failure stays available for
        # inspection/retries through :attr:`uncertain`.
        for record in records:
            record.error = exc
            record.state = _FAILOVER_STATES[record.state]
            self._records.pop(
                self._key(record.channel_key, record.delivery_tag), None)

    def _settle(self, record: DeliveryRecord) -> None:
        self._records.pop(
            self._key(record.channel_key, record.delivery_tag), None)
        self._settled.append(record)

    # ------------------------------------------------------------------
    # inspection
    # ------------------------------------------------------------------
    @property
    def pending(self) -> List[DeliveryRecord]:
        """Records still in flight (not yet reported as completed)."""
        return [r for records in self._channels.values()
                for r in records if r.state is DeliveryState.PENDING]

    @property
    def completed(self) -> List[DeliveryRecord]:
        """Completed records waiting for the prefix to reach them."""
        return [r for records in self._channels.values()
                for r in records
                if r.state in _INTENDED_STATES.values()]

    @property
    def uncertain(self) -> List[DeliveryRecord]:
        """Records whose channel call failed with an unknown outcome."""
        return [r for records in self._channels.values()
                for r in records if r.state.is_uncertain]

    @property
    def stale(self) -> List[DeliveryRecord]:
        """Records orphaned by a channel reconnect."""
        return list(self._stale)

    @property
    def settled(self) -> List[DeliveryRecord]:
        """Records acknowledged/rejected/requeued successfully."""
        return list(self._settled)

    def get(self, delivery_tag: Any,
            channel_key: Any = None) -> DeliveryRecord:
        """Return the record for a tag, including uncertain/stale ones."""
        if channel_key is not None:
            record = self._records.get(self._key(channel_key, delivery_tag))
            if record is not None:
                return record
        else:
            matches = [r for r in self._records.values()
                       if r.delivery_tag == delivery_tag]
            if len(matches) > 1:
                raise AckCoordinationError(
                    f'Delivery tag {delivery_tag!r} exists on multiple '
                    'channels; channel_key is required')
            if matches:
                return matches[0]
        for candidate in self.uncertain:
            if candidate.delivery_tag == delivery_tag and (
                    channel_key is None or
                    candidate.channel_key == channel_key):
                return candidate
        for bucket in (self._settled, self._stale):
            for candidate in bucket:
                if candidate.delivery_tag == delivery_tag and (
                        channel_key is None or
                        candidate.channel_key == channel_key):
                    return candidate
        raise UnknownDeliveryTag(
            f'Delivery tag {delivery_tag!r} is not tracked')

    def epoch(self, channel_key: Any) -> int:
        """Return the current epoch of a channel (0 if unknown)."""
        return self._epochs.get(channel_key, 0)

    def forget(self, delivery_tag: Any,
               channel_key: Any = None) -> None:
        """Drop a live delivery without issuing any channel call.

        Used when the channel itself restores its unacknowledged messages
        (for example at shutdown) and therefore owns the final disposition
        of the delivery.  Settled and stale records are kept for inspection
        and are not touched.
        """
        record = None
        if channel_key is not None:
            record = self._records.pop(
                self._key(channel_key, delivery_tag), None)
        else:
            for key, candidate in list(self._records.items()):
                if candidate.delivery_tag == delivery_tag:
                    record = self._records.pop(key)
                    break
        if record is None:
            # An uncertain record left the live map but still blocks its
            # channel's completed prefix; look it up for removal as well.
            for candidate in self.uncertain:
                if candidate.delivery_tag == delivery_tag and (
                        channel_key is None or
                        candidate.channel_key == channel_key):
                    record = candidate
                    break
        if record is None:
            return
        records = self._channels.get(record.channel_key)
        if records is not None:
            try:
                records.remove(record)
            except ValueError:  # pragma: no cover
                pass

    def retry_uncertain(self, delivery_tag: Any = None) -> None:
        """Move uncertain record(s) back to their intended completion state.

        This lets a caller that has recovered the channel attempt the
        operation again instead of losing the record.  Without an explicit
        tag every uncertain record is retried.
        """
        targets = self.uncertain if delivery_tag is None else [
            r for r in self.uncertain if r.delivery_tag == delivery_tag]
        if delivery_tag is not None and not targets:
            raise UnknownDeliveryTag(
                f'No uncertain delivery with tag {delivery_tag!r}')
        for record in targets:
            record.error = None
            if record.state is DeliveryState.ACK_UNCERTAIN:
                record.state = DeliveryState.ACKED
            elif record.state is DeliveryState.REJECT_UNCERTAIN:
                record.state = DeliveryState.REJECTED
            else:
                record.state = DeliveryState.REQUEUED
            self._records[self._key(
                record.channel_key, record.delivery_tag)] = record
