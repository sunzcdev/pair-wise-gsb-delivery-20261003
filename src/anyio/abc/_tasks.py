from __future__ import annotations

import sys
from abc import ABCMeta, abstractmethod
from collections.abc import Callable, Coroutine
from contextvars import Context
from dataclasses import dataclass
from types import TracebackType
from typing import TYPE_CHECKING, Any, Literal, Protocol, final, overload

if sys.version_info >= (3, 13):
    from typing import TypeVar
else:
    from typing_extensions import TypeVar

from typing import TypeVarTuple, Unpack

if TYPE_CHECKING:
    from .._core._tasks import CancelScope, TaskHandle

T_co = TypeVar("T_co", covariant=True)
T_contra = TypeVar("T_contra", contravariant=True, default=None)
PosArgsT = TypeVarTuple("PosArgsT")


def get_coro_name(coro: Coroutine[Any, Any, object], override: object = None) -> str:
    if override is not None:
        return str(override)

    try:
        cr_frame = coro.cr_frame  # type: ignore[attr-defined]
        module = cr_frame.f_globals["__name__"]
    except (AttributeError, KeyError):
        module = None

    qualname = getattr(coro, "__qualname__", None)
    return ".".join([x for x in (module, qualname) if x])


def get_callable_name(func: Callable, override: object = None) -> str:
    if override is not None:
        return str(override)

    module = getattr(func, "__module__", None)
    qualname = getattr(func, "__qualname__", None)
    return ".".join([x for x in (module, qualname) if x])


def call_for_coroutine(
    func: Callable[[Unpack[PosArgsT]], Coroutine[Any, Any, T_co]],
    args: tuple[*PosArgsT],
    **kwargs: Any,
) -> Coroutine[Any, Any, T_co]:
    """
    Call the given function with the given positional and keyword arguments.

    :return: the resulting coroutine
    :raises TypeError: if the return value was not a coroutine object

    """
    coro = func(*args, **kwargs)
    if not isinstance(coro, Coroutine):
        prefix = f"{func.__module__}." if hasattr(func, "__module__") else ""
        raise TypeError(
            f"Expected {prefix}{func.__qualname__}() to return a coroutine, but "
            f"the return value ({coro!r}) is not a coroutine object"
        )

    return coro


class TaskStatus(Protocol[T_contra]):
    @overload
    def started(self: TaskStatus[None]) -> None: ...

    @overload
    def started(self, value: T_contra) -> None: ...

    def started(self, value: T_contra | None = None) -> None:
        """
        Signal that the task has started.

        :param value: object passed back to the starter of the task
        """


@final
@dataclass(frozen=True)
class TaskFailure:
    """
    A diagnostic record of a single task failure in a :class:`TaskGroup`.

    Instances of this class are collected on demand (see
    :attr:`TaskGroup.collect_failures`) and exposed via :attr:`TaskGroup.failures`.
    They are purely informational – the exceptions are still raised from the task
    group in an exception group, as usual.

    .. versionadded:: 4.17.0
    """

    exception: BaseException
    """The exception raised by the task."""

    task_name: str
    """The name of the task that raised the exception."""

    cleanup: bool
    """
    ``True`` if the exception was raised while the task was already being cancelled
    (typically from a ``finally:`` block or an exception handler doing cleanup),
    ``False`` if the exception was a root failure that caused the task group to
    initiate cancellation.
    """


