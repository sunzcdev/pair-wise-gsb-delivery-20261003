"""A pure python implementation of multicast DNS service discovery.

Licensed under LGPL-2.1-or-later; see COPYING for details. This file is
part of a continuously modified work; modification dates are recorded
in the project's git history.
"""

from __future__ import annotations

import random
from collections import deque
from typing import TYPE_CHECKING, cast

from .._dns import DNSPointer, DNSRecord
from .._utils.time import current_time_millis, millis_to_seconds
from ..const import (
    _ADDRESS_RECORD_TYPES,
    _SERVICE_TYPE_ENUMERATION_NAME,
    _TYPE_NSEC,
    _TYPE_PTR,
    _TYPE_SRV,
    _TYPE_TXT,
)
from .answers import (
    MULTICAST_DELAY_RANDOM_INTERVAL,
    AnswerGroup,
    _AnswerWithAdditionalsType,
    construct_outgoing_multicast_answers,
)

RAND_INT = random.randint

if TYPE_CHECKING:
    from .._core import Zeroconf

_float = float
_int = int


class MulticastOutgoingQueue:
    """An outgoing queue used to aggregate multicast responses."""

    __slots__ = (
        "_additional_delay",
        "_aggregation_delay",
        "_multicast_delay_random_max",
        "_multicast_delay_random_min",
        "queue",
        "zc",
    )

    def __init__(self, zeroconf: Zeroconf, additional_delay: _int, max_aggregation_delay: _int) -> None:
        self.zc = zeroconf
        self.queue: deque[AnswerGroup] = deque()
        # Additional delay is used to implement
        # Protect the network against excessive packet flooding
        # https://datatracker.ietf.org/doc/html/rfc6762#section-14
        self._multicast_delay_random_min = MULTICAST_DELAY_RANDOM_INTERVAL[0]
        self._multicast_delay_random_max = MULTICAST_DELAY_RANDOM_INTERVAL[1]
        self._additional_delay = additional_delay
        self._aggregation_delay = max_aggregation_delay

    def async_add(self, now: _float, answers: _AnswerWithAdditionalsType) -> None:
        """Add a group of answers with additionals to the outgoing queue."""
        loop = self.zc.loop
        if TYPE_CHECKING:
            assert loop is not None
        random_int = RAND_INT(self._multicast_delay_random_min, self._multicast_delay_random_max)
        random_delay = random_int + self._additional_delay
        send_after = now + random_delay
        send_before = now + self._aggregation_delay + self._additional_delay
        if len(self.queue):
            # If we calculate a random delay for the send after time
            # that is less than the last group scheduled to go out,
            # we instead add the answers to the last group as this
            # allows aggregating additional responses
            last_group = self.queue[-1]
            if send_after <= last_group.send_after:
                last_group.answers.update(answers)
                return
        else:
            loop.call_at(loop.time() + millis_to_seconds(random_delay), self.async_ready)
        self.queue.append(AnswerGroup(send_after, send_before, answers))

    def async_ready(self) -> None:
        """Process anything in the queue that is ready."""
        zc = self.zc
        loop = zc.loop
        if TYPE_CHECKING:
            assert loop is not None
        now = current_time_millis()

        if len(self.queue) > 1 and self.queue[0].send_before > now:
            # There is more than one answer in the queue,
            # delay until we have to send it (first answer group reaches send_before)
            loop.call_at(
                loop.time() + millis_to_seconds(self.queue[0].send_before - now),
                self.async_ready,
            )
            return

        answers: _AnswerWithAdditionalsType = {}
        # Add all groups that can be sent now
        while len(self.queue) and self.queue[0].send_after <= now:
            answers.update(self.queue.popleft().answers)

        if len(self.queue):
            # If there are still groups in the queue that are not ready to send
            # be sure we schedule them to go out later
            loop.call_at(
                loop.time() + millis_to_seconds(self.queue[0].send_after - now),
                self.async_ready,
            )

        if answers:  # pragma: no branch
            # If we have the same answer scheduled to go out, remove them
            self._remove_answers_from_queue(answers)
            # The registry may have changed while the answers waited in
            # the queue; only send what still matches the registration
            current_answers = self._current_answers(answers)
            if current_answers:
                zc.async_send(construct_outgoing_multicast_answers(current_answers))

    def _current_answers(self, answers: _AnswerWithAdditionalsType) -> _AnswerWithAdditionalsType:
        """Drop or replace queued records that no longer match the registry."""
        registry = self.zc.registry
        if not registry.has_entries:
            # All queued answers originate from the registry; if it is
            # empty there is nothing current left to send.
            return {}
        current: _AnswerWithAdditionalsType = {}
        for answer, additionals in answers.items():
            current_answer = self._current_record(answer)
            if current_answer is None:
                continue
            current_additionals: set[DNSRecord] = set()
            for additional in additionals:
                current_additional = self._current_record(additional)
                if current_additional is not None:
                    current_additionals.add(current_additional)
            current[current_answer] = current_additionals
        return current

    def _current_record(self, record: DNSRecord) -> DNSRecord | None:
        """Return the record to send in place of a queued record, or None to drop it."""
        registry = self.zc.registry
        record_type = record.type
        if record_type == _TYPE_PTR:
            pointer = cast(DNSPointer, record)
            if pointer.key == _SERVICE_TYPE_ENUMERATION_NAME:
                # Service type enumeration; the alias is a service type
                return record if registry.async_get_infos_type(pointer.alias_key) else None
            return record if registry.async_get_info_name(pointer.alias_key) is not None else None
        if record_type == _TYPE_SRV:
            info = registry.async_get_info_name(record.key)
            return info.dns_service() if info is not None else None
        if record_type == _TYPE_TXT:
            info = registry.async_get_info_name(record.key)
            return info.dns_text() if info is not None else None
        if record_type in _ADDRESS_RECORD_TYPES:
            for info in registry.async_get_infos_server(record.key):
                if record in info.dns_addresses():
                    return record
            return None
        if record_type == _TYPE_NSEC:
            for info in registry.async_get_infos_server(record.key):
                if record == info.dns_address_nsec():
                    return record
            return None
        # Any other record types are kept as-is
        return record

    def _remove_answers_from_queue(self, answers: _AnswerWithAdditionalsType) -> None:
        """Remove a set of answers from the outgoing queue."""
        for pending in self.queue:
            for record in answers:
                pending.answers.pop(record, None)
