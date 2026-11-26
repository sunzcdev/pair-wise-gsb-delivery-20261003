"""Message scoped sharing for dependencies.

This module provides an *explicit* sharing declaration for Taskiq
dependencies.  Dependencies declared as shared are constructed at most once
per processing message for the same share key (which normally encodes the
resource and the invocation tenant/parameters).  Different parameters and
explicitly independent dependencies keep constructing their own instances.

The public surface is intentionally small:

* :class:`SharedScope` -- the per message cache and LIFO teardown stack.
* :func:`shared_scope` -- bind a scope for the current context (async CM).
* :func:`current_scope` -- get the scope bound to the current context.
* :func:`SharedDepends` / :func:`Shared` -- the explicit declaration that is
  used as a ``TaskiqDepends`` compatible default.
* :func:`shared_dependency` -- alias of :func:`SharedDepends`.

Ordinary :class:`taskiq.TaskiqDepends` declarations without sharing options
keep their original behaviour.
"""

from __future__ import annotations

import asyncio
import inspect
from contextvars import ContextVar
from typing import Any, Callable, Dict, Optional, get_type_hints

from taskiq_dependencies import Depends
from taskiq_dependencies.dependency import Dependency
from taskiq_dependencies.utils import isasynccontextmanager, iscontextmanager

__all__ = [
    "Share",
    "ShareKey",
    "Shared",
    "SharedDependency",
    "SharedDepends",
    "SharedScope",
    "TaskiqDepends",
    "TaskiqSharedDepends",
    "current_scope",
    "reset_current_scope",
    "set_current_scope",
    "shared",
    "shared_depends",
    "shared_dependency",
    "shared_scope",
]


_current_scope: "ContextVar[Optional[SharedScope]]" = ContextVar(
    "taskiq_shared_scope",
    default=None,
)


class ShareKey:
    """Identity of a shared instance.

    The key compares an optional explicit name and the construction
    parameters.  Unhashable parameters are supported because equality (not a
    dict lookup of the raw parameters) is used to find an existing entry.
    """

    __slots__ = ("name", "resource", "params", "_hash")

    def __init__(
        self,
        name: Any = None,
        resource: Any = None,
        params: Optional[Dict[str, Any]] = None,
    ) -> None:
        self.name = name
        self.resource = resource
        self.params = dict(params or {})
        if name is not None:
            self._hash = hash(("named", _stable_hash_token(name)))
        else:
            # Resource identity plus parameters.  Parameters participate in
            # equality but a coarse hash is enough to bucket candidates.
            try:
                resource_hash = hash(resource)
            except TypeError:
                resource_hash = id(resource)
            self._hash = hash(("resource", resource_hash))

    def __hash__(self) -> int:
        return self._hash

    def __eq__(self, other: object) -> bool:
        if not isinstance(other, ShareKey):
            return NotImplemented
        if self.name is not None or other.name is not None:
            return self.name == other.name
        return (
            self.resource == other.resource
            and self.params == other.params
        )

    def __repr__(self) -> str:  # pragma: no cover - debugging helper
        if self.name is not None:
            return f"ShareKey(name={self.name!r})"
        return f"ShareKey(resource={getattr(self.resource, '__name__', self.resource)!r}, params={self.params!r})"


def _stable_hash_token(value: Any) -> int:
    """Best effort stable hash for possibly unhashable user keys."""
    try:
        return hash(value)
    except TypeError:
        return hash(repr(value))


def coerce_key(
    shared: Any = None,
    explicit_key: Any = None,
    dependency: Any = None,
    params: Optional[Dict[str, Any]] = None,
) -> ShareKey:
    """Build a :class:`ShareKey` from the declaration options."""
    name = explicit_key
    if name is None and isinstance(shared, ShareKey):
        return shared
    if name is None and shared is not None and shared is not True:
        # A string / number / tuple key passed as ``shared=...``.
        name = shared
    if isinstance(name, ShareKey):
        return name
    return ShareKey(name=name, resource=dependency, params=params)


class _Entry:
    """An entered shared resource and how to close it."""

    __slots__ = ("key", "value", "closable", "kind")

    def __init__(self, key: ShareKey, value: Any, closable: Any, kind: str) -> None:
        self.key = key
        self.value = value
        self.closable = closable
        # kind: "asyncgen", "syncgen", "async_cm", "sync_cm", "value"
        self.kind = kind


