"""Explicit publish outcomes and retry policies for producers.

AMQP ``basic.publish`` is asynchronous: when the connection drops after
the message was written but before the broker's publisher confirm
arrives, the client cannot know whether the broker accepted the message.
Transparently retrying in that situation can deliver the message twice.

This module makes the outcome of a publish explicit instead of hiding
that ambiguity:

* :class:`PublishState` classifies every attempt as ``NOT_SENT``,
  ``CONFIRMED``, ``REJECTED`` or ``UNKNOWN``.
* :class:`PublishResult` records the stable ``message_id`` and the
  identity of every attempt, so callers can reconcile duplicates.
* :class:`PublishRetryPolicy` is a declared policy object: the producer
  never decides on its own whether another attempt is allowed.

Two rules are honored throughout:

* ``UNKNOWN`` is never treated as a broker rejection, and retrying an
  ``UNKNOWN`` attempt always requires an explicit opt-in because the
  message may already have been accepted by the broker.
* No client-side mechanism can guarantee exactly-once delivery; this
  API only makes the ambiguity observable so that deduplication (using
  the preserved ``message_id``) can happen at the broker or consumer.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Optional

__all__ = (
    'PublishState',
    'PublishAttempt',
    'PublishResult',
    'PublishRetryPolicy',
    'BROKER_REJECTED_ERRORS',
    'FALLBACK_PUBLISH_ERRORS',
    'channel_supports_confirm',
)

try:  # pragma: no cover - amqp is a hard dependency, guard is cosmetic
    from amqp.exceptions import ChannelError as _AmqpChannelError
    from amqp.exceptions import ConnectionError as _AmqpConnectionError
    from amqp.exceptions import MessageNacked as _MessageNacked
except ImportError:  # pragma: no cover
    BROKER_REJECTED_ERRORS: tuple[type[BaseException], ...] = ()
    FALLBACK_PUBLISH_ERRORS: tuple[type[BaseException], ...] = ()
else:
    #: Exception types meaning the broker explicitly rejected the
    #: message (``basic.nack`` in publisher-confirm mode).
    BROKER_REJECTED_ERRORS = (_MessageNacked,)
    #: Connection/channel error types used to classify failed attempts
    #: when no connection object is available to ask.
    FALLBACK_PUBLISH_ERRORS = (_AmqpConnectionError, _AmqpChannelError)


class PublishState(Enum):
    """Classification of a single publish attempt.

    NOT_SENT:
        The attempt failed before the message was written to the
        connection.  Retrying it cannot create a duplicate.

    CONFIRMED:
        The broker positively acknowledged the message (publisher
        confirms).  Without publisher confirms a clean return only
        proves the transport accepted the write, which is reported as
        ``UNKNOWN`` instead.

    REJECTED:
        The broker explicitly refused the message (``basic.nack``).

    UNKNOWN:
        The message may or may not have been accepted by the broker
        (e.g. the connection dropped after the write but before the
        confirm arrived).  Retrying may create a duplicate.
    """

    NOT_SENT = 'not_sent'
    CONFIRMED = 'confirmed'
    REJECTED = 'rejected'
    UNKNOWN = 'unknown'


@dataclass(frozen=True)
class PublishAttempt:
    """Record of a single publish attempt.

    Attributes:
        attempt_id: Unique identity of this attempt.  Every attempt of
            the same message gets a fresh id, while
            :attr:`PublishResult.message_id` stays stable.
        state: Classification of this attempt.
        error: The exception that ended the attempt, if any.
    """

    attempt_id: str
    state: PublishState
    error: Optional[BaseException] = None


@dataclass
class PublishResult:
    """Explicit outcome of publishing one logical message.

    Attributes:
        message_id: Stable identity of the logical message, preserved
            across all attempts and sent to the broker as the AMQP
            ``message_id`` property.
        attempts: One :class:`PublishAttempt` per try, in order.
        state: Outcome of the last attempt.
    """

    message_id: str
    attempts: list[PublishAttempt] = field(default_factory=list)
    state: PublishState = PublishState.NOT_SENT

    @property
    def confirmed(self) -> bool:
        """The broker positively acknowledged the message."""
        return self.state is PublishState.CONFIRMED

    @property
    def rejected(self) -> bool:
        """The broker explicitly rejected the message."""
        return self.state is PublishState.REJECTED

    @property
    def unknown(self) -> bool:
        """The broker outcome is unknown; the message may be delivered.

        This is *not* a rejection: the message may already be queued.
        """
        return self.state is PublishState.UNKNOWN

    @property
    def not_sent(self) -> bool:
        """No attempt wrote the message to the connection."""
        return self.state is PublishState.NOT_SENT

    @property
    def attempt_count(self) -> int:
        """Number of attempts performed."""
        return len(self.attempts)


@dataclass(frozen=True)
class PublishRetryPolicy:
    """Declared policy deciding whether another attempt is allowed.

    The producer never retries on its own: every retry decision is
    delegated to this object via :meth:`should_retry`.

    Attributes:
        max_attempts: Maximum total number of attempts (>= 1).
        retry_states: Set of :class:`PublishState` values that may be
            followed by another attempt.  ``UNKNOWN`` is only included
            when the caller explicitly accepts possible duplicates.
    """

    max_attempts: int = 1
    retry_states: frozenset = frozenset({PublishState.NOT_SENT})

    def __post_init__(self):
        if (isinstance(self.max_attempts, bool) or
                not isinstance(self.max_attempts, int) or
                self.max_attempts < 1):
            raise ValueError('max_attempts must be a positive integer')
        states = frozenset(self.retry_states)
        unknown = [s for s in states if not isinstance(s, PublishState)]
        if unknown:
            raise ValueError(
                f'retry_states must contain PublishState values, '
                f'got {unknown!r}')
        object.__setattr__(self, 'retry_states', states)

    def should_retry(self, result: PublishResult) -> bool:
        """Return True if the policy allows another attempt."""
        return (
            result.state in self.retry_states and
            result.attempt_count < self.max_attempts
        )

    @classmethod
    def never(cls) -> 'PublishRetryPolicy':
        """Perform exactly one attempt, whatever the outcome."""
        return cls(max_attempts=1, retry_states=frozenset())

    @classmethod
    def no_duplicates(cls, max_attempts: int = 3) -> 'PublishRetryPolicy':
        """Retry only attempts that provably never reached the broker.

        Only ``NOT_SENT`` attempts are retried, so this policy cannot
        create duplicates.  An ``UNKNOWN`` outcome is returned to the
        caller instead of being retried.
        """
        return cls(max_attempts=max_attempts,
                   retry_states=frozenset({PublishState.NOT_SENT}))

    @classmethod
    def at_least_once(cls, max_attempts: int = 3) -> 'PublishRetryPolicy':
        """Retry ``NOT_SENT`` and ``UNKNOWN`` attempts.

        .. warning::
            Retrying an ``UNKNOWN`` attempt can deliver the message
            more than once.  Consumers must deduplicate using the
            preserved ``message_id``.
        """
        return cls(
            max_attempts=max_attempts,
            retry_states=frozenset(
                {PublishState.NOT_SENT, PublishState.UNKNOWN}),
        )


def channel_supports_confirm(channel: Any) -> bool:
    """Return whether the channel waits for broker publisher confirms."""
    for obj in (channel, getattr(channel, 'connection', None)):
        if obj is None:
            continue
        confirms = getattr(obj, 'confirm_publish', None)
        if isinstance(confirms, bool):
            return confirms
    return bool(getattr(channel, '_confirm_selected', False))
