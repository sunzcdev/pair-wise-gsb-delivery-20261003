# SPDX-License-Identifier: MIT OR Apache-2.0
# This file is dual licensed under the terms of the Apache License, Version
# 2.0, and the MIT License.  See the LICENSE file in the root of this
# repository for complete details.

"""
Global state department.  Don't reload this module or everything breaks.
"""

from __future__ import annotations


__lazy_modules__ = ["warnings"]

import os
import sys
import threading
import warnings

from collections.abc import Callable, Iterable, Sequence
from enum import Enum
from typing import Any, cast

from ._native import make_filtering_bound_logger
from ._output import PrintLoggerFactory
from .contextvars import merge_contextvars
from .dev import ConsoleRenderer, _has_colors, set_exc_info
from .processors import StackInfoRenderer, TimeStamper, add_log_level
from .typing import BindableLogger, Context, Processor, WrappedLogger


"""
Any changes to these defaults must be reflected in:

- `getting-started`.
- structlog.stdlib.recreate_defaults()'s docstring.
"""

_no_colors = os.environ.get("NO_COLOR", "") != ""
_force_colors = os.environ.get("FORCE_COLOR", "") != ""

_BUILTIN_DEFAULT_PROCESSORS: Sequence[Processor] = [
    merge_contextvars,
    add_log_level,
    StackInfoRenderer(),
    set_exc_info,
    TimeStamper(fmt="%Y-%m-%d %H:%M:%S", utc=False),
    ConsoleRenderer(
        colors=not _no_colors
        and (
            _force_colors
            or (
                _has_colors
                and sys.stdout is not None
                and hasattr(sys.stdout, "isatty")
                and sys.stdout.isatty()
            )
        ),
        force_colors=_force_colors,
    ),
]
_BUILTIN_DEFAULT_CONTEXT_CLASS = cast(type[Context], dict)
_BUILTIN_DEFAULT_WRAPPER_CLASS = make_filtering_bound_logger(0)
_BUILTIN_DEFAULT_LOGGER_FACTORY = PrintLoggerFactory()
_BUILTIN_CACHE_LOGGER_ON_FIRST_USE = False


class _Configuration:
    """
    Global defaults.
    """

    is_configured: bool = False
    default_processors: Iterable[Processor] = _BUILTIN_DEFAULT_PROCESSORS[:]
    default_context_class: type[Context] = _BUILTIN_DEFAULT_CONTEXT_CLASS
    default_wrapper_class: Any = _BUILTIN_DEFAULT_WRAPPER_CLASS
    logger_factory: Callable[..., WrappedLogger] = (
        _BUILTIN_DEFAULT_LOGGER_FACTORY
    )
    cache_logger_on_first_use: bool = _BUILTIN_CACHE_LOGGER_ON_FIRST_USE


_CONFIG = _Configuration()
"""
Global defaults used when arguments to `wrap_logger` are omitted.
"""


class MaterializedLoggerPolicy(Enum):
    """
    Decides what happens to already-materialized (cached) loggers when a new
    configuration generation is published.

    .. versionadded:: 26.2.0
    """

    KEEP = "keep"
    """
    Materialized loggers keep the configuration generation they were built
    from. This is the default and matches historic *structlog* behavior.
    """

    REFRESH = "refresh"
    """
    Materialized loggers lazily re-assemble themselves from the newest
    configuration generation on their next use.
    """


class _ConfigSnapshot:
    """
    An immutable, atomically-swappable view of the global configuration.

    Lazy loggers read exactly one snapshot when they materialize, so a hot
    reload can never split a processor chain across an old and a new
    configuration.
    """

    __slots__ = (
        "cache_logger_on_first_use",
        "context_class",
        "generation",
        "logger_factory",
        "materialized_policy",
        "processors",
        "wrapper_class",
    )

    def __init__(
        self,
        generation: int,
        processors: Iterable[Processor],
        context_class: type[Context],
        wrapper_class: Any,
        logger_factory: Callable[..., WrappedLogger],
        cache_logger_on_first_use: bool,
        materialized_policy: MaterializedLoggerPolicy,
    ) -> None:
        self.generation = generation
        # Deliberately *not* copied: historically, bound loggers share the
        # configured processor list object and structlog.testing.capture_logs
        # relies on that. `publish_config` passes its own private copy, so
        # hot-reloaded generations are still immune to caller-side mutation.
        self.processors = processors
        self.context_class = context_class
        self.wrapper_class = wrapper_class
        self.logger_factory = logger_factory
        self.cache_logger_on_first_use = cache_logger_on_first_use
        self.materialized_policy = materialized_policy

    def __repr__(self) -> str:
        return (
            f"<_ConfigSnapshot(generation={self.generation}, "
            f"processors={self.processors!r}, "
            f"context_class={self.context_class!r}, "
            f"wrapper_class={self.wrapper_class!r}, "
            f"logger_factory={self.logger_factory!r}, "
            f"cache_logger_on_first_use={self.cache_logger_on_first_use!r}, "
            f"materialized_policy={self.materialized_policy!r})>"
        )