async def _open_async(
    factory: Callable[..., Any],
    params: Dict[str, Any],
) -> _Entry:
    """Call ``factory`` and enter the produced resource.

    If construction or entering raises, no entry is returned, so callers can
    never cache a half constructed handle.
    """
    produced = factory(**params)
    if asyncio.iscoroutine(produced):
        produced = await produced

    if inspect.isasyncgen(produced):
        value = await produced.__anext__()
        return _Entry(None, value, produced, "asyncgen")  # type: ignore[arg-type]
    if inspect.isgenerator(produced):
        value = next(produced)
        return _Entry(None, value, produced, "syncgen")  # type: ignore[arg-type]
    if isasynccontextmanager(produced) or (
        hasattr(produced, "__aenter__") and hasattr(produced, "__aexit__")
    ):
        value = await produced.__aenter__()
        return _Entry(None, value, produced, "async_cm")  # type: ignore[arg-type]
    if iscontextmanager(produced) or (
        hasattr(produced, "__enter__") and hasattr(produced, "__exit__")
    ):
        value = produced.__enter__()
        return _Entry(None, value, produced, "sync_cm")  # type: ignore[arg-type]
    return _Entry(None, produced, None, "value")  # type: ignore[arg-type]


def _open_sync(
    factory: Callable[..., Any],
    params: Dict[str, Any],
) -> _Entry:
    """Sync counterpart of :func:`_open_async`."""
    produced = factory(**params)
    if inspect.isgenerator(produced):
        value = next(produced)
        return _Entry(None, value, produced, "syncgen")  # type: ignore[arg-type]
    if inspect.isasyncgen(produced) or asyncio.iscoroutine(produced):
        raise RuntimeError(
            "Async dependencies cannot be used in sync context. "
            "Please use async context instead.",
        )
    if iscontextmanager(produced) or (
        hasattr(produced, "__enter__") and hasattr(produced, "__exit__")
    ):
        value = produced.__enter__()
        return _Entry(None, value, produced, "sync_cm")  # type: ignore[arg-type]
    return _Entry(None, produced, None, "value")  # type: ignore[arg-type]


async def _aclose_entry_async(entry: _Entry, exc: Optional[BaseException]) -> None:
    """Close one entry, mirroring taskiq_dependencies teardown semantics."""
    closable = entry.closable
    if closable is None:
        return
    if entry.kind == "asyncgen":
        if exc is not None:
            try:
                await closable.athrow(type(exc), exc, exc.__traceback__)
            except StopAsyncIteration:
                return
            except BaseException:
                # Teardown errors must not prevent other resources closing.
                return
            return
        async for _ in closable:
            pass
        return
    if entry.kind == "syncgen":
        if exc is not None:
            try:
                closable.throw(type(exc), exc, exc.__traceback__)
            except StopIteration:
                return
            except BaseException:
                return
            return
        for _ in closable:
            pass
        return
    if entry.kind == "async_cm":
        if exc is not None:
            await closable.__aexit__(type(exc), exc, exc.__traceback__)
        else:
            await closable.__aexit__(None, None, None)
        return
    if entry.kind == "sync_cm":
        if exc is not None:
            closable.__exit__(type(exc), exc, exc.__traceback__)
        else:
            closable.__exit__(None, None, None)


def _close_entry_sync(entry: _Entry, exc: Optional[BaseException]) -> None:
    closable = entry.closable
    if closable is None:
        return
    if entry.kind == "syncgen":
        if exc is not None:
            try:
                closable.throw(type(exc), exc, exc.__traceback__)
            except StopIteration:
                return
            except BaseException:
                return
            return
        for _ in closable:
            pass
        return
    if entry.kind == "sync_cm":
        closable.__exit__(
            type(exc) if exc else None,
            exc,
            exc.__traceback__ if exc else None,
        )
        return
    if entry.kind in {"asyncgen", "async_cm"}:
        raise RuntimeError(
            "Async dependencies cannot be closed from sync context.",
        )


