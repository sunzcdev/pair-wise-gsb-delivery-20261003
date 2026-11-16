"""
Optional per-origin concurrency limits.

A single client is typically used to collect from many different sites at the
same time. A client-wide connection pool limit does not stop a single site from
using up all of the available capacity, so this module also supports limiting
the number of *in-flight requests* on a per-origin basis.

An origin permit is acquired just before a request is about to be sent, which is
before its request hooks run and just before it is handed over to the transport.
It is held until the response body has been consumed or the response has been
closed. Permits are handed over in first-in first-out order for any given
origin, while waits on one origin never hold up requests to any other origin.

```python
client = httpx.Client(limits=httpx.Limits(max_connections_per_origin=2))
```
"""

from __future__ import annotations

import collections
import threading
import typing

from ._exceptions import ClientClosed
from ._urls import URL

__all__ = ["AsyncOriginLimiter", "OriginLimiter", "OriginPermit", "origin_key"]

# The (scheme, host, port) triple that identifies an origin.
Origin = typing.Tuple[str, str, typing.Optional[int]]

_DEFAULT_PORTS = {"http": 80, "https": 443}


def origin_key(url: URL) -> Origin:
    """
    Return the origin that the given URL belongs to.
    """
    port = url.port
    if port is None:
        port = _DEFAULT_PORTS.get(url.scheme)
    return (url.scheme, url.host, port)


class _Waiter:
    """
    A queued acquirer, waiting for a permit to be handed over to it.
    """

    __slots__ = ("granted",)

    def __init__(self) -> None:
        self.granted = False

    def wake(self) -> None:  # pragma: no cover - overridden
        raise NotImplementedError


class _SyncWaiter(_Waiter):
    __slots__ = ("event",)

    def __init__(self) -> None:
        super().__init__()
        self.event = threading.Event()

    def wake(self) -> None:
        self.event.set()


class _AsyncWaiter(_Waiter):
    __slots__ = ("event",)

    def __init__(self) -> None:
        super().__init__()
        # `anyio` is only imported for async usage, keeping it out of the import
        # path for applications that only use `httpx.Client`.
        import anyio

        self.event = anyio.Event()

    def wake(self) -> None:
        self.event.set()


class _OriginState:
    """
    The permit bookkeeping for a single origin.
    """

    __slots__ = ("in_flight", "waiters")

    def __init__(self) -> None:
        self.in_flight = 0
        self.waiters: typing.Deque[_Waiter] = collections.deque()


class OriginPermit:
    """
    A permit for one in-flight request against a given origin.

    The permit is released when the response body is exhausted, when the
    response is closed, or when the request fails to send. Releasing a permit
    more than once is harmless.
    """

    __slots__ = ("_limiter", "_origin", "_released")

    def __init__(self, limiter: _BaseOriginLimiter, origin: Origin) -> None:
        self._limiter = limiter
        self._origin = origin
        self._released = False

    @property
    def released(self) -> bool:
        return self._released

    def release(self) -> None:
        self._limiter.release(self)


