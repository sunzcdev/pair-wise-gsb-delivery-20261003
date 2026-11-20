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


def retry_delay_from_counts(counts, amount, maximum):
    """Work out how many of the oldest counts have to leave the window
    before an operation of `amount` units fits under `maximum`.

    Backends use this to turn the counts that a rate limiter like
    |WindowRateLimiter| keeps per bucket into a retry hint.  `counts`
    must be ordered from newest to oldest, which is the order the
    limiter's key list uses, so the oldest entry rolls out of the
    window first.

    Parameters:
      counts(list[int]): The counts of the buckets in the window,
        ordered from newest to oldest.
      amount(int): The amount the rejected operation wanted to add.
      maximum(int): The maximum sum of the buckets.

    Returns:
      int: The number of whole buckets that must be dropped.  This is
      always at least 1 because it's only computed after a rejection.
    """
    remaining = sum(counts) + amount
    delay = 0
    for count in reversed(counts):
        if remaining <= maximum:
            break

        remaining -= count
        delay += 1

    return max(delay, 1)


class RateLimiterBackend:
    """ABC for rate limiter backends."""

    def add(self, key, value, ttl):  # pragma: no cover
        """Add a key to the backend iff it doesn't exist.

        Parameters:
          key(str): The key to add.
          value(int): The value to add.
          ttl(int): The max amount of time in milliseconds the key can
            live in the backend for.
        """
        raise NotImplementedError

    def incr(self, key, amount, maximum, ttl):  # pragma: no cover
        """Atomically increment a key in the backend up to the given
        maximum.

        Parameters:
          key(str): The key to increment.
          amount(int): The amount to increment the value by.
          maximum(int): The maximum amount the value can have.
          ttl(int): The max amount of time in milliseconds the key can
            live in the backend for.

        Returns:
          bool: True if the key was successfully incremented.
        """
        raise NotImplementedError

    def decr(self, key, amount, minimum, ttl):  # pragma: no cover
        """Atomically decrement a key in the backend up to the given
        maximum.

        Parameters:
          key(str): The key to decrement.
          amount(int): The amount to decrement the value by.
          minimum(int): The minimum amount the value can have.
          ttl(int): The max amount of time in milliseconds the key can
            live in the backend for.

        Returns:
          bool: True if the key was successfully decremented.
        """
        raise NotImplementedError

    def incr_and_sum(self, key, keys, amount, maximum, ttl):  # pragma: no cover
        """Atomically increment a key unless the sum of keys is greater
        than the given maximum.

        Parameters:
          key(str): The key to increment.
          keys(callable): A callable to return the list of keys to be
            summed over.
          amount(int): The amount to decrement the value by.
          maximum(int): The maximum sum of the keys.
          ttl(int): The max amount of time in milliseconds the key can
            live in the backend for.

        Returns:
          bool: True if the key was successfully incremented.
        """
        raise NotImplementedError

    def incr_and_sum_with_retry_after(self, key, keys, amount, maximum, ttl):
        """Like :meth:`incr_and_sum`, but also report how long a
        rejection should last.

        The default implementation falls back to :meth:`incr_and_sum`
        and reports `None`, so backends that can't estimate a boundary
        stay usable.  Overrides must read the buckets and decide the
        outcome under one consistent view, so the hint matches the
        counts that produced the rejection.

        Parameters:
          key(str): The key to increment.
          keys(callable): A callable to return the list of keys to be
            summed over.
          amount(int): The amount to increment the value by.
          maximum(int): The maximum sum of the keys.
          ttl(int): The max amount of time in milliseconds the key can
            live in the backend for.

        Returns:
          tuple[bool, Optional[int]]: Whether the key was incremented
          and, when it wasn't, how many buckets must roll out of the
          window before the same request is expected to fit again.
          The second item is `None` when the backend can't tell.
        """
        return self.incr_and_sum(key, keys, amount, maximum, ttl), None

    def wait(self, key, timeout):  # pragma: no cover
        """Wait until an event is published to the given key or the
        timeout expires.  This is used to implement efficient blocking
        against a synchronized resource.

        Parameters:
          key(str): The key to wait on.
          timeout(int): The timeout in milliseconds.

        Returns:
          bool: True if en event was published before the timeout.
        """
        raise NotImplementedError

    def wait_notify(self, key, ttl):  # pragma: no cover
        """Notify parties wait()ing on a key that an event has
        occurred.  The default implementation is a no-op.

        Parameters:
          key(str): The key to notify on.
          ttl(int): The max amount of time in milliseconds that the
            notification should exist for.

        Returns:
          None
        """
