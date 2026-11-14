import asyncio

import pytest

from ijson.backends import python
from ijson.utils35 import async_iterable


class OversizedReader:
    def __init__(self, data):
        self.data = data
        self.reads = 0

    async def read(self, n):
        await asyncio.sleep(0)
        self.reads += 1
        data, self.data = self.data, b''
        return data


class HangingReader:
    def __init__(self):
        self.reading = asyncio.Event()
        self.cancelled = False

    async def read(self, n):
        if n == 0:
            return b''
        self.reading.set()
        try:
            await asyncio.Event().wait()
        except asyncio.CancelledError:
            self.cancelled = True
            raise


def test_pending_events_are_bounded_across_oversized_reads():
    async def run():
        content = b'[' + b'0,' * 1000 + b'0]'
        reader = OversizedReader(content)
        events = python.basic_parse_async(
            reader, max_pending_events=5
        )

        first = await events.__anext__()
        assert first == ('start_array', None)
        assert len(events.events) <= 5
        assert reader.reads == 1

        count = 1
        async for event in events:
            assert len(events.events) <= 5
            count += 1
        assert count == 1003
        assert events._closed

    asyncio.run(run())


def test_cancelling_a_read_releases_production_state():
    async def run():
        reader = HangingReader()
        events = python.parse_async(reader, max_pending_events=5)
        task = asyncio.create_task(events.__anext__())
        await reader.reading.wait()
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        assert reader.cancelled
        assert events._closed
        assert events.coro is None
        assert events.f is None

    asyncio.run(run())


def test_early_close_releases_production_state():
    async def run():
        reader = OversizedReader(b'[1, 2, 3, 4, 5]')
        events = python.basic_parse_async(
            reader, max_pending_events=1
        )
        assert await events.__anext__() == ('start_array', None)
        await events.aclose()
        assert events._closed
        assert events.coro is None
        assert not events.events
        assert events.f is None

    asyncio.run(run())


def test_parse_failure_is_not_replaced_by_close_failure():
    class FailingCoroutine:
        def send(self, data):
            raise ValueError('parse failed')

        def close(self):
            raise RuntimeError('close failed')

    async def run():
        reader = OversizedReader(b'[]')
        events = async_iterable(reader, 16)
        try:
            events.coro.close()
        except Exception:
            pass
        events.coro = FailingCoroutine()
        with pytest.raises(ValueError, match='parse failed'):
            await events.__anext__()
        assert events._closed

    asyncio.run(run())


def test_items_can_return_large_results_without_prefetching_many():
    async def run():
        content = (
            b'[{"value":"' + b'a' * 5000 + b'"},'
            b'{"value":"' + b'b' * 5000 + b'"}]'
        )
        reader = OversizedReader(content)
        items = python.items_async(
            reader, 'item', max_pending_events=1
        )

        first = await items.__anext__()
        assert first == {'value': 'a' * 5000}
        assert len(items.events) == 0
        second = await items.__anext__()
        assert second == {'value': 'b' * 5000}

    asyncio.run(run())
