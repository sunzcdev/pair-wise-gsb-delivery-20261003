"""Compatibility module for the base retry middleware."""

from taskiq.middlewares.retry_middleware import (
    BaseRetryMiddleware,
    coerce_retry_phases,
    get_retry_phase,
    parse_retry_phase,
)

RetryMiddleware = BaseRetryMiddleware

__all__ = (
    "BaseRetryMiddleware",
    "RetryMiddleware",
    "coerce_retry_phases",
    "get_retry_phase",
    "parse_retry_phase",
)
