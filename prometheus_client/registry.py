import copy
from threading import Lock
from typing import (
    AbstractSet, Any, Dict, Iterable, List, Mapping, Optional, Protocol, Set,
    Tuple,
)

from .metrics_core import Metric
from .samples import Sample
from .validation import _validate_metric_name

# Public names a metric family exposes in addition to its own family name.
# Every name below is a real timeseries name (or the name of a metadata block
# emitted for a derived sample) and must be reserved together with the family,
# otherwise a clash would only surface on a _bucket or _created sample.
_TYPE_SUFFIXES: Dict[str, Tuple[str, ...]] = {
    'counter': ('_total', '_created'),
    'summary': ('_sum', '_count', '_created'),
    'histogram': ('_bucket', '_sum', '_count', '_created'),
    'gaugehistogram': ('_bucket', '_gsum', '_gcount'),
    'info': ('_info',),
}


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


class NamespaceConflict(DuplicateTimeseries):
    """Raised when namespaced views would publish duplicate public names.

    ``duplicates`` holds the clashing public names, ``namespaces`` maps every
    clashing name to the namespaces that produced it. A name is reported as
    soon as two views are combined, before any scrape has to fail, and a
    clash on a derived sample (``_bucket``, ``_created``, ...) is reported
    just like a clash on a family name.
    """

    def __init__(self, duplicates: Set[str], namespaces: Optional[Mapping[str, Iterable[str]]] = None):
        super().__init__(set(duplicates))
        self.namespaces: Dict[str, Tuple[str, ...]] = {
            name: tuple(sorted(set(sources)))
            for name, sources in (namespaces or {}).items()
        }
        if self.namespaces:
            detail = ', '.join(
                '{} (from {})'.format(name, ', '.join(self.namespaces[name]))
                for name in sorted(self.namespaces))
            self.args = ('Duplicate timeseries across namespaced views: ' + detail,)


def _prefix_name(namespace: str, name: str) -> str:
    """Public name of ``name`` inside ``namespace``."""
    return namespace + '_' + name


def _reserved_names(name: str, typ: str) -> Set[str]:
    """Public names a family of ``typ`` reserves: its own name and suffixes.

    The suffixes cover everything the exposition formats emit as separate
    metadata blocks or samples, so a family name and a derived sample name
    can be compared before any sample is collected.
    """
    names = {name}
    for suffix in _TYPE_SUFFIXES.get(typ, ()):
        names.add(name + suffix)
    return names


def _metric_public_names(metric: Metric) -> Set[str]:
    """Every public name a materialised metric publishes."""
    names = _reserved_names(metric.name, metric.type)
    for sample in metric.samples:
        names.add(sample.name)
    return names


def _namespaced_metric(namespace: str, metric: Metric) -> Metric:
    """Return a copy of ``metric`` published under ``namespace``.

    A new Metric and new Sample objects are built; the input metric, its
    sample labels, its exemplars and any native histogram are never mutated.
    That keeps the source registry on its original names and makes repeated
    collection idempotent instead of stacking prefixes.
    """
    m = Metric(_prefix_name(namespace, metric.name), metric.documentation, metric.type)
    # Set the unit attribute directly: the family name already carries any
    # unit suffix, so passing it to the constructor would append it twice.
    m.unit = metric.unit
    m.samples = [_namespaced_sample(namespace, s) for s in metric.samples]
    return m


def _namespaced_sample(namespace: str, sample: Sample) -> Sample:
    """Copy of ``sample`` under ``namespace``, leaving label values untouched.

    Only the metric name is rewritten. Label names and label values - and in
    particular exemplar label values - are copied verbatim, because they are
    data, not metric names.
    """
    exemplar = sample.exemplar
    if exemplar is not None:
        exemplar = exemplar._replace(labels=dict(exemplar.labels))
    return sample._replace(
        name=_prefix_name(namespace, sample.name),
        labels=dict(sample.labels),
        exemplar=exemplar,
    )


def _select_samples(metric: Metric, names: AbstractSet[str]) -> Optional[Metric]:
    """Copy of ``metric`` keeping only samples whose public name is selected."""
    samples = [s for s in metric.samples if s.name in names]
    if not samples:
        return None
    m = Metric(metric.name, metric.documentation, metric.type)
    m.unit = metric.unit
    m.samples = samples
    return m


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
        for metric in desc_func():
            result.append(metric.name)
            for suffix in _TYPE_SUFFIXES.get(metric.type, ()):
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


class NamespaceView:
    """Read-only view of one collector registry under a stable namespace.

    Collecting the view never touches the wrapped registry: every metric and
    every sample is copied and renamed, so the source registry keeps
    publishing its original names. Because copies are built afresh on every
    collect(), repeated collection publishes the same names instead of
    stacking the prefix again.
    """

    def __init__(self, registry: Collector, namespace: str):
        if not isinstance(namespace, str) or not namespace:
            raise ValueError('Namespace must be a non-empty string')
        _validate_metric_name(namespace)
        self._registry = registry
        self._namespace = namespace

    @property
    def namespace(self) -> str:
        """The prefix prepended to every public name of this view."""
        return self._namespace

    @property
    def registry(self) -> Collector:
        """The wrapped registry, unmodified."""
        return self._registry

    def declared_names(self) -> Optional[Set[str]]:
        """Namespaced names the source declares, or None if it declares none.

        The result includes derived names (``_bucket``, ``_created``, ...) so
        conflicts on those can be reported before the first scrape.
        """
        names = _declared_public_names(self._registry)
        if names is None:
            return None
        return {_prefix_name(self._namespace, name) for name in names}

    def collect(self) -> Iterable[Metric]:
        """Yield the source metrics copied under this view's namespace."""
        for metric in self._registry.collect():
            yield _namespaced_metric(self._namespace, metric)


