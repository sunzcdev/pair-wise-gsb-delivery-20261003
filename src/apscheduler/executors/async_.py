from __future__ import annotations

from collections.abc import Callable
from inspect import isawaitable
from typing import Any

from .._structures import Job
from ..abc import JobExecutor


class AsyncJobExecutor(JobExecutor):
    """
    Executes functions directly on the event loop thread.

    If the function returns a coroutine object (or another kind of awaitable), that is
    awaited on and its return value is used as the job's return value.

    :param max_running_jobs: the maximum number of jobs the scheduler is allowed to
        have in flight on this executor at once (``None`` = no limit)
    """

    def __init__(self, *, max_running_jobs: int | None = None) -> None:
        self.max_running_jobs = max_running_jobs

    async def run_job(self, func: Callable[..., Any], job: Job) -> Any:
        retval = func(*job.args, **job.kwargs)
        if isawaitable(retval):
            retval = await retval

        return retval
