from __future__ import annotations

from collections.abc import Callable
from contextlib import AsyncExitStack
from functools import partial
from typing import Any

import attrs
from anyio import CapacityLimiter, to_process
from attr.validators import instance_of, optional

from .._structures import Job
from ..abc import JobExecutor


@attrs.define(eq=False, kw_only=True)
class ProcessPoolJobExecutor(JobExecutor):
    """
    Executes functions in a process pool.

    :param max_workers: the maximum number of worker processes to keep
    :param max_running_jobs: the maximum number of jobs the scheduler is allowed to
        have in flight on this executor at once (``None`` = no limit)
    """

    max_workers: int = 40
    max_running_jobs: int | None = attrs.field(
        default=None, validator=optional(instance_of(int))
    )
    _limiter: CapacityLimiter = attrs.field(init=False)

    async def start(self, exit_stack: AsyncExitStack) -> None:
        self._limiter = CapacityLimiter(self.max_workers)

    async def run_job(self, func: Callable[..., Any], job: Job) -> Any:
        wrapped = partial(func, *job.args, **job.kwargs)
        return await to_process.run_sync(
            wrapped, cancellable=True, limiter=self._limiter
        )