_CONFIG_LOCK = threading.Lock()
"""
Serializes configuration updates.

Readers never take this lock: they atomically load the current `_SNAPSHOT`
reference instead and therefore always see one complete generation.
"""

_CONFIG_GENERATION = 0


def _rebuild_snapshot(
    materialized_policy: MaterializedLoggerPolicy,
) -> int:
    """
    Publish the current `_CONFIG` as a new generation.

    Must be called with `_CONFIG_LOCK` held. Returns the new generation id.
    """
    global _CONFIG_GENERATION, _SNAPSHOT

    _CONFIG_GENERATION += 1
    _SNAPSHOT = _ConfigSnapshot(
        generation=_CONFIG_GENERATION,
        processors=_CONFIG.default_processors,
        context_class=_CONFIG.default_context_class,
        wrapper_class=_CONFIG.default_wrapper_class,
        logger_factory=_CONFIG.logger_factory,
        cache_logger_on_first_use=_CONFIG.cache_logger_on_first_use,
        materialized_policy=materialized_policy,
    )

    return _CONFIG_GENERATION


with _CONFIG_LOCK:
    _rebuild_snapshot(MaterializedLoggerPolicy.KEEP)


def is_configured() -> bool:
    """
    Return whether *structlog* has been configured.

    If `False`, *structlog* is running with builtin defaults.

    .. versionadded: 18.1.0
    """
    return _CONFIG.is_configured


def get_config() -> dict[str, Any]:
    """
    Get a dictionary with the current configuration.

    .. note::

       Changes to the returned dictionary do *not* affect *structlog*.

    .. versionadded: 18.1.0
    """
    return {
        "processors": _CONFIG.default_processors,
        "context_class": _CONFIG.default_context_class,
        "wrapper_class": _CONFIG.default_wrapper_class,
        "logger_factory": _CONFIG.logger_factory,
        "cache_logger_on_first_use": _CONFIG.cache_logger_on_first_use,
    }


def get_logger(*args: Any, **initial_values: Any) -> Any:
    """
    Convenience function that returns a logger according to configuration.

    >>> from structlog import get_logger
    >>> log = get_logger(y=23)
    >>> log.info("hello", x=42)
    y=23 x=42 event='hello'

    Args:
        args:
            *Optional* positional arguments that are passed unmodified to the
            logger factory.  Therefore it depends on the factory what they
            mean.

        initial_values: Values that are used to pre-populate your contexts.

    Returns:
        A proxy that creates a correctly configured bound logger when
        necessary. The type of that bound logger depends on your configuration
        and is `structlog.BoundLogger` by default.

    See `configuration` for details.

    If you prefer CamelCase, there's an alias for your reading pleasure:
    `structlog.getLogger`.

    .. versionadded:: 0.4.0 *args*
    """
    return wrap_logger(None, logger_factory_args=args, **initial_values)


getLogger = get_logger  # noqa: N816
"""
CamelCase alias for `structlog.get_logger`.

This function is supposed to be in every source file -- we don't want it to
stick out like a sore thumb in frameworks like Twisted or Zope.
"""


def wrap_logger(
    logger: WrappedLogger | None,
    processors: Iterable[Processor] | None = None,
    wrapper_class: type[BindableLogger] | None = None,
    context_class: type[Context] | None = None,
    cache_logger_on_first_use: bool | None = None,
    logger_factory_args: Iterable[Any] | None = None,
    **initial_values: Any,
) -> Any:
    """
    Create a new bound logger for an arbitrary *logger*.

    Default values for *processors*, *wrapper_class*, and *context_class* can
    be set using `configure`.

    If you set an attribute here, `configure` calls have *no* effect for the
    *respective* attribute.

    In other words: selective overwriting of the defaults while keeping some
    *is* possible.

    Args:
        initial_values: Values that are used to pre-populate your contexts.

        logger_factory_args:
            Values that are passed unmodified as ``*logger_factory_args`` to
            the logger factory if not `None`.

    Returns:
        A proxy that creates a correctly configured bound logger when
        necessary.

    See `configure` for the meaning of the rest of the arguments.

    .. versionadded:: 0.4.0 *logger_factory_args*
    """
    return BoundLoggerLazyProxy(
        logger,
        wrapper_class=wrapper_class,
        processors=processors,
        context_class=context_class,
        cache_logger_on_first_use=cache_logger_on_first_use,
        initial_values=initial_values,
        logger_factory_args=logger_factory_args,
    )


