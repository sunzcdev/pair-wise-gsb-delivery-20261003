"""Distributed task manager."""

from importlib.metadata import version

from taskiq_dependencies import Depends as TaskiqDepends

from taskiq.abc.broker import AsyncBroker, AsyncTaskiqDecoratedTask
from taskiq.abc.formatter import TaskiqFormatter
from taskiq.abc.middleware import (
    EXECUTIONS_LABEL,
    EXECUTION_COUNT_LABEL,
    RETRY_PHASE_LABEL,
    RETRY_STAGE_LABEL,
    Phase,
    RetryPhase,
    RetryStage,
    RetryStep,
    TaskPhase,
    TaskiqMiddleware,
)
from taskiq.abc.result_backend import AsyncResultBackend
from taskiq.abc.schedule_source import ScheduleSource
from taskiq.acks import AckableMessage, AcknowledgeType
from taskiq.brokers.inmemory_broker import InMemoryBroker
from taskiq.brokers.multi_broker import MultiBroker
from taskiq.brokers.shared_broker import async_shared_broker
from taskiq.brokers.zmq_broker import ZeroMQBroker
from taskiq.context import Context
from taskiq.events import TaskiqEvents
from taskiq.exceptions import (
    NoResultError,
    ResultGetError,
    ResultIsReadyError,
    SecurityError,
    SendTaskError,
    TaskiqError,
    TaskiqResultTimeoutError,
)
from taskiq.funcs import gather
from taskiq.message import BrokerMessage, TaskiqMessage
from taskiq.middlewares import (
    BaseRetryMiddleware,
    PrometheusMiddleware,
    SimpleRetryMiddleware,
    SmartRetryMiddleware,
)
from taskiq.middlewares.retry_middleware import get_retry_phase
from taskiq.result import TaskiqResult
from taskiq.scheduler.scheduled_task import ScheduledTask
from taskiq.scheduler.scheduler import TaskiqScheduler
from taskiq.state import TaskiqState
from taskiq.task import AsyncTaskiqTask

__version__ = version("taskiq")

__all__ = [
    "AckableMessage",
    "AcknowledgeType",
    "AsyncBroker",
    "AsyncResultBackend",
    "AsyncTaskiqDecoratedTask",
    "AsyncTaskiqTask",
    "BaseRetryMiddleware",
    "BrokerMessage",
    "Context",
    "EXECUTIONS_LABEL",
    "EXECUTION_COUNT_LABEL",
    "InMemoryBroker",
    "MultiBroker",
    "NoResultError",
    "PrometheusMiddleware",
    "RETRY_PHASE_LABEL",
    "RETRY_STAGE_LABEL",
    "ResultGetError",
    "ResultIsReadyError",
    "RetryPhase",
    "RetryStage",
    "ScheduleSource",
    "ScheduledTask",
    "SecurityError",
    "SendTaskError",
    "SimpleRetryMiddleware",
    "SmartRetryMiddleware",
    "TaskiqDepends",
    "TaskiqError",
    "TaskiqEvents",
    "TaskiqFormatter",
    "TaskiqMessage",
    "TaskiqMiddleware",
    "TaskiqResult",
    "TaskiqResultTimeoutError",
    "TaskiqScheduler",
    "TaskiqState",
    "ZeroMQBroker",
    "__version__",
    "async_shared_broker",
    "gather",
    "get_retry_phase",
]
