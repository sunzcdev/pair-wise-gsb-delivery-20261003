import asyncio
import inspect
import logging
from threading import Lock
from typing import Any, Awaitable, Callable, Dict, Iterable, List, Optional, Protocol, Set

from .metrics_core import Metric

logger = logging.getLogger(__name__)


class Collector(Protocol):
    def collect(self) -> Iterable[Metric]:
        """Collect metrics."""


class AsyncCollector(Protocol):
    async def collect(self) -> Iterable[Metric]:
        """Collect metrics, optionally asynchronously."""


# Default upper bound on how many collector coroutines may run at the same
# time during an asynchronous scrape.
DEFAULT_ASYNC_CONCURRENCY = 8
# Default per-collector timeout for asynchronous collectors. Synchronous
# collectors block the event loop while they run and therefore cannot be
# interrupted by a timeout.
DEFAULT_COLLECTOR_TIMEOUT = 10.0


def _is_async_collect(collector: Any) -> bool:
    """Return True if the collector's collect method is a coroutine function."""
    collect = getattr(collector, 'collect', None)
    return collect is not None and inspect.iscoroutinefunction(collect)


class _CollectorFailure(Exception):
    """A single collector failed (business error or timeout).

    The family is dropped from the scrape rather than allowing partial
    output, while the scrape as a whole continues with the other families.
    """


class _EmptyCollector:
    def collect(self) -> Iterable[Metric]:
        return []


async def _collect_one(
    collector: Any,
    timeout: Optional[float],
    transform: Optional[Callable[[Metric], Optional[Metric]]] = None,
) -> List[Metric]:
    """Run a single collector and materialise its whole family at once.

    Materialising before returning guarantees that a failing collector can
    never leave a half-collected family in the output. Synchronous
    collectors are awaited inline on the running event loop; they are never
    pushed into a hidden thread pool.
    """
    if _is_async_collect(collector):
        coro: Awaitable[Iterable[Metric]] = collector.collect()
        if timeout is not None:
            try:
                result = await asyncio.wait_for(coro, timeout=timeout)
            except asyncio.TimeoutError:
                logger.error(
                    'Collector %r timed out after %.3fs; dropping its metric family.',
                    collector, timeout,
                )
                raise _CollectorFailure()
        else:
            result = await coro
    else:
        # A synchronous collector runs directly on the current event loop
        # thread. It cannot be cancelled or timed out from outside because
        # it holds the loop, and it must not be offloaded to a thread pool
        # implicitly.
        result = collector.collect()

    metrics: List[Metric] = []
    for metric in result:
        if transform is not None:
            metric = transform(metric)
            if metric is None:
                continue
        metrics.append(metric)
    return metrics