def configure(
    processors: Iterable[Processor] | None = None,
    wrapper_class: type[BindableLogger] | None = None,
    context_class: type[Context] | None = None,
    logger_factory: Callable[..., WrappedLogger] | None = None,
    cache_logger_on_first_use: bool | None = None,
) -> None:
    """
    Configures the **global** defaults.

    They are used if `wrap_logger` or `get_logger` are called without
    arguments.

    Can be called several times, keeping an argument at `None` leaves it
    unchanged from the current setting.

    After calling for the first time, `is_configured` starts returning `True`.

    Use `reset_defaults` to undo your changes.

    Args:
        processors: The processor chain. See :doc:`processors` for details.

        wrapper_class:
            Class to use for wrapping loggers instead of
            `structlog.BoundLogger`.  See `standard-library`, :doc:`twisted`,
            and `custom-wrappers`.

        context_class:
            Class to be used for internal context keeping. The default is a
            `dict` and since dictionaries are ordered as of Python 3.6, there's
            few reasons to change this option.

        logger_factory:
            Factory to be called to create a new logger that shall be wrapped.

        cache_logger_on_first_use:
            `wrap_logger` doesn't return an actual wrapped logger but a proxy
            that assembles one when it's first used. If this option is set to
            `True`, this assembled logger is cached. See `performance`.

    .. versionadded:: 0.3.0 *cache_logger_on_first_use*

    .. versionchanged:: 26.2.0
       The new configuration is published as one atomic generation: lazy
       loggers that materialize while `configure` runs never see a mix of
       old and new components.
    """
    with _CONFIG_LOCK:
        _CONFIG.is_configured = True

        if processors is not None:
            _CONFIG.default_processors = processors
        if wrapper_class is not None:
            _CONFIG.default_wrapper_class = wrapper_class
        if context_class is not None:
            _CONFIG.default_context_class = context_class
        if logger_factory is not None:
            _CONFIG.logger_factory = logger_factory
        if cache_logger_on_first_use is not None:
            _CONFIG.cache_logger_on_first_use = cache_logger_on_first_use

        _rebuild_snapshot(MaterializedLoggerPolicy.KEEP)


def configure_once(
    processors: Iterable[Processor] | None = None,
    wrapper_class: type[BindableLogger] | None = None,
    context_class: type[Context] | None = None,
    logger_factory: Callable[..., WrappedLogger] | None = None,
    cache_logger_on_first_use: bool | None = None,
) -> None:
    """
    Configures if structlog isn't configured yet.

    It does *not* matter whether it was configured using `configure` or
    `configure_once` before.

    Raises:
        RuntimeWarning: if repeated configuration is attempted.
    """
    if not _CONFIG.is_configured:
        configure(
            processors=processors,
            wrapper_class=wrapper_class,
            context_class=context_class,
            logger_factory=logger_factory,
            cache_logger_on_first_use=cache_logger_on_first_use,
        )
    else:
        warnings.warn(
            "Repeated configuration attempted.", RuntimeWarning, stacklevel=2
        )


def _validate_processors(processors: Iterable[Processor]) -> list[Processor]:
    """
    Ensure *processors* is an iterable of callables and return a copy.

    The copy makes sure that neither validation nor a later cache
    flush/refresh ever touches the list that the caller passed in.
    """
    if isinstance(processors, (str, bytes)) or not isinstance(
        processors, Iterable
    ):
        msg = "processors must be an iterable of callables"
        raise TypeError(msg)

    procs = list(processors)
    for p in procs:
        if not callable(p):
            msg = f"processor {p!r} is not callable"
            raise TypeError(msg)

    return procs


