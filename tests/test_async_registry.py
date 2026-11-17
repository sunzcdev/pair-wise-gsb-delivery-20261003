import asyncio
import logging
import threading
import unittest

from prometheus_client.metrics_core import (
    CounterMetricFamily, GaugeMetricFamily, Metric,
)
from prometheus_client.registry import (
    DEFAULT_ASYNC_CONCURRENCY, CollectorRegistry,
)


def _family(name, value):
    return GaugeMetricFamily(name, 'help', value=value)


class SyncCollector:
    def __init__(self, name, value=1):
        self.name = name
        self.value = value
        self.calls = 0
        self.thread_ids = []

    def collect(self):
        self.calls += 1
        self.thread_ids.append(threading.get_ident())
        return [_family(self.name, self.value)]


class AsyncCollector:
    def __init__(self, name, value=1, delay=0):
        self.name = name
        self.value = value
        self.delay = delay
        self.calls = 0
        self.thread_ids = []

    def describe(self):
        # Registration-time name discovery must stay synchronous even for
        # asynchronous collectors.
        return [GaugeMetricFamily(self.name, 'help')]

    async def collect(self):
        self.calls += 1
        self.thread_ids.append(threading.get_ident())
        if self.delay:
            await asyncio.sleep(self.delay)
        return [_family(self.name, self.value)]


class FailingAsyncCollector:
    def __init__(self, name, exc=RuntimeError('boom')):
        self.name = name
        self.exc = exc

    async def collect(self):
        raise self.exc


class FailingSyncCollector:
    def __init__(self, name):
        self.name = name

    def collect(self):
        # Build a family, then fail before returning so nothing is emitted.
        family = _family(self.name, 1)
        raise RuntimeError('failure before family is returned')


class SlowAsyncCollector:
    def __init__(self, name, delay, started=None, finished=None):
        self.name = name
        self.delay = delay
        self.started = started if started is not None else asyncio.Event()
        self.finished = finished if finished is not None else asyncio.Event()

    async def collect(self):
        self.started.set()
        try:
            await asyncio.sleep(self.delay)
            return [_family(self.name, 1)]
        finally:
            self.finished.set()


def run(coro):
    return asyncio.run(coro)


