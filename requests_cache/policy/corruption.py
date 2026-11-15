"""Handling for cache entries that cannot be deserialized."""

from __future__ import annotations

from enum import Enum
from logging import Logger
from typing import Any, Callable, Dict, Optional, Type

__all__ = [
    'CorruptionCallback',
    'CorruptionPolicy',
    'CorruptEntryError',
    'notify_corruption',
]


class CorruptionPolicy(str, Enum):
    """How to handle a cache entry that cannot be deserialized.

    * ``report``: Log the failure and treat the entry as a cache miss, but leave it in place.
    * ``delete``: Log the failure, delete the entry (and any redirect aliases pointing to it), and
      treat it as a cache miss so it can be fetched again.
    * ``raise``: Raise a :py:class:`CorruptEntryError`.
    """

    REPORT = 'report'
    DELETE = 'delete'
    RAISE = 'raise'

    # Readable aliases for the same policies
    REPORT_ONLY = 'report'
    DELETE_AND_REFETCH = 'delete'
    STRICT = 'raise'

    @classmethod
    def parse(cls, value: Any) -> 'CorruptionPolicy':
        """Normalize a policy name or value."""
        if isinstance(value, cls):
            return cls(value.value)
        if value is None:
            return cls.REPORT

        normalized = str(value).strip().lower().replace('-', '_').replace(' ', '_')
        aliases = {
            'report': cls.REPORT,
            'report_only': cls.REPORT,
            'ignore': cls.REPORT,
            'log': cls.REPORT,
            'delete': cls.DELETE,
            'delete_and_refetch': cls.DELETE,
            'refetch': cls.DELETE,
            'remove': cls.DELETE,
            'raise': cls.RAISE,
            'strict': cls.RAISE,
            'error': cls.RAISE,
        }
        try:
            return aliases[normalized]
        except KeyError:
            raise ValueError(
                f'Invalid corruption policy: {value!r}; expected one of '
                f'{[policy.value for policy in cls]}'
            ) from None

    def __str__(self) -> str:
        return self.value


CorruptionCallback = Callable[['CorruptEntryError'], Any]


class CorruptEntryError(Exception):
    """Error raised for a single cache entry that cannot be deserialized.

    The original exception is available as ``error`` (and as ``__cause__`` when raised), along
    with the cache key and the stage in which the failure occurred.
    """

    def __init__(
        self,
        key: str,
        stage: str,
        error: BaseException,
        policy: Optional[CorruptionPolicy] = None,
    ):
        self.key = key
        self.stage = stage
        self.error = error
        self.exception = error
        self.exception_type: Type[BaseException] = type(error)
        self.exception_class: Type[BaseException] = type(error)
        self.exception_name = type(error).__name__
        self.exception_category = self.exception_name
        self.policy = policy
        super().__init__(
            f'Corrupt cache entry (key={key!r}, stage={stage}, '
            f'error={self.exception_name}): {error}'
        )

    def as_dict(self) -> Dict[str, Any]:
        """Get a serializable summary for logging or diagnostics."""
        return {
            'key': self.key,
            'stage': self.stage,
            'exception_type': self.exception_name,
            'exception_category': self.exception_category,
            'message': str(self.error),
        }

    def __getitem__(self, field: str) -> Any:
        """Allow read-only attribute access by name, for convenience."""
        summary = self.as_dict()
        return summary[field] if field in summary else getattr(self, field)


def notify_corruption(
    callback: Optional[CorruptionCallback],
    event: CorruptEntryError,
    logger: Logger,
) -> None:
    """Call a corruption callback without allowing it to trigger recursive recovery."""
    if callback is None:
        return
    try:
        callback(event)
    except Exception as callback_error:
        logger.exception(
            'Corrupt entry callback failed '
            '(key=%r, stage=callback, error=%s): %s',
            event.key,
            type(callback_error).__name__,
            callback_error,
            extra={
                'cache_key': event.key,
                'stage': 'callback',
                'exception_type': type(callback_error).__name__,
            },
        )
