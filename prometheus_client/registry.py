import copy
from threading import Lock
from typing import Dict, Iterable, Iterator, List, Optional, Protocol, Set

from .metrics_core import Metric


class Collector(Protocol):
    def collect(self) -> Iterable[Metric]:
        """Collect metrics."""


class CollectorFailure:
    """Diagnostic information about a collector that raised during a tolerant collect.

    This is plain data, not a metric, so inspecting it can never trigger
    further collection.
    """

    def __init__(self, collector: "Collector", exception: BaseException):
        self.collector = collector
        self.collector_name = type(collector).__name__
        self.exception = exception
        self.error_type = type(exception).__name__
        self.error_message = str(exception)

    def __repr__(self) -> str:
        return "CollectorFailure({!r}, {}: {})".format(
            self.collector_name, self.error_type, self.error_message)


class TolerantCollectionResult:
    """Result of a fault-tolerant collect.

    Attributes:
        metrics: Complete metric families from collectors that succeeded.
            A collector that fails part-way through contributes nothing, so
            no partial metric family (e.g. half a Histogram) is ever present.
        failures: Diagnostics for each collector that raised, in collection
            order. Empty when every collector succeeded.
    """

    def __init__(self, metrics: List[Metric], failures: List[CollectorFailure]):
        self.metrics = metrics
        self.failures = failures

    @property
    def ok(self) -> bool:
        """True only if every collector completed without error."""
        return not self.failures

    def __iter__(self) -> Iterator[Metric]:
        return iter(self.metrics)

    def __len__(self) -> int:
        return len(self.metrics)


class _EmptyCollector:
    def collect(self) -> Iterable[Metric]:
        return []


class DuplicateTimeseries(ValueError):
    def __init__(self, duplicates: Set[str]):
        msg = 'Duplicated timeseries in CollectorRegistry: {}'.format(
            duplicates)
        super().__init__(msg)
        self.duplicates: Set[str] = duplicates


