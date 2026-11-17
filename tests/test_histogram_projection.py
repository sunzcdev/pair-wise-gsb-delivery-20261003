import pytest

from prometheus_client import (
    BucketProjectionCollector, CollectorRegistry, Counter, Histogram,
    project_histogram_buckets,
)
from prometheus_client.exposition import generate_latest
from prometheus_client.histogram_projection import _prepare_target_buckets
from prometheus_client.metrics_core import (
    GaugeHistogramMetricFamily, HistogramMetricFamily,
)
from prometheus_client.openmetrics.exposition import \
    generate_latest as openmetrics_generate_latest
from prometheus_client.openmetrics.parser import \
    text_string_to_metric_families as openmetrics_parse
from prometheus_client.parser import \
    text_string_to_metric_families as text_parse
from prometheus_client.samples import Exemplar


def _bucket_values(metric):
    """Return a dict of finite/le bucket label -> value for a metric family."""
    return {s.labels['le']: s.value
            for s in metric.samples if s.name == metric.name + '_bucket'}


def _sample(metric, name, labels):
    for s in metric.samples:
        if s.name == name and s.labels == labels:
            return s
    raise KeyError((name, labels))


class TestPrepareTargetBuckets:
    def test_appends_inf_when_missing(self):
        assert _prepare_target_buckets([1.0, 2.0]) == [1.0, 2.0, float('inf')]

    def test_keeps_inf_when_present(self):
        assert _prepare_target_buckets([1.0, float('inf')]) == [1.0, float('inf')]

    def test_accepts_string_bounds(self):
        assert _prepare_target_buckets(['1', '2.5']) == [1.0, 2.5, float('inf')]

    @pytest.mark.parametrize('bad', [
        [float('nan')],
        [1.0, float('nan')],
        [1.0, 1.0],
        [2.0, 1.0],
        [float('-inf')],
        [],
    ])
    def test_rejects_invalid_targets(self, bad):
        with pytest.raises(ValueError):
            _prepare_target_buckets(bad)

    def test_inf_must_be_last(self):
        with pytest.raises(ValueError):
            _prepare_target_buckets([float('inf'), 1.0])