class SharedScope:
    """Cache and teardown stack for resources shared inside one message.

    The scope keeps two structures:

    * a mapping of share keys to entered values (so repeated requests reuse
      the handle), and
    * a stack of successfully entered entries (so teardown happens in the
      reverse order of the actual entry).

    A value is only cached *after* its resource has been constructed and
    entered successfully.  A failing construction therefore leaves no
    reusable handle behind.
    """

    def __init__(self) -> None:
        self._values: Dict[ShareKey, Any] = {}
        self._entries: Dict[ShareKey, _Entry] = {}
        self._stack: list[_Entry] = []
        self._closed = False

    # -- context var helpers -------------------------------------------------
    @classmethod
    def current(cls) -> "Optional[SharedScope]":
        """Return the scope bound to the current context, if any."""
        return _current_scope.get()

    def bind(self) -> Any:
        """Return a context manager that binds this scope."""
        return _BoundScope(self)

    # -- lookup / construction ---------------------------------------------
    def _find_existing(self, key: ShareKey) -> Optional[_Entry]:
        entry = self._entries.get(key)
        if entry is not None:
            return entry
        # Fallback for keys that hash coarsely / compare equal.
        for candidate in self._stack:
            if candidate.key == key:
                return candidate
        return None

    async def get_or_create_async(
        self,
        key: Any,
        factory: Callable[..., Any],
        params: Optional[Dict[str, Any]] = None,
    ) -> Any:
        """Return the shared value for ``key``, constructing it once."""
        if not isinstance(key, ShareKey):
            key = ShareKey(name=key)
        existing = self._find_existing(key)
        if existing is not None:
            return existing.value
        entry = await _open_async(factory, dict(params or {}))
        entry.key = key
        # Publish only after a fully successful entry.
        self._entries[key] = entry
        self._values[key] = entry.value
        self._stack.append(entry)
        return entry.value

    # Ergonomic aliases (async).
    get_or_create = get_or_create_async
    acquire = get_or_create_async
    provide = get_or_create_async
    get_async = get_or_create_async

    def get_or_create_sync(
        self,
        key: Any,
        factory: Callable[..., Any],
        params: Optional[Dict[str, Any]] = None,
    ) -> Any:
        """Sync version of :meth:`get_or_create_async`."""
        if not isinstance(key, ShareKey):
            key = ShareKey(name=key)
        existing = self._find_existing(key)
        if existing is not None:
            return existing.value
        entry = _open_sync(factory, dict(params or {}))
        entry.key = key
        self._entries[key] = entry
        self._values[key] = entry.value
        self._stack.append(entry)
        return entry.value

    # Ergonomic aliases (sync).
    acquire_sync = get_or_create_sync
    provide_sync = get_or_create_sync
    get_sync = get_or_create_sync

    # -- teardown ------------------------------------------------------------
    async def aclose(self, exc: Optional[BaseException] = None) -> None:
        """Close every entered resource in reverse entry order."""
        if self._closed:
            return
        self._closed = True
        while self._stack:
            entry = self._stack.pop()
            try:
                await _aclose_entry_async(entry, exc)
            except BaseException:  # noqa: BLE001 - teardown must continue
                continue
        self._entries.clear()
        self._values.clear()

    def close(self, exc: Optional[BaseException] = None) -> None:
        """Sync close, reverse entry order."""
        if self._closed:
            return
        self._closed = True
        while self._stack:
            entry = self._stack.pop()
            try:
                _close_entry_sync(entry, exc)
            except BaseException:  # noqa: BLE001 - teardown must continue
                continue
        self._entries.clear()
        self._values.clear()

    async def __aenter__(self) -> "SharedScope":
        self._token = _current_scope.set(self)  # type: ignore[attr-defined]
        return self

    async def __aexit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        try:
            await self.aclose(exc if isinstance(exc, BaseException) else None)
        finally:
            _current_scope.reset(self._token)  # type: ignore[attr-defined]

    def __enter__(self) -> "SharedScope":
        self._token = _current_scope.set(self)  # type: ignore[attr-defined]
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        try:
            self.close(exc if isinstance(exc, BaseException) else None)
        finally:
            _current_scope.reset(self._token)  # type: ignore[attr-defined]


class _BoundScope:
    """Context manager returned by :meth:`SharedScope.bind`."""

    def __init__(self, scope: SharedScope) -> None:
        self.scope = scope
        self._token: Any = None

    async def __aenter__(self) -> SharedScope:
        self._token = _current_scope.set(self.scope)
        return self.scope

    async def __aexit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        try:
            await self.scope.aclose(
                exc if isinstance(exc, BaseException) else None,
            )
        finally:
            _current_scope.reset(self._token)

    def __enter__(self) -> SharedScope:
        self._token = _current_scope.set(self.scope)
        return self.scope

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        try:
            self.scope.close(exc if isinstance(exc, BaseException) else None)
        finally:
            _current_scope.reset(self._token)


