import copy
import unittest

import pytest

from prometheus_client import (
    assert_valid_metric_family, MetricFamilyValidationError,
    validate_metric_family,
)
from prometheus_client.core import (
    CollectorRegistry, CounterMetricFamily, GaugeHistogramMetricFamily,
    GaugeMetricFamily, HistogramMetricFamily, InfoMetricFamily, Metric,
    StateSetMetricFamily, SummaryMetricFamily, UnknownMetricFamily,
)
from prometheus_client.exposition import generate_latest
from prometheus_client.openmetrics import exposition as openmetrics_exposition


def rules(issues):
    return {issue.rule for issue in issues}


class TestValidFamilies(unittest.TestCase):
    def test_counter_with_created(self):
        metric = CounterMetricFamily('c', 'help', labels=['l'])
        metric.add_metric(['a'], 5, created=123)
        self.assertEqual([], validate_metric_family(metric))

    def test_gauge(self):
        metric = GaugeMetricFamily('g', 'help', labels=['l'])
        metric.add_metric(['a'], 1.5)
        metric.add_metric(['b'], 2.5)
        self.assertEqual([], validate_metric_family(metric))

    def test_unknown(self):
        self.assertEqual([], validate_metric_family(UnknownMetricFamily('u', 'help', value=3)))

    def test_summary(self):
        metric = SummaryMetricFamily('s', 'help', count_value=3, sum_value=10)
        self.assertEqual([], validate_metric_family(metric))

    def test_summary_with_quantiles(self):
        metric = SummaryMetricFamily('s', 'help', count_value=3, sum_value=10)
        metric.add_sample('s', {'quantile': '0.5'}, 2)
        metric.add_sample('s', {'quantile': '0.9'}, 4)
        self.assertEqual([], validate_metric_family(metric))

    def test_histogram(self):
        metric = HistogramMetricFamily('h', 'help', buckets=[('0.5', 1), ('1', 2), ('+Inf', 3)], sum_value=4)
        self.assertEqual([], validate_metric_family(metric))

    def test_histogram_with_negative_bucket_boundary(self):
        # A negative bucket boundary is legal: it is not the same problem as
        # a negative count. The family omits _sum/_count in this case.
        metric = HistogramMetricFamily('h', 'help', buckets=[('-1', 1), ('1', 2), ('+Inf', 3)])
        self.assertEqual([], validate_metric_family(metric))

    def test_gaugehistogram(self):
        metric = GaugeHistogramMetricFamily('gh', 'help', buckets=[('1', 1), ('+Inf', 2)], gsum_value=3)
        self.assertEqual([], validate_metric_family(metric))

    def test_info(self):
        self.assertEqual([], validate_metric_family(InfoMetricFamily('i', 'help', value={'k': 'v'})))

    def test_stateset(self):
        self.assertEqual([], validate_metric_family(StateSetMetricFamily('ss', 'help', value={'a': True, 'b': False})))

    def test_unit_suffix(self):
        self.assertEqual([], validate_metric_family(GaugeMetricFamily('g', 'help', value=1, unit='seconds')))

    def test_special_floats_legal_for_gauge(self):
        metric = GaugeMetricFamily('g', 'help', labels=['l'])
        metric.add_metric(['nan'], float('nan'))
        metric.add_metric(['pinf'], float('inf'))
        metric.add_metric(['ninf'], float('-inf'))
        self.assertEqual([], validate_metric_family(metric))

    def test_does_not_modify_input(self):
        metric = HistogramMetricFamily('h', 'help', buckets=[('1', 2), ('0.5', 1)], sum_value=4)
        snapshot = copy.deepcopy(metric)
        validate_metric_family(metric)
        self.assertEqual(snapshot, metric)


class TestFamilyLevelRules(unittest.TestCase):
    def test_invalid_type(self):
        metric = Metric('m', 'help', 'gauge')
        metric.add_sample('m', {}, 1)
        metric.type = 'bogus'
        issues = validate_metric_family(metric)
        self.assertIn('family.type', rules(issues))

    def test_unit_mismatch(self):
        metric = GaugeMetricFamily('m', 'help', value=1, unit='seconds')
        metric.unit = 'bytes'
        issues = validate_metric_family(metric)
        self.assertIn('family.unit', rules(issues))

    def test_assert_raises_with_issues(self):
        metric = Metric('m', 'help', 'gauge')
        metric.type = 'bogus'
        with self.assertRaises(MetricFamilyValidationError) as cm:
            assert_valid_metric_family(metric)
        self.assertEqual('m', cm.exception.metric_name)
        self.assertTrue(cm.exception.issues)
        self.assertIn('family.type', str(cm.exception))

    def test_assert_passes_valid_family(self):
        assert_valid_metric_family(GaugeMetricFamily('g', 'help', value=1))


