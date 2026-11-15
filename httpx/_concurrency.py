"""
Per-origin concurrency limiting for client requests.

The limiter deliberately tracks in-flight *responses* rather than connections.
An origin permit is acquired immediately before a request is handed to the
transport, and is only returned once the response body has been consumed or
closed.
"""

from __future__ import annotations

import threading
import typing
from collections import deque

import anyio

Origin = typing.Tuple[str, str, int | None]

CLIENT_CLOSED_MESSAGE = "Cannot send a request, as the client has been closed."


class _OriginState:
    __slots__ = ("active", "waiters")

    def __init__(self) -> None:
        self.active = 0
        self.waiters: typing.Deque[typing.Any] = deque()


class _Waiter:
    __slots__ = ("event", "granted", "closed")

    def __init__(self, event: typing.Any) -> None:
        self.event = event
        self.granted = False
        self.closed = False


class _Permit:
    __slots__ = ("_limiter", "_origin", "_lock", "_released")

    def __init__(self, limiter: "_BaseOriginLimiter", origin: Origin) -> None:
        self._limiter = limiter
        self._origin = origin
        self._lock = threading.Lock()
        self._released = False

    def release(self) -> None:
        with self._lock:
            if self._released:
                return
            self._released = True
        self._limiter._release(self._origin)


class _NullPermit:
    __slots__ = ()

    def release(self) -> None:
        pass


class _BaseOriginLimiter:
    def __init__(self, max_connections_per_origin: int | None) -> None:
        if max_connections_per_origin is not None and max_connections_per_origin < 1:
            raise ValueError("max_connections_per_origin must be at least 1, or None.")

        self._max = max_connections_per_origin
        self._lock = threading.Lock()
        self._states: dict[Origin, _OriginState] = {}
        self._closed = False

    def _grant_waiters(self, origin: Origin, state: _OriginState) -> None:
        assert self._max is not None
        while state.active < self._max and state.waiters:
            waiter = state.waiters.popleft()
            if waiter.closed:
                continue
            state.active += 1
            waiter.granted = True
            waiter.event.set()

    def _abandon(self, origin: Origin, state: _OriginState, waiter: _Waiter) -> None:
        if waiter.granted:
            waiter.granted = False
            state.active -= 1
            if not self._closed:
                self._grant_waiters(origin, state)
        else:
            try:
                state.waiters.remove(waiter)
            except ValueError:
                pass
        self._cleanup(origin, state)

    def _cleanup(self, origin: Origin, state: _OriginState) -> None:
        if state.active == 0 and not state.waiters:
            self._states.pop(origin, None)

    def _release(self, origin: Origin) -> None:
        with self._lock:
            state = self._states.get(origin)
            if state is None:
                return

            state.active -= 1
            if not self._closed:
                self._grant_waiters(origin, state)
            self._cleanup(origin, state)

    def close(self) -> None:
        with self._lock:
            if self._closed:
                return
            self._closed = True

            for origin, state in list(self._states.items()):
                waiters = list(state.waiters)
                state.waiters.clear()
                for waiter in waiters:
                    waiter.closed = True
                    waiter.granted = False
                    waiter.event.set()
                if state.active == 0:
                    self._states.pop(origin, None)


class SyncOriginLimiter(_BaseOriginLimiter):
    def acquire(self, origin: Origin) -> _Permit | _NullPermit:
        if self._max is None:
            return _NullPermit()

        waiter = _Waiter(threading.Event())
        with self._lock:
            if self._closed:
                raise RuntimeError(CLIENT_CLOSED_MESSAGE)

            state = self._states.get(origin)
            if state is None:
                state = self._states[origin] = _OriginState()
            state.waiters.append(waiter)
            self._grant_waiters(origin, state)

        if not waiter.granted:
            try:
                waiter.event.wait()
            except BaseException:
                with self._lock:
                    self._abandon(origin, state, waiter)
                raise

        with self._lock:
            if self._closed:
                self._abandon(origin, state, waiter)
                raise RuntimeError(CLIENT_CLOSED_MESSAGE)
            return _Permit(self, origin)


class AsyncOriginLimiter(_BaseOriginLimiter):
    async def acquire(self, origin: Origin) -> _Permit | _NullPermit:
        if self._max is None:
            return _NullPermit()

        waiter = _Waiter(anyio.Event())
        with self._lock:
            if self._closed:
                raise RuntimeError(CLIENT_CLOSED_MESSAGE)

            state = self._states.get(origin)
            if state is None:
                state = self._states[origin] = _OriginState()
            state.waiters.append(waiter)
            self._grant_waiters(origin, state)

        if not waiter.granted:
            try:
                await waiter.event.wait()
            except BaseException:
                with self._lock:
                    self._abandon(origin, state, waiter)
                raise

        with self._lock:
            if self._closed:
                self._abandon(origin, state, waiter)
                raise RuntimeError(CLIENT_CLOSED_MESSAGE)
            return _Permit(self, origin)
