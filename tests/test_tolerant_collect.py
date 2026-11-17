import unittest

from prometheus_client import CollectorRegistry, Metric
from prometheus_client.registry import (
    CollectorFailure, TolerantCollectionResult,
)


class HealthyCollector:
    def __init__(self, name='healthy_metric'):
        self.name = name

    def collect(self):
        m = Metric(self.name, 'A healthy metric', 'gauge')
        m.add_sample(self.name, {}, 1.0)
        return [m]


class FailingCollector:
    def __init__(self, exc=None):
        self.calls = 0
        self.exc = exc or RuntimeError('boom')

    def collect(self):
        self.calls += 1
        raise self.exc


class PartialThenFailingCollector:
    """Yields a complete metric, then raises mid-iteration."""

    def __init__(self):
        self.calls = 0

    def collect(self):
        self.calls += 1
        m = Metric('partial_histogram', 'Half of a histogram', 'histogram')
        m.add_sample('partial_histogram_bucket', {'le': '1.0'}, 5.0)
        yield m
        raise RuntimeError('failed halfway')


class FlakyCollector:
    """Fails once, then succeeds on subsequent scrapes."""

    def __init__(self):
        self.calls = 0

    def collect(self):
        self.calls += 1
        if self.calls == 1:
            raise RuntimeError('transient failure')
        m = Metric('flaky_metric', 'Recovered metric', 'gauge')
        m.add_sample('flaky_metric', {}, float(self.calls))
        return [m]


class TestTolerantCollect(unittest.TestCase):
    def setUp(self):
        self.registry = CollectorRegistry()

    def test_healthy_metrics_returned_when_other_collector_fails(self):
        self.registry.register(HealthyCollector())
        self.registry.register(FailingCollector())
        result = self.registry.collect_tolerant()
        self.assertIsInstance(result, TolerantCollectionResult)
        names = [m.name for m in result.metrics]
        self.assertEqual(['healthy_metric'], names)
        self.assertFalse(result.ok)
        self.assertEqual(1, len(result.failures))
        failure = result.failures[0]
        self.assertIsInstance(failure, CollectorFailure)
        self.assertEqual('FailingCollector', failure.collector_name)
        self.assertEqual('RuntimeError', failure.error_type)
        self.assertEqual('boom', failure.error_message)

    def test_all_healthy_is_ok(self):
        self.registry.register(HealthyCollector('a'))
        self.registry.register(HealthyCollector('b'))
        result = self.registry.collect_tolerant()
        self.assertTrue(result.ok)
        self.assertEqual([], result.failures)
        self.assertEqual({'a', 'b'}, {m.name for m in result.metrics})
        self.assertEqual(2, len(result))

    def test_partial_output_discarded_on_failure(self):
        self.registry.register(PartialThenFailingCollector())
        self.registry.register(HealthyCollector())
        result = self.registry.collect_tolerant()
        names = [m.name for m in result.metrics]
        # No half histogram: the metric yielded before the raise is dropped.
        self.assertNotIn('partial_histogram', names)
        self.assertEqual(['healthy_metric'], names)
        self.assertEqual(1, len(result.failures))

    def test_strict_collect_still_raises(self):
        self.registry.register(FailingCollector())
        with self.assertRaises(RuntimeError):
            list(self.registry.collect())

    def test_partial_collector_strict_collect_raises(self):
        self.registry.register(PartialThenFailingCollector())
        with self.assertRaises(RuntimeError):
            list(self.registry.collect())

    def test_retry_after_failure_and_no_stale_replay(self):
        flaky = FlakyCollector()
        self.registry.register(flaky)
        first = self.registry.collect_tolerant()
        self.assertFalse(first.ok)
        self.assertEqual([], first.metrics)
        # Next scrape retries the collector rather than replaying old data.
        second = self.registry.collect_tolerant()
        self.assertTrue(second.ok)
        self.assertEqual(['flaky_metric'], [m.name for m in second.metrics])
        self.assertEqual(2.0, second.metrics[0].samples[0].value)
        # And again: fresh values, not a cached replay.
        third = self.registry.collect_tolerant()
        self.assertEqual(3.0, third.metrics[0].samples[0].value)

    def test_failure_totals_do_not_recurse(self):
        failing = FailingCollector()
        self.registry.register(failing)
        self.registry.register(HealthyCollector())
        self.registry.collect_tolerant()
        self.registry.collect_tolerant()
        self.assertEqual({'FailingCollector': 2},
                         self.registry.get_collector_failure_totals())
        # Diagnostics are not metrics: they never appear in collection output.
        result = self.registry.collect_tolerant()
        names = [m.name for m in result.metrics]
        self.assertEqual(['healthy_metric'], names)
        self.assertEqual(3, failing.calls)

    def test_target_info_included(self):
        registry = CollectorRegistry(target_info={'foo': 'bar'})
        registry.register(FailingCollector())
        result = registry.collect_tolerant()
        self.assertEqual(['target'], [m.name for m in result.metrics])
        self.assertFalse(result.ok)

    def test_result_iterates_metrics(self):
        self.registry.register(HealthyCollector())
        result = self.registry.collect_tolerant()
        self.assertEqual(['healthy_metric'], [m.name for m in result])


if __name__ == '__main__':
    unittest.main()