class TestSampleRules(unittest.TestCase):
    def test_derived_sample_name(self):
        metric = Metric('g', 'help', 'gauge')
        metric.add_sample('g_total', {}, 1)
        issues = validate_metric_family(metric)
        self.assertIn('sample.derived_name', rules(issues))
        issue = next(i for i in issues if i.rule == 'sample.derived_name')
        self.assertEqual('g_total', issue.sample_name)
        self.assertEqual(0, issue.sample_index)

    def test_counter_sample_names(self):
        metric = Metric('c', 'help', 'counter')
        metric.add_sample('c', {}, 1)
        self.assertIn('sample.derived_name', rules(validate_metric_family(metric)))

    def test_labels_inconsistent(self):
        metric = Metric('g', 'help', 'gauge')
        metric.add_sample('g', {'a': '1'}, 1)
        metric.add_sample('g', {'b': '2'}, 2)
        issues = validate_metric_family(metric)
        self.assertIn('labels.inconsistent', rules(issues))
        issue = next(i for i in issues if i.rule == 'labels.inconsistent')
        self.assertEqual({'b': '2'}, issue.sample_labels)

    def test_duplicate_series(self):
        metric = Metric('g', 'help', 'gauge')
        metric.add_sample('g', {'a': '1'}, 1)
        metric.add_sample('g', {'a': '1'}, 2)
        self.assertIn('series.duplicate', rules(validate_metric_family(metric)))

    def test_reserved_label(self):
        metric = Metric('g', 'help', 'gauge')
        metric.add_sample('g', {'__reserved': '1'}, 1)
        self.assertIn('labels.reserved', rules(validate_metric_family(metric)))

    def test_le_misplaced(self):
        metric = Metric('g', 'help', 'gauge')
        metric.add_sample('g', {'le': '1'}, 1)
        self.assertIn('sample.le_misplaced', rules(validate_metric_family(metric)))

    def test_missing_value(self):
        metric = Metric('g', 'help', 'gauge')
        metric.add_sample('g', {}, None)
        self.assertIn('sample.value_missing', rules(validate_metric_family(metric)))


class TestSpecialFloatRules(unittest.TestCase):
    def test_counter_negative(self):
        metric = Metric('c', 'help', 'counter')
        metric.add_sample('c_total', {}, -1)
        self.assertIn('counter.value_negative', rules(validate_metric_family(metric)))

    def test_counter_nan(self):
        metric = Metric('c', 'help', 'counter')
        metric.add_sample('c_total', {}, float('nan'))
        self.assertIn('counter.value_nan', rules(validate_metric_family(metric)))

    def test_count_negative(self):
        metric = Metric('s', 'help', 'summary')
        metric.add_sample('s_count', {}, -1)
        metric.add_sample('s_sum', {}, 5)
        self.assertIn('summary.count_negative', rules(validate_metric_family(metric)))

    def test_count_nan(self):
        metric = Metric('s', 'help', 'summary')
        metric.add_sample('s_count', {}, float('nan'))
        metric.add_sample('s_sum', {}, 5)
        self.assertIn('summary.count_nan', rules(validate_metric_family(metric)))

    def test_sum_nan_is_legal(self):
        metric = Metric('s', 'help', 'summary')
        metric.add_sample('s_count', {}, 1)
        metric.add_sample('s_sum', {}, float('nan'))
        self.assertEqual([], validate_metric_family(metric))

    def test_stateset_value(self):
        metric = Metric('ss', 'help', 'stateset')
        metric.add_sample('ss', {'ss': 'a'}, 2)
        self.assertIn('stateset.value', rules(validate_metric_family(metric)))

    def test_stateset_missing_state_label(self):
        metric = Metric('ss', 'help', 'stateset')
        metric.add_sample('ss', {}, 1)
        self.assertIn('stateset.state_label_missing', rules(validate_metric_family(metric)))

    def test_info_value(self):
        metric = Metric('i', 'help', 'info')
        metric.add_sample('i_info', {}, 0)
        self.assertIn('info.value', rules(validate_metric_family(metric)))