def publish_config(
    processors: Iterable[Processor] | None = None,
    wrapper_class: type[BindableLogger] | None = None,
    context_class: type[Context] | None = None,
    logger_factory: Callable[..., WrappedLogger] | None = None,
    cache_logger_on_first_use: bool | None = None,
    *,
    materialized_loggers: MaterializedLoggerPolicy | str = (
        MaterializedLoggerPolicy.KEEP
    ),
) -> int:
    """
    Validate and publish a whole configuration generation atomically.

    This is the hot-reload entry point: unlike a series of `configure`
    calls, the *processors*, *wrapper_class*, *context_class*, and
    *logger_factory* that a lazy logger assembles itself from always belong
    to exactly one generation -- a lazy logger never ends up with a chain
    that mixes components from the old and the new configuration.

    The new configuration is fully validated *before* anything is changed.
    If validation fails, an exception is raised and the previous
    configuration stays in effect.

    Args:
        processors: The processor chain. See :doc:`processors` for details.

        wrapper_class:
            Class to use for wrapping loggers instead of
            `structlog.BoundLogger`.

        context_class: Class to be used for internal context keeping.

        logger_factory:
            Factory to be called to create a new logger that shall be
            wrapped.

        cache_logger_on_first_use:
            Same as in `configure`.

        materialized_loggers:
            A `MaterializedLoggerPolicy` (or its string value) that decides
            what happens to loggers that have already been materialized
            (cached) from a previous generation:

            - `MaterializedLoggerPolicy.KEEP` (default): they keep using the
              old generation.
            - `MaterializedLoggerPolicy.REFRESH`: they lazily re-assemble
              themselves from this generation on their next use.

    Returns:
        The id of the published configuration generation. Ids are
        monotonically increasing.

    Raises:
        TypeError: If a component fails validation. The old configuration
            is kept in that case.
        ValueError: If *materialized_loggers* is not a valid
            `MaterializedLoggerPolicy`. The old configuration is kept in
            that case.

    .. versionadded:: 26.2.0
    """
    # Validate *everything* before touching any state, so a failed publish
    # leaves the old configuration fully intact.
    if processors is not None:
        processors = _validate_processors(processors)
    if wrapper_class is not None and not callable(wrapper_class):
        msg = f"wrapper_class {wrapper_class!r} is not callable"
        raise TypeError(msg)
    if context_class is not None and not callable(context_class):
        msg = f"context_class {context_class!r} is not callable"
        raise TypeError(msg)
    if logger_factory is not None and not callable(logger_factory):
        msg = f"logger_factory {logger_factory!r} is not callable"
        raise TypeError(msg)
    if cache_logger_on_first_use is not None and not isinstance(
        cache_logger_on_first_use, bool
    ):
        msg = "cache_logger_on_first_use must be a bool"
        raise TypeError(msg)
    materialized_loggers = MaterializedLoggerPolicy(materialized_loggers)

    with _CONFIG_LOCK:
        _CONFIG.is_configured = True

        if processors is not None:
            _CONFIG.default_processors = processors
        if wrapper_class is not None:
            _CONFIG.default_wrapper_class = wrapper_class
        if context_class is not None:
            _CONFIG.default_context_class = context_class
        if logger_factory is not None:
            _CONFIG.logger_factory = logger_factory
        if cache_logger_on_first_use is not None:
            _CONFIG.cache_logger_on_first_use = cache_logger_on_first_use

        return _rebuild_snapshot(materialized_loggers)


def reset_defaults() -> None:
    """
    Resets global default values to builtin defaults.

    `is_configured` starts returning `False` afterwards.
    """
    with _CONFIG_LOCK:
        _CONFIG.is_configured = False
        _CONFIG.default_processors = _BUILTIN_DEFAULT_PROCESSORS[:]
        _CONFIG.default_wrapper_class = _BUILTIN_DEFAULT_WRAPPER_CLASS
        _CONFIG.default_context_class = _BUILTIN_DEFAULT_CONTEXT_CLASS
        _CONFIG.logger_factory = _BUILTIN_DEFAULT_LOGGER_FACTORY
        _CONFIG.cache_logger_on_first_use = _BUILTIN_CACHE_LOGGER_ON_FIRST_USE

        _rebuild_snapshot(MaterializedLoggerPolicy.KEEP)