async def _run_collectors(
    collectors: List[Any],
    *,
    max_concurrency: int,
    timeout: Optional[float],
    transform: Optional[Callable[[Metric], Optional[Metric]]] = None,
) -> List[List[Metric]]:
    """Run collectors with a bounded number of concurrent coroutines.

    Collectors are scheduled in registration order and the results are
    returned in that same order, regardless of which collector finishes
    first. Per-collector timeouts and business exceptions are logged and
    cause only the affected family to be dropped (an empty list).

    If the surrounding scrape is cancelled, collectors that have already
    started are awaited to completion (or their own timeout) so their work
    converges instead of being abandoned mid-flight.
    """
    if max_concurrency < 1:
        raise ValueError('max_concurrency must be at least 1')

    semaphore = asyncio.Semaphore(max_concurrency)
    pending: Set[asyncio.Task] = set()
    # Index of the next collector to launch, in registration order.
    next_index = 0
    # Results keyed by registration index; absent entries are still running
    # or have not been scheduled yet.
    results: Dict[int, List[Metric]] = {}

    async def guarded(index: int, collector: Any) -> None:
        async with semaphore:
            try:
                results[index] = await _collect_one(collector, timeout, transform)
            except _CollectorFailure:
                results[index] = []
            except asyncio.CancelledError:
                raise
            except Exception:
                logger.exception(
                    'Collector %r raised an exception; dropping its metric family.',
                    collector,
                )
                results[index] = []

    def schedule() -> None:
        nonlocal next_index
        while len(pending) < max_concurrency and next_index < len(collectors):
            task = asyncio.ensure_future(guarded(next_index, collectors[next_index]))
            pending.add(task)
            next_index += 1

    schedule()
    try:
        while pending:
            done, pending = await asyncio.wait(pending, return_when=asyncio.FIRST_COMPLETED)
            # A task here can only fail with an error that escaped guarded();
            # normal business and timeout failures are already handled inside.
            for task in done:
                if task.cancelled():
                    continue
                exc = task.exception()
                if exc is not None:
                    logger.exception('Collector task failed.', exc_info=exc)
            schedule()
    except asyncio.CancelledError:
        # The caller cancelled the scrape. Stop launching new collectors
        # and converge the ones already started: cancel them, then await so
        # any in-flight work (including synchronous collectors) finishes
        # cleanly before propagating the cancellation.
        for task in pending:
            task.cancel()
        await asyncio.gather(*pending, return_exceptions=True)
        raise

    # Every collector has produced a result by the time the loop ends.
    return [results.get(index, []) for index in range(len(collectors))]


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
        """Yields metrics from the collectors in the registry.

        Only synchronous collectors are supported here; this method never
        starts or runs an event loop. If the registry contains asynchronous
        collectors use :meth:`async_collect` from within a running event
        loop. Each collector's family is materialised before it is yielded so
        a collector that raises partway through cannot leave a partial family
        in the output.
        """
        collectors = None
        ti = None
        with self._lock:
            collectors = list(self._collector_to_names)
            if self._target_info:
                ti = self._target_info_metric()
        if ti:
            yield ti
        for collector in collectors:
            if _is_async_collect(collector):
                raise RuntimeError(
                    'Collector {!r} provides an asynchronous collect(); '
                    'use CollectorRegistry.async_collect() within a running '
                    'event loop instead.'.format(collector),
                )
            # Materialise the whole family before yielding anything so that a
            # failure never produces a half-populated metric family.
            metrics = list(collector.collect())
            yield from metrics

    async def async_collect(
        self,
        max_concurrency: int = DEFAULT_ASYNC_CONCURRENCY,
        timeout: Optional[float] = DEFAULT_COLLECTOR_TIMEOUT,
    ) -> Iterable[Metric]:
        """Asynchronously collect metrics from all registered collectors.

        Synchronous and asynchronous collectors may be mixed in the same
        registry. At most ``max_concurrency`` collectors execute at once;
        synchronous collectors run inline on the event loop rather than in a
        hidden thread pool. Each asynchronous collector is bounded by
        ``timeout`` seconds (``None`` disables the timeout).

        Metric families are emitted in collector registration order, never in
        completion order. A collector that times out or raises a business
        exception is logged and its whole family is skipped, while the rest
        of the scrape continues. If this coroutine is cancelled, collectors
        that have already started are converged before the cancellation
        propagates.
        """
        ti = None
        with self._lock:
            collectors = list(self._collector_to_names)
            if self._target_info:
                ti = self._target_info_metric()

        families = await _run_collectors(
            collectors,
            max_concurrency=max_concurrency,
            timeout=timeout,
        )
        result: List[Metric] = []
        if ti:
            result.append(ti)
        for family in families:
            result.extend(family)
        return result

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
            if _is_async_collect(collector):
                raise RuntimeError(
                    'Collector {!r} provides an asynchronous collect(); '
                    'use RestrictedRegistry.async_collect() within a running '
                    'event loop instead.'.format(collector),
                )
            # Materialise the whole (restricted) family first so a failure
            # cannot leave a partial metric family in the output.
            restricted: List[Metric] = []
            for metric in collector.collect():
                m = metric._restricted_metric(self._name_set)
                if m:
                    restricted.append(m)
            yield from restricted

    async def async_collect(
        self,
        max_concurrency: int = DEFAULT_ASYNC_CONCURRENCY,
        timeout: Optional[float] = DEFAULT_COLLECTOR_TIMEOUT,
    ) -> Iterable[Metric]:
        """Asynchronous counterpart of :meth:`collect`.

        Runs the selected collectors through the same bounded,
        order-preserving pipeline as
        :meth:`CollectorRegistry.async_collect`, restricting each produced
        family to the named timeseries.
        """
        collectors = set(self._registry._collectors_without_names)
        target_info_metric = None
        with self._registry._lock:
            if 'target_info' in self._name_set and self._registry._target_info:
                target_info_metric = self._registry._target_info_metric()
            for name in self._name_set:
                if name != 'target_info' and name in self._registry._names_to_collectors:
                    collectors.add(self._registry._names_to_collectors[name])

        name_set = self._name_set

        def restrict(metric: Metric) -> Optional[Metric]:
            return metric._restricted_metric(name_set)

        # Sets do not preserve registration order; sort by the registry's
        # registration sequence so output families do not drift.
        registration_order = {
            collector: index
            for index, collector in enumerate(self._registry._collector_to_names)
        }
        # Nameless collectors (not keyed in _collector_to_names) keep their
        # own relative order and sort before named collectors.
        nameless_order = {
            collector: index
            for index, collector in enumerate(self._registry._collectors_without_names)
        }
        ordered_collectors = sorted(
            collectors,
            key=lambda c: (
                0 if c in nameless_order else 1,
                nameless_order.get(c, registration_order.get(c, 0)),
            ),
        )
        families = await _run_collectors(
            ordered_collectors,
            max_concurrency=max_concurrency,
            timeout=timeout,
            transform=restrict,
        )
        result: List[Metric] = []
        if target_info_metric:
            result.append(target_info_metric)
        for family in families:
            result.extend(family)
        return result


REGISTRY = CollectorRegistry(auto_describe=True)
