# This file is a part of Dramatiq.
#
# Copyright (C) 2017,2018 CLEARTYPE SRL <bogdan@cleartype.io>
#
# Dramatiq is free software; you can redistribute it and/or modify it
# under the terms of the GNU Lesser General Public License as published by
# the Free Software Foundation, either version 3 of the License, or (at
# your option) any later version.
#
# Dramatiq is distributed in the hope that it will be useful, but WITHOUT
# ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
# FITNESS FOR A PARTICULAR PURPOSE. See the GNU Lesser General Public
# License for more details.
#
# You should have received a copy of the GNU Lesser General Public License
# along with this program.  If not, see <http://www.gnu.org/licenses/>.

from __future__ import annotations

from contextlib import contextmanager
from typing import Optional

from ..errors import RateLimitExceeded


class RateLimitResult:
    """The outcome of a rate limit acquisition attempt.

    Instances behave like the booleans they replace, so they're truthy
    exactly when the rate limit slot was acquired.

    Attributes:
      acquired(bool): Whether or not the rate limit slot was acquired.
      retry_at(Optional[float]): For rejected attempts, the Unix
        timestamp, in seconds, marking the start of the earliest whole
        second at which a new attempt is expected to succeed.  It uses
        the same clock as :func:`time.time`, the one the limiter uses to
        decide admission.  It is only a hint: obtaining it doesn't
        reserve any quota, other callers may still win the race and the
        real boundary may move.  It is ``None`` when the slot was
        acquired or when the limiter can't estimate a boundary.
    """

    __slots__ = ("acquired", "retry_at")

    def __init__(self, acquired: bool, retry_at: Optional[float] = None):
        self.acquired = bool(acquired)
        self.retry_at = retry_at

    def __bool__(self):
        return self.acquired

    def __eq__(self, other):
        if isinstance(other, RateLimitResult):
            return self.acquired == other.acquired and self.retry_at == other.retry_at

        # Behave like the bool this replaces, so comparisons written
        # against the old return value keep working.
        if isinstance(other, bool):
            return self.acquired is other

        return NotImplemented

    def __hash__(self):
        # Must agree with the bool comparisons above.
        return hash(self.acquired)

    def __repr__(self):
        return "RateLimitResult(acquired=%r, retry_at=%r)" % (self.acquired, self.retry_at)


class RateLimiter:
    """ABC for rate limiters.

    Examples:

      >>> from dramatiq.rate_limits.backends import RedisBackend

      >>> backend = RedisBackend()
      >>> limiter = ConcurrentRateLimiter(backend, "distributed-mutex", limit=1)

      >>> with limiter.acquire(raise_on_failure=False) as acquired:
      ...     if not acquired:
      ...         print("Mutex not acquired.")
      ...         return
      ...
      ...     print("Mutex acquired.")

    Parameters:
      backend(RateLimiterBackend): The rate limiting backend to use.
      key(str): The key to rate limit on.
    """

    def __init__(self, backend, key):
        self.backend = backend
        self.key = key

    def _acquire(self):  # pragma: no cover
        raise NotImplementedError

    def _acquire_result(self):
        """Attempt to acquire a slot under this rate limiter, reporting
        the outcome as a |RateLimitResult|.

        Limiters that can estimate when a rejected attempt is worth
        retrying should override this method.  The default wraps
        :meth:`_acquire` and never reports a retry boundary.
        """
        return RateLimitResult(self._acquire())

    def _release(self):  # pragma: no cover
        raise NotImplementedError

    @contextmanager
    def acquire(self, *, raise_on_failure=True, with_retry_at=False):
        """Attempt to acquire a slot under this rate limiter.

        Parameters:
          raise_on_failure(bool): Whether or not failures should raise an
            exception.  If this is false, the context manager will instead
            return a boolean value representing whether or not the rate
            limit slot was acquired.
          with_retry_at(bool): Whether or not failures should report when
            it's worth retrying.  When true, the context manager yields a
            |RateLimitResult| instead of a plain boolean.  Only limiters
            that can estimate that boundary fill in its ``retry_at``
            attribute (see |WindowRateLimiter|); the others leave it
            unset.  Defaults to false so the yielded value stays a bool.

        Returns:
          Union[bool, RateLimitResult]: Whether or not the slot could be
          acquired, or a result carrying that flag and the next time an
          attempt is expected to succeed when ``with_retry_at`` is true.
        """
        result = RateLimitResult(False)

        try:
            result = self._acquire_result()
            if raise_on_failure and not result:
                raise RateLimitExceeded("rate limit exceeded for key %(key)r" % vars(self))

            yield result if with_retry_at else bool(result)
        finally:
            if result:
                self._release()