class CollectorRegistry:
    """Metric collector registry.

    Collectors must have a no-argument method 'collect' that returns a list of
    Metric objects. The returned metrics should be consistent with the Prometheus
    exposition formats.
    """

    def __init__(self, auto_describe: bool = False, target_info: Optional[Dict[str, str]] = None,
                 support_collectors_without_names: bool = False):
        self._collector_to_names: Dict[Collector, List[str]] = {}
        self._names_to_collectors: Dict[str, Collector] = {}
        self._auto_describe = auto_describe
        self._lock = Lock()
        self._target_info: Optional[Dict[str, str]] = {}
        self._support_collectors_without_names = support_collectors_without_names
        self._collectors_without_names: List[Collector] = []
        # Plain in-memory counters of collection failures per collector.
        # Deliberately not metrics and not a registered collector, so the
        # diagnostics can never recursively trigger their own collection.
        self._collector_failure_totals: Dict[Collector, int] = {}
        self.set_target_info(target_info)

    def register(self, collector: Collector) -> None:
        """Add a collector to the registry."""
        with self._lock:
            names = self._get_names(collector)
            duplicates = set(self._names_to_collectors).intersection(names)
            if duplicates:
                raise DuplicateTimeseries(duplicates)
            for name in names:
                self._names_to_collectors[name] = collector
            self._collector_to_names[collector] = names
            if self._support_collectors_without_names and not names:
                self._collectors_without_names.append(collector)

    def unregister(self, collector: Collector) -> None:
        """Remove a collector from the registry."""
        with self._lock:
            for name in self._collector_to_names[collector]:
                del self._names_to_collectors[name]
            del self._collector_to_names[collector]
            if collector in self._collectors_without_names:
                self._collectors_without_names.remove(collector)

    def _get_names(self, collector):
        """Get names of timeseries the collector produces and clashes with."""
        desc_func = None
        # If there's a describe function, use it.
        try:
            desc_func = collector.describe
        except AttributeError:
            pass
        # Otherwise, if auto describe is enabled use the collect function.
        if not desc_func and self._auto_describe:
            desc_func = collector.collect

        if not desc_func:
            return []

        result = []
        type_suffixes = {
            'counter': ['_total', '_created'],
            'summary': ['_sum', '_count', '_created'],
            'histogram': ['_bucket', '_sum', '_count', '_created'],
            'gaugehistogram': ['_bucket', '_gsum', '_gcount'],
            'info': ['_info'],
        }
        for metric in desc_func():
            result.append(metric.name)
            for suffix in type_suffixes.get(metric.type, []):
                result.append(metric.name + suffix)
        return result

    def collect(self) -> Iterable[Metric]:
        """Yields metrics from the collectors in the registry."""
        collectors = None
        ti = None
        with self._lock:
            collectors = copy.copy(self._collector_to_names)
            if self._target_info:
                ti = self._target_info_metric()
        if ti:
            yield ti
        for collector in collectors:
            yield from collector.collect()

    def collect_tolerant(self) -> TolerantCollectionResult:
        """Collects metrics, tolerating collectors that raise.

        Unlike collect(), a collector that raises does not abort the scrape:
        the complete metric families of every healthy collector are returned
        in the result's metrics, and each failed collector is reported in the
        result's failures so the scrape is not mistaken for a full success.

        Collection is all-or-nothing per collector: if a collector yields
        some metrics and then raises, everything it produced during this
        scrape is discarded, so no partial metric family (e.g. half a
        Histogram) is exposed.

        Nothing is cached or replayed: every call invokes collect() on every
        collector afresh, so a collector that failed on a previous scrape is
        retried on the next one and stale values are never served in place of
        current ones.

        Failure diagnostics are kept as plain data (and plain in-memory
        counters, see get_collector_failure_totals), never as registered
        collectors, so they cannot recursively trigger their own collection.
        """
        with self._lock:
            collectors = copy.copy(self._collector_to_names)
            ti = self._target_info_metric() if self._target_info else None
        metrics: List[Metric] = []
        failures: List[CollectorFailure] = []
        if ti:
            metrics.append(ti)
        for collector in collectors:
            try:
                # Materialize fully before accepting anything, so a collector
                # that fails part-way through contributes no partial output.
                collected = list(collector.collect())
            except Exception as e:
                failures.append(CollectorFailure(collector, e))
                with self._lock:
                    self._collector_failure_totals[collector] = \
                        self._collector_failure_totals.get(collector, 0) + 1
                continue
            metrics.extend(collected)
        return TolerantCollectionResult(metrics, failures)

    def get_collector_failure_totals(self) -> Dict[str, int]:
        """Returns per-collector failure counts observed by collect_tolerant().

        Keyed by collector class name. These are plain counters, not metrics,
        so reading them never triggers collection.
        """
        with self._lock:
            totals: Dict[str, int] = {}
            for collector, count in self._collector_failure_totals.items():
                name = type(collector).__name__
                totals[name] = totals.get(name, 0) + count
            return totals

    def restricted_registry(self, names: Iterable[str]) -> "RestrictedRegistry":
        """Returns object that only collects some metrics.

        Returns an object which upon collect() will return
        only samples with the given names.

        Intended usage is:
            generate_latest(REGISTRY.restricted_registry(['a_timeseries']), escaping)

        Experimental."""
        names = set(names)
        return RestrictedRegistry(names, self)

    def set_target_info(self, labels: Optional[Dict[str, str]]) -> None:
        with self._lock:
            if labels:
                if not self._target_info and 'target_info' in self._names_to_collectors:
                    raise ValueError('CollectorRegistry already contains a target_info metric')
                self._names_to_collectors['target_info'] = _EmptyCollector()
            elif self._target_info:
                self._names_to_collectors.pop('target_info', None)
            self._target_info = labels

    def get_target_info(self) -> Optional[Dict[str, str]]:
        with self._lock:
            return self._target_info

    def _target_info_metric(self):
        m = Metric('target', 'Target metadata', 'info')
        m.add_sample('target_info', self._target_info, 1)
        return m

    def get_sample_value(self, name: str, labels: Optional[Dict[str, str]] = None) -> Optional[float]:
        """Returns the sample value, or None if not found.

        This is inefficient, and intended only for use in unittests.
        """
        if labels is None:
            labels = {}
        for metric in self.collect():
            for s in metric.samples:
                if s.name == name and s.labels == labels:
                    return s.value
        return None


class RestrictedRegistry:
    def __init__(self, names: Iterable[str], registry: CollectorRegistry):
        self._name_set = set(names)
        self._registry = registry

    def collect(self) -> Iterable[Metric]:
        collectors = set(self._registry._collectors_without_names)
        target_info_metric = None
        with self._registry._lock:
            if 'target_info' in self._name_set and self._registry._target_info:
                target_info_metric = self._registry._target_info_metric()
            for name in self._name_set:
                if name != 'target_info' and name in self._registry._names_to_collectors:
                    collectors.add(self._registry._names_to_collectors[name])
        if target_info_metric:
            yield target_info_metric
        for collector in collectors:
            for metric in collector.collect():
                m = metric._restricted_metric(self._name_set)
                if m:
                    yield m


REGISTRY = CollectorRegistry(auto_describe=True)
