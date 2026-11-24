from collections.abc import Iterable
from collections.abc import Set as AbstractSet
from typing import Any

from taskiq.abc.middleware import RetryPhase
from taskiq.middlewares.retry_middleware import BaseRetryMiddleware

__all__ = ("SimpleRetryMiddleware",)


class SimpleRetryMiddleware(BaseRetryMiddleware):
    """
    Middleware to add retries.

    By default only failures while preparing dependencies or executing
    the task are retried. Pass :attr:`RetryPhase.SAVE` via ``retry_on``
    to also retry result backend failures; such retries only write the
    already computed result again and never re-run the task function.
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
        Initialize the simple retry middleware.

        :param default_retry_count: Default max retries if not specified.
        :param default_retry_label: Whether to retry tasks by default.
        :param no_result_on_retry: Replace result with NoResultError on retry.
        :param types_of_exceptions: Types of exceptions to retry from.
        :param retry_on: Phases that are allowed to be retried. Defaults
            to dependency preparation and task execution.
        :param default_delay: Optional delay before in-place save retries.
        :param phase_aliases: Alternative names for ``retry_on``.
        """
        super().__init__(
            default_retry_count=default_retry_count,
            default_retry_label=default_retry_label,
            no_result_on_retry=no_result_on_retry,
            types_of_exceptions=types_of_exceptions,
            retry_on=retry_on,
            default_delay=default_delay,
            **phase_aliases,
        )

    @classmethod
    def task_only_labels(cls) -> AbstractSet[str]:
        """
        Adds ``types_of_exceptions`` to the set of task-only labels.

        Per-task ``types_of_exceptions`` set via the task decorator is read
        from the registered task object and cannot be properly serialized.
        """
        return super().task_only_labels()
