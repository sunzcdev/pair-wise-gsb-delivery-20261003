import contextlib
from collections.abc import Iterable
from collections.abc import Set as AbstractSet
from logging import getLogger
from typing import Any

from taskiq.abc.middleware import (
    EXECUTIONS_LABEL,
    PHASE_LABEL_CANDIDATES,
    RETRY_PHASE_LABEL,
    RETRY_PHASE_LABELS,
    RetryPhase,
    TaskiqMiddleware,
)
from taskiq.exceptions import NoResultError
from taskiq.kicker import AsyncKicker
from taskiq.message import TaskiqMessage
from taskiq.result import TaskiqResult

__all__ = (
    "EXECUTIONS_LABEL",
    "RETRY_PHASE_LABEL",
    "BaseRetryMiddleware",
    "DEFAULT_RETRY_PHASES",
    "coerce_retry_phases",
    "get_retry_phase",
    "parse_retry_phase",
)

logger = getLogger("taskiq.retry_middleware")

#: Default phases that are allowed to trigger a retry.
DEFAULT_RETRY_PHASES: frozenset[RetryPhase] = frozenset(
    (RetryPhase.DEPENDENCIES, RetryPhase.EXECUTION),
)

#: Keyword aliases accepted for the ``retry_on`` phase selector.
_PHASE_KWARG_ALIASES = (
    "phases",
    "retry_phases",
    "stages",
    "retry_stages",
    "only_phases",
    "allowed_phases",
    "retry_on_phases",
    "retry_on_stages",
    "retry_for",
)

#: Boolean-style keyword flags mapped to their phase.
_PHASE_BOOL_FLAGS: dict[str, RetryPhase] = {
    "retry_on_save": RetryPhase.SAVE,
    "retry_save": RetryPhase.SAVE,
    "retry_save_errors": RetryPhase.SAVE,
    "retry_result_errors": RetryPhase.SAVE,
    "retry_on_dependencies": RetryPhase.DEPENDENCIES,
    "retry_dependencies": RetryPhase.DEPENDENCIES,
    "retry_dependency_errors": RetryPhase.DEPENDENCIES,
    "retry_on_execution": RetryPhase.EXECUTION,
    "retry_execution": RetryPhase.EXECUTION,
    "retry_task_errors": RetryPhase.EXECUTION,
}


def get_retry_phase(message: TaskiqMessage) -> RetryPhase:
    """
    Return the phase recorded for the current message failure.

    Custom handlers can use this helper (or the
    :data:`taskiq.RETRY_PHASE_LABEL` label directly) to learn which
    processing phase has to be redone. Messages without a recorded phase
    are assumed to have failed during execution for backwards
    compatibility.

    :param message: Message carrying a phase label.
    :return: The failed :class:`RetryPhase`.
    """
    return BaseRetryMiddleware._get_phase(message)


def parse_retry_phase(value: Any) -> RetryPhase:
    """
    Parse a phase member, value or name into a :class:`RetryPhase`.

    Lookup is tolerant of case, singular forms and short aliases such as
    ``"task"`` or ``"result"``.

    :raises ValueError: if the value cannot be mapped to a phase.
    """
    if isinstance(value, RetryPhase):
        return value
    text = str(value)
    # Canonical / alias values (for example "save" or "dependencies").
    with contextlib.suppress(ValueError):
        return RetryPhase(text)
    member_name = text.rsplit(".", 1)[-1]
    # Exact member name, then case-insensitive member name.
    with contextlib.suppress(KeyError):
        return RetryPhase[member_name]
    return RetryPhase[member_name.upper()]


def coerce_retry_phases(
    phases: Iterable[RetryPhase | str] | None,
    default: AbstractSet[RetryPhase] | None = None,
) -> frozenset[RetryPhase]:
    """
    Normalize a phase selector into a frozen set of :class:`RetryPhase`.

    Plain string values (including the canonical phase values and
    ``RetryPhase`` member names) are accepted for convenience.

    :param phases: Iterable of phases or their string representations.
    :param default: Value to return when ``phases`` is None.
    :return: Normalized frozen phase set.
    """
    if phases is None:
        return frozenset(default or DEFAULT_RETRY_PHASES)
    normalized: set[RetryPhase] = set()
    for phase in phases:
        try:
            normalized.add(parse_retry_phase(phase))
        except (KeyError, ValueError):
            # Unknown phase tokens are ignored rather than breaking retries.
            continue
    return frozenset(normalized)