def shared_scope() -> _BoundScope:
    """Create and bind a fresh :class:`SharedScope`.

    Works as both a sync and an async context manager.
    """
    return SharedScope().bind()


def current_scope() -> Optional[SharedScope]:
    """Return the :class:`SharedScope` for the current message, if any."""
    return _current_scope.get()


def set_current_scope(scope: Optional[SharedScope]) -> Any:
    """Bind a scope to the current context, returning its reset token."""
    return _current_scope.set(scope)


def reset_current_scope(token: Any) -> None:
    """Reset the scope context variable using a token from set."""
    _current_scope.reset(token)


# ---------------------------------------------------------------------------
# Declarative integration with taskiq_dependencies
# ---------------------------------------------------------------------------


def _make_runner(
    key: ShareKey,
    factory: Callable[..., Any],
    params: Optional[Dict[str, Any]] = None,
) -> Callable[..., Any]:
    """Create the dependency callable for one sharing declaration.

    The runner closes over its own ``factory`` so redefining a factory in
    another test or message never leaves stale callables.  When a message
    :class:`SharedScope` is active the scope de-duplicates by ``key`` and owns
    the lifecycle; without one the runner owns its handle itself.
    """
    static_params = dict(params or {})

    async def _shared_runner(**resolved_kwargs: Any) -> Any:
        # Statically supplied kwargs seed the call; dependency resolved
        # parameters take precedence, matching taskiq_dependencies behaviour.
        call_kwargs = {**static_params, **resolved_kwargs}
        lookup_key = key
        if key.name is None:
            # Without an explicit name the parameters (including the
            # invocation tenant resolved from another dependency) participate
            # in the identity.
            lookup_key = ShareKey(name=None, resource=factory, params=call_kwargs)
        scope = _current_scope.get()
        if scope is not None:
            # The scope owns caching and teardown.  On a key hit the scope
            # returns the existing handle without invoking this factory.
            value = await scope.get_or_create_async(
                lookup_key,
                factory,
                call_kwargs,
            )
            try:
                yield value
            except BaseException:  # noqa: BLE001 - teardown is scope owned
                # The scope closes the underlying resource with the same
                # error afterwards; do not surface a duplicate teardown error.
                return
            return

        # Standalone usage (no Receiver bound scope): this runner owns the
        # handle and closes it when the dependency context tears it down.
        entry = await _open_async(factory, call_kwargs)
        try:
            yield entry.value
        except BaseException as exc:  # propagated by dependency athrow
            await _aclose_entry_async(entry, exc)
            raise
        else:
            await _aclose_entry_async(entry, None)

    _shared_runner.__name__ = getattr(factory, "__name__", "shared_runner")
    _shared_runner.__qualname__ = _shared_runner.__name__
    _propagate_signature(_shared_runner, factory)
    return _shared_runner


def _propagate_signature(runner: Callable[..., Any], factory: Any) -> None:
    """Expose the factory signature so taskiq resolves its dependencies."""
    try:
        hints = get_type_hints(factory)
    except Exception:  # noqa: BLE001 - best effort; empty signature still works
        hints = dict(getattr(factory, "__annotations__", {}) or {})
    try:
        signature = inspect.signature(factory)
        parameters = []
        for parameter in signature.parameters.values():
            annotation = hints.get(parameter.name, parameter.annotation)
            parameters.append(parameter.replace(annotation=annotation))
        runner.__signature__ = signature.replace(parameters=parameters)  # type: ignore[attr-defined]
        runner.__annotations__ = dict(hints)
    except (TypeError, ValueError):
        return
    except Exception:  # noqa: BLE001 - never break declaration on introspection
        return


def SharedDepends(  # noqa: N802 - public declaration mirrors TaskiqDepends
    dependency: Optional[Callable[..., Any]] = None,
    *,
    key: Any = None,
    use_cache: bool = True,
    kwargs: Optional[Dict[str, Any]] = None,
    share_key: Any = None,
    name: Any = None,
) -> Any:
    """Declare an explicitly shared dependency.

    Repeated declarations with the same share key reuse one entered instance
    inside the processed message.  ``key``/``share_key``/``name`` are aliases
    for the explicit share identity; when omitted the identity is the
    dependency callable together with its static ``kwargs``.

    :return: a :class:`taskiq_dependencies.Dependency` marker.
    """
    explicit = share_key if share_key is not None else key
    if explicit is None:
        explicit = name
    share = coerce_key(
        shared=True,
        explicit_key=explicit,
        dependency=dependency,
        params=kwargs,
    )
    if dependency is None:
        # Let taskiq_dependencies infer the dependency from the type hint.
        marker = Depends(None, use_cache=use_cache, kwargs=kwargs)
    else:
        runner = _make_runner(share, dependency, kwargs)
        marker = Depends(runner, use_cache=use_cache, kwargs=None)
    # Preserve sharing metadata on the marker itself.
    marker.share_key = share  # type: ignore[attr-defined]
    marker.shared = True  # type: ignore[attr-defined]
    return marker


