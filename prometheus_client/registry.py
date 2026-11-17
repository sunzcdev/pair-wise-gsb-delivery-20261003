import copy
from threading import Lock
from typing import Dict, Iterable, List, Optional, Protocol, Set

from .metrics_core import Metric


class Collector(Protocol):
    def collect(self) -> Iterable[Metric]:
        """Collect metrics."""


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

    def replace_collectors(self, old_collectors: Iterable[Collector],
                           new_collectors: Iterable[Collector]) -> None:
        """Atomically replace a group of collectors with a new group.

        Intended for hot-reloading exporters: the metric catalog switches from
        the old group to the new group in one step, so a scrape never observes
        a half-swapped set of names. A collect() that already started keeps
        using its snapshot of the old collectors; later collects see the new
        group.

        The new collectors are prepared (their declared metric names and
        derived suffixes are collected via describe/collect) before anything
        is changed. Preparation happens without holding the registry lock, so
        a describe/collect implementation may raise or re-enter the registry
        without deadlocking. If preparation fails, if any new name conflicts
        with a collector outside the old group, or if any old collector is no
        longer registered, the registry is left untouched and an exception is
        raised.
        """
        old_collectors = list(dict.fromkeys(old_collectors))
        new_collectors = list(dict.fromkeys(new_collectors))

        # Phase 1: prepare without holding the lock. _get_names may call
        # arbitrary user code (describe, or collect with auto_describe) which
        # may raise or re-enter the registry.
        names_per_collector: List[List[str]] = []
        all_new_names: Set[str] = set()
        for collector in new_collectors:
            names = self._get_names(collector)
            duplicates = all_new_names.intersection(names)
            if duplicates or len(names) != len(set(names)):
                duplicates |= {n for n in names if names.count(n) > 1}
                raise DuplicateTimeseries(duplicates)
            all_new_names.update(names)
            names_per_collector.append(names)

        # Phase 2: validate and swap atomically under the lock. No user code
        # is called here, and on any failure nothing has been modified yet.
        with self._lock:
            old_name_set: Set[str] = set()
            for collector in old_collectors:
                if collector not in self._collector_to_names:
                    raise ValueError('Collector is not registered: {}'.format(collector))
                old_name_set.update(self._collector_to_names[collector])

            for collector in new_collectors:
                if collector in self._collector_to_names and collector not in old_collectors:
                    raise ValueError('Collector is already registered: {}'.format(collector))

            conflicts = all_new_names.intersection(self._names_to_collectors) - old_name_set
            if conflicts:
                raise DuplicateTimeseries(conflicts)

            for collector in old_collectors:
                for name in self._collector_to_names[collector]:
                    del self._names_to_collectors[name]
                del self._collector_to_names[collector]
                if collector in self._collectors_without_names:
                    self._collectors_without_names.remove(collector)

            for collector, names in zip(new_collectors, names_per_collector):
                for name in names:
                    self._names_to_collectors[name] = collector
                self._collector_to_names[collector] = names
                if self._support_collectors_without_names and not names:
                    self._collectors_without_names.append(collector)

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
