"""Compatibility shim for phase-aware retry middleware primitives."""

from taskiq.abc.middleware import (
    EXECUTIONS_LABEL,
    RETRY_PHASE_LABEL,
    RetryPhase,
    RetryStage,
)
from taskiq.middlewares.retry_middleware import (
    BaseRetryMiddleware,
    coerce_retry_phases,
    get_retry_phase,
)

__all__ = (
    "EXECUTIONS_LABEL",
    "RETRY_PHASE_LABEL",
    "BaseRetryMiddleware",
    "RetryPhase",
    "RetryStage",
    "coerce_retry_phases",
    "get_retry_phase",
)
