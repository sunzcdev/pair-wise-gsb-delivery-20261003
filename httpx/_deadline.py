from __future__ import annotations

import time
import typing

from ._exceptions import TotalTimeout

if typing.TYPE_CHECKING:  # pragma: no cover
    from ._models import Request

# The clock used by `Deadline` instances by default. Bound at module level
# so that tests can monkeypatch it with a controllable clock.
monotonic = time.monotonic


class Deadline:
    """
    Tracks a total time budget for a single `Client.send()` call.

    The budget starts when the first request is sent, spans any redirects
    or authentication challenges (follow-up requests do not reset it), and
    ends once the response body has been fully read.

    When a deadline is used together with the per-operation `timeout`
    configuration, whichever expires first takes effect.
    """

    def __init__(
        self,
        timeout: float | None,
        *,
        clock: typing.Callable[[], float] | None = None,
    ) -> None:
        self._timeout = timeout
        self._clock = monotonic if clock is None else clock
        self._start = self._clock()

    @property
    def timeout(self) -> float | None:
        return self._timeout

    @property
    def is_active(self) -> bool:
        return self._timeout is not None

    @property
    def elapsed(self) -> float:
        return self._clock() - self._start

    @property
    def remaining(self) -> float | None:
        if self._timeout is None:
            return None
        return max(self._timeout - self.elapsed, 0.0)

    @property
    def expired(self) -> bool:
        return self._timeout is not None and self.elapsed >= self._timeout

    def check(self, request: Request, original_request: Request) -> None:
        """
        Raise `TotalTimeout` if the budget has been exhausted.

        The exception carries the original request, as first passed to
        `client.send()`, together with the request that was most recently
        in flight.
        """
        if self.expired:
            raise TotalTimeout(
                f"Total deadline of {self._timeout} seconds exceeded.",
                request=original_request,
                last_request=request,
            )

    def clamp_timeout(
        self, timeout: dict[str, float | None]
    ) -> dict[str, float | None]:
        """
        Clamp per-operation timeouts to the remaining budget, so that
        whichever of the per-phase timeout or the total deadline expires
        first takes effect.
        """
        remaining = self.remaining
        if remaining is None:
            return timeout
        return {
            key: remaining if value is None else min(value, remaining)
            for key, value in timeout.items()
        }