class TestProjectHistogramBuckets:
    def setup_method(self):
        self.registry = CollectorRegistry()
        # Bounds: 1, 2.5, 5, +Inf
        self.histogram = Histogram(
            'h', 'help', registry=self.registry, buckets=(1.0, 2.5, 5.0))
        for value in (0.5, 1.5, 2.0, 4.0):
            self.histogram.observe(value)

    def _source(self):
        return next(self.registry.collect())

    def test_retains_only_requested_buckets_and_preserves_cumulative_counts(self):
        projected = project_histogram_buckets(self._source(), (1.0, 5.0))

        assert _bucket_values(projected) == {
            '1.0': 1,       # only 0.5
            '5.0': 4,       # all four observations
            '+Inf': 4,
        }
        # Counts are the original cumulative counts, not recomputed intervals.
        assert _sample(projected, 'h_bucket', {'le': '5.0'}).value == 4
        assert _sample(projected, 'h_bucket', {'le': '+Inf'}).value == 4

    def test_count_sum_and_inf_are_preserved(self):
        source = self._source()
        projected = project_histogram_buckets(source, (2.5,))

        assert _sample(projected, 'h_count', {}).value == \
            _sample(source, 'h_count', {}).value == 4
        assert _sample(projected, 'h_sum', {}).value == \
            _sample(source, 'h_sum', {}).value == 8.0
        assert _bucket_values(projected)['+Inf'] == 4

    def test_inf_is_always_retained_even_if_omitted(self):
        projected = project_histogram_buckets(self._source(), (1.0,))
        assert set(_bucket_values(projected)) == {'1.0', '+Inf'}

    def test_missing_boundary_is_rejected(self):
        with pytest.raises(ValueError, match='missing'):
            project_histogram_buckets(self._source(), (1.0, 3.0, 5.0))

    def test_nan_target_is_rejected(self):
        with pytest.raises(ValueError):
            project_histogram_buckets(self._source(), (1.0, float('nan')))

    def test_duplicate_target_is_rejected(self):
        with pytest.raises(ValueError):
            project_histogram_buckets(self._source(), (1.0, 1.0, 5.0))

    def test_non_histogram_metric_is_rejected(self):
        counter = Counter('c', 'help', registry=CollectorRegistry())
        counter.inc()
        family = counter.collect()[0]
        with pytest.raises(ValueError):
            project_histogram_buckets(family, (1.0,))

    def test_source_metric_is_not_modified(self):
        source = self._source()
        before = list(source.samples)
        project_histogram_buckets(source, (1.0, 5.0))
        assert source.samples == before
        # The source still exposes every original bucket.
        assert set(_bucket_values(source)) == {
            '1.0', '2.5', '5.0', '+Inf'}

    def test_metadata_is_preserved(self):
        projected = project_histogram_buckets(self._source(), (1.0,))
        assert projected.name == 'h'
        assert projected.documentation == 'help'
        assert projected.type == 'histogram'

    def test_labels_are_preserved_per_series(self):
        registry = CollectorRegistry()
        histogram = Histogram(
            'hl', 'help', labelnames=('code',),
            registry=registry, buckets=(1.0, 2.5, 5.0))
        histogram.labels('200').observe(0.5)
        histogram.labels('200').observe(4.0)
        histogram.labels('500').observe(2.0)

        projected = project_histogram_buckets(next(registry.collect()), (1.0, 5.0))
        bucket_labels = [
            (s.labels['code'], s.labels['le'])
            for s in projected.samples if s.name == 'hl_bucket']

        assert ('200', '1.0') in bucket_labels
        assert ('200', '5.0') in bucket_labels
        assert ('200', '+Inf') in bucket_labels
        assert ('500', '1.0') in bucket_labels
        assert ('500', '5.0') in bucket_labels
        assert ('500', '+Inf') in bucket_labels
        # The 2.5 boundary was dropped for every series.
        assert all(le != '2.5' for _, le in bucket_labels)

        assert _sample(projected, 'hl_bucket',
                       {'code': '200', 'le': '5.0'}).value == 2
        assert _sample(projected, 'hl_bucket',
                       {'code': '500', 'le': '5.0'}).value == 1

    def test_exemplar_kept_only_on_retained_buckets(self):
        family = HistogramMetricFamily('h', 'help')
        buckets = [
            ('1.0', 1, Exemplar({'id': 'a'}, 0.5)),
            ('2.5', 2, Exemplar({'id': 'b'}, 2.0)),
            ('5.0', 3, Exemplar({'id': 'c'}, 4.0)),
            ('+Inf', 3),
        ]
        family.add_metric([], buckets, sum_value=6.5)

        projected = project_histogram_buckets(family, (1.0, 5.0))

        retained = {s.labels['le']: s.exemplar
                    for s in projected.samples if s.name == 'h_bucket'}
        assert retained['1.0'] == Exemplar({'id': 'a'}, 0.5)
        assert retained['5.0'] == Exemplar({'id': 'c'}, 4.0)
        assert retained['+Inf'] is None
        # The dropped 2.5 bucket (and its exemplar) must not appear.
        assert '2.5' not in retained

    def test_exemplar_on_inf_bucket_is_retained(self):
        family = HistogramMetricFamily('h', 'help')
        family.add_metric([], [
            ('1.0', 0),
            ('+Inf', 1, Exemplar({'id': 'z'}, 9.0)),
        ], sum_value=9.0)
        projected = project_histogram_buckets(family, (1.0,))
        inf_sample = _sample(projected, 'h_bucket', {'le': '+Inf'})
        assert inf_sample.exemplar == Exemplar({'id': 'z'}, 9.0)

    def test_gauge_histogram_is_supported(self):
        family = GaugeHistogramMetricFamily('gh', 'help', buckets=[
            ('1.0', 1), ('2.5', 2), ('+Inf', 3)], gsum_value=4.0)
        projected = project_histogram_buckets(family, (1.0,))
        assert projected.type == 'gaugehistogram'
        assert _bucket_values(projected) == {'1.0': 1, '+Inf': 3}
        assert _sample(projected, 'gh_gcount', {}).value == 3
        assert _sample(projected, 'gh_gsum', {}).value == 4.0

    def test_projecting_a_projection_is_an_ordered_subset(self):
        first = project_histogram_buckets(self._source(), (1.0, 2.5, 5.0))
        second = project_histogram_buckets(first, (2.5,))
        assert set(_bucket_values(second)) == {'2.5', '+Inf'}
        assert _bucket_values(second)['2.5'] == 3

    def test_negative_buckets_are_supported_when_present_in_source(self):
        family = HistogramMetricFamily('h', 'help')
        # Negative buckets omit _sum/_count by convention; +Inf still carries
        # the total count.
        family.add_metric([], [
            ('-2.0', 1), ('-1.0', 2), ('0.0', 3), ('+Inf', 4)],
            sum_value=None)
        projected = project_histogram_buckets(family, (-1.0, 0.0))
        assert _bucket_values(projected) == {
            '-1.0': 2, '0.0': 3, '+Inf': 4}

    def test_rejects_missing_inf_in_source(self):
        family = HistogramMetricFamily('h', 'help')
        family.add_metric([], [('1.0', 1), ('2.5', 2)], sum_value=2)
        # Remove the +Inf bucket to simulate a malformed source family.
        family.samples = [
            s for s in family.samples if s.labels.get('le') != '+Inf']
        with pytest.raises(ValueError, match=r'\+Inf'):
            project_histogram_buckets(family, (1.0,))

    def test_inf_target_is_idempotent(self):
        projected = project_histogram_buckets(
            self._source(), (1.0, float('inf')))
        assert set(_bucket_values(projected)) == {'1.0', '+Inf'}


