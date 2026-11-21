from __future__ import annotations

import asyncio

import pytest

from uvicorn.protocols.http.flow_control import FlowControl, WriteTimeoutError

pytestmark = pytest.mark.anyio


class CloseTrackingTransport:
    """Minimal transport stub recording the flow-control operations.

    A real transport invokes ``Protocol.connection_lost`` when it is closed,
    and the HTTP protocols call ``flow.resume_writing()`` from there. The stub
    mirrors that through ``on_close`` so draining tasks get unblocked exactly
    as in production.
    """

    def __init__(self) -> None:
        self.closed = False
        self.close_count = 0
        self.read_paused = False
        self.on_close: object = None

    def pause_reading(self) -> None:
        self.read_paused = True

    def resume_reading(self) -> None:
        self.read_paused = False

    def is_closing(self) -> bool:
        return self.closed

    def close(self) -> None:
        self.close_count += 1
        self.closed = True
        if self.on_close is not None:
            self.on_close()


def make_flow(write_timeout: float | None) -> FlowControl:
    transport = CloseTrackingTransport()
    flow = FlowControl(transport, write_timeout=write_timeout)
    transport.on_close = flow.resume_writing
    return flow


async def test_drain_without_timeout_waits_indefinitely_without_timers():
    flow = make_flow(None)
    transport = flow._transport

    flow.pause_writing()
    drain_task = asyncio.create_task(flow.drain())
    await asyncio.sleep(0)
    assert not drain_task.done()

    flow.resume_writing()
    await asyncio.wait_for(drain_task, timeout=1)
    assert transport.close_count == 0


async def test_drain_times_out_when_pause_outlasts_timeout():
    flow = make_flow(0.05)
    transport = flow._transport

    flow.pause_writing()
    started = asyncio.get_running_loop().time()
    with pytest.raises(WriteTimeoutError):
        await flow.drain()
    elapsed = asyncio.get_running_loop().time() - started

    # Allow for the event loop's coarse timer granularity (the Proactor loop on
    # Windows can fire slightly early); the order of magnitude must be the
    # configured timeout rather than the much longer waits in the other tests.
    assert 0.04 <= elapsed < 0.2
    # The timeout closes the connection exactly once and clears the pause.
    assert transport.close_count == 1
    assert not flow.write_paused


async def test_drain_returns_normally_when_writing_resumes_before_timeout():
    flow = make_flow(5)
    transport = flow._transport
    tasks_before = asyncio.all_tasks()

    flow.pause_writing()
    drain_task = asyncio.create_task(flow.drain())
    await asyncio.sleep(0)

    flow.resume_writing()
    await asyncio.wait_for(drain_task, timeout=1)
    # The pending timeout timer must be disarmed, leaving nothing in flight.
    assert transport.close_count == 0
    assert asyncio.all_tasks() == tasks_before
    assert drain_task not in asyncio.all_tasks()


async def test_write_timeout_does_not_count_application_think_time():
    flow = make_flow(0.05)
    transport = flow._transport

    # While writable, the timeout is never armed: a long "application compute"
    # pause must not consume the write budget.
    await asyncio.sleep(0.12)

    flow.pause_writing()
    started = asyncio.get_running_loop().time()
    with pytest.raises(WriteTimeoutError):
        await flow.drain()
    elapsed = asyncio.get_running_loop().time() - started

    # The timeout starts counting from the actual wait, not from construction.
    assert 0.04 <= elapsed < 0.2
    assert transport.close_count == 1


async def test_each_drain_gets_a_fresh_timeout_window():
    flow = make_flow(0.05)
    transport = flow._transport

    # A short, well under the timeout pause must succeed.
    flow.pause_writing()
    flow.resume_writing()
    await flow.drain()
    assert transport.close_count == 0

    # A second, longer pause on the same flow gets its own full budget.
    flow.pause_writing()
    with pytest.raises(WriteTimeoutError):
        await flow.drain()
    assert transport.close_count == 1


async def test_connection_closed_while_draining_does_not_raise():
    flow = make_flow(5)
    transport = flow._transport

    flow.pause_writing()
    drain_task = asyncio.create_task(flow.drain())
    await asyncio.sleep(0)

    # Simulate the peer dropping the connection: ``connection_lost`` resumes
    # writing. Because the transport is already closing, no extra close call
    # and no spurious timeout error should occur.
    transport.closed = True
    flow.resume_writing()
    await asyncio.wait_for(drain_task, timeout=1)
    assert transport.close_count == 0


async def test_non_positive_timeout_disables_the_timer():
    flow = make_flow(0)
    transport = flow._transport

    flow.pause_writing()
    drain_task = asyncio.create_task(flow.drain())
    await asyncio.sleep(0.05)
    assert not drain_task.done()

    flow.resume_writing()
    await asyncio.wait_for(drain_task, timeout=1)
    assert transport.close_count == 0