class TaskGroup(metaclass=ABCMeta):
    """
    Groups several asynchronous tasks together.

    :ivar cancel_scope: the cancel scope inherited by all child tasks
    :vartype cancel_scope: CancelScope

    .. note:: On asyncio, support for eager task factories is considered to be
        **experimental**. In particular, they don't follow the usual semantics of new
        tasks being scheduled on the next iteration of the event loop, and may thus
        cause unexpected behavior in code that wasn't written with such semantics in
        mind.
    """

    cancel_scope: CancelScope

    collect_failures: bool = False
    """
    Whether to record per-task failure diagnostics (see :attr:`failures`).

    This must be enabled before spawning any tasks in the group, either by passing
    ``collect_failures=True`` to :func:`~anyio.create_task_group` or by setting this
    attribute before entering the task group. Failure collection is disabled by
    default, in which case :attr:`failures` will always be empty.

    .. versionadded:: 4.17.0
    """

    @property
    def failures(self) -> tuple[TaskFailure, ...]:
        """
        Diagnostics of the failures recorded from tasks in this task group.

        Each entry identifies the exception raised by a task, the name of that task,
        and whether the exception was a root failure or one raised during cleanup
        after cancellation. Tasks that finished normally and tasks that were simply
        cancelled are never recorded here.

        This will always be empty unless :attr:`collect_failures` was enabled.

        .. versionadded:: 4.17.0
        """
        return ()

    def cancel(self, reason: str | None = None) -> None:
        """
        Cancel this task group's cancel scope immediately.

        This is a shortcut for calling ``.cancel_scope.cancel()`` on the task group.

        :param reason: a message describing the reason for the cancellation

        .. versionadded:: 4.14.0

        """
        self.cancel_scope.cancel(reason)

    @abstractmethod
    def create_task(
        self,
        coro: Coroutine[Any, Any, T_co],
        *,
        name: object = None,
        context: Context | None = None,
    ) -> TaskHandle[T_co]:
        """
        Create a new task from a coroutine object and schedule it to run.

        :param coro: a coroutine object
        :param name: optional name to give the task
        :param context: optional context to run the task in
        :return: a task handle

        .. versionadded:: 4.14.0
        """

    @final
    def start_soon(
        self,
        func: Callable[[Unpack[PosArgsT]], Coroutine[Any, Any, T_co]],
        *args: *PosArgsT,
        name: object = None,
    ) -> TaskHandle[T_co]:
        """
        Start a new task in this task group.

        :param func: a coroutine function
        :param args: positional arguments to call the function with
        :param name: name of the task, for the purposes of introspection and debugging
        :return: a task handle

        .. versionadded:: 3.0
        .. versionchanged:: 4.14.0
            This method now returns a task handle.

        """
        final_name = get_callable_name(func, name)
        return self.create_task(call_for_coroutine(func, args), name=final_name)

    @overload
    async def start(
        self,
        func: Callable[..., Coroutine[Any, Any, T_co]],
        *args: object,
        name: object = None,
        return_handle: Literal[False] = ...,
    ) -> Any: ...

    @overload
    async def start(
        self,
        func: Callable[..., Coroutine[Any, Any, T_co]],
        *args: object,
        name: object = None,
        return_handle: Literal[True],
    ) -> TaskHandle[T_co, Any]: ...

    @abstractmethod
    async def start(
        self,
        func: Callable[..., Coroutine[Any, Any, T_co]],
        *args: object,
        name: object = None,
        return_handle: Literal[False, True] = False,
    ) -> Any:
        """
        Start a new task and wait until it signals for readiness.

        The target callable must accept a keyword argument ``task_status`` (of type
        :class:`TaskStatus`). Awaiting on this method will return whatever was passed to
        ``task_status.started()`` (``None`` by default).

        .. note:: The :class:`TaskStatus` class is generic, and the type argument should
            indicate the type of the value that will be passed to
            ``task_status.started()``.

        :param func: a coroutine function that accepts the ``task_status`` keyword
            argument
        :param args: positional arguments to call the function with
        :param name: an optional name for the task, for introspection and debugging
        :param return_handle: if ``True``, return a :class:`TaskHandle` which also
            contains the start value in ``start_value``
        :return: the value passed to ``task_status.started()``
        :raises RuntimeError: if the task finishes without calling
            ``task_status.started()``

        .. seealso:: :ref:`start_initialize`

        .. versionadded:: 3.0
        """

    @abstractmethod
    async def __aenter__(self) -> TaskGroup:
        """Enter the task group context and allow starting new tasks."""

    @abstractmethod
    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc_val: BaseException | None,
        exc_tb: TracebackType | None,
    ) -> bool:
        """Exit the task group context waiting for all tasks to finish."""