class TestBucketProjectionCollector:
    def setup_method(self):
        self.registry = CollectorRegistry()
        self.histogram = Histogram(
            'h', 'help', registry=self.registry, buckets=(1.0, 2.5, 5.0))
        for value in (0.5, 1.5, 4.0):
            self.histogram.observe(value)
        self.counter = Counter('c', 'help', registry=self.registry)
        self.counter.inc(7)

    def test_collect_projects_histograms_and_passes_other_metrics_through(self):
        collector = BucketProjectionCollector(self.registry, (1.0, 5.0))
        families = list(collector.collect())
        by_name = {m.name: m for m in families}

        assert set(by_name) == {'h', 'c'}
        assert _bucket_values(by_name['h']) == {
            '1.0': 1, '5.0': 3, '+Inf': 3}
        assert by_name['c'].samples[0].value == 7

    def test_does_not_mutate_underlying_registry(self):
        BucketProjectionCollector(self.registry, (1.0,)).collect()
        source = next(self.registry.collect())
        assert set(_bucket_values(source)) == {'1.0', '2.5', '5.0', '+Inf'}

    def test_describe_delegates_to_underlying_collector(self):
        collector = BucketProjectionCollector(self.registry, (1.0,))
        names = {m.name for m in collector.describe()}
        assert names == {'h', 'c'}

    def test_invalid_buckets_fail_at_construction(self):
        with pytest.raises(ValueError):
            BucketProjectionCollector(self.registry, (float('nan'),))

    def test_works_as_a_registered_collector(self):
        projected_registry = CollectorRegistry()
        projected_registry.register(BucketProjectionCollector(self.registry, (2.5,)))
        output = generate_latest(projected_registry).decode('utf-8')
        assert 'h_bucket{le="2.5"}' in output
        assert 'h_bucket{le="1.0"}' not in output
        assert 'h_bucket{le="+Inf"}' in output
        assert 'h_count' in output
        assert 'h_sum' in output
        # The counter passes through untouched.
        assert 'c_total 7.0' in output


class TestRoundTrip:
    def _family(self):
        registry = CollectorRegistry()
        histogram = Histogram(
            'rt', 'help', labelnames=('code',),
            registry=registry, buckets=(1.0, 2.5, 5.0))
        histogram.labels('200').observe(0.5, {'trace_id': 'a'})
        histogram.labels('200').observe(4.0, {'trace_id': 'b'})
        histogram.labels('500').observe(2.0)
        return project_histogram_buckets(next(registry.collect()), (1.0, 5.0))

    def test_text_output_reparses_to_matching_family(self):
        collector = _SingleCollector(self._family())
        output = generate_latest(collector).decode('utf-8')
        parsed = next(text_parse(output))

        assert parsed.name == 'rt'
        assert parsed.type == 'histogram'
        projected_buckets = {
            (s.labels.get('code'), s.labels['le']): s.value
            for s in parsed.samples if s.name == 'rt_bucket'}
        assert projected_buckets == {
            ('200', '1.0'): 1,
            ('200', '5.0'): 2,
            ('200', '+Inf'): 2,
            ('500', '1.0'): 0,
            ('500', '5.0'): 1,
            ('500', '+Inf'): 1,
        }

    def test_openmetrics_output_reparses_and_validates(self):
        collector = _SingleCollector(self._family())
        output = openmetrics_generate_latest(collector).decode('utf-8')
        # The strict OpenMetrics parser enforces bucket order, +Inf and that the
        # count matches +Inf. A successful parse proves the family is valid.
        parsed = next(openmetrics_parse(output))

        assert parsed.name == 'rt'
        bucket_samples = [s for s in parsed.samples if s.name == 'rt_bucket']
        # Exemplars survive only on retained buckets.
        exemplars = {
            (s.labels['code'], s.labels['le']): s.exemplar
            for s in bucket_samples if s.exemplar is not None}
        assert set(exemplars) == {('200', '1.0'), ('200', '5.0')}
        assert all(s.labels['le'] != '2.5' for s in bucket_samples)
        assert bucket_samples[-1].labels['le'] == '+Inf'

    def test_openmetrics_text_contains_only_retained_buckets(self):
        collector = _SingleCollector(self._family())
        output = openmetrics_generate_latest(collector).decode('utf-8')
        assert 'rt_bucket{' in output
        assert 'le="2.5"' not in output
        assert 'le="1.0"' in output
        assert 'le="5.0"' in output
        assert 'le="+Inf"' in output
        assert output.rstrip().endswith('# EOF')


class _SingleCollector:
    def __init__(self, metric):
        self._metric = metric

    def collect(self):
        return [self._metric]