class _BaseOriginLimiter:
    """
    Shared permit bookkeeping for the sync and async limiters.

    All state is guarded by a plain lock, so that the critical sections are
    identical for both flavors. Waiting is the only part that differs, and it
    always happens outside of the lock.
    """

    def __init__(self, max_concurrency: int) -> None:
        if isinstance(max_concurrency, bool) or not isinstance(max_concurrency, int):
            raise TypeError(
                "The maximum number of in-flight requests per origin must be an "
                f"integer, but got {max_concurrency!r}."
            )
        if max_concurrency < 1:
            raise ValueError(
                "The maximum number of in-flight requests per origin must be "
                f"at least 1, but got {max_concurrency!r}."
            )
        self._max_concurrency = max_concurrency
        self._lock = threading.Lock()
        self._states: typing.Dict[Origin, _OriginState] = {}
        self._closed = False

    @property
    def max_concurrency(self) -> int:
        return self._max_concurrency

    @property
    def is_closed(self) -> bool:
        return self._closed

    def close(self) -> None:
        """
        Wake up any waiting callers, and reject any new acquisitions.

        Permits that are still held by responses are released when those
        responses are closed. Waiting callers are not handed permits, so that
        closing the client cannot leave an unbounded queue of pending requests
        behind.
        """
        with self._lock:
            if self._closed:
                return
            self._closed = True

            waiters: typing.List[_Waiter] = []
            for origin, state in list(self._states.items()):
                waiters.extend(state.waiters)
                state.waiters.clear()
                if state.in_flight == 0:
                    del self._states[origin]

        for waiter in waiters:
            waiter.wake()

    def release(self, permit: OriginPermit) -> None:
        """
        Return a permit, handing it over to the next queued caller if any.
        """
        with self._lock:
            if permit.released:
                return
            permit._released = True
            waiter = self._release_locked(permit._origin)

        if waiter is not None:
            waiter.wake()

    def _raise_if_closed_locked(self) -> None:
        if self._closed:
            raise ClientClosed("Cannot send a request, as the client has been closed.")

    def _state_for_locked(self, origin: Origin) -> _OriginState:
        state = self._states.get(origin)
        if state is None:
            state = self._states[origin] = _OriginState()
        return state

    def _try_acquire_locked(self, origin: Origin) -> typing.Optional[OriginPermit]:
        """
        Return a permit if the origin is below its limit, otherwise `None`.
        """
        state = self._states.get(origin)
        if state is not None and state.waiters:
            # Never let a new arrival jump the queue for this origin.
            return None

        if state is not None and state.in_flight >= self._max_concurrency:
            return None

        state = self._state_for_locked(origin)
        state.in_flight += 1
        return OriginPermit(self, origin)

    def _release_locked(self, origin: Origin) -> typing.Optional[_Waiter]:
        """
        Hand the permit over to the next waiter, or give up the slot.

        Returns the waiter that must be woken up, if any.
        """
        state = self._states.get(origin)
        if state is None:  # pragma: no cover - defensive
            return None

        if state.waiters:
            # Passing the permit along keeps the number of in-flight requests
            # constant, and keeps the per-origin ordering first-in first-out.
            waiter = state.waiters.popleft()
            waiter.granted = True
            return waiter

        state.in_flight -= 1
        if state.in_flight == 0:
            del self._states[origin]
        return None

    def _abandon_locked(
        self, origin: Origin, waiter: _Waiter
    ) -> typing.Optional[_Waiter]:
        """
        Give up on a waiter, which was either cancelled or interrupted.
        """
        state = self._states.get(origin)
        if state is None:
            # The client was closed and the bookkeeping for this origin has
            # already been dropped, or the waiter was already settled.
            return None

        if waiter.granted:
            # The permit was handed over but will not be used, so pass it on.
            return self._release_locked(origin)

        try:
            state.waiters.remove(waiter)
        except ValueError:  # pragma: no cover - already removed on close
            pass
        if state.in_flight == 0 and not state.waiters:
            del self._states[origin]
        return None

    def _finish_wait_locked(
        self, origin: Origin, waiter: _Waiter
    ) -> tuple[OriginPermit | None, _Waiter | None]:
        """
        Translate the outcome of a wait into a permit, or a terminal error.

        Returns the permit to use, if any, along with a waiter that must be
        woken up after the lock has been released. A `None` permit means the
        client was closed, so the caller must raise `ClientClosed` once any
        handed-over waiter has been woken up.
        """
        if waiter.granted and self._closed:
            # Closing the client won the race with the hand-over, so the permit
            # must be given back rather than used for a new request.
            handed_over = self._release_locked(origin)
            waiter.granted = False
            return None, handed_over

        if not waiter.granted:
            return None, None

        return OriginPermit(self, origin), None


class OriginLimiter(_BaseOriginLimiter):
    """
    A blocking, per-origin limit on in-flight requests.
    """

    def acquire(self, url: URL) -> OriginPermit:
        origin = origin_key(url)

        with self._lock:
            self._raise_if_closed_locked()
            permit = self._try_acquire_locked(origin)
            if permit is not None:
                return permit
            waiter = _SyncWaiter()
            self._state_for_locked(origin).waiters.append(waiter)

        try:
            waiter.event.wait()
        except BaseException:
            with self._lock:
                handed_over = self._abandon_locked(origin, waiter)
            if handed_over is not None:
                handed_over.wake()
            raise

        with self._lock:
            permit, handed_over = self._finish_wait_locked(origin, waiter)
        if handed_over is not None:
            handed_over.wake()
        if permit is None:
            raise ClientClosed("Cannot send a request, as the client has been closed.")
        return permit


class AsyncOriginLimiter(_BaseOriginLimiter):
    """
    An async, per-origin limit on in-flight requests.
    """

    async def acquire(self, url: URL) -> OriginPermit:
        origin = origin_key(url)

        with self._lock:
            self._raise_if_closed_locked()
            permit = self._try_acquire_locked(origin)
            if permit is not None:
                return permit
            waiter = _AsyncWaiter()
            self._state_for_locked(origin).waiters.append(waiter)

        try:
            await waiter.event.wait()
        except BaseException:
            # The task was cancelled while queued, so it must not end up
            # consuming a permit, and must not be left in the queue.
            with self._lock:
                handed_over = self._abandon_locked(origin, waiter)
            if handed_over is not None:
                handed_over.wake()
            raise

        with self._lock:
            permit, handed_over = self._finish_wait_locked(origin, waiter)
        if handed_over is not None:
            handed_over.wake()
        if permit is None:
            raise ClientClosed("Cannot send a request, as the client has been closed.")
        return permit