class TestCompositeStructure(unittest.TestCase):
    def make_histogram(self, buckets, count=None, sum_=None):
        metric = Metric('h', 'help', 'histogram')
        for le, value in buckets:
            metric.add_sample('h_bucket', {'le': le}, value)
        if count is not None:
            metric.add_sample('h_count', {}, count)
        if sum_ is not None:
            metric.add_sample('h_sum', {}, sum_)
        return metric

    def test_buckets_unsorted(self):
        metric = self.make_histogram([('1', 1), ('0.5', 2), ('+Inf', 3)])
        self.assertIn('histogram.buckets_sorted', rules(validate_metric_family(metric)))

    def test_buckets_not_cumulative(self):
        metric = self.make_histogram([('0.5', 5), ('1', 2), ('+Inf', 3)])
        self.assertIn('histogram.buckets_cumulative', rules(validate_metric_family(metric)))

    def test_bucket_inf_missing(self):
        metric = self.make_histogram([('0.5', 1), ('1', 2)])
        issues = validate_metric_family(metric)
        self.assertIn('histogram.bucket_inf_missing', rules(issues))
        issue = next(i for i in issues if i.rule == 'histogram.bucket_inf_missing')
        self.assertEqual('h_bucket', issue.sample_name)

    def test_bucket_bound_invalid(self):
        metric = self.make_histogram([('not-a-number', 1), ('+Inf', 1)])
        self.assertIn('histogram.bucket_bound_invalid', rules(validate_metric_family(metric)))

    def test_bucket_le_missing(self):
        metric = Metric('h', 'help', 'histogram')
        metric.add_sample('h_bucket', {}, 1)
        self.assertIn('histogram.bucket_le_missing', rules(validate_metric_family(metric)))

    def test_count_mismatch(self):
        metric = self.make_histogram([('1', 1), ('+Inf', 3)], count=2, sum_=4)
        self.assertIn('histogram.count_mismatch', rules(validate_metric_family(metric)))

    def test_count_sum_unpaired(self):
        metric = self.make_histogram([('1', 1), ('+Inf', 3)], count=3)
        self.assertIn('histogram.count_sum_unpaired', rules(validate_metric_family(metric)))

    def test_negative_bucket_count_is_an_error(self):
        # Negative bucket boundaries are fine, negative bucket counts are not.
        metric = self.make_histogram([('-1', -5), ('+Inf', 1)])
        self.assertIn('histogram.bucket_count_negative', rules(validate_metric_family(metric)))

    def test_summary_count_sum_unpaired(self):
        metric = Metric('s', 'help', 'summary')
        metric.add_sample('s_sum', {}, 5)
        self.assertIn('summary.count_sum_unpaired', rules(validate_metric_family(metric)))

    def test_gaugehistogram_gcount_mismatch(self):
        metric = Metric('gh', 'help', 'gaugehistogram')
        metric.add_sample('gh_bucket', {'le': '1'}, 1)
        metric.add_sample('gh_bucket', {'le': '+Inf'}, 2)
        metric.add_sample('gh_gcount', {}, 5)
        metric.add_sample('gh_gsum', {}, 3)
        self.assertIn('gaugehistogram.count_mismatch', rules(validate_metric_family(metric)))

    def test_histogram_per_label_set(self):
        # Each label set forms its own bucket series; the second one is unsorted.
        metric = Metric('h', 'help', 'histogram')
        metric.add_sample('h_bucket', {'l': 'a', 'le': '1'}, 1)
        metric.add_sample('h_bucket', {'l': 'a', 'le': '+Inf'}, 2)
        metric.add_sample('h_bucket', {'l': 'b', 'le': '5'}, 1)
        metric.add_sample('h_bucket', {'l': 'b', 'le': '1'}, 2)
        metric.add_sample('h_bucket', {'l': 'b', 'le': '+Inf'}, 3)
        issues = validate_metric_family(metric)
        self.assertIn('histogram.buckets_sorted', rules(issues))
        issue = next(i for i in issues if i.rule == 'histogram.buckets_sorted')
        self.assertEqual({'l': 'b', 'le': '1'}, issue.sample_labels)


class BrokenCollector:
    def collect(self):
        metric = Metric('broken', 'help', 'gauge')
        metric.add_sample('broken_total', {}, 1)
        return [metric]


class _ValidCollector:
    def collect(self):
        return [GaugeMetricFamily('g', 'help', value=1)]


class TestStrictExport(unittest.TestCase):
    def setUp(self):
        self.registry = CollectorRegistry()
        self.registry.register(BrokenCollector())

    def test_text_format_strict_raises(self):
        with pytest.raises(MetricFamilyValidationError):
            generate_latest(self.registry, strict=True)

    def test_text_format_default_unchanged(self):
        output = generate_latest(self.registry)
        self.assertIn(b'broken_total 1.0\n', output)

    def test_openmetrics_strict_raises(self):
        with pytest.raises(MetricFamilyValidationError):
            openmetrics_exposition.generate_latest(self.registry, strict=True)

    def test_openmetrics_default_unchanged(self):
        output = openmetrics_exposition.generate_latest(self.registry)
        self.assertIn(b'broken_total 1.0\n', output)

    def test_strict_passes_valid_registry(self):
        registry = CollectorRegistry()
        registry.register(_ValidCollector())
        self.assertIn(b'g 1.0\n', generate_latest(registry, strict=True))
        self.assertIn(b'# EOF\n', openmetrics_exposition.generate_latest(registry, strict=True))


if __name__ == '__main__':
    unittest.main()
