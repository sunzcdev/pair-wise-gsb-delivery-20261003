"""Run-scope and ownership helpers for scoped result storage."""

from contextvars import ContextVar, Token
from typing import TYPE_CHECKING, Any
from uuid import uuid4

if TYPE_CHECKING:  # pragma: no cover
    from taskiq.brokers.inmemory_broker import InmemoryResultBackend

current_scope_id: ContextVar[str | None] = ContextVar(
    "taskiq_current_scope_id",
    default=None,
)


def new_scope_id(prefix: str = "run") -> str:
    """Generate an opaque scope identifier."""
    return f"{prefix}-{uuid4().hex}"


def scope_id_of(scope: Any | None) -> str | None:
    """
    Resolve a scope identifier from a string, scope object, or broker-like object.

    The helper intentionally accepts the common names used by callers when
    describing a run, owner, or tenant boundary.
    """
    if scope is None:
        return None
    if isinstance(scope, str):
        return scope

    for attr in (
        "scope_id",
        "run_scope_id",
        "run_id",
        "owner_id",
        "tenant_id",
        "id",
    ):
        value = getattr(scope, attr, None)
        if value is not None and value is not scope:
            return value if isinstance(value, str) else str(value)

    return str(scope)


class _CompletedAwaitable:
    """An awaitable that is immediately complete and can be awaited many times."""

    __slots__ = ()

    def __await__(self) -> Any:
        async def completed() -> None:
            return None

        return completed().__await__()


COMPLETED = _CompletedAwaitable()


class ResultScope:
    """
    Explicit lifetime and ownership boundary for results.

    A scope is normally created by :class:`InmemoryResultBackend` or an
    :class:`InMemoryBroker`. Closing a scope releases results owned by it when
    no explicit cross-scope references remain.
    """

    def __init__(
        self,
        scope_id: str | None = None,
        name: str | None = None,
        *,
        backend: "InmemoryResultBackend[Any] | None" = None,
        cleanup_on_close: bool = True,
    ) -> None:
        self.scope_id = scope_id or name or new_scope_id()
        self.id = self.scope_id
        self.name = name
        self.backend = backend
        self.cleanup_on_close = cleanup_on_close
        self.closed = False
        self._token: Token[str | None] | None = None

    def bind(
        self,
        backend: "InmemoryResultBackend[Any]",
    ) -> "ResultScope":
        """Bind this scope to a result backend."""
        self.backend = backend
        return self

    def close(self) -> _CompletedAwaitable:
        """
        Close the scope immediately.

        The in-memory backend performs cleanup synchronously. Returning an
        already completed awaitable keeps both ``scope.close()`` and
        ``await scope.close()`` convenient and race-free for this backend.
        """
        if not self.closed:
            self.closed = True
            if self.backend is not None and self.cleanup_on_close:
                self.backend.release_scope_now(self.scope_id)
        return COMPLETED

    # Common lifecycle aliases. They return the same immediately-complete
    # awaitable, so callers may use either sync or async resource conventions.
    aclose = close
    shutdown = close
    cleanup = close
    dispose = close
    release = close

    async def __aenter__(self) -> "ResultScope":
        self._token = current_scope_id.set(self.scope_id)
        return self

    def __enter__(self) -> "ResultScope":
        self._token = current_scope_id.set(self.scope_id)
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        traceback: Any,
    ) -> None:
        if self._token is not None:
            current_scope_id.reset(self._token)
            self._token = None
        self.close()

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        traceback: Any,
    ) -> None:
        if self._token is not None:
            current_scope_id.reset(self._token)
            self._token = None
        await self.close()

    def __await__(self) -> Any:
        async def identity() -> "ResultScope":
            return self

        return identity().__await__()

    def __repr__(self) -> str:
        return (
            "ResultScope("
            f"scope_id={self.scope_id!r}, closed={self.closed}, "
            f"cleanup_on_close={self.cleanup_on_close}"
            ")"
        )


# Descriptive aliases for code that prefers "run" terminology.
RunScope = ResultScope
ResultRunScope = ResultScope
RunContext = ResultScope
ResultContext = ResultScope
