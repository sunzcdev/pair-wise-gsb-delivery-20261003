import glob
import math
import os
import shutil
import struct
import tempfile
import unittest
from unittest import mock
import warnings

from prometheus_client import mmap_dict, values
from prometheus_client.core import (
    CollectorRegistry, Counter, Gauge, Histogram, Sample, Summary,
)
from prometheus_client.multiprocess import (
    build_cleanup_plan, default_process_is_alive, execute_cleanup_plan,
    mark_process_dead, MultiProcessCollector, parse_metric_filename,
    REASON_CUMULATIVE_METRIC, REASON_LIVE_GAUGE_DEAD_PROCESS,
    REASON_LIVE_GAUGE_PROCESS_ALIVE, REASON_NONLIVE_GAUGE,
    REASON_PID_NOT_REQUESTED,
)
from prometheus_client.openmetrics.exposition import (
    generate_latest as openmetrics_generate_latest,
)
from prometheus_client.values import (
    get_value_class, MultiProcessValue, MutexValue,
)


class TestMultiProcessDeprecation(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.mkdtemp()

    def tearDown(self):
        os.environ.pop('prometheus_multiproc_dir', None)
        os.environ.pop('PROMETHEUS_MULTIPROC_DIR', None)
        values.close_all_multiprocess_files()
        values.ValueClass = MutexValue
        shutil.rmtree(self.tempdir)

    def test_deprecation_warning(self):
        os.environ['prometheus_multiproc_dir'] = self.tempdir
        with warnings.catch_warnings(record=True) as w:
            warnings.simplefilter("always")
            values.ValueClass = get_value_class()
            registry = CollectorRegistry()
            collector = MultiProcessCollector(registry)
            Counter('c', 'help', registry=None)

            assert os.environ['PROMETHEUS_MULTIPROC_DIR'] == self.tempdir
            if os.name != 'nt':
                assert len(w) == 1
                assert issubclass(w[-1].category, DeprecationWarning)
                assert "PROMETHEUS_MULTIPROC_DIR" in str(w[-1].message)
            else:
                assert len(w) == 0

    def test_mark_process_dead_respects_lowercase(self):
        os.environ['prometheus_multiproc_dir'] = self.tempdir
        # Just test that this does not raise with a lowercase env var. The
        # logic is tested elsewhere.
        mark_process_dead(123)

    def test_mark_process_dead_noop_when_path_unset(self):
        os.environ.pop('prometheus_multiproc_dir', None)
        os.environ.pop('PROMETHEUS_MULTIPROC_DIR', None)
        mark_process_dead(123)


class TestMultiProcess(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.mkdtemp()
        os.environ['PROMETHEUS_MULTIPROC_DIR'] = self.tempdir
        values.ValueClass = MultiProcessValue(lambda: 123)
        self.registry = CollectorRegistry(support_collectors_without_names=True)
        self.collector = MultiProcessCollector(self.registry)

    @property
    def _value_class(self):
        return

    def tearDown(self):
        del os.environ['PROMETHEUS_MULTIPROC_DIR']
        values.close_all_multiprocess_files()
        values.ValueClass = MutexValue
        shutil.rmtree(self.tempdir)

    def test_counter_adds(self):
        c1 = Counter('c', 'help', registry=None)
        values.ValueClass = MultiProcessValue(lambda: 456)
        c2 = Counter('c', 'help', registry=None)
        self.assertEqual(0, self.registry.get_sample_value('c_total'))
        c1.inc(1)
        c2.inc(2)
        self.assertEqual(3, self.registry.get_sample_value('c_total'))

    def test_summary_adds(self):
        s1 = Summary('s', 'help', registry=None)
        values.ValueClass = MultiProcessValue(lambda: 456)
        s2 = Summary('s', 'help', registry=None)
        self.assertEqual(0, self.registry.get_sample_value('s_count'))
        self.assertEqual(0, self.registry.get_sample_value('s_sum'))
        s1.observe(1)
        s2.observe(2)
        self.assertEqual(2, self.registry.get_sample_value('s_count'))
        self.assertEqual(3, self.registry.get_sample_value('s_sum'))

    def test_histogram_adds(self):
        h1 = Histogram('h', 'help', registry=None)
        values.ValueClass = MultiProcessValue(lambda: 456)
        h2 = Histogram('h', 'help', registry=None)
        self.assertEqual(0, self.registry.get_sample_value('h_count'))
        self.assertEqual(0, self.registry.get_sample_value('h_sum'))
        self.assertEqual(0, self.registry.get_sample_value('h_bucket', {'le': '5.0'}))
        h1.observe(1)
        h2.observe(2)
        self.assertEqual(2, self.registry.get_sample_value('h_count'))
        self.assertEqual(3, self.registry.get_sample_value('h_sum'))
        self.assertEqual(2, self.registry.get_sample_value('h_bucket', {'le': '5.0'}))

    def test_histogram_nan(self):
        h1 = Histogram('h', 'help', registry=None)
        values.ValueClass = MultiProcessValue(lambda: 456)
        h2 = Histogram('h', 'help', registry=None)
        h1.observe(float('nan'))
        h2.observe(2)

        self.assertEqual(1, self.registry.get_sample_value('h_bucket', {'le': '5.0'}))
        self.assertEqual(2, self.registry.get_sample_value('h_bucket', {'le': '+Inf'}))
        self.assertEqual(2, self.registry.get_sample_value('h_count'))
        self.assertTrue(math.isnan(self.registry.get_sample_value('h_sum')))

    def test_histogram_negative_buckets(self):
        h1 = Histogram('h', 'help', labelnames=['l'], buckets=[-1, 0, 1], registry=None)
        values.ValueClass = MultiProcessValue(lambda: 456)
        h2 = Histogram('h', 'help', labelnames=['l'], buckets=[-1, 0, 1], registry=None)
        h1.labels('x').observe(-0.5)
        h2.labels('x').observe(2)

        self.assertEqual(1, self.registry.get_sample_value('h_bucket', {'l': 'x', 'le': '0.0'}))
        self.assertEqual(2, self.registry.get_sample_value('h_bucket', {'l': 'x', 'le': '+Inf'}))
        self.assertEqual(2, self.registry.get_sample_value('h_count', {'l': 'x'}))
        self.assertEqual(1.5, self.registry.get_sample_value('h_sum', {'l': 'x'}))
        # OpenMetrics 1.0 doesn't allow _sum with negative buckets, or _count without _sum. 2.0 has both.
        om_1_0 = openmetrics_generate_latest(self.registry, version='1.0.0')
        self.assertNotIn(b'h_sum', om_1_0)
        self.assertNotIn(b'h_count', om_1_0)
        om_2_0 = openmetrics_generate_latest(self.registry, version='2.0.0')
        self.assertIn(b'h_sum{l="x"} 1.5', om_2_0)
        self.assertIn(b'h_count{l="x"} 2.0', om_2_0)

    def test_gauge_all(self):
        g1 = Gauge('g', 'help', registry=None)
        values.ValueClass = MultiProcessValue(lambda: 456)
        g2 = Gauge('g', 'help', registry=None)
        self.assertEqual(0, self.registry.get_sample_value('g', {'pid': '123'}))
        self.assertEqual(0, self.registry.get_sample_value('g', {'pid': '456'}))
        g1.set(1)
        g2.set(2)
        mark_process_dead(123)
        self.assertEqual(1, self.registry.get_sample_value('g', {'pid': '123'}))
        self.assertEqual(2, self.registry.get_sample_value('g', {'pid': '456'}))

    def test_gauge_liveall(self):
        g1 = Gauge('g', 'help', registry=None, multiprocess_mode='liveall')
        values.ValueClass = MultiProcessValue(lambda: 456)
        g2 = Gauge('g', 'help', registry=None, multiprocess_mode='liveall')
        self.assertEqual(0, self.registry.get_sample_value('g', {'pid': '123'}))
        self.assertEqual(0, self.registry.get_sample_value('g', {'pid': '456'}))
        g1.set(1)
        g2.set(2)
        self.assertEqual(1, self.registry.get_sample_value('g', {'pid': '123'}))
        self.assertEqual(2, self.registry.get_sample_value('g', {'pid': '456'}))
        values.close_all_multiprocess_files()
        mark_process_dead(123, os.environ['PROMETHEUS_MULTIPROC_DIR'])
        self.assertEqual(None, self.registry.get_sample_value('g', {'pid': '123'}))
        self.assertEqual(2, self.registry.get_sample_value('g', {'pid': '456'}))

    def test_gauge_min(self):
        g1 = Gauge('g', 'help', registry=None, multiprocess_mode='min')
        values.ValueClass = MultiProcessValue(lambda: 456)
        g2 = Gauge('g', 'help', registry=None, multiprocess_mode='min')
        self.assertEqual(0, self.registry.get_sample_value('g'))
        g1.set(1)
        g2.set(2)
        self.assertEqual(1, self.registry.get_sample_value('g'))

    def test_gauge_livemin(self):
        g1 = Gauge('g', 'help', registry=None, multiprocess_mode='livemin')
        values.ValueClass = MultiProcessValue(lambda: 456)
        g2 = Gauge('g', 'help', registry=None, multiprocess_mode='livemin')
        self.assertEqual(0, self.registry.get_sample_value('g'))
        g1.set(1)
        g2.set(2)
        self.assertEqual(1, self.registry.get_sample_value('g'))
        values.close_all_multiprocess_files()
        mark_process_dead(123, os.environ['PROMETHEUS_MULTIPROC_DIR'])
        self.assertEqual(2, self.registry.get_sample_value('g'))

    def test_gauge_max(self):
        g1 = Gauge('g', 'help', registry=None, multiprocess_mode='max')
        values.ValueClass = MultiProcessValue(lambda: 456)
        g2 = Gauge('g', 'help', registry=None, multiprocess_mode='max')
        self.assertEqual(0, self.registry.get_sample_value('g'))
        g1.set(1)
        g2.set(2)
        self.assertEqual(2, self.registry.get_sample_value('g'))

    def test_gauge_livemax(self):
        g1 = Gauge('g', 'help', registry=None, multiprocess_mode='livemax')
        values.ValueClass = MultiProcessValue(lambda: 456)
        g2 = Gauge('g', 'help', registry=None, multiprocess_mode='livemax')
        self.assertEqual(0, self.registry.get_sample_value('g'))
        g1.set(2)
        g2.set(1)
        self.assertEqual(2, self.registry.get_sample_value('g'))
        values.close_all_multiprocess_files()
        mark_process_dead(123, os.environ['PROMETHEUS_MULTIPROC_DIR'])
        self.assertEqual(1, self.registry.get_sample_value('g'))

    def test_gauge_sum(self):
        g1 = Gauge('g', 'help', registry=None, multiprocess_mode='sum')
        values.ValueClass = MultiProcessValue(lambda: 456)
        g2 = Gauge('g', 'help', registry=None, multiprocess_mode='sum')
        self.assertEqual(0, self.registry.get_sample_value('g'))
        g1.set(1)
        g2.set(2)
        self.assertEqual(3, self.registry.get_sample_value('g'))
        values.close_all_multiprocess_files()
        mark_process_dead(123, os.environ['PROMETHEUS_MULTIPROC_DIR'])
        self.assertEqual(3, self.registry.get_sample_value('g'))

    def test_gauge_livesum(self):
        g1 = Gauge('g', 'help', registry=None, multiprocess_mode='livesum')
        values.ValueClass = MultiProcessValue(lambda: 456)
        g2 = Gauge('g', 'help', registry=None, multiprocess_mode='livesum')
        self.assertEqual(0, self.registry.get_sample_value('g'))
        g1.set(1)
        g2.set(2)
        self.assertEqual(3, self.registry.get_sample_value('g'))
        values.close_all_multiprocess_files()
        mark_process_dead(123, os.environ['PROMETHEUS_MULTIPROC_DIR'])
        self.assertEqual(2, self.registry.get_sample_value('g'))

    def test_gauge_mostrecent(self):
        g1 = Gauge('g', 'help', registry=None, multiprocess_mode='mostrecent')
        values.ValueClass = MultiProcessValue(lambda: 456)
        g2 = Gauge('g', 'help', registry=None, multiprocess_mode='mostrecent')
        # Give each set() a distinct timestamp so the result doesn't depend on
        # clock resolution or the order the .db files are read in.
        with mock.patch('prometheus_client.metrics.time.time', side_effect=[1.0, 2.0]):
            g2.set(2)
            g1.set(1)
        self.assertEqual(1, self.registry.get_sample_value('g'))
        values.close_all_multiprocess_files()
        mark_process_dead(123, os.environ['PROMETHEUS_MULTIPROC_DIR'])
        self.assertEqual(1, self.registry.get_sample_value('g'))

    def test_gauge_livemostrecent(self):
        g1 = Gauge('g', 'help', registry=None, multiprocess_mode='livemostrecent')
        values.ValueClass = MultiProcessValue(lambda: 456)
        g2 = Gauge('g', 'help', registry=None, multiprocess_mode='livemostrecent')
        # Give each set() a distinct timestamp so the result doesn't depend on
        # clock resolution or the order the .db files are read in.
        with mock.patch('prometheus_client.metrics.time.time', side_effect=[1.0, 2.0]):
            g2.set(2)
            g1.set(1)
        self.assertEqual(1, self.registry.get_sample_value('g'))
        values.close_all_multiprocess_files()
        mark_process_dead(123, os.environ['PROMETHEUS_MULTIPROC_DIR'])
        self.assertEqual(2, self.registry.get_sample_value('g'))

    def test_namespace_subsystem(self):
        c1 = Counter('c', 'help', registry=None, namespace='ns', subsystem='ss')
        c1.inc(1)
        self.assertEqual(1, self.registry.get_sample_value('ns_ss_c_total'))

    def test_counter_across_forks(self):
        pid = 0
        values.ValueClass = MultiProcessValue(lambda: pid)
        c1 = Counter('c', 'help', registry=None)
        self.assertEqual(0, self.registry.get_sample_value('c_total'))
        c1.inc(1)
        c1.inc(1)
        pid = 1
        c1.inc(1)
        self.assertEqual(3, self.registry.get_sample_value('c_total'))
        self.assertEqual(1, c1._value.get())

    def test_initialization_detects_pid_change(self):
        pid = 0
        values.ValueClass = MultiProcessValue(lambda: pid)

        # can not inspect the files cache directly, as it's a closure, so we
        # check for the actual files themselves
        def files():
            fs = os.listdir(os.environ['PROMETHEUS_MULTIPROC_DIR'])
            fs.sort()
            return fs

        c1 = Counter('c1', 'c1', registry=None)
        self.assertEqual(files(), ['counter_0.db'])
        c2 = Counter('c2', 'c2', registry=None)
        self.assertEqual(files(), ['counter_0.db'])
        pid = 1
        c3 = Counter('c3', 'c3', registry=None)
        self.assertEqual(files(), ['counter_0.db', 'counter_1.db'])

    def test_collect(self):
        pid = 0
        values.ValueClass = MultiProcessValue(lambda: pid)
        labels = {i: i for i in 'abcd'}

        def add_label(key, value):
            l = labels.copy()
            l[key] = value
            return l

        c = Counter('c', 'help', labelnames=labels.keys(), registry=None)
        g = Gauge('g', 'help', labelnames=labels.keys(), registry=None)
        h = Histogram('h', 'help', labelnames=labels.keys(), registry=None)

        c.labels(**labels).inc(1)
        g.labels(**labels).set(1)
        h.labels(**labels).observe(1)

        pid = 1

        c.labels(**labels).inc(1)
        g.labels(**labels).set(1)
        h.labels(**labels).observe(5)

        metrics = {m.name: m for m in self.collector.collect()}

        self.assertEqual(
            metrics['c'].samples, [Sample('c_total', labels, 2.0)]
        )
        metrics['g'].samples.sort(key=lambda x: x[1]['pid'])
        self.assertEqual(metrics['g'].samples, [
            Sample('g', add_label('pid', '0'), 1.0),
            Sample('g', add_label('pid', '1'), 1.0),
        ])

        expected_histogram = [
            Sample('h_sum', labels, 6.0),
            Sample('h_bucket', add_label('le', '0.005'), 0.0),
            Sample('h_bucket', add_label('le', '0.01'), 0.0),
            Sample('h_bucket', add_label('le', '0.025'), 0.0),
            Sample('h_bucket', add_label('le', '0.05'), 0.0),
            Sample('h_bucket', add_label('le', '0.075'), 0.0),
            Sample('h_bucket', add_label('le', '0.1'), 0.0),
            Sample('h_bucket', add_label('le', '0.25'), 0.0),
            Sample('h_bucket', add_label('le', '0.5'), 0.0),
            Sample('h_bucket', add_label('le', '0.75'), 0.0),
            Sample('h_bucket', add_label('le', '1.0'), 1.0),
            Sample('h_bucket', add_label('le', '2.5'), 1.0),
            Sample('h_bucket', add_label('le', '5.0'), 2.0),
            Sample('h_bucket', add_label('le', '7.5'), 2.0),
            Sample('h_bucket', add_label('le', '10.0'), 2.0),
            Sample('h_bucket', add_label('le', '+Inf'), 2.0),
            Sample('h_count', labels, 2.0),
        ]

        self.assertEqual(metrics['h'].samples, expected_histogram)

    def test_collect_histogram_ordering(self):
        pid = 0
        values.ValueClass = MultiProcessValue(lambda: pid)
        labels = {i: i for i in 'abcd'}

        def add_label(key, value):
            l = labels.copy()
            l[key] = value
            return l

        h = Histogram('h', 'help', labelnames=['view'], registry=None)

        h.labels(view='view1').observe(1)

        pid = 1

        h.labels(view='view1').observe(5)
        h.labels(view='view2').observe(1)

        metrics = {m.name: m for m in self.collector.collect()}

        expected_histogram = [
            Sample('h_sum', {'view': 'view1'}, 6.0),
            Sample('h_bucket', {'view': 'view1', 'le': '0.005'}, 0.0),
            Sample('h_bucket', {'view': 'view1', 'le': '0.01'}, 0.0),
            Sample('h_bucket', {'view': 'view1', 'le': '0.025'}, 0.0),
            Sample('h_bucket', {'view': 'view1', 'le': '0.05'}, 0.0),
            Sample('h_bucket', {'view': 'view1', 'le': '0.075'}, 0.0),
            Sample('h_bucket', {'view': 'view1', 'le': '0.1'}, 0.0),
            Sample('h_bucket', {'view': 'view1', 'le': '0.25'}, 0.0),
            Sample('h_bucket', {'view': 'view1', 'le': '0.5'}, 0.0),
            Sample('h_bucket', {'view': 'view1', 'le': '0.75'}, 0.0),
            Sample('h_bucket', {'view': 'view1', 'le': '1.0'}, 1.0),
            Sample('h_bucket', {'view': 'view1', 'le': '2.5'}, 1.0),
            Sample('h_bucket', {'view': 'view1', 'le': '5.0'}, 2.0),
            Sample('h_bucket', {'view': 'view1', 'le': '7.5'}, 2.0),
            Sample('h_bucket', {'view': 'view1', 'le': '10.0'}, 2.0),
            Sample('h_bucket', {'view': 'view1', 'le': '+Inf'}, 2.0),
            Sample('h_count', {'view': 'view1'}, 2.0),
            Sample('h_sum', {'view': 'view2'}, 1.0),
            Sample('h_bucket', {'view': 'view2', 'le': '0.005'}, 0.0),
            Sample('h_bucket', {'view': 'view2', 'le': '0.01'}, 0.0),
            Sample('h_bucket', {'view': 'view2', 'le': '0.025'}, 0.0),
            Sample('h_bucket', {'view': 'view2', 'le': '0.05'}, 0.0),
            Sample('h_bucket', {'view': 'view2', 'le': '0.075'}, 0.0),
            Sample('h_bucket', {'view': 'view2', 'le': '0.1'}, 0.0),
            Sample('h_bucket', {'view': 'view2', 'le': '0.25'}, 0.0),
            Sample('h_bucket', {'view': 'view2', 'le': '0.5'}, 0.0),
            Sample('h_bucket', {'view': 'view2', 'le': '0.75'}, 0.0),
            Sample('h_bucket', {'view': 'view2', 'le': '1.0'}, 1.0),
            Sample('h_bucket', {'view': 'view2', 'le': '2.5'}, 1.0),
            Sample('h_bucket', {'view': 'view2', 'le': '5.0'}, 1.0),
            Sample('h_bucket', {'view': 'view2', 'le': '7.5'}, 1.0),
            Sample('h_bucket', {'view': 'view2', 'le': '10.0'}, 1.0),
            Sample('h_bucket', {'view': 'view2', 'le': '+Inf'}, 1.0),
            Sample('h_count', {'view': 'view2'}, 1.0),
        ]

        self.assertEqual(metrics['h'].samples, expected_histogram)

    def test_restrict(self):
        pid = 0
        values.ValueClass = MultiProcessValue(lambda: pid)
        labels = {i: i for i in 'abcd'}

        def add_label(key, value):
            l = labels.copy()
            l[key] = value
            return l

        c = Counter('c', 'help', labelnames=labels.keys(), registry=None)
        g = Gauge('g', 'help', labelnames=labels.keys(), registry=None)

        c.labels(**labels).inc(1)
        g.labels(**labels).set(1)

        pid = 1

        c.labels(**labels).inc(1)
        g.labels(**labels).set(1)

        metrics = {m.name: m for m in self.registry.restricted_registry(['c_total']).collect()}

        self.assertEqual(metrics.keys(), {'c'})

        self.assertEqual(
            metrics['c'].samples, [Sample('c_total', labels, 2.0)]
        )

    def test_collect_preserves_help(self):
        pid = 0
        values.ValueClass = MultiProcessValue(lambda: pid)
        labels = {i: i for i in 'abcd'}

        c = Counter('c', 'c help', labelnames=labels.keys(), registry=None)
        g = Gauge('g', 'g help', labelnames=labels.keys(), registry=None)
        h = Histogram('h', 'h help', labelnames=labels.keys(), registry=None)

        c.labels(**labels).inc(1)
        g.labels(**labels).set(1)
        h.labels(**labels).observe(1)

        pid = 1

        c.labels(**labels).inc(1)
        g.labels(**labels).set(1)
        h.labels(**labels).observe(5)

        metrics = {m.name: m for m in self.collector.collect()}

        self.assertEqual(metrics['c'].documentation, 'c help')
        self.assertEqual(metrics['g'].documentation, 'g help')
        self.assertEqual(metrics['h'].documentation, 'h help')

    def test_merge_no_accumulate(self):
        pid = 0
        values.ValueClass = MultiProcessValue(lambda: pid)
        labels = {i: i for i in 'abcd'}

        def add_label(key, value):
            l = labels.copy()
            l[key] = value
            return l

        h = Histogram('h', 'help', labelnames=labels.keys(), registry=None)
        h.labels(**labels).observe(1)
        pid = 1
        h.labels(**labels).observe(5)

        path = os.path.join(os.environ['PROMETHEUS_MULTIPROC_DIR'], '*.db')
        files = glob.glob(path)
        metrics = {
            m.name: m for m in self.collector.merge(files, accumulate=False)
        }

        expected_histogram = [
            Sample('h_sum', labels, 6.0),
            Sample('h_bucket', add_label('le', '0.005'), 0.0),
            Sample('h_bucket', add_label('le', '0.01'), 0.0),
            Sample('h_bucket', add_label('le', '0.025'), 0.0),
            Sample('h_bucket', add_label('le', '0.05'), 0.0),
            Sample('h_bucket', add_label('le', '0.075'), 0.0),
            Sample('h_bucket', add_label('le', '0.1'), 0.0),
            Sample('h_bucket', add_label('le', '0.25'), 0.0),
            Sample('h_bucket', add_label('le', '0.5'), 0.0),
            Sample('h_bucket', add_label('le', '0.75'), 0.0),
            Sample('h_bucket', add_label('le', '1.0'), 1.0),
            Sample('h_bucket', add_label('le', '2.5'), 0.0),
            Sample('h_bucket', add_label('le', '5.0'), 1.0),
            Sample('h_bucket', add_label('le', '7.5'), 0.0),
            Sample('h_bucket', add_label('le', '10.0'), 0.0),
            Sample('h_bucket', add_label('le', '+Inf'), 0.0),
        ]

        self.assertEqual(metrics['h'].samples, expected_histogram)

    def test_missing_gauge_file_during_merge(self):
        # These files don't exist, just like if mark_process_dead(9999999) had been
        # called during self.collector.collect(), after the glob found it
        # but before the merge actually happened.
        # This should not raise and return no metrics
        self.assertFalse(self.collector.merge([
            os.path.join(self.tempdir, 'gauge_liveall_9999999.db'),
            os.path.join(self.tempdir, 'gauge_livesum_9999999.db'),
        ]))

    def test_uninitialized_file_during_merge(self):
        # This file exists but is still zero bytes, just like a file created by
        # MmapedDict.__init__ that has not been truncated to its initial size
        # yet, or one left behind by a process that died in that window.
        # This should not raise and return no metrics
        empty_file = os.path.join(self.tempdir, 'counter_9999999.db')
        open(empty_file, 'wb').close()

        self.assertFalse(self.collector.merge([empty_file]))

    def test_uninitialized_file_does_not_hide_other_metrics(self):
        # A single unreadable file must not take the whole collection with it,
        # as an empty file can be left behind indefinitely: it is named after a
        # pid that never comes back, so nothing cleans it up.
        c = Counter('c', 'help', registry=None)
        c.inc(1)
        open(os.path.join(self.tempdir, 'counter_9999999.db'), 'wb').close()

        self.assertEqual(1, self.registry.get_sample_value('c_total'))

    def test_remove_clear_warning(self):
        os.environ['PROMETHEUS_MULTIPROC_DIR'] = self.tempdir
        with warnings.catch_warnings(record=True) as w:
            values.ValueClass = get_value_class()
            registry = CollectorRegistry()
            collector = MultiProcessCollector(registry)
            counter = Counter('c', 'help', labelnames=['label'], registry=None)
            counter.labels('label').inc()
            counter.remove('label')
            counter.clear()
            assert os.environ['PROMETHEUS_MULTIPROC_DIR'] == self.tempdir
            assert issubclass(w[0].category, UserWarning)
            assert "Removal of labels has not been implemented" in str(w[0].message)
            assert issubclass(w[-1].category, UserWarning)
            assert "Clearing labels has not been implemented" in str(w[-1].message)
    
    def test_child_name_is_built_once_with_namespace_subsystem_unit(self):
        """
        Repro for #1035:
        In multiprocess mode, child metrics must NOT rebuild the full name
        (namespace/subsystem/unit) a second time. The exported family name should
        be built once, and Counter samples should use "<family>_total".
        """
        from prometheus_client import Counter

        class CustomCounter(Counter):
            def __init__(
                self, 
                name, 
                documentation, 
                labelnames=(),
                namespace="mydefaultnamespace", 
                subsystem="mydefaultsubsystem",
                unit="", 
                registry=None, 
                _labelvalues=None
            ):
                # Intentionally provide non-empty defaults to trigger the bug path.
                super().__init__(
                    name=name, 
                    documentation=documentation,
                    labelnames=labelnames, 
                    namespace=namespace,
                    subsystem=subsystem, 
                    unit=unit, 
                    registry=registry,
                    _labelvalues=_labelvalues)

        # Create a Counter with explicit namespace/subsystem/unit
        c = CustomCounter(
            name='m',
            documentation='help',
            labelnames=('status', 'method'),
            namespace='ns',
            subsystem='ss',
            unit='seconds',   # avoid '_total_total' confusion
            registry=None,    # not registered in local registry in multiprocess mode
        )

        # Create two labeled children
        c.labels(status='200', method='GET').inc()
        c.labels(status='404', method='POST').inc()

        # Collect from the multiprocess collector initialized in setUp()
        metrics = {m.name: m for m in self.collector.collect()}

        # Family name should be built once (no '_total' in family name)
        expected_family = 'ns_ss_m_seconds'
        self.assertIn(expected_family, metrics, f"missing family {expected_family}")

        # Counter samples must use '<family>_total'
        mf = metrics[expected_family]
        sample_names = {s.name for s in mf.samples}
        self.assertTrue(
            all(name == expected_family + '_total' for name in sample_names),
            f"unexpected sample names: {sample_names}"
        )

        # Ensure no double-built prefix sneaks in (the original bug)
        bad_prefix = 'mydefaultnamespace_mydefaultsubsystem_'
        all_names = {mf.name, *sample_names}
        self.assertTrue(
            all(not n.startswith(bad_prefix) for n in all_names),
            f"found double-built name(s): {[n for n in all_names if n.startswith(bad_prefix)]}"
        )

    def test_child_preserves_parent_context_for_subclasses(self):
        """
        Ensure child metrics preserve parent's namespace/subsystem/unit information
        so that subclasses can correctly use these parameters in their logic.
        """
        class ContextAwareCounter(Counter):
            def __init__(self,
                         name,
                         documentation,
                         labelnames=(),
                         namespace="",
                         subsystem="",
                         unit="",
                         **kwargs):
                self.context = {
                    'namespace': namespace,
                    'subsystem': subsystem,
                    'unit': unit
                }
                super().__init__(name, documentation,
                                 labelnames=labelnames,
                                 namespace=namespace,
                                 subsystem=subsystem,
                                 unit=unit,
                                 **kwargs)

        parent = ContextAwareCounter('m', 'help',
                                     labelnames=['status'],
                                     namespace='prod',
                                     subsystem='api',
                                     unit='seconds',
                                     registry=None)

        child = parent.labels(status='200')

        # Verify that child retains parent's context
        self.assertEqual(child.context['namespace'], 'prod')
        self.assertEqual(child.context['subsystem'], 'api')
        self.assertEqual(child.context['unit'], 'seconds')


class TestMmapedDict(unittest.TestCase):
    def setUp(self):
        fd, self.tempfile = tempfile.mkstemp()
        os.close(fd)
        self.d = mmap_dict.MmapedDict(self.tempfile)

    def test_process_restart(self):
        self.d.write_value('abc', 123.0, 987.0)
        self.d.close()
        self.d = mmap_dict.MmapedDict(self.tempfile)
        self.assertEqual((123, 987.0), self.d.read_value('abc'))
        self.assertEqual([('abc', 123.0, 987.0)], list(self.d.read_all_values()))

    def test_expansion(self):
        key = 'a' * mmap_dict._INITIAL_MMAP_SIZE
        self.d.write_value(key, 123.0, 987.0)
        self.assertEqual([(key, 123.0, 987.0)], list(self.d.read_all_values()))

    def test_multi_expansion(self):
        key = 'a' * mmap_dict._INITIAL_MMAP_SIZE * 4
        self.d.write_value('abc', 42.0, 987.0)
        self.d.write_value(key, 123.0, 876.0)
        self.d.write_value('def', 17.0, 765.0)
        self.assertEqual(
            [('abc', 42.0, 987.0), (key, 123.0, 876.0), ('def', 17.0, 765.0)],
            list(self.d.read_all_values()))

    def test_corruption_detected(self):
        self.d.write_value('abc', 42.0, 987.0)
        # corrupt the written data
        self.d._m[8:16] = b'somejunk'
        with self.assertRaises(RuntimeError):
            list(self.d.read_all_values())

    def test_read_all_values_from_empty_file(self):
        # A reader can observe the file in the window between __init__ creating
        # it and truncating it to _INITIAL_MMAP_SIZE, and a process dying in
        # that window leaves it empty for good. Either way it holds no values,
        # so it should read as empty rather than failing to unpack a header
        # that was never written.
        fd, empty_file = tempfile.mkstemp()
        os.close(fd)
        try:
            self.assertEqual([], list(mmap_dict.MmapedDict.read_all_values_from_file(empty_file)))
        finally:
            os.unlink(empty_file)

    def test_read_all_values_from_file_with_truncated_contents(self):
        # Only a wholly empty file is tolerated: a file claiming to hold more
        # than it does is corrupt, and should still be reported as such.
        fd, short_file = tempfile.mkstemp()
        os.close(fd)
        try:
            with open(short_file, 'wb') as outfp:
                outfp.write(mmap_dict._pack_integer_func(100))
            with self.assertRaises(struct.error):
                list(mmap_dict.MmapedDict.read_all_values_from_file(short_file))
        finally:
            os.unlink(short_file)

    def tearDown(self):
        self.d.close()
        os.unlink(self.tempfile)


class TestUnsetEnv(unittest.TestCase):
    def setUp(self):
        self.registry = CollectorRegistry()
        fp, self.tmpfl = tempfile.mkstemp()
        os.close(fp)

    def test_unset_syncdir_env(self):
        self.assertRaises(
            ValueError, MultiProcessCollector, self.registry)

    def test_file_syncpath(self):
        registry = CollectorRegistry()
        self.assertRaises(
            ValueError, MultiProcessCollector, registry, self.tmpfl)

    def tearDown(self):
        os.remove(self.tmpfl)


class TestCleanupPlan(unittest.TestCase):
    """Reviewable, controlled cleanup of the multiprocess directory.

    Everything here runs against controlled files in a temp directory with an
    injected liveness callback, so the same tests work natively on Windows
    without spawning or killing real processes.
    """

    def setUp(self):
        self.tempdir = tempfile.mkdtemp()

    def tearDown(self):
        shutil.rmtree(self.tempdir)

    def _touch(self, name):
        path = os.path.join(self.tempdir, name)
        with open(path, 'wb'):
            pass
        return path

    def _entries_by_path(self, entries):
        return {os.path.basename(entry.file.path): entry for entry in entries}

    def _unrecognized_names(self, plan):
        return {os.path.basename(entry.path) for entry in plan.unrecognized}

    def test_plan_classifies_every_file_without_deleting_anything(self):
        live = self._touch('gauge_liveall_123.db')
        live_sum = self._touch('gauge_livesum_123.db')
        nonlive = self._touch('gauge_all_123.db')
        counter = self._touch('counter_123.db')
        histogram = self._touch('histogram_123.db')
        summary = self._touch('summary_123.db')
        other_pid = self._touch('gauge_liveall_456.db')
        for name in ('random.txt', 'counter_123', 'gauge_wrongmode_123.db',
                     'unknown_123.db', 'gauge_liveall_abc.db'):
            self._touch(name)
        os.mkdir(os.path.join(self.tempdir, 'subdir_123.db'))

        alive = {123: False, 456: True}
        plan = build_cleanup_plan(
            123, self.tempdir, process_is_alive=alive.get)

        # Building the plan must not change the directory at all.
        for path in (live, live_sum, nonlive, counter, histogram,
                     summary, other_pid):
            self.assertTrue(os.path.exists(path))

        to_delete = self._entries_by_path(plan.to_delete)
        kept = self._entries_by_path(plan.kept)
        self.assertEqual(
            set(to_delete), {'gauge_liveall_123.db', 'gauge_livesum_123.db'})
        self.assertTrue(all(entry.delete for entry in plan.to_delete))
        self.assertTrue(all(
            entry.reason == REASON_LIVE_GAUGE_DEAD_PROCESS
            for entry in plan.to_delete))

        self.assertEqual(kept['gauge_all_123.db'].reason,
                         REASON_NONLIVE_GAUGE)
        for name in ('counter_123.db', 'histogram_123.db', 'summary_123.db'):
            self.assertEqual(kept[name].reason, REASON_CUMULATIVE_METRIC)
        self.assertEqual(kept['gauge_liveall_456.db'].reason,
                         REASON_PID_NOT_REQUESTED)

        # Everything outside the protocol is reported, not silently ignored.
        self.assertEqual(
            self._unrecognized_names(plan),
            {'random.txt', 'counter_123', 'gauge_wrongmode_123.db',
             'unknown_123.db', 'gauge_liveall_abc.db', 'subdir_123.db'})

        # The plan is reviewable as text and accounts for every entry.
        rendered = plan.summary()
        self.assertIn('gauge_liveall_123.db', rendered)
        self.assertIn(REASON_CUMULATIVE_METRIC, rendered)
        self.assertEqual(
            len(plan.to_delete) + len(plan.kept) + len(plan.unrecognized), 13)

    def test_execute_only_removes_planned_live_gauges(self):
        self._touch('gauge_liveall_123.db')
        self._touch('gauge_livesum_123.db')
        self._touch('gauge_all_123.db')
        self._touch('counter_123.db')
        self._touch('histogram_123.db')
        self._touch('summary_123.db')
        self._touch('notes.txt')

        plan = build_cleanup_plan(
            123, self.tempdir, process_is_alive=lambda _: False)
        result = execute_cleanup_plan(plan, process_is_alive=lambda _: False)

        self.assertEqual(
            {os.path.basename(entry.file.path) for entry in result.deleted},
            {'gauge_liveall_123.db', 'gauge_livesum_123.db'})
        self.assertEqual(result.skipped, ())
        remaining = set(os.listdir(self.tempdir))
        self.assertEqual(remaining, {
            'gauge_all_123.db', 'counter_123.db', 'histogram_123.db',
            'summary_123.db', 'notes.txt'})

    def test_live_gauge_of_live_process_is_kept(self):
        self._touch('gauge_liveall_123.db')
        plan = build_cleanup_plan(
            123, self.tempdir, process_is_alive=lambda _: True)
        self.assertEqual(plan.to_delete, ())
        self.assertEqual(len(plan.kept), 1)
        self.assertEqual(plan.kept[0].reason,
                         REASON_LIVE_GAUGE_PROCESS_ALIVE)

        result = execute_cleanup_plan(plan, process_is_alive=lambda _: True)
        self.assertEqual(result.deleted, ())
        self.assertEqual(len(result.skipped), 0)
        self.assertTrue(
            os.path.exists(os.path.join(self.tempdir, 'gauge_liveall_123.db')))

    def test_cumulative_metrics_survive_mark_process_dead(self):
        # A process's counters/histograms/summaries must not be wiped when it
        # dies: their values are still part of the aggregated totals.
        self._touch('gauge_liveall_123.db')
        self._touch('gauge_livemin_123.db')
        self._touch('gauge_livemax_123.db')
        self._touch('gauge_livesum_123.db')
        self._touch('gauge_livemostrecent_123.db')
        self._touch('counter_123.db')
        self._touch('histogram_123.db')
        self._touch('summary_123.db')
        self._touch('gauge_all_123.db')

        mark_process_dead(123, self.tempdir)

        remaining = set(os.listdir(self.tempdir))
        self.assertEqual(remaining, {
            'counter_123.db', 'histogram_123.db', 'summary_123.db',
            'gauge_all_123.db'})

    def test_stale_plan_skips_replaced_file(self):
        path = self._touch('gauge_liveall_123.db')
        plan = build_cleanup_plan(
            123, self.tempdir, process_is_alive=lambda _: False)

        # Simulate the file being replaced (same name, new content/inode)
        # between planning and execution.
        os.remove(path)
        with open(path, 'wb') as outfp:
            outfp.write(b'new process owns this pid now')

        result = execute_cleanup_plan(plan, process_is_alive=lambda _: False)
        self.assertEqual(result.deleted, ())
        self.assertEqual(len(result.skipped), 1)
        self.assertIn('replaced', result.skipped[0].reason)
        # The replacement file must still be there.
        self.assertTrue(os.path.exists(path))

    def test_stale_plan_skips_when_pid_is_reused(self):
        self._touch('gauge_liveall_123.db')
        plan = build_cleanup_plan(
            123, self.tempdir, process_is_alive=lambda _: False)

        # Between planning and execution pid 123 is handed to a new process.
        result = execute_cleanup_plan(plan, process_is_alive=lambda _: True)
        self.assertEqual(result.deleted, ())
        self.assertEqual(len(result.skipped), 1)
        self.assertIn('alive again', result.skipped[0].reason)
        self.assertTrue(
            os.path.exists(os.path.join(self.tempdir, 'gauge_liveall_123.db')))

    def test_stale_plan_skips_file_vanished_before_execution(self):
        path = self._touch('gauge_liveall_123.db')
        plan = build_cleanup_plan(
            123, self.tempdir, process_is_alive=lambda _: False)
        os.remove(path)

        result = execute_cleanup_plan(plan, process_is_alive=lambda _: False)
        self.assertEqual(result.deleted, ())
        self.assertEqual(len(result.skipped), 1)

    def test_unrecognized_files_are_never_deleted(self):
        for name in ('readme.txt', 'counter_123', 'weird_123.db',
                     'gauge_123.db', 'counter.db'):
            self._touch(name)
        plan = build_cleanup_plan(
            123, self.tempdir, process_is_alive=lambda _: False)
        self.assertEqual(plan.to_delete, ())
        result = execute_cleanup_plan(plan, process_is_alive=lambda _: False)
        self.assertEqual(result.deleted, ())
        self.assertEqual(
            set(os.listdir(self.tempdir)),
            {'readme.txt', 'counter_123', 'weird_123.db',
             'gauge_123.db', 'counter.db'})

    def test_plan_for_all_pids_uses_liveness_per_pid(self):
        self._touch('gauge_liveall_123.db')
        self._touch('gauge_liveall_456.db')
        self._touch('gauge_liveall_789.db')
        self._touch('counter_123.db')
        alive = {123: False, 456: True, 789: False}

        plan = build_cleanup_plan(
            path=self.tempdir, process_is_alive=alive.get)
        to_delete = self._entries_by_path(plan.to_delete)
        self.assertEqual(
            set(to_delete), {'gauge_liveall_123.db', 'gauge_liveall_789.db'})
        kept = self._entries_by_path(plan.kept)
        self.assertEqual(kept['gauge_liveall_456.db'].reason,
                         REASON_LIVE_GAUGE_PROCESS_ALIVE)
        self.assertEqual(kept['counter_123.db'].reason,
                         REASON_CUMULATIVE_METRIC)

    def test_liveness_callback_is_called_at_most_once_per_pid(self):
        self._touch('gauge_liveall_123.db')
        self._touch('gauge_livesum_123.db')
        calls = []

        def process_is_alive(pid):
            calls.append(pid)
            return False

        build_cleanup_plan(
            123, self.tempdir, process_is_alive=process_is_alive)
        self.assertEqual(calls, [123])

    def test_build_plan_rejects_missing_directory(self):
        missing = os.path.join(self.tempdir, 'does_not_exist')
        with self.assertRaises(ValueError):
            build_cleanup_plan(123, missing, process_is_alive=lambda _: False)

    def test_build_plan_falls_back_to_environment_variable(self):
        self._touch('gauge_liveall_123.db')
        os.environ['PROMETHEUS_MULTIPROC_DIR'] = self.tempdir
        try:
            plan = build_cleanup_plan(
                123, process_is_alive=lambda _: False)
        finally:
            os.environ.pop('PROMETHEUS_MULTIPROC_DIR', None)
        self.assertEqual(
            len(plan.to_delete), 1)
        self.assertEqual(
            os.path.basename(plan.to_delete[0].file.path),
            'gauge_liveall_123.db')

    def test_mark_process_dead_noop_without_directory(self):
        os.environ.pop('PROMETHEUS_MULTIPROC_DIR', None)
        os.environ.pop('prometheus_multiproc_dir', None)
        # Must simply return rather than raise.
        mark_process_dead(123)

    def test_execute_empty_plan_touches_nothing(self):
        self._touch('counter_123.db')
        plan = build_cleanup_plan(
            123, self.tempdir, process_is_alive=lambda _: False)
        self.assertEqual(plan.to_delete, ())
        result = execute_cleanup_plan(plan, process_is_alive=lambda _: False)
        self.assertEqual(result.deleted, ())
        self.assertEqual(result.skipped, ())
        self.assertEqual(os.listdir(self.tempdir), ['counter_123.db'])

    def test_execute_uses_plan_liveness_callback_by_default(self):
        self._touch('gauge_liveall_123.db')
        # The plan is built while the process is dead, so the file is marked
        # for deletion...
        plan = build_cleanup_plan(
            123, self.tempdir, process_is_alive=lambda _: False)
        self.assertEqual(len(plan.to_delete), 1)

        # ...and without an explicit execute callback the plan's own liveness
        # verdict (dead) is reused, so the file is deleted.
        result = execute_cleanup_plan(plan)
        self.assertEqual(len(result.deleted), 1)
        self.assertEqual(result.skipped, ())
        self.assertFalse(
            os.path.exists(os.path.join(self.tempdir, 'gauge_liveall_123.db')))

        # Rebuild: a different callback at execution time overrides the
        # plan's verdict, simulating pid reuse discovered late.
        self._touch('gauge_liveall_123.db')
        plan = build_cleanup_plan(
            123, self.tempdir, process_is_alive=lambda _: False)
        result = execute_cleanup_plan(plan, process_is_alive=lambda _: True)
        self.assertEqual(result.deleted, ())
        self.assertEqual(len(result.skipped), 1)
        self.assertTrue(
            os.path.exists(os.path.join(self.tempdir, 'gauge_liveall_123.db')))

    def test_parse_metric_filename_protocol(self):
        gauge = parse_metric_filename(self._touch('gauge_liveall_123.db'))
        self.assertEqual(gauge.metric_type, 'gauge')
        self.assertEqual(gauge.gauge_mode, 'liveall')
        self.assertEqual(gauge.pid, 123)
        self.assertTrue(gauge.is_live_gauge)
        self.assertFalse(gauge.is_cumulative)

        counter = parse_metric_filename(self._touch('counter_456.db'))
        self.assertEqual(counter.metric_type, 'counter')
        self.assertIsNone(counter.gauge_mode)
        self.assertEqual(counter.pid, 456)
        self.assertTrue(counter.is_cumulative)

        for name in ('counter_abc.db', 'gauge_bogus_1.db', 'thing_1.db',
                     'counter_1', 'counter_.db', 'counter_1.db.bak'):
            self.assertIsNone(
                parse_metric_filename(self._touch(name)), name)

    def test_default_process_is_alive_smoke_test(self):
        # Uses only the current process and an implausibly large pid, so it is
        # safe on every platform, including native Windows.
        self.assertTrue(default_process_is_alive(os.getpid()))
        self.assertFalse(default_process_is_alive(4194304))
        self.assertFalse(default_process_is_alive(0))
        self.assertFalse(default_process_is_alive(-1))

    def test_aggregated_cumulative_values_survive_cleanup(self):
        # End-to-end with real mmap files: after a worker dies and its live
        # gauges are cleaned up, its counters/histograms must still contribute
        # to the aggregated exposition.
        os.environ['PROMETHEUS_MULTIPROC_DIR'] = self.tempdir
        try:
            values.ValueClass = MultiProcessValue(lambda: 123)
            registry = CollectorRegistry(
                support_collectors_without_names=True)
            MultiProcessCollector(registry)

            counter = Counter('requests', 'help', registry=None)
            histogram = Histogram('latency', 'help', registry=None)
            live_gauge = Gauge(
                'inflight', 'help', registry=None,
                multiprocess_mode='livesum')
            counter.inc(7)
            histogram.observe(0.5)
            live_gauge.set(3)
            values.close_all_multiprocess_files()

            self.assertEqual(
                7, registry.get_sample_value('requests_total'))
            self.assertEqual(
                1, registry.get_sample_value('latency_count'))
            self.assertEqual(
                3, registry.get_sample_value('inflight'))

            mark_process_dead(123, self.tempdir)

            remaining = set(os.listdir(self.tempdir))
            self.assertNotIn('gauge_livesum_123.db', remaining)
            self.assertIn('counter_123.db', remaining)
            self.assertIn('histogram_123.db', remaining)
            # Nothing cumulative was erased by the cleanup.
            self.assertEqual(
                7, registry.get_sample_value('requests_total'))
            self.assertEqual(
                1, registry.get_sample_value('latency_count'))
            self.assertEqual(
                0.5, registry.get_sample_value('latency_sum'))
            # The dead worker no longer contributes to the live gauge.
            self.assertIsNone(registry.get_sample_value('inflight'))
        finally:
            os.environ.pop('PROMETHEUS_MULTIPROC_DIR', None)
            values.ValueClass = MutexValue
