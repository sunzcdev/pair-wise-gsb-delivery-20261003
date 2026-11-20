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

import time

from .rate_limiter import RateLimiter, RateLimitResult


class WindowRateLimiter(RateLimiter):
    """A rate limiter that ensures that only `limit` operations may
    happen over some sliding window.

    Note:

      Windows are in seconds rather that milliseconds.  This is
      different from most durations and intervals used in Dramatiq,
      because keeping metadata at the millisecond level is far too
      expensive for most use cases.

    Examples:

      Ask a rejected caller when it's worth retrying:

      >>> with limiter.acquire(raise_on_failure=False, with_retry_at=True) as result:
      ...     if result:
      ...         print("Acquired.")
      ...     else:
      ...         print("Retry around", result.retry_at)

    Parameters:
      backend(RateLimiterBackend): The backend to use.
      key(str): The key to rate limit on.
      limit(int): The maximum number of operations per window per key.
      window(int): The window size in *seconds*.  The wider the
        window, the more expensive it is to maintain.
    """

    def __init__(self, backend, key, *, limit=1, window=1):
        assert limit >= 1, "limit must be positive"
        assert window >= 1, "window must be positive"

        super().__init__(backend, key)
        self.limit = limit
        self.window = window
        self.window_millis = window * 1000

    def _get_keys(self, timestamp=None):
        if timestamp is None:
            timestamp = int(time.time())
        return ["%s@%s" % (self.key, timestamp - i) for i in range(self.window)]

    def _acquire(self):
        return self._acquire_result().acquired

    def _acquire_result(self):
        # Sample the clock once so the bucket keys and the retry
        # boundary that is reported alongside them agree.
        timestamp = int(time.time())
        keys = self._get_keys(timestamp)
        acquired, retry_after = self.backend.incr_and_sum_with_retry_after(
            keys[0],
            lambda: keys,
            1,
            maximum=self.limit,
            ttl=self.window_millis,
        )
        if acquired:
            return RateLimitResult(True)

        # Rejections don't touch any bucket, so `retry_after` is only a
        # hint: the earliest whole second at which the same request
        # would have fit under the limit if nothing else changed.  It
        # doesn't reserve anything, so other callers may still win.
        retry_at = None if retry_after is None else timestamp + retry_after
        return RateLimitResult(False, retry_at=retry_at)

    def _release(self):
        pass