class TestAsyncCollect(unittest.TestCase):
    def test_pure_async_collectors(self):
        registry = CollectorRegistry()
        registry.register(AsyncCollector('a_total'))
        registry.register(AsyncCollector('b_total'))

        metrics = run(registry.async_collect())
        self.assertEqual(['a_total', 'b_total'], [m.name for m in metrics])

    def test_mixed_sync_and_async_collectors(self):
        registry = CollectorRegistry()
        sync = SyncCollector('s_total')
        asyncc = AsyncCollector('a_total')
        registry.register(sync)
        registry.register(asyncc)

        metrics = run(registry.async_collect())
        self.assertEqual(['s_total', 'a_total'], [m.name for m in metrics])
        self.assertEqual(1, sync.calls)
        self.assertEqual(1, asyncc.calls)

    def test_family_order_follows_registration_not_completion(self):
        registry = CollectorRegistry()
        # Register in order 0..4, but make earlier collectors finish last.
        collectors = [
            AsyncCollector('m{}_total'.format(i), delay=(5 - i) * 0.01)
            for i in range(5)
        ]
        for collector in collectors:
            registry.register(collector)

        metrics = run(registry.async_collect())
        self.assertEqual(
            ['m{}_total'.format(i) for i in range(5)],
            [m.name for m in metrics],
        )

    def test_concurrency_is_bounded(self):
        registry = CollectorRegistry()
        max_seen = {'value': 0}
        active = {'value': 0}
        lock = threading.Lock()

        class ConcurrencyProbe:
            def __init__(self, name):
                self.name = name

            async def collect(self):
                with lock:
                    active['value'] += 1
                    max_seen['value'] = max(max_seen['value'], active['value'])
                await asyncio.sleep(0.02)
                with lock:
                    active['value'] -= 1
                return [_family(self.name, 1)]

        for i in range(12):
            registry.register(ConcurrencyProbe('p{}_total'.format(i)))

        run(registry.async_collect(max_concurrency=3))
        self.assertLessEqual(max_seen['value'], 3)
        self.assertEqual(active['value'], 0)

    def test_default_concurrency_constant(self):
        self.assertGreaterEqual(DEFAULT_ASYNC_CONCURRENCY, 1)

    def test_invalid_max_concurrency(self):
        registry = CollectorRegistry()
        registry.register(AsyncCollector('a_total'))
        with self.assertRaises(ValueError):
            run(registry.async_collect(max_concurrency=0))

    def test_sync_collector_runs_inline_no_thread_pool(self):
        registry = CollectorRegistry()
        sync = SyncCollector('s_total')
        registry.register(sync)
        main_thread = threading.get_ident()

        run(registry.async_collect())
        self.assertEqual([main_thread], sync.thread_ids)

    def test_sync_collect_never_touches_event_loop_for_async_collector(self):
        registry = CollectorRegistry()
        registry.register(AsyncCollector('a_total'))
        # The synchronous entry point must not spin up or run an event loop.
        with self.assertRaises(RuntimeError):
            list(registry.collect())

    def test_async_business_exception_drops_only_that_family(self):
        registry = CollectorRegistry()
        registry.register(AsyncCollector('good1_total'))
        registry.register(FailingAsyncCollector('bad_total'))
        registry.register(AsyncCollector('good2_total'))

        with self.assertLogs('prometheus_client.registry', level='ERROR') as logs:
            metrics = run(registry.async_collect())

        names = [m.name for m in metrics]
        self.assertEqual(['good1_total', 'good2_total'], names)
        self.assertTrue(any('exception' in message for message in logs.output))

    def test_async_timeout_drops_only_that_family(self):
        registry = CollectorRegistry()
        registry.register(AsyncCollector('fast_total'))
        registry.register(SlowAsyncCollector('slow_total', delay=1.0))
        registry.register(AsyncCollector('fast2_total'))

        with self.assertLogs('prometheus_client.registry', level='ERROR') as logs:
            metrics = run(registry.async_collect(timeout=0.05))

        names = [m.name for m in metrics]
        self.assertEqual(['fast_total', 'fast2_total'], names)
        self.assertTrue(any('timed out' in message for message in logs.output))

    def test_timeout_none_disables_timeout(self):
        registry = CollectorRegistry()
        registry.register(SlowAsyncCollector('slow_total', delay=0.05))
        metrics = run(registry.async_collect(timeout=None))
        self.assertEqual(['slow_total'], [m.name for m in metrics])

    def test_timeout_and_business_exception_logged_separately(self):
        registry = CollectorRegistry()
        registry.register(FailingAsyncCollector('boom_total'))
        registry.register(SlowAsyncCollector('slow_total', delay=1.0))

        with self.assertLogs('prometheus_client.registry', level='ERROR') as logs:
            metrics = run(registry.async_collect(timeout=0.05))

        self.assertEqual([], metrics)
        joined = '\n'.join(logs.output)
        self.assertIn('timed out', joined)
        self.assertIn('exception', joined)

    def test_sync_failure_in_async_scrape_drops_family_and_continues(self):
        registry = CollectorRegistry()

        class PartialSyncCollector:
            def collect(self):
                yield _family('partial_total', 1)
                raise RuntimeError('sync boom')

        registry.register(AsyncCollector('before_total'))
        registry.register(PartialSyncCollector())
        registry.register(AsyncCollector('after_total'))

        with self.assertLogs('prometheus_client.registry', level='ERROR'):
            metrics = run(registry.async_collect())

        # No partial family may leak into the output.
        self.assertEqual(
            ['before_total', 'after_total'], [m.name for m in metrics],
        )

    def test_sync_collect_failure_does_not_emit_half_family(self):
        registry = CollectorRegistry()
        registry.register(FailingSyncCollector('half_total'))
        with self.assertRaises(RuntimeError):
            list(registry.collect())

    def test_cancellation_converges_started_collectors(self):
        registry = CollectorRegistry()
        started = asyncio.Event()
        finished = asyncio.Event()
        # The slow collector holds a slot so the later collectors never start.
        registry.register(
            SlowAsyncCollector('slow_total', delay=5.0,
                               started=started, finished=finished),
        )
        for i in range(3):
            registry.register(AsyncCollector('tail{}_total'.format(i), delay=0))

        async def scenario():
            scrape = asyncio.ensure_future(
                registry.async_collect(max_concurrency=1),
            )
            await started.wait()
            scrape.cancel()
            with self.assertRaises(asyncio.CancelledError):
                await scrape
            # The already-started collector is converged (its finally block
            # runs) before cancellation propagates to the caller.
            await asyncio.wait_for(finished.wait(), timeout=1.0)
            self.assertTrue(finished.is_set())

        run(scenario())

    def test_cancellation_with_sync_collector_does_not_hang(self):
        registry = CollectorRegistry()

        class InlineSyncCollector:
            def collect(self):
                return [_family('sync_total', 1)]

        registry.register(InlineSyncCollector())

        async def scenario():
            scrape = asyncio.ensure_future(registry.async_collect())
            await asyncio.sleep(0)
            scrape.cancel()
            with self.assertRaises(asyncio.CancelledError):
                await scrape

        run(scenario())

    def test_target_info_emitted_first(self):
        registry = CollectorRegistry(target_info={'foo': 'bar'})
        registry.register(AsyncCollector('a_total'))
        metrics = run(registry.async_collect())
        self.assertEqual('target', metrics[0].name)
        self.assertEqual('a_total', metrics[1].name)

    def test_restricted_registry_async(self):
        registry = CollectorRegistry(auto_describe=True)
        registry.register(AsyncCollector('a_total'))
        registry.register(AsyncCollector('b_total'))

        restricted = registry.restricted_registry(['a_total'])
        metrics = run(restricted.async_collect())
        self.assertEqual(['a_total'], [m.name for m in metrics])

    def test_restricted_registry_async_preserves_order(self):
        registry = CollectorRegistry(auto_describe=True)
        for i in range(4):
            registry.register(
                AsyncCollector('m{}_total'.format(i), delay=(4 - i) * 0.01),
            )
        restricted = registry.restricted_registry(
            ['m3_total', 'm1_total', 'm2_total', 'm0_total'],
        )
        metrics = run(restricted.async_collect())
        self.assertEqual(
            ['m0_total', 'm1_total', 'm2_total', 'm3_total'],
            [m.name for m in metrics],
        )

    def test_restricted_registry_sync_rejects_async_collector(self):
        registry = CollectorRegistry(auto_describe=True)
        registry.register(AsyncCollector('a_total'))
        with self.assertRaises(RuntimeError):
            list(registry.restricted_registry(['a_total']).collect())

    def test_sync_generator_failure_in_async_scrape_drops_whole_family(self):
        registry = CollectorRegistry()

        class GeneratorSyncCollector:
            def collect(self):
                yield _family('partial_a_total', 1)
                yield _family('partial_b_total', 2)
                raise RuntimeError('generator failed before exhaustion')

        registry.register(AsyncCollector('before_total'))
        registry.register(GeneratorSyncCollector())
        registry.register(AsyncCollector('after_total'))

        with self.assertLogs('prometheus_client.registry', level='ERROR'):
            metrics = run(registry.async_collect())

        # The whole family is materialised first, so neither partial metric
        # may reach the output.
        self.assertEqual(
            ['before_total', 'after_total'], [m.name for m in metrics],
        )

    def test_empty_registry_async_collect(self):
        registry = CollectorRegistry()
        self.assertEqual([], run(registry.async_collect()))

    def test_mixed_collectors_order_preserved_with_slow_async(self):
        registry = CollectorRegistry()
        registry.register(SyncCollector('first_total'))
        registry.register(AsyncCollector('second_total', delay=0.05))
        registry.register(SyncCollector('third_total'))
        registry.register(AsyncCollector('fourth_total', delay=0.01))

        metrics = run(registry.async_collect())
        self.assertEqual(
            ['first_total', 'second_total', 'third_total', 'fourth_total'],
            [m.name for m in metrics],
        )

    def test_sync_collect_still_works(self):
        registry = CollectorRegistry()
        registry.register(SyncCollector('s_total', value=7))
        metrics = list(registry.collect())
        self.assertEqual(1, len(metrics))
        self.assertEqual(7, metrics[0].samples[0].value)

    def test_async_collector_returns_multiple_families_atomically(self):
        class MultiFamily:
            async def collect(self):
                return [
                    CounterMetricFamily('x_total', 'help', value=1),
                    GaugeMetricFamily('y', 'help', value=2),
                ]

        registry = CollectorRegistry()
        registry.register(MultiFamily())
        metrics = run(registry.async_collect())
        # CounterMetricFamily normalises the family name (the _total suffix
        # only appears on the sample).
        self.assertEqual(['x', 'y'], [m.name for m in metrics])
        self.assertIsInstance(metrics[0], Metric)


if __name__ == '__main__':
    unittest.main()
