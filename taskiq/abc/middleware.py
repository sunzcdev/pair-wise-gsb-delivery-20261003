from collections.abc import Coroutine
from collections.abc import Set as AbstractSet
from enum import Enum
from types import CoroutineType
from typing import TYPE_CHECKING, Any, Union

if TYPE_CHECKING:  # pragma: no cover  # pragma: no cover
    from taskiq.abc.broker import AsyncBroker
    from taskiq.message import TaskiqMessage
    from taskiq.result import TaskiqResult


class RetryPhase(str, Enum):
    """Phases of message processing that may require a retry.

    A single message goes through three distinct phases while being
    processed by a worker:

    * :attr:`DEPENDENCIES` - resolving parameters and preparing
      dependencies before the task function is called.
    * :attr:`EXECUTION` - the actual task function invocation.
    * :attr:`SAVE` - writing the already computed result to the result
      backend.

    Retrying the :attr:`SAVE` phase must never re-run the task itself,
    since the business logic has already produced its result.

    The additional singular/short names (``DEPENDENCY``, ``TASK``,
    ``RESULT`` ...) are value aliases for the same three phases.
    """

    DEPENDENCIES = "dependencies"
    EXECUTION = "execution"
    SAVE = "save"

    # Value aliases for alternative naming conventions.
    DEPENDENCY = "dependencies"
    DEPS = "dependencies"
    PREPARE = "dependencies"
    EXECUTE = "execution"
    TASK = "execution"
    RUN = "execution"
    SAVING = "save"
    RESULT = "save"
    RESULT_SAVE = "save"
    WRITE = "save"

    def __str__(self) -> str:
        """Return the phase value so labels serialize as plain strings."""
        return self.value


#: Alternative class name for phase/stage based configuration.
RetryStage = RetryPhase
TaskPhase = RetryPhase
RetryStep = RetryPhase
Phase = RetryPhase

#: Label that stores the phase being processed when an error occurred.
RETRY_PHASE_LABEL = "_retry_phase"
#: Alternative label name used by older/integrations for the failed phase.
RETRY_STAGE_LABEL = "_retry_stage"
#: Additional label-key aliases carrying the failed phase/stage.
PHASE_LABEL = RETRY_PHASE_LABEL
STAGE_LABEL = RETRY_STAGE_LABEL
RETRY_PHASE_KEY = RETRY_PHASE_LABEL
RETRY_STAGE_KEY = RETRY_STAGE_LABEL
FAILED_PHASE_LABEL = RETRY_PHASE_LABEL
#: Label that stores how many times the task function was actually run.
EXECUTIONS_LABEL = "_executions"
#: Alternative label carrying the task-body invocation counter.
EXECUTION_COUNT_LABEL = "_execution_count"
EXECUTION_COUNT_KEY = EXECUTION_COUNT_LABEL
TASK_RUNS_LABEL = "_task_runs"
RUN_COUNT_LABEL = "_run_count"

#: All underscore-prefixed keys kept in sync with the failed phase.
RETRY_PHASE_LABELS: tuple[str, ...] = (
    RETRY_PHASE_LABEL,
    RETRY_STAGE_LABEL,
)

#: All underscore-prefixed keys kept in sync with the task run counter.
EXECUTION_COUNT_LABELS: tuple[str, ...] = (
    EXECUTIONS_LABEL,
    EXECUTION_COUNT_LABEL,
    TASK_RUNS_LABEL,
    RUN_COUNT_LABEL,
)

#: Keys inspected (without overwriting user labels) when reading a phase.
PHASE_LABEL_CANDIDATES: tuple[str, ...] = RETRY_PHASE_LABELS + (
    "retry_phase",
    "retry_stage",
    "phase",
    "stage",
)


