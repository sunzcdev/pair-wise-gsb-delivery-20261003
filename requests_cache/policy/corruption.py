"""Handling for cache entries that cannot be deserialized.

.. automodsumm:: requests_cache.policy.corruption
   :nosignatures:
"""

from __future__ import annotations

from enum import Enum
from json import JSONDecodeError
from logging import Logger
from pickle import PickleError
from typing import Any, Callable, Dict, Optional, Type

__all__ = [
    'STRUCTURE_ERRORS',
    'CorruptionCallback',
    'CorruptionPolicy',
    'CorruptEntryError',
    'get_exception_category',
    'log_corruption',
    'notify_corruption',
]

# Errors raised by cattrs while restructuring a cached response. Depending on the cattrs version
# and python version, only the base class may be available.
try:
    from cattrs.errors import CattrsError
except ImportError:  # pragma: no cover
    CattrsError = None  # type: ignore[assignment,misc]

#: Exceptions from cattrs that indicate a single value couldn't be restructured
STRUCTURE_ERRORS = (CattrsError,) if CattrsError is not None else ()


class CorruptionPolicy(str, Enum):
    """How to handle a single cache entry that cannot be deserialized.

    * ``report``: Log the failure and treat the entry as a cache miss, but leave it in place.
    * ``delete``: Log the failure, delete the entry (and any redirect aliases that resolve to it),
      and treat it as a cache miss so it can be fetched again.
    * ``raise``: Raise a :py:class:`CorruptEntryError`.
    """

    REPORT = 'report'
    DELETE = 'delete'
    RAISE = 'raise'

    @classmethod
    def parse(cls, value: Any) -> 'CorruptionPolicy':
        """Normalize a policy name, value, or existing policy object."""
        if isinstance(value, cls):
            return value
        if value is None:
            return cls.REPORT

        normalized = str(value).strip().lower().replace('-', '_').replace(' ', '_')
        aliases = {
            'report': cls.REPORT,
            'report_only': cls.REPORT,
            'log': cls.REPORT,
            'ignore': cls.REPORT,
            'delete': cls.DELETE,
            'delete_and_refetch': cls.DELETE,
            'delete_and_reload': cls.DELETE,
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
    """Error for a single cache entry that cannot be deserialized.

    This identifies which entry failed (``key``), at which ``stage`` the failure happened, and
    which kind of exception was raised. The original exception is available as ``error`` (and as
    ``__cause__`` when this is raised).

    Args:
        key: Cache key of the affected entry
        stage: Stage in which the failure occurred (for example ``deserialize``, ``delete``, or
            ``callback``)
        error: Original exception
        policy: Policy that was in effect, if any
    """

    def __init__(
        self,
        key: Any,
        stage: str,
        error: BaseException,
        policy: Optional[CorruptionPolicy] = None,
    ):
        self.key = key
        self.stage = stage
        self.error = error
        self.exception = error
        self.exception_type: Type[BaseException] = type(error)
        self.exception_name = type(error).__name__
        self.exception_category = get_exception_category(error)
        self.policy = policy
        super().__init__(
            f'Corrupt cache entry (key={key!r}, stage={stage}, '
            f'error={self.exception_name}): {error}'
        )

    def as_dict(self) -> Dict[str, Any]:
        """Get a serializable summary for logging or diagnostics"""
        return {
            'key': self.key,
            'stage': self.stage,
            'exception_type': self.exception_name,
            'exception_category': self.exception_category,
            'message': str(self.error),
        }


def get_exception_category(error: BaseException) -> str:
    """Get a coarse category for an exception raised while handling a cache entry"""
    if isinstance(error, PickleError):
        return 'pickle'
    if isinstance(error, (EOFError, JSONDecodeError, UnicodeError)):
        return 'decode'
    if CattrsError is not None and isinstance(error, CattrsError):
        return 'structure'
    if isinstance(error, ImportError):
        return 'import'
    if isinstance(error, (AttributeError, TypeError)):
        return 'type'
    if isinstance(error, ValueError):
        return 'value'
    return 'other'


def log_corruption(
    event: CorruptEntryError,
    logger: Logger,
    message: str = 'Unable to deserialize cache entry (key=%r, stage=%s, error=%s): %s',
    stage: Optional[str] = None,
) -> None:
    """Log a corruption event with structured ``cache_key``, ``stage``, and ``exception_type``"""
    stage = stage or event.stage
    logger.error(
        message,
        event.key,
        stage,
        event.exception_name,
        event.error,
        extra={
            'cache_key': event.key,
            'stage': stage,
            'exception_type': event.exception_name,
        },
    )


def notify_corruption(
    callback: Optional[CorruptionCallback],
    event: CorruptEntryError,
    logger: Logger,
) -> None:
    """Call a corruption callback without allowing it to trigger recursive recovery.

    A failing callback is logged (with ``stage=callback``) and otherwise ignored, so it can't turn
    a handled entry into an unhandled error or start another recovery attempt.
    """
    if callback is None:
        return
    try:
        callback(event)
    except Exception as callback_error:
        logger.error(
            'Corrupt entry callback failed (key=%r, stage=callback, error=%s): %s',
            event.key,
            type(callback_error).__name__,
            callback_error,
            extra={
                'cache_key': event.key,
                'stage': 'callback',
                'exception_type': type(callback_error).__name__,
            },
        )
        logger.debug(callback_error, exc_info=True)