# Short alias used in ``Annotated[...]`` or as a default value.
Shared = SharedDepends
# Alternative spellings that callers may look for.
Share = SharedDepends


def shared(
    dependency: Optional[Callable[..., Any]] = None,
    **options: Any,
) -> Any:
    """Lower case alias of :class:`SharedDepends`.

    It can be used as a declaration factory (``shared(fn, key="k")``) or as a
    bare decorator (``@shared``), in which case it returns the dependency
    marker itself.
    """
    return SharedDepends(dependency, **options)


# Additional naming aliases that callers may import.
SharedDependency = SharedDepends
TaskiqSharedDepends = SharedDepends
shared_depends = SharedDepends

# Scope naming aliases: the scope is strictly bound to the processed message
# (often described as request scope).
MessageScope = SharedScope
RequestScope = SharedScope
ShareScope = SharedScope
get_shared_scope = current_scope



def shared_dependency(
    dependency: Optional[Callable[..., Any]] = None,
    **options: Any,
) -> Any:
    """Lower case alias of :func:`SharedDepends`."""
    return SharedDepends(dependency, **options)


def TaskiqDepends(  # noqa: N802 - keeps the public TaskiqDepends name
    dependency: Any = None,
    *,
    use_cache: bool = True,
    kwargs: Optional[Dict[str, Any]] = None,
    shared: Any = None,
    share: Any = None,
    share_key: Any = None,
    key: Any = None,
    name: Any = None,
    independent: Optional[bool] = None,
    isolated: Optional[bool] = None,
    scope: Any = None,
    **extra: Any,
) -> Any:
    """Drop in replacement for ``taskiq_dependencies.Depends``.

    Without any sharing option this is exactly the ordinary
    :class:`taskiq.TaskiqDepends` declaration.  Sharing is opt-in::

        TaskiqDepends(session, shared=True)                # by callable+kwargs
        TaskiqDepends(session, shared="tenant-a")          # explicit key
        TaskiqDepends(session, share_key=("s", tenant))    # explicit key

    ``independent=True`` forces a fresh instance (equivalent to the existing
    ``use_cache=False`` behaviour).
    """
    # Already a Dependency/Shared marker: honour it directly.
    if isinstance(dependency, Dependency):
        return dependency

    # ``share=`` is an alias of ``shared=``.
    if shared is None and share is not None:
        shared = share

    wants_independent = bool(independent or isolated)
    if wants_independent and "use_cache" not in extra:
        use_cache = False

    explicit_key = share_key if share_key is not None else key
    if explicit_key is None:
        explicit_key = name
    if (
        explicit_key is None
        and shared is not None
        and shared is not True
        and shared is not False
    ):
        # ``shared="tenant-a"`` uses the value as the explicit key.
        explicit_key = shared
    # ``scope`` selects the lifecycle boundary (only the message/request
    # scope is supported) but does not replace the resource identity.
    wants_scope = scope is not None and scope is not False

    wants_share = bool(shared) or explicit_key is not None or wants_scope
    # ``shared=False`` explicitly opts out of sharing.
    if shared is False:
        wants_share = False

    if not wants_share:
        return Depends(
            dependency,
            use_cache=use_cache,
            kwargs=kwargs,
            **extra,
        )

    share = coerce_key(
        shared=True,
        explicit_key=explicit_key,
        dependency=dependency,
        params=kwargs,
    )
    if extra:
        # Forward any unknown options to the underlying Depends constructor.
        runner = _make_runner(share, dependency, kwargs)
        marker = Depends(
            runner,
            use_cache=use_cache,
            kwargs=None,
            **extra,
        )
        marker.shared = True  # type: ignore[attr-defined]
        marker.share_key = share  # type: ignore[attr-defined]
        return marker

    return SharedDepends(
        dependency,
        key=share,
        use_cache=use_cache,
        kwargs=kwargs,
    )