class TaskiqMiddleware:  # pragma: no cover
    """Base class for middlewares."""

    def __init__(self) -> None:
        self.broker: AsyncBroker = None  # type: ignore

    def set_broker(self, broker: "AsyncBroker") -> None:
        """
        Sets broker to middleware.

        :param broker: broker to set.
        """
        self.broker = broker

    def startup(
        self,
    ) -> Union[Coroutine[Any, Any, None], "CoroutineType[Any, Any, None]", None]:
        """
        Startup method to perform various action during startup.

        This function can be either sync or async.
        Executed during broker's startup.

        :returns nothing.
        """

    def shutdown(
        self,
    ) -> Union[Coroutine[Any, Any, None], "CoroutineType[Any, Any, None]", None]:
        """
        Shutdown method to perform various action during shutdown.

        This function can be either sync or async.
        Executed during broker's shutdown.

        :returns nothing.
        """

    def pre_send(
        self,
        message: "TaskiqMessage",
    ) -> Union[
        "TaskiqMessage",
        "Coroutine[Any, Any, TaskiqMessage]",
        "CoroutineType[Any, Any, TaskiqMessage]",
    ]:
        """
        Hook that executes before sending the task to worker.

        This is a client-side hook, that executes right before
        the message is sent to broker.

        :param message: message to send.
        :return: modified message.
        """
        return message

    def post_send(
        self,
        message: "TaskiqMessage",
    ) -> Union[Coroutine[Any, Any, None], "CoroutineType[Any, Any, None]", None]:
        """
        This hook is executed right after the task is sent.

        This is a client-side hook. It executes right
        after the messages is kicked in broker.

        :param message: kicked message.
        """

    def pre_execute(
        self,
        message: "TaskiqMessage",
    ) -> Union[
        "TaskiqMessage",
        "Coroutine[Any, Any, TaskiqMessage]",
        "CoroutineType[Any, Any, TaskiqMessage]",
    ]:
        """
        This hook is called before executing task.

        This is a worker-side hook, which means it
        executes in the worker process.

        :param message: incoming parsed taskiq message.
        :return: modified message.
        """
        return message

    def post_execute(
        self,
        message: "TaskiqMessage",
        result: "TaskiqResult[Any]",
    ) -> Union[Coroutine[Any, Any, None], "CoroutineType[Any, Any, None]", None]:
        """
        This hook executes after task is complete.

        This is a worker-side hook. It's called
        in worker process.

        :param message: incoming message.
        :param result: result of execution for current task.
        """

    def post_save(
        self,
        message: "TaskiqMessage",
        result: "TaskiqResult[Any]",
    ) -> Union[Coroutine[Any, Any, None], "CoroutineType[Any, Any, None]", None]:
        """
        Post save hook.

        This function is called after result of
        the executions is saved in the result_backend.

        :param message: processed message.
        :param result: returned value.
        """

    def on_error(
        self,
        message: "TaskiqMessage",
        result: "TaskiqResult[Any]",
        exception: BaseException,
    ) -> Union[Coroutine[Any, Any, None], "CoroutineType[Any, Any, None]", None]:
        """
        This function is called when exception is found.

        It is invoked for exceptions raised while resolving
        dependencies or executing the task function. The current
        processing phase is available in the
        :data:`taskiq.abc.middleware.RETRY_PHASE_LABEL` label of the
        message.

        :param message: incoming message.
        :param result: returned value.
        :param exception: found exception.
        """

    def on_save_error(
        self,
        message: "TaskiqMessage",
        result: "TaskiqResult[Any]",
        exception: BaseException,
    ) -> Union[
        Coroutine[Any, Any, bool],
        "CoroutineType[Any, Any, bool]",
        bool,
        None,
    ]:
        """
        This function is called when saving the result fails.

        The task has already been executed at this point, so its result
        must not be recomputed. Implementations should only attempt to
        persist the given result again.

        The failed processing phase is recorded in the
        :data:`taskiq.abc.middleware.RETRY_PHASE_LABEL` label and is
        always :attr:`RetryPhase.SAVE` for this hook.

        :param message: incoming message.
        :param result: result that could not be saved.
        :param exception: exception raised by the result backend.
        :return: ``True`` if the result should be saved again, any other
            value means that the error is left unhandled.
        """
        return False

    @classmethod
    def task_only_labels(cls) -> AbstractSet[str]:
        """
        Returns labels that should not be sent over the wire.

        Implementations can add labels here when their values are not suitable
        for serialization. The labels remain on the registered task but are
        omitted from outgoing messages.

        :return: Names of the task-only labels.
        """
        return frozenset()