def _declared_public_names(registry: Any) -> Optional[Set[str]]:
    """Best-effort set of public names a collector declares, without collecting.

    A plain CollectorRegistry already knows every family name and every
    derived suffix it registered, which is exactly the information needed to
    report namespace conflicts before collecting. Collectors that only offer
    collect() return None and are checked when the view is materialised.
    """
    if isinstance(registry, NamespaceView):
        return registry.declared_names()
    if isinstance(registry, CompositeRegistry):
        return registry.declared_names()
    names_to_collectors = getattr(registry, '_names_to_collectors', None)
    if names_to_collectors is None:
        return None
    lock = getattr(registry, '_lock', None)
    if lock is None:
        return set(names_to_collectors)
    with lock:
        return set(names_to_collectors)


class RestrictedCompositeRegistry:
    """Read-only slice of a composite view addressed by public view names.

    Selectors match the public sample names of the composite view, i.e. the
    namespaced names a scrape would actually expose; the source names are
    never consulted. A family from which no sample is selected is dropped
    together with its metadata, and namespace conflicts are still reported
    because the slice is resolved from a fully validated collection.
    """

    def __init__(self, names: Iterable[str], composite: "CompositeRegistry"):
        self._name_set = frozenset(names)
        self._composite = composite

    def collect(self) -> Iterable[Metric]:
        for _, metric in self._composite._materialise():
            m = _select_samples(metric, self._name_set)
            if m is not None:
                yield m


class CompositeRegistry:
    """Read-only composite of several registries, each under its own namespace.

    Pair a source registry with a namespace once; afterwards collect() yields
    every source metric with the namespace applied to the family name, to
    every derived sample name, and with the source documentation preserved.
    Source registries are never modified and keep their original names.

    Conflicts between namespaces are reported as ``NamespaceConflict``, which
    is a ``DuplicateTimeseries`` and therefore a ``ValueError``. Names the
    sources declare are checked in :meth:`add`, so a clash on a family name or
    on a derived sample such as ``_bucket`` or ``_created`` fails before the
    view is ever scraped; names that can only be learnt by collecting are
    checked in :meth:`check` and :meth:`collect` before any metric is yielded.
    """

    def __init__(self, registries: Iterable[Tuple[Collector, str]] = ()):
        self._views: List[NamespaceView] = []
        self._by_namespace: Dict[str, NamespaceView] = {}
        self._lock = Lock()
        for registry, namespace in registries:
            self.add(registry, namespace)

    @property
    def namespaces(self) -> Tuple[str, ...]:
        """Namespaces in registration order."""
        with self._lock:
            return tuple(view.namespace for view in self._views)

    @property
    def views(self) -> Tuple[NamespaceView, ...]:
        """The individual read-only views, in registration order."""
        with self._lock:
            return tuple(self._views)

    def add(self, registry: Collector, namespace: str) -> NamespaceView:
        """Add a registry under ``namespace`` and return its read-only view."""
        view = NamespaceView(registry, namespace)
        with self._lock:
            if namespace in self._by_namespace:
                raise ValueError('Namespace already registered: ' + namespace)
            self._check_against_registered(view)
            self._views.append(view)
            self._by_namespace[namespace] = view
        return view

    def _check_against_registered(self, view: NamespaceView) -> None:
        new_names = view.declared_names()
        if new_names is None:
            return
        owners: Dict[str, str] = {}
        for existing in self._views:
            names = existing.declared_names()
            if names is None:
                # This source cannot be inspected without collecting, so its
                # names may still clash. Later registrations and the final
                # check()/collect() settle that case, but every name we do
                # know is still checked now.
                continue
            for name in names:
                owners[name] = existing.namespace
        duplicates = new_names.intersection(owners)
        if duplicates:
            raise NamespaceConflict(
                duplicates,
                {name: [owners[name], view.namespace] for name in duplicates})

    def declared_names(self) -> Optional[Set[str]]:
        """Every public name the composite declares, or None if any is unknown."""
        with self._lock:
            views = list(self._views)
        declared: Set[str] = set()
        for view in views:
            names = view.declared_names()
            if names is None:
                return None
            declared.update(names)
        return declared

    def _materialise(self) -> List[Tuple[str, Metric]]:
        """Collect and validate the whole view without yielding anything."""
        with self._lock:
            views = list(self._views)
        materialised: List[Tuple[str, Metric]] = []
        owners: Dict[str, str] = {}
        conflicts: Dict[str, List[str]] = {}
        for view in views:
            for metric in view.collect():
                for name in _metric_public_names(metric):
                    previous = owners.setdefault(name, view.namespace)
                    if previous != view.namespace:
                        conflicts.setdefault(name, [previous]).append(view.namespace)
                materialised.append((view.namespace, metric))
        if conflicts:
            raise NamespaceConflict(set(conflicts), conflicts)
        return materialised

    def check(self) -> None:
        """Materialise the view once and raise on any namespace conflict."""
        self._materialise()

    def collect(self) -> Iterable[Metric]:
        """Yield every source metric copied under its namespace."""
        for _, metric in self._materialise():
            yield metric

    def restricted_registry(self, names: Iterable[str]) -> RestrictedCompositeRegistry:
        """Return a view exposing only the given public view names."""
        return RestrictedCompositeRegistry(names, self)

    def get_sample_value(self, name: str, labels: Optional[Dict[str, str]] = None) -> Optional[float]:
        """Return the value of a public view sample, or None if not found."""
        if labels is None:
            labels = {}
        for metric in self.collect():
            for sample in metric.samples:
                if sample.name == name and sample.labels == labels:
                    return sample.value
        return None


REGISTRY = CollectorRegistry(auto_describe=True)