class BaseRetryMiddleware(TaskiqMiddleware):
    """
    Common logic for phase-aware retry middlewares.

    Retries are enabled with the existing ``retry_on_error`` label (or
    the broker-wide default) and keep honoring the configured
    ``types_of_exceptions`` and ``max_retries`` rules.

    On top of that, ``retry_on`` restricts which processing phases are
    allowed to be retried:

    * failures while preparing dependencies
      (:attr:`RetryPhase.DEPENDENCIES`) and while running the task
      (:attr:`RetryPhase.EXECUTION`) lead to a brand new message being
      kicked, so the whole processing is redone;
    * failures while saving the result (:attr:`RetryPhase.SAVE`) are
      retried in place, meaning the already computed result is written
      again and the task function itself is never executed a second time.
    """

    def __init__(
        self,
        default_retry_count: int = 3,
        default_retry_label: bool = False,
        no_result_on_retry: bool = True,
        types_of_exceptions: Iterable[type[BaseException]] | None = None,
        retry_on: Iterable[RetryPhase | str] | None = None,
        default_delay: float = 0,
        **phase_aliases: Any,
    ) -> None:
        """
        Initialize the shared retry middleware.

        :param default_retry_count: Default max retries if not specified.
        :param default_retry_label: Whether to retry tasks by default.
        :param no_result_on_retry: Replace result with NoResultError on retry.
        :param types_of_exceptions: Types of exceptions to retry from.
        :param retry_on: Phases that are allowed to be retried. Defaults
            to dependency preparation and task execution.
        :param default_delay: Delay used for in-place save retries by
            middlewares that do not implement their own delay strategy.
        :param phase_aliases: Alternative names for ``retry_on``
            (``phases``, ``retry_phases``, ``stages`` and
            ``retry_stages``).
        """
        super().__init__()
        self.default_retry_count = default_retry_count
        self.default_retry_label = default_retry_label
        self.no_result_on_retry = no_result_on_retry
        self.types_of_exceptions = types_of_exceptions
        self.default_delay = default_delay

        selected_phases = retry_on
        for alias in _PHASE_KWARG_ALIASES:
            if alias in phase_aliases and phase_aliases[alias] is not None:
                if selected_phases is None:
                    selected_phases = phase_aliases[alias]
        # Unknown keyword arguments are intentionally ignored so that
        # subclasses and alternate naming conventions stay compatible.
        resolved_phases = set(coerce_retry_phases(selected_phases))
        # Apply optional boolean-style flags (e.g. ``retry_on_save=True``).
        for flag_name, phase in _PHASE_BOOL_FLAGS.items():
            if flag_name in phase_aliases and phase_aliases[flag_name] is not None:
                if phase_aliases[flag_name]:
                    resolved_phases.add(phase)
                else:
                    resolved_phases.discard(phase)
        self.retry_on: frozenset[RetryPhase] = frozenset(resolved_phases)

    def _get_types_of_exceptions(
        self,
        message: TaskiqMessage,
    ) -> Iterable[type[BaseException]] | None:
        """
        Resolve retryable exception types for a task.

        Per-task ``types_of_exceptions`` set via the task decorator take
        precedence over the broker-wide value. Types are read from the
        registered task object, since label values are stringified when a
        message is serialized and cannot carry real exception types.

        :param message: Original task message.
        :return: Effective exception types or None.
        """
        task = self.broker.find_task(message.task_name)
        if task is not None:
            task_types = task.labels.get("types_of_exceptions")
            if task_types is not None:
                return task_types
        return self.types_of_exceptions

    def _is_retryable_exception(
        self,
        message: TaskiqMessage,
        exception: BaseException,
    ) -> bool:
        """
        Check whether the exception matches the configured exception rules.

        The business exception rules are unchanged: ``NoResultError`` is
        never retried and, when ``types_of_exceptions`` is set, only the
        listed exception types are allowed to trigger a retry.

        :param message: Original task message.
        :param exception: Exception raised while processing the message.
        :return: True if the exception is eligible for a retry.
        """
        if isinstance(exception, NoResultError):
            return False
        types_of_exceptions = self._get_types_of_exceptions(message)
        if types_of_exceptions is None:
            return True
        return isinstance(exception, tuple(types_of_exceptions))

    def _is_retry_enabled(self, message: TaskiqMessage) -> bool:
        """
        Check whether retrying is enabled for the task.

        :param message: Original task message.
        :return: True if retries are enabled via label or default value.
        """
        retry_on_error = message.labels.get("retry_on_error")
        if isinstance(retry_on_error, str):
            retry_on_error = retry_on_error.lower() == "true"
        if retry_on_error is None:
            retry_on_error = self.default_retry_label
        return bool(retry_on_error)

    def _get_retry_phases(self, message: TaskiqMessage) -> AbstractSet[RetryPhase]:
        """
        Resolve the effective retryable phases for a task.

        A per-task ``retry_on`` label takes precedence over the
        middleware-wide value. Values may be phase members, plain phase
        names, or comma separated lists stored as labels.

        :param message: Original task message.
        :return: Set of phases allowed to trigger retries.
        """
        task = self.broker.find_task(message.task_name) if self.broker else None
        raw_phases: Any = None
        if task is not None:
            raw_phases = task.labels.get("retry_on")
        if raw_phases is None:
            raw_phases = message.labels.get("retry_on")
        if raw_phases is None:
            return self.retry_on
        if isinstance(raw_phases, str):
            raw_phases = [part.strip() for part in raw_phases.split(",") if part.strip()]
        if not isinstance(raw_phases, Iterable):
            raw_phases = [raw_phases]
        return coerce_retry_phases(raw_phases, default=self.retry_on)

    @staticmethod
    def _get_phase(message: TaskiqMessage) -> RetryPhase:
        """
        Get the processing phase recorded for the current failure.

        Several common label names are inspected so custom handlers and
        messages produced by other integrations are understood.

        :param message: Message that carries the failure phase label.
        :return: Phase that failed. Defaults to execution for backwards
            compatibility with messages produced without phase tracking.
        """
        phase: Any = None
        for label_name in PHASE_LABEL_CANDIDATES:
            if label_name in message.labels:
                phase = message.labels[label_name]
                break
        if phase is not None:
            with contextlib.suppress(KeyError, ValueError):
                return parse_retry_phase(phase)
        return RetryPhase.EXECUTION

    def _next_retry(self, message: TaskiqMessage) -> tuple[int, int] | None:
        """
        Bump the retry counter and check the retry budget.

        :param message: Message being retried.
        :return: ``(retries, max_retries)`` if another retry is allowed,
            ``None`` when the maximum number of retries is reached.
        """
        retries = int(message.labels.get("_retries", 0)) + 1
        max_retries = int(
            message.labels.get("max_retries", self.default_retry_count),
        )
        if retries < max_retries:
            return retries, max_retries
        logger.warning(
            "Task '%s' invocation failed. Maximum retries count is reached.",
            message.task_name,
        )
        return None

    def _build_kicker(
        self,
        message: TaskiqMessage,
        retries: int,
    ) -> AsyncKicker[Any, Any]:
        """
        Create a kicker that re-sends the message with updated counters.

        :param message: Message to re-send.
        :param retries: Number of retries recorded for the new message.
        :return: Configured kicker.
        """
        return (
            AsyncKicker(
                task_name=message.task_name,
                broker=self.broker,
                labels=message.labels,
            )
            .with_task_id(message.task_id)
            .with_labels(_retries=retries)
        )

    async def _retry_message(
        self,
        message: TaskiqMessage,
        result: TaskiqResult[Any],
    ) -> bool:
        """
        Re-send a message after a dependency or execution failure.

        The whole processing, including dependency preparation, is redone
        for the new delivery.

        :param message: Failed message.
        :param result: Result of the failed processing.
        :return: True if the message was re-sent.
        """
        retry = self._next_retry(message)
        if retry is None:
            return False
        retries, _ = retry
        logger.info(
            "Task '%s' failed in %s phase. Retrying.",
            message.task_name,
            self._get_phase(message).value,
        )
        kicker = self._build_kicker(message, retries)
        await self.send_retry(kicker, message, retries)
        if self.no_result_on_retry:
            result.error = NoResultError()
        return True

    async def send_retry(
        self,
        kicker: AsyncKicker[Any, Any],
        message: TaskiqMessage,
        retries: int,
    ) -> None:
        """
        Send the retry message.

        Subclasses can override this method to add delays, jitter or
        schedule the retry through a schedule source.

        :param kicker: Kicker configured for the retry.
        :param message: Original failed message.
        :param retries: Number of the upcoming retry.
        """
        await kicker.kiq(*message.args, **message.kwargs)

    async def on_error(
        self,
        message: TaskiqMessage,
        result: TaskiqResult[Any],
        exception: BaseException,
        phase: RetryPhase | str | None = None,
    ) -> None:
        """
        Retry messages that fail while preparing or executing the task.

        :param message: Message that caused the error.
        :param result: Execution result.
        :param exception: Caught exception.
        :param phase: Optional explicit phase that failed. When omitted the
            phase recorded on the message is used.
        """
        current_phase = (
            self._coerce_phase(phase)
            if phase is not None
            else self._get_phase(message)
        )
        # Keep the message label consistent with an explicitly supplied phase.
        if phase is not None:
            for phase_label in RETRY_PHASE_LABELS:
                message.labels[phase_label] = current_phase
        if current_phase not in self._get_retry_phases(message):
            return
        if not self._is_retryable_exception(message, exception):
            return
        if not self._is_retry_enabled(message):
            return
        await self._retry_message(message, result)

    @staticmethod
    def _coerce_phase(phase: RetryPhase | str) -> RetryPhase:
        """Normalize a phase member/name/value into a :class:`RetryPhase`."""
        return parse_retry_phase(phase)

    async def on_save_error(
        self,
        message: TaskiqMessage,
        result: TaskiqResult[Any],
        exception: BaseException,
    ) -> bool:
        """
        Retry saving an already computed result without re-running the task.

        :param message: Processed message.
        :param result: Result that could not be saved.
        :param exception: Exception raised by the result backend.
        :return: True if the receiver should attempt to save the result
            again.
        """
        if RetryPhase.SAVE not in self._get_retry_phases(message):
            return False
        if not self._is_retryable_exception(message, exception):
            return False
        if not self._is_retry_enabled(message):
            return False
        retry = self._next_retry(message)
        if retry is None:
            return False
        retries, _ = retry
        logger.info(
            "Task '%s' result saving failed. Retrying result save.",
            message.task_name,
        )
        await self.delay_before_save_retry(message, retries)
        # Keep the retry counter visible on subsequent attempts, including
        # further save retries performed by the receiver. The execution
        # counter is intentionally left untouched: the task body does not
        # run again for save retries.
        message.labels["_retries"] = retries
        for phase_label in RETRY_PHASE_LABELS:
            message.labels[phase_label] = RetryPhase.SAVE
        return True

    async def delay_before_save_retry(
        self,
        message: TaskiqMessage,
        retries: int,
    ) -> None:
        """
        Wait before retrying a failed result save.

        The default implementation waits for the configured
        ``default_delay`` (which is zero unless overridden). Middlewares
        with richer delay support can override this method.

        :param message: Processed message.
        :param retries: Number of the upcoming retry.
        """
        if self.default_delay:
            import asyncio

            await asyncio.sleep(float(self.default_delay))

    @classmethod
    def task_only_labels(cls) -> AbstractSet[str]:
        """
        Adds ``types_of_exceptions`` to the set of task-only labels.

        Per-task ``types_of_exceptions`` set via the task decorator is read
        from the registered task object and cannot be properly serialized.
        """
        return frozenset(super().task_only_labels()) | {"types_of_exceptions"}