class BoundLoggerLazyProxy:
    """
    Instantiates a bound logger on first usage.

    Takes both configuration and instantiation parameters into account.

    The only points where a bound logger changes state are ``bind()``,
    ``unbind()``, and ``new()`` and that return the actual ``BoundLogger``.

    If and only if configuration says so, that actual bound logger is cached on
    first usage.

    .. versionchanged:: 0.4.0 Added support for *logger_factory_args*.
    """

    # fulfill BindableLogger protocol without carrying accidental state
    @property
    def _context(self) -> dict[str, str]:
        return self._initial_values

    def __init__(
        self,
        logger: WrappedLogger | None,
        wrapper_class: type[BindableLogger] | None = None,
        processors: Iterable[Processor] | None = None,
        context_class: type[Context] | None = None,
        cache_logger_on_first_use: bool | None = None,
        initial_values: dict[str, Any] | None = None,
        logger_factory_args: Any = None,
    ) -> None:
        self._logger = logger
        self._wrapper_class = wrapper_class
        self._processors = processors
        self._context_class = context_class
        self._cache_logger_on_first_use = cache_logger_on_first_use
        self._initial_values = initial_values or {}
        self._logger_factory_args = logger_factory_args or ()

    def __repr__(self) -> str:
        return (
            f"<BoundLoggerLazyProxy(logger={self._logger!r}, wrapper_class="
            f"{self._wrapper_class!r}, processors={self._processors!r}, "
            f"context_class={self._context_class!r}, "
            f"initial_values={self._initial_values!r}, "
            f"logger_factory_args={self._logger_factory_args!r})>"
        )

    def bind(self, **new_values: Any) -> BindableLogger:
        """
        Assemble a new BoundLogger from arguments and configuration.

        The configuration is read as one atomic generation: the processors,
        logger factory, wrapper class, and context class always belong to
        the same configuration generation, even if a hot reload happens
        concurrently.
        """
        # Atomically load the current configuration generation. The
        # snapshot is immutable, so everything derived from it below is
        # guaranteed to be consistent -- never a mix of old and new config.
        snap = _SNAPSHOT

        if self._context_class:
            ctx = self._context_class(self._initial_values)
        else:
            ctx = snap.context_class(self._initial_values)

        _logger = self._logger
        if not _logger:
            _logger = snap.logger_factory(*self._logger_factory_args)

        if self._processors is None:
            procs = snap.processors
        else:
            procs = self._processors

        cls = self._wrapper_class or snap.wrapper_class
        # Looks like Protocols ignore definitions of __init__ so we have to
        # silence Mypy here.
        logger = cls(
            _logger,
            processors=procs,
            context=ctx,  # type: ignore[call-arg]
        )

        def finalized_bind(**new_values: Any) -> BindableLogger:
            """
            Use cached assembled logger to bind potentially new values.
            """
            current = _SNAPSHOT
            if (
                current.generation != snap.generation
                and current.materialized_policy
                is MaterializedLoggerPolicy.REFRESH
            ):
                # A newer generation that asks materialized loggers to move
                # along has been published: drop the cached logger and
                # re-assemble from the new generation instead of serving a
                # stale chain.
                self.__dict__.pop("bind", None)

                return self.bind(**new_values)

            if new_values:
                return logger.bind(**new_values)

            return logger

        if self._cache_logger_on_first_use is True or (
            self._cache_logger_on_first_use is None
            and snap.cache_logger_on_first_use is True
        ):
            self.bind = finalized_bind  # type: ignore[method-assign]

        return finalized_bind(**new_values)

    def unbind(self, *keys: str) -> BindableLogger:
        """
        Same as bind, except unbind *keys* first.

        In our case that could be only initial values.
        """
        return self.bind().unbind(*keys)

    def try_unbind(self, *keys: str) -> BindableLogger:
        return self.bind().try_unbind(*keys)

    def new(self, **new_values: Any) -> BindableLogger:
        """
        Clear context, then bind.
        """
        if self._context_class:
            self._context_class().clear()
        else:
            _SNAPSHOT.context_class().clear()

        return self.bind(**new_values)

    def __getattr__(self, name: str) -> Any:
        """
        If a logging method if called on a lazy proxy, we have to create an
        ephemeral BoundLogger first.
        """
        if name == "__isabstractmethod__":
            raise AttributeError

        bl = self.bind()

        return getattr(bl, name)

    def __getstate__(self) -> dict[str, Any]:
        """
        Our __getattr__ magic makes this necessary.
        """
        return self.__dict__

    def __setstate__(self, state: dict[str, Any]) -> None:
        """
        Our __getattr__ magic makes this necessary.
        """
        for k, v in state.items():
            setattr(self, k, v)
